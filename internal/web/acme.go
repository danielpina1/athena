package web

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
	"github.com/go-acme/lego/v4/providers/dns/exec"
	"github.com/go-acme/lego/v4/providers/dns/httpreq"
	"github.com/go-acme/lego/v4/registration"
)

// Obtainer gets a certificate for host from a public CA. certPEM is the leaf followed by its chain.
type Obtainer interface {
	Obtain(ctx context.Context, host string) (certPEM, keyPEM []byte, err error)
}

// legoObtainer is an Obtainer that answers ACME DNS-01 challenges through lego.
type legoObtainer struct {
	acmeDir    string
	cfg        ACMEConfig
	provider   challenge.Provider
	accountKey *ecdsa.PrivateKey
}

// NewLegoObtainer builds an Obtainer for cfg. The account key lives in <stateDir>/tls/acme/account.key and is created
// on first use; the DNS provider reads its credentials from lego's environment variables, including the _FILE variants.
// The provider is built here by name from the three bundled ones, so a wrong name or missing credential fails at
// startup and no network is touched until Obtain.
func NewLegoObtainer(stateDir string, c ACMEConfig) (Obtainer, error) {
	provider, err := newDNSProvider(c.Provider)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(stateDir, tlsDirName, "acme")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("web: acme: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("web: acme: %w", err)
	}
	key, err := loadOrCreateAccountKey(filepath.Join(dir, "account.key"))
	if err != nil {
		return nil, err
	}
	return &legoObtainer{acmeDir: c.Directory, cfg: c, provider: provider, accountKey: key}, nil
}

// newDNSProvider is a switch on the name rather than lego's all-providers factory, which would link every DNS
// provider into the binary.
func newDNSProvider(name string) (challenge.Provider, error) {
	switch name {
	case "cloudflare":
		p, err := cloudflare.NewDNSProvider()
		if err != nil {
			return nil, fmt.Errorf("web: acme provider cloudflare: %w", err)
		}
		return p, nil
	case "exec":
		p, err := exec.NewDNSProvider()
		if err != nil {
			return nil, fmt.Errorf("web: acme provider exec: %w", err)
		}
		return p, nil
	case "httpreq":
		p, err := httpreq.NewDNSProvider()
		if err != nil {
			return nil, fmt.Errorf("web: acme provider httpreq: %w", err)
		}
		return p, nil
	}
	return nil, fmt.Errorf("web: unknown acme provider %q (want cloudflare, exec or httpreq)", name)
}

func loadOrCreateAccountKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("web: acme: %w", err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("web: acme: %w", err)
		}
		if err := writeFileAtomic(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, fmt.Errorf("web: acme: %w", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("web: acme: %w", err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("web: acme account key %s has mode %04o: group and others must have no access (chmod 0600)", path, perm)
	}
	b, _ := pem.Decode(data)
	if b == nil || b.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("web: %s is not a PKCS#8 PEM private key", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return nil, fmt.Errorf("web: %s: %w", path, err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("web: %s is not an ECDSA key", path)
	}
	return key, nil
}

// acmeUser is the lego account: its key and, once known, its registration.
type acmeUser struct {
	email string
	reg   *registration.Resource
	key   crypto.PrivateKey
}

func (u *acmeUser) GetEmail() string                        { return u.email }
func (u *acmeUser) GetRegistration() *registration.Resource { return u.reg }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.key }

// Obtain registers the account if the CA does not know its key yet (giving the address in the configuration as
// contact and agreeing to the CA's terms of service on the owner's behalf by configuring acme), then issues host.
// lego does not take a context, so cancellation only stops waiting for it.
func (o *legoObtainer) Obtain(ctx context.Context, host string) ([]byte, []byte, error) {
	type result struct {
		cert, key []byte
		err       error
	}
	done := make(chan result, 1)
	go func() {
		cert, key, err := o.obtain(host)
		done <- result{cert, key, err}
	}()
	select {
	case r := <-done:
		return r.cert, r.key, r.err
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func (o *legoObtainer) obtain(host string) ([]byte, []byte, error) {
	user := &acmeUser{email: o.cfg.Email, key: o.accountKey}
	lc := lego.NewConfig(user)
	if o.acmeDir != "" {
		lc.CADirURL = o.acmeDir
	}
	lc.Certificate.KeyType = "P256" // certcrypto.EC256, spelled out to keep certcrypto out of the imports
	client, err := lego.NewClient(lc)
	if err != nil {
		return nil, nil, fmt.Errorf("web: acme client: %w", err)
	}
	var opts []dns01.ChallengeOption
	if len(o.cfg.Resolvers) > 0 {
		opts = append(opts, dns01.AddRecursiveNameservers(o.cfg.Resolvers))
	}
	if err := client.Challenge.SetDNS01Provider(o.provider, opts...); err != nil {
		return nil, nil, fmt.Errorf("web: acme dns-01: %w", err)
	}
	reg, err := client.Registration.ResolveAccountByKey()
	if err != nil {
		reg, err = client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
		if err != nil {
			return nil, nil, fmt.Errorf("web: acme account: %w", err)
		}
	}
	user.reg = reg
	res, err := client.Certificate.Obtain(certificate.ObtainRequest{Domains: []string{host}, Bundle: true})
	if err != nil {
		return nil, nil, fmt.Errorf("web: acme obtain %s: %w", host, err)
	}
	return res.Certificate, res.PrivateKey, nil
}
