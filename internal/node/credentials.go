package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/danielpina1/athena/internal/redact"
)

// GitHub credentials of worker sessions: the server mints a short-lived installation token per Assignment, scoped to
// its Ticket's repository; the node holds it in memory only and hands it out solely to git's credential helper
// (`athena git-credential`) and the gh wrapper (`athena gh`) of that Assignment's own session, over the node socket.
// It never enters a session's environment, a prompt, a transcript or a log line.

// Credentials is the server's GitHub port, given to the node: a token for one repository (owner/name) with its expiry,
// and the commit identity of the App's bot.
type Credentials interface {
	Token(ctx context.Context, repo string) (string, time.Time, error)
	Identity() (login, email string)
}

const (
	// tokenRefreshMargin is how long before its expiry a held token is replaced.
	tokenRefreshMargin = 5 * time.Minute
	// tokenAssumedLife is the life of a token whose expiry the port does not say.
	tokenAssumedLife = time.Hour
	// tokenFetchTimeout bounds one request for a token.
	tokenFetchTimeout = 30 * time.Second
)

// secret is a string that prints masked, so a token that slips into a log line or an error shows as ***.
type secret string

func (secret) String() string               { return "***" }
func (secret) GoString() string             { return "***" }
func (secret) LogValue() slog.Value         { return slog.StringValue("***") }
func (secret) MarshalText() ([]byte, error) { return []byte("***"), nil }

// tokenStore holds each running Assignment's token.
type tokenStore struct {
	creds Credentials
	now   func() time.Time

	mu   sync.Mutex
	held map[string]*heldToken // by Assignment ID
	// dead holds the Assignments whose token was dropped: none is fetched for them until revive.
	dead map[string]bool
}

// heldToken is one Assignment's token; its own lock serializes fetches, so concurrent requests fetch once.
type heldToken struct {
	mu      sync.Mutex
	repo    string
	value   secret
	expires time.Time
	dropped bool
}

func newTokenStore(c Credentials) *tokenStore {
	return &tokenStore{creds: c, now: time.Now, held: map[string]*heldToken{}, dead: map[string]bool{}}
}

// token returns the Assignment's token for repo, fetching one when it holds none, holds one for another repository,
// or holds one that expires within tokenRefreshMargin.
func (ts *tokenStore) token(ctx context.Context, assignmentID, repo string) (string, error) {
	if ts.creds == nil {
		return "", errors.New("this node has no GitHub credentials")
	}
	ts.mu.Lock()
	if ts.dead[assignmentID] {
		ts.mu.Unlock()
		return "", errors.New("the assignment has ended")
	}
	h := ts.held[assignmentID]
	if h == nil {
		h = &heldToken{}
		ts.held[assignmentID] = h
	}
	ts.mu.Unlock()

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dropped {
		return "", errors.New("the assignment has ended")
	}
	if h.value != "" && strings.EqualFold(h.repo, repo) && ts.now().Before(h.expires.Add(-tokenRefreshMargin)) {
		return string(h.value), nil
	}
	fctx, cancel := context.WithTimeout(ctx, tokenFetchTimeout)
	defer cancel()
	v, exp, err := ts.creds.Token(fctx, repo)
	if err != nil {
		return "", fmt.Errorf("github token for %s: %s", repo, redact.String(err.Error()))
	}
	if v == "" {
		return "", fmt.Errorf("github token for %s: empty token", repo)
	}
	if exp.IsZero() {
		exp = ts.now().Add(tokenAssumedLife)
	}
	ts.mu.Lock()
	current := ts.held[assignmentID] == h
	ts.mu.Unlock()
	if !current {
		// Dropped while it was fetched: the Assignment ended, nothing keeps the token.
		return "", errors.New("the assignment has ended")
	}
	h.repo, h.value, h.expires = repo, secret(v), exp
	return v, nil
}

// drop forgets the Assignment's token; later requests for it are refused until revive.
func (ts *tokenStore) drop(assignmentID string) {
	ts.mu.Lock()
	h := ts.held[assignmentID]
	delete(ts.held, assignmentID)
	ts.dead[assignmentID] = true
	ts.mu.Unlock()
	if h == nil {
		return
	}
	h.mu.Lock()
	h.value, h.dropped = "", true
	h.mu.Unlock()
}

// revive lets a new session of an Assignment whose token was dropped hold one again.
func (ts *tokenStore) revive(assignmentID string) {
	ts.mu.Lock()
	delete(ts.dead, assignmentID)
	ts.mu.Unlock()
}

// holds reports whether the store keeps a token for the Assignment.
func (ts *tokenStore) holds(assignmentID string) bool {
	ts.mu.Lock()
	h := ts.held[assignmentID]
	ts.mu.Unlock()
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.value != ""
}

// --- the credential op ---

// OpCredential asks the node for the token of a session's own repository; `athena git-credential` and `athena gh`
// send it.
const OpCredential = "credential"

// Purposes of a credential request.
const (
	CredentialGit = "git"
	CredentialGh  = "gh"
	// CredentialNode is the node's own git on a clone, by the nonce of that one call.
	CredentialNode = "node"
)

// CredentialRequest asks for the token of the session of Ticket and Assignment. A git request carries git's
// credential request: it is answered only for https://github.com/<the Ticket's repository>.
type CredentialRequest struct {
	Op         string `json:"op"`
	Ticket     string `json:"ticket"`
	Assignment string `json:"assignment"`
	Purpose    string `json:"purpose"`
	Protocol   string `json:"protocol,omitempty"`
	Host       string `json:"host,omitempty"`
	Path       string `json:"path,omitempty"`
	// Nonce names the node's own git call (CredentialNode).
	Nonce string `json:"nonce,omitempty"`
}

// CredentialResponse carries the token, the repository and the session's directory, or the Error that refused it.
type CredentialResponse struct {
	OK    bool   `json:"ok"`
	Token string `json:"token,omitempty"`
	Repo  string `json:"repo,omitempty"`
	Dir   string `json:"dir,omitempty"`
	Error string `json:"error,omitempty"`
}

// handleCredential serves the token of a live session's own repository, and nothing else.
func (n *Node) handleCredential(ctx context.Context, req CredentialRequest) CredentialResponse {
	if req.Purpose == CredentialNode {
		return n.handleNodeCredential(req)
	}
	s := n.lookup(req.Ticket, req.Assignment)
	if s == nil || s.over() {
		n.log.Warn("credential request from an unknown or ended session", "ticket", req.Ticket, "assignment", req.Assignment)
		return CredentialResponse{Error: denyNoSession}
	}
	repo := s.t.Repo
	if repo == "" {
		return CredentialResponse{Error: "ticket " + s.t.ID + " works in no GitHub repository"}
	}
	switch req.Purpose {
	case CredentialGit:
		if !strings.EqualFold(req.Protocol, "https") || !strings.EqualFold(req.Host, githubHost) || !samePath(req.Path, repo) {
			s.log.Warn("git credential refused: not the ticket's repository", "protocol", req.Protocol, "host", req.Host)
			return CredentialResponse{Error: "credentials are only for https://" + githubHost + "/" + repo}
		}
	case CredentialGh:
	default:
		return CredentialResponse{Error: fmt.Sprintf("unknown credential purpose %q", req.Purpose)}
	}
	tok, err := n.tokens.token(ctx, s.a.ID, repo)
	if err != nil {
		s.log.Warn("credential unavailable", "purpose", req.Purpose, "error", err)
		return CredentialResponse{Error: err.Error()}
	}
	s.log.Info("credential served", "purpose", req.Purpose)
	return CredentialResponse{OK: true, Token: tok, Repo: repo, Dir: s.place.workDir}
}

// handleNodeCredential answers the helper of one of the node's own git calls, by its nonce, for that call's
// repository on https://github.com only.
func (n *Node) handleNodeCredential(req CredentialRequest) CredentialResponse {
	v, ok := n.grants.Load(req.Nonce)
	if req.Nonce == "" || !ok {
		n.log.Warn("node credential request with an unknown nonce")
		return CredentialResponse{Error: "unknown or expired nonce"}
	}
	g := v.(nodeGrant)
	if !strings.EqualFold(req.Protocol, "https") || !strings.EqualFold(req.Host, githubHost) || !samePath(req.Path, g.repo) {
		n.log.Warn("node credential refused: not the clone's repository", "repo", g.repo, "protocol", req.Protocol, "host", req.Host)
		return CredentialResponse{Error: "credentials are only for https://" + githubHost + "/" + g.repo}
	}
	return CredentialResponse{OK: true, Token: string(g.token), Repo: g.repo}
}

// samePath reports whether git's credential path names repo: owner/name or owner/name.git, any case.
func samePath(path, repo string) bool {
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	return path != "" && strings.EqualFold(path, repo)
}

// githubVariables are a GitHub Ticket's ATHENA_GITHUB_* execution variables: its repository, the bot account and its
// commit email, and the Issue or pull request its origin Trigger came from, with the pull request's head ref. None
// holds a credential.
func (n *Node) githubVariables(s *session) []string {
	repo := s.t.Repo
	if repo == "" || n.cfg.Credentials == nil {
		return nil
	}
	login, email := n.cfg.Credentials.Identity()
	vars := []string{"ATHENA_GITHUB_REPOSITORY=" + repo, "ATHENA_GITHUB_ACCOUNT=" + login, "ATHENA_GITHUB_COMMIT_EMAIL=" + email}
	if id := s.t.Origin.TriggerID; id != "" {
		tr, err := n.engine().Trigger(id)
		switch {
		case err != nil:
			s.log.Warn("read origin trigger", "trigger", id, "error", err)
		case strings.EqualFold(tr.Meta["repo"], repo) && tr.Meta["number"] != "":
			vars = append(vars, "ATHENA_GITHUB_NUMBER="+tr.Meta["number"])
			if tr.Meta["kind"] == "pull" && tr.Meta["head"] != "" {
				vars = append(vars, "ATHENA_GITHUB_PR_HEAD="+tr.Meta["head"])
			}
		}
	}
	return vars
}

// githubEnv is the rest of a GitHub session's environment: the shims first on PATH (gh runs `athena gh`), git's
// credential helper replaced by `athena git-credential` (asked per repository path), and the bot as the author and
// committer of every commit. It holds no token.
func (n *Node) githubEnv(s *session) []string {
	if s.t.Repo == "" || n.cfg.Credentials == nil {
		return nil
	}
	login, email := n.cfg.Credentials.Identity()
	env := []string{
		"PATH=" + filepath.Join(n.cfg.StateDir, ShimsDir) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GIT_AUTHOR_NAME=" + login, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + login, "GIT_COMMITTER_EMAIL=" + email,
	}
	return withGitConfig(env, [][2]string{
		{"credential.helper", ""}, // the owner's helpers would answer with the owner's own credentials
		{"credential.helper", "!" + shellQuote(n.cfg.AthenaBin) + " git-credential"},
		{"credential.useHttpPath", "true"},
		// The owner's global insteadOf (such as git@github.com:) would push over SSH as the owner: the longer
		// identity rewrite of the repository's URL wins, and SSH is off.
		{"url." + githubTarget(s.t.Repo) + ".insteadOf", cloneURL(s.t.Repo)},
		{"protocol.ssh.allow", "never"},
	})
}

// shellQuote quotes s for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
