package web

// Nodes (plan 6): the join that enrolls one on the dashboard listener, and the node listener it then connects to over
// mutual TLS. A node makes its key itself and sends only a certificate request; the node CA signs it for the id the
// server picks. On the node listener a node is the node its verified certificate names, and only while its row is
// unrevoked and names that very certificate.

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	stdlog "log"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/pki"
	"github.com/danielpina1/athena/internal/store"
)

// The join's refusals, word for word.
const (
	joinTokenUsed    = "This join token was already used; create a new one."
	joinTokenExpired = "This join token has expired; create a new one."
	joinTokenInvalid = "This join token is not valid."
	joinNeedsTLS     = "Nodes need the dashboard on TLS."
	joinBadName      = "A node name is 1 to 32 characters of a-z, 0-9 and -, starting with a letter."
	joinBadCSR       = "The certificate request is not valid: it needs a P-256 key and its own signature."
)

func joinNameTaken(name string) string { return "A node named " + name + " already exists." }

var (
	nodeNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	nodeIDRe   = regexp.MustCompile(`^n[0-9a-f]{10}$`)
)

// NodeLinkHandler serves a node's link once its certificate has been checked. nodeID comes from that certificate,
// never from anything the node sends. ServeNode returns when the link ends. The fleet (internal/fleet) implements it.
type NodeLinkHandler interface {
	ServeNode(nodeID string, c link.Conn)
}

type joinRequest struct {
	Token string `json:"token"`
	Name  string `json:"name"`
	CSR   []byte `json:"csr"` // DER, base64 in JSON
}

type joinAnswer struct {
	NodeID  string `json:"nodeId"`
	Name    string `json:"name"`
	Cert    []byte `json:"cert"`   // the node's certificate, DER
	CACert  []byte `json:"caCert"` // the node CA that signed it, DER
	NodeURL string `json:"nodeURL"`
}

// NodeJoin serves POST /node/join on the dashboard listener, which a node with no certificate yet can reach. The body
// is {token, name, csr}; a valid unspent join token (kind store.EnrollNode) is spent, the node CA signs the CSR for
// <id>.nodes.athena, the node row is made and the answer is {nodeId, name, cert, caCert, nodeURL}. The token is
// checked first, so a caller without one learns nothing about names; a bad name, CSR or taken name is refused before
// the token is spent, so the owner can retry. nodeURL is where the node listener is (wss://<host>:<port>/node); empty
// means there is none (web TLS off), and every join is refused.
func NodeJoin(st *store.Store, ca *pki.CA, now func() time.Time, nodeURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if nodeURL == "" || ca == nil {
			fail(w, http.StatusForbidden, "forbidden", joinNeedsTLS)
			return
		}
		var req joinRequest
		if !readJSON(w, r, &req) {
			return
		}
		hash, ok := hashOf(req.Token)
		if !ok {
			refuseJoin(w, store.EnrollTokenUnknown)
			return
		}
		at := now()
		state, err := st.EnrollTokenState(hash, store.EnrollNode, at)
		if err != nil {
			internalError(w, "web: node join: token state", err)
			return
		}
		if state != store.EnrollTokenValid {
			refuseJoin(w, state)
			return
		}
		if !nodeNameRe.MatchString(req.Name) {
			fail(w, http.StatusBadRequest, "invalid", joinBadName)
			return
		}
		if csr, err := x509.ParseCertificateRequest(req.CSR); err != nil || csr.CheckSignature() != nil {
			fail(w, http.StatusBadRequest, "invalid", joinBadCSR)
			return
		}
		taken, err := nodeNameTaken(st, req.Name)
		if err != nil {
			internalError(w, "web: node join: nodes", err)
			return
		}
		if taken {
			fail(w, http.StatusBadRequest, "exists", joinNameTaken(req.Name))
			return
		}
		id, err := newNodeID(st)
		if err != nil {
			internalError(w, "web: node join: id", err)
			return
		}
		// The CSR is signed before the token is spent: a CSR the CA refuses (an RSA key) leaves the token unspent. A
		// certificate made for a join that then fails names no row, so the node listener refuses it.
		cert, serial, err := ca.SignClientCSR(req.CSR, id+"."+pki.NodeCAHost)
		if err != nil {
			fail(w, http.StatusBadRequest, "invalid", joinBadCSR)
			return
		}
		leaf, err := x509.ParseCertificate(cert)
		if err != nil {
			internalError(w, "web: node join: parse the signed certificate", err)
			return
		}
		spent, err := st.UseEnrollToken(hash, store.EnrollNode, at)
		if err != nil {
			internalError(w, "web: node join: spend token", err)
			return
		}
		if !spent {
			// Another join spent it, or it expired, since the check above.
			state, err := st.EnrollTokenState(hash, store.EnrollNode, at)
			if err != nil {
				internalError(w, "web: node join: token state", err)
				return
			}
			if state == store.EnrollTokenValid {
				state = store.EnrollTokenUsed
			}
			refuseJoin(w, state)
			return
		}
		if err := st.PutNode(store.Node{ID: id, Name: req.Name, CertSerial: serial, CreatedAt: at, CertNotAfter: leaf.NotAfter}); err != nil {
			// A concurrent join took the name after the check (the token is spent: the owner makes another).
			if taken, terr := nodeNameTaken(st, req.Name); terr == nil && taken {
				fail(w, http.StatusBadRequest, "exists", joinNameTaken(req.Name))
				return
			}
			internalError(w, "web: node join: put node", err)
			return
		}
		slog.Info("web: node joined", "node", id, "name", req.Name)
		writeJSON(w, http.StatusOK, joinAnswer{NodeID: id, Name: req.Name, Cert: cert, CACert: ca.DER(), NodeURL: nodeURL})
	})
}

// refuseJoin answers a token that is not valid with the sentence for its state.
func refuseJoin(w http.ResponseWriter, state string) {
	msg := joinTokenInvalid
	switch state {
	case store.EnrollTokenUsed:
		msg = joinTokenUsed
	case store.EnrollTokenExpired:
		msg = joinTokenExpired
	}
	slog.Info("web: node join refused", "token", state)
	fail(w, http.StatusForbidden, "forbidden", msg)
}

func internalError(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	fail(w, http.StatusInternalServerError, "internal", "internal error")
}

// nodeNameTaken reports whether a node, revoked ones included, has the name.
func nodeNameTaken(st *store.Store, name string) (bool, error) {
	nodes, err := st.Nodes()
	if err != nil {
		return false, err
	}
	for _, n := range nodes {
		if n.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// newNodeID is "n" and 10 random hex characters, one no node has: PutNode would overwrite a row with the same id.
func newNodeID(st *store.Store) (string, error) {
	for range 8 {
		b := make([]byte, 5)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		id := "n" + hex.EncodeToString(b)
		n, err := st.GetNode(id)
		if err != nil {
			return "", err
		}
		if n == nil {
			return id, nil
		}
	}
	return "", errors.New("no free node id")
}

// NodeListenerConfig is what the node listener serves.
type NodeListenerConfig struct {
	Store *store.Store
	// CA is the node CA: the only issuer of client certificates the listener accepts.
	CA *pki.CA
	// GetCertificate is the dashboard's (TLSManager.Config().GetCertificate): nodes see the same server certificate.
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	Handler        NodeLinkHandler
	Log            *slog.Logger
}

// NodeListener is the node endpoint, GET /node, as an http.Server for ServeTLS(l, "", ""). The handshake requires a
// client certificate that verifies against the node CA (tls.RequireAndVerifyClientCert), and VerifyConnection then
// refuses one whose node is unknown or revoked, or whose serial is not the one the node row holds (a resumed session
// included). The link handler gets the node id from that certificate.
func NodeListener(cfg NodeListenerConfig) (*http.Server, error) {
	if cfg.Store == nil || cfg.CA == nil || cfg.GetCertificate == nil || cfg.Handler == nil {
		return nil, errors.New("web: node listener needs a store, the node CA, a server certificate and a handler")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	caCert, err := x509.ParseCertificate(cfg.CA.DER())
	if err != nil {
		return nil, fmt.Errorf("web: node CA: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	check := func(certs []*x509.Certificate) (string, error) {
		if len(certs) == 0 {
			return "", errors.New("no client certificate")
		}
		return checkNodeCert(cfg.Store, certs[0])
	}
	tlsCfg := &tls.Config{
		MinVersion:     tls.VersionTLS13,
		NextProtos:     []string{"http/1.1"},
		GetCertificate: cfg.GetCertificate,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      pool,
		VerifyConnection: func(cs tls.ConnectionState) error {
			id, err := check(cs.PeerCertificates)
			if err != nil {
				log.Warn("node: refused at the handshake", "node", id, "reason", err)
			}
			return err
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /node", func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		id, err := check(r.TLS.PeerCertificates)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		c, err := link.Accept(w, r)
		if err != nil {
			log.Info("node: link not accepted", "node", id, "err", err)
			return
		}
		cfg.Handler.ServeNode(id, c)
	})
	return &http.Server{
		Handler:           mux,
		TLSConfig:         tlsCfg,
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){}, // HTTP/1.1 only: the link upgrades
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          stdlog.New(serverErrors{log.With("component", "node-listener")}, "", 0),
	}, nil
}

// checkNodeCert returns the node id a verified node-CA certificate names, or an error when the name is not a node id,
// the node is unknown or revoked, or its row holds another certificate's serial.
func checkNodeCert(st *store.Store, leaf *x509.Certificate) (string, error) {
	id, ok := strings.CutSuffix(leaf.Subject.CommonName, "."+pki.NodeCAHost)
	if !ok || !nodeIDRe.MatchString(id) {
		return "", errors.New("the certificate does not name a node")
	}
	n, err := st.GetNode(id)
	switch {
	case err != nil:
		return id, err
	case n == nil:
		return id, errors.New("unknown node")
	case n.RevokedAt != nil:
		return id, errors.New("node revoked")
	case n.CertSerial == "" || n.CertSerial != leaf.SerialNumber.Text(16):
		return id, errors.New("not the node's current certificate")
	}
	return id, nil
}
