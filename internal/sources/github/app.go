package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// jwtBackdate absorbs clock drift between this host and GitHub (GitHub's advice: iat a minute in the past).
	jwtBackdate = 60 * time.Second
	// jwtLifetime is how long past now the JWT lasts; GitHub allows at most 10 minutes.
	jwtLifetime = 9 * time.Minute
	// refreshMargin is how long before expiry an installation token is replaced.
	refreshMargin = 5 * time.Minute

	redactedToken = "ghs_…redacted"
)

// AppConfig configures the GitHub App.
type AppConfig struct {
	// AppID is the App's numeric id; it is the JWT's iss claim, sent as a string (GitHub accepts the App id or the
	// client id there).
	AppID int64
	// Slug is the App's URL name; its bot account is <slug>[bot].
	Slug string
	// KeyFile is the App's private key (PEM, PKCS#1 as GitHub issues it or PKCS#8), mode 0600.
	KeyFile string
	// InstallationID is the installation Athena works through.
	InstallationID int64
	// APIURL defaults to https://api.github.com.
	APIURL string
	// Version is the Athena version for the User-Agent; "dev" when empty.
	Version string
}

// Token is a GitHub access token. Every way of printing it, logging it or encoding it shows "ghs_…redacted"; callers
// that must send it read Value.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

func (Token) String() string       { return redactedToken }
func (Token) GoString() string     { return redactedToken }
func (Token) LogValue() slog.Value { return slog.StringValue(redactedToken) }

// Format prints the redacted form for every verb, including %d and %x, which String does not cover.
func (Token) Format(f fmt.State, verb rune) { io.WriteString(f, redactedToken) }

func (Token) MarshalText() ([]byte, error) { return []byte(redactedToken), nil }
func (Token) MarshalJSON() ([]byte, error) { return json.Marshal(redactedToken) }

// Repo is a repository the installation can reach.
type Repo struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived,omitempty"`
}

// WorkerPermissions are the permissions of a worker's repository-scoped token. It returns a new map each time.
func WorkerPermissions() map[string]string {
	return map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "write",
		"checks": "read", "actions": "read", "metadata": "read",
	}
}

// App authenticates as a GitHub App. It is safe for concurrent use.
type App struct {
	cfg  AppConfig
	key  *rsa.PrivateKey
	now  func() time.Time
	jwt  *Client // authenticates with the App's JWT: token minting
	inst *Client // authenticates with the installation token: everything else

	refresh chan struct{} // one slot: whoever holds it is refreshing the installation token
	mu      sync.Mutex    // guards tok and hasTok
	tok     Token
	hasTok  bool

	accMu   sync.Mutex // guards account
	account string     // the installation account's login, learned once
}

// NewApp reads and checks the key file and returns the App. It refuses a key file that group or others can access
// (any mode wider than 0600). hc defaults to a client with a 30 s timeout and now to time.Now.
func NewApp(cfg AppConfig, hc *http.Client, now func() time.Time) (*App, error) {
	if cfg.AppID <= 0 {
		return nil, errors.New("github app: app id is required")
	}
	if cfg.KeyFile == "" {
		return nil, errors.New("github app: key file is required")
	}
	if now == nil {
		now = time.Now
	}
	key, err := readKey(cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	a := &App{cfg: cfg, key: key, now: now, refresh: make(chan struct{}, 1)}
	if a.jwt, err = newClient(hc, cfg.APIURL, func(context.Context) (string, error) { return a.JWT() }, cfg.Version, now); err != nil {
		return nil, err
	}
	if a.inst, err = newClient(hc, cfg.APIURL, func(ctx context.Context) (string, error) {
		t, err := a.InstallationToken(ctx)
		return t.Value, err
	}, cfg.Version, now); err != nil {
		return nil, err
	}
	a.inst.onUnauthorized = a.invalidate
	return a, nil
}

// readKey loads an RSA private key from a PEM file, checking its mode on the opened file.
func readKey(path string) (*rsa.PrivateKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("github app: key file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("github app: key file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("github app: key file %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("github app: key file %s has mode %04o: group and others must have no access (chmod 0600)", path, perm)
	}
	data := make([]byte, info.Size())
	if _, err := f.ReadAt(data, 0); err != nil && info.Size() > 0 {
		return nil, fmt.Errorf("github app: key file: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("github app: key file %s holds no PEM block", path)
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, fmt.Errorf("github app: key file %s holds a %T key, want RSA", path, k)
	}
	return nil, fmt.Errorf("github app: key file %s is not a PKCS#1 or PKCS#8 RSA private key", path)
}

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))

// JWT returns a JWT signed with the App's key (RS256): iat a minute in the past, exp nine minutes ahead, iss the App id.
func (a *App) JWT() (string, error) {
	now := a.now()
	claims, err := json.Marshal(struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}{now.Add(-jwtBackdate).Unix(), now.Add(jwtLifetime).Unix(), strconv.FormatInt(a.cfg.AppID, 10)})
	if err != nil {
		return "", err
	}
	signing := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("github app: sign JWT: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// InstallationToken returns the installation's token. It is cached in memory and replaced five minutes before it
// expires; concurrent callers share one refresh. If a refresh fails while the cached token has not yet expired, that
// token is returned.
func (a *App) InstallationToken(ctx context.Context) (Token, error) {
	if tok, ok := a.cachedToken(); ok {
		return tok, nil
	}
	select {
	case a.refresh <- struct{}{}:
		defer func() { <-a.refresh }()
	case <-ctx.Done():
		return Token{}, ctx.Err()
	}
	if tok, ok := a.cachedToken(); ok { // another caller refreshed while this one waited
		return tok, nil
	}
	tok, err := a.mint(ctx, nil)
	if err != nil {
		if old, ok := a.unexpiredToken(); ok {
			return old, nil
		}
		return Token{}, err
	}
	a.mu.Lock()
	a.tok, a.hasTok = tok, true
	a.mu.Unlock()
	return tok, nil
}

func (a *App) cachedToken() (Token, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tok, a.hasTok && a.now().Before(a.tok.ExpiresAt.Add(-refreshMargin))
}

func (a *App) unexpiredToken() (Token, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tok, a.hasTok && a.now().Before(a.tok.ExpiresAt)
}

// invalidate drops the cached installation token if it is still token: a token GitHub rejected (revoked, or the clock
// is skewed). Compare-and-clear, so a token another caller has just refreshed is kept.
func (a *App) invalidate(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hasTok && a.tok.Value == token {
		a.tok, a.hasTok = Token{}, false
	}
}

// ScopedToken mints a token for one repository with the given permissions, for a worker. repo is a bare name or
// owner/name; an owner must be the installation's account (compared case-insensitively), so another owner's repository
// of the same name is refused. A nil perms asks for the installation's own permissions. The token is never cached and
// never shared with the installation token.
func (a *App) ScopedToken(ctx context.Context, repo string, perms map[string]string) (Token, error) {
	owner, name := "", repo
	if strings.Contains(repo, "/") {
		parts := strings.Split(repo, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return Token{}, fmt.Errorf("github app: repository %q is not a name or owner/name", repo)
		}
		owner, name = parts[0], parts[1]
	}
	if name == "" {
		return Token{}, errors.New("github app: scoped token needs a repository")
	}
	if owner != "" {
		account, err := a.accountLogin(ctx)
		if err != nil {
			return Token{}, err
		}
		if !strings.EqualFold(owner, account) {
			return Token{}, fmt.Errorf("github app: repository owner %q is not the installation's account %q: %w", owner, account, ErrPermanent)
		}
	}
	body := map[string]any{"repositories": []string{name}}
	if perms != nil {
		body["permissions"] = perms
	}
	return a.mint(ctx, body)
}

// Account returns the login of the account the App is installed on (fetched once, then remembered): the owner of the
// installation's repositories, and the person the default Policy allows.
func (a *App) Account(ctx context.Context) (string, error) { return a.accountLogin(ctx) }

// accountLogin is the login of the account the App is installed on, fetched once from GET /app/installations/{id}.
func (a *App) accountLogin(ctx context.Context) (string, error) {
	a.accMu.Lock()
	defer a.accMu.Unlock()
	if a.account != "" {
		return a.account, nil
	}
	if a.cfg.InstallationID <= 0 {
		return "", errors.New("github app: installation id is required")
	}
	path := "/app/installations/" + strconv.FormatInt(a.cfg.InstallationID, 10)
	var out struct {
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	notModified, err := a.jwt.Get(ctx, path, &out)
	if err != nil {
		return "", err
	}
	if notModified {
		a.jwt.Cached(path, &out)
	}
	if out.Account.Login == "" {
		return "", errors.New("github app: the installation has no account login")
	}
	a.account = out.Account.Login
	return a.account, nil
}

// mint POSTs for an installation access token, authenticated with the App's JWT.
func (a *App) mint(ctx context.Context, body map[string]any) (Token, error) {
	if a.cfg.InstallationID <= 0 {
		return Token{}, errors.New("github app: installation id is required")
	}
	var in any = struct{}{}
	if body != nil {
		in = body
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := "/app/installations/" + strconv.FormatInt(a.cfg.InstallationID, 10) + "/access_tokens"
	if err := a.jwt.Post(ctx, path, in, &out); err != nil {
		return Token{}, err
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return Token{}, errors.New("github app: access token response has no token or expiry")
	}
	return Token{Value: out.Token, ExpiresAt: out.ExpiresAt}, nil
}

// Client returns the client that authenticates with the installation token.
func (a *App) Client() *Client { return a.inst }

// Bot returns the App's bot account: its login (<slug>[bot]) and user id.
func (a *App) Bot(ctx context.Context) (login string, id int64, err error) {
	if a.cfg.Slug == "" {
		return "", 0, errors.New("github app: slug is required")
	}
	var u struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	if _, err := a.inst.Get(ctx, "/users/"+url.PathEscape(a.cfg.Slug)+"[bot]", &u); err != nil {
		return "", 0, err
	}
	return u.Login, u.ID, nil
}

// Repos lists the repositories the installation can reach.
func (a *App) Repos(ctx context.Context) ([]Repo, error) {
	var repos []Repo
	err := a.inst.Paginate(ctx, "/installation/repositories?per_page=100", func(raw json.RawMessage) error {
		var page struct {
			Repositories []Repo `json:"repositories"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		repos = append(repos, page.Repositories...)
		return nil
	})
	return repos, err
}
