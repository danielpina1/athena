package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/danielpina1/athena/internal/redact"
	"github.com/danielpina1/athena/internal/runtime/rt"
)

const (
	// checkTimeout bounds a whole Check: `codex --version` and the short-lived app-server.
	checkTimeout = 30 * time.Second
	// checkTTL is how long a Check result is reused.
	checkTTL = 10 * time.Minute
	// NotLoggedIn is Check's Why when Athena's CODEX_HOME holds no account.
	NotLoggedIn = "Codex is not logged in for Athena: run athena codex login"
)

// Runtime is Codex as a runtime of the node. It is used through a pointer: Check caches its result.
type Runtime struct {
	// Binary is the codex executable (default "codex").
	Binary string
	// Home is Athena's CODEX_HOME (<stateDir>/codex); it is required, so a Codex process never falls back to ~/.codex.
	Home string
	// Model is the default model written into config.toml; empty leaves it to Codex.
	Model string
	// AthenaBin is the athena binary the hooks and the MCP server run; Socket the node's socket.
	AthenaBin, Socket string
	// WorkRoot is the directory the hooks are checked for (the node's work root); empty is Home.
	WorkRoot string
	// KeepHome is for the `athena codex` commands: an existing config.toml is never rewritten (it is the server's, with
	// its binary and the trust Codex recorded), only compared with what this Runtime would write (Status.HomeStale);
	// it is written only when there is none. The server leaves it false and rewrites the file when it differs.
	KeepHome bool

	mu      sync.Mutex
	cached  *Status
	at      time.Time
	now     func() time.Time // tests
	timeout time.Duration    // tests; zero is checkTimeout
}

// Status is what Check found, with what `athena codex status` shows besides.
type Status struct {
	rt.Info
	// Account is the kind of account Athena's CODEX_HOME is logged in with ("chatgpt", "apiKey", ...), never its id
	// or email; empty when it is not logged in or Codex was not asked.
	Account string
	// Hooks is "trusted" when Athena's hooks are the only ones Codex runs, else why not; empty when Codex was not asked.
	Hooks string
	// AccountAsked: Codex answered account/read, so an empty Account means it is not logged in.
	AccountAsked bool
	// HomeStale (KeepHome only): the existing config.toml is not what this Runtime would write (another athena binary
	// wrote it); it was left as it is, and its hooks were not checked.
	HomeStale bool
}

// StaleHome is the Why of a KeepHome check that found another athena's config.toml.
const StaleHome = "config.toml was written by another athena (a different binary or version): restart the server to rewrite it"

// Name is "codex".
func (*Runtime) Name() string { return "codex" }

// Prepare does nothing: Codex reads Athena's CODEX_HOME, and a session's own values are -c overrides at Start.
func (*Runtime) Prepare(rt.Prepare) error { return nil }

func (r *Runtime) binary() string {
	if r.Binary == "" {
		return "codex"
	}
	return r.Binary
}

func (r *Runtime) home() Home {
	return Home{Dir: r.Home, AthenaBin: r.AthenaBin, Socket: r.Socket, Model: r.Model}
}

// writeHome writes h (WriteHome), or with KeepHome creates it only when absent (CreateHome, create-only, so a file the
// server writes meanwhile is never replaced) and otherwise reports whether the existing file differs (stale), leaving
// it as it is.
func (r *Runtime) writeHome(h Home) (stale bool, err error) {
	if !r.KeepHome {
		return false, WriteHome(h)
	}
	created, err := CreateHome(h)
	if err != nil || created {
		return false, err
	}
	_, cur, err := HomeCurrent(h)
	if err != nil {
		return false, err
	}
	return !cur, nil
}

func (r *Runtime) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// Check says whether Codex can run Athena's sessions (Status).
func (r *Runtime) Check(ctx context.Context) rt.Info { return r.Status(ctx).Info }

// Invalidate drops the cached Check result, so the next Check asks Codex again.
func (r *Runtime) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cached = nil
}

// Status runs `codex --version` and applies Supported; writes Athena's CODEX_HOME (WriteHome); starts a short-lived
// app-server (no thread), and asks it for the account (account/read) and, through TrustHooks, for Athena's hooks in
// the work root. It is Ready when the version is supported, an account is logged in and the hooks are Athena's alone;
// a version newer than VerifiedVersion is Ready but not Tested. The result is reused for 10 minutes, or until
// Invalidate; one cut short (by ctx, or by the check's own 30-second bound) is not kept. Calls are serialised.
func (r *Runtime) Status(ctx context.Context) Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cached != nil && r.clock().Sub(r.at) < checkTTL {
		return *r.cached
	}
	timeout := r.timeout
	if timeout <= 0 {
		timeout = checkTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s := r.check(cctx)
	if cctx.Err() == nil {
		r.cached, r.at = &s, r.clock()
	}
	return s
}

func (r *Runtime) check(ctx context.Context) Status {
	var s Status
	if r.Home == "" {
		s.Why = "codex: Athena's CODEX_HOME is not configured"
		return s
	}
	home := r.home()
	if abs, err := filepath.Abs(home.Dir); err == nil {
		home.Dir = abs
	}
	// The home first: Codex must not create it with looser permissions.
	if err := EnsureHomeDir(home.Dir); err != nil {
		s.Why = err.Error()
		return s
	}
	env := Env(os.Environ(), home.Dir)
	cmd := exec.CommandContext(ctx, r.binary(), "--version")
	cmd.Env = env
	cmd.WaitDelay = time.Second // a killed --version must not wait on a child holding its output
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			s.Why = "Codex is not installed (" + r.binary() + " not found)"
		} else {
			s.Why = "codex --version: " + err.Error()
		}
		return s
	}
	s.Installed = true
	v, err := ParseVersion(string(out))
	if err != nil {
		s.Why = err.Error()
		return s
	}
	s.Version = v
	ok, tested := Supported(v)
	s.Tested = tested
	if !ok {
		s.Why = fmt.Sprintf("Codex %s is older than the supported %s", v, MinVersion)
		return s
	}
	stale, err := r.writeHome(home)
	if err != nil {
		s.Why = err.Error()
		return s
	}
	// Another athena's file (stale): its hooks are not this binary's, so they cannot be judged from here.
	s.HomeStale = stale
	cwd := r.WorkRoot
	if cwd == "" {
		cwd = home.Dir
	}
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		s.Why = "codex: " + err.Error()
		return s
	}
	loggedIn, why := r.ask(ctx, home, cwd, env, !stale, &s)
	switch {
	case why != "":
		s.Why = why
	case !loggedIn:
		s.Why = NotLoggedIn
	case stale:
		s.Hooks, s.Why = "not checked: "+StaleHome, StaleHome
	case s.Hooks != "trusted":
		s.Why = s.Hooks
	default:
		s.Ready = true
		if !tested {
			s.Why = fmt.Sprintf("untested Codex version %s: newer than the verified %s", v, VerifiedVersion)
		}
	}
	return s
}

// ask runs a short-lived app-server in cwd and fills s.Account and, with hooks, s.Hooks from it; why is set when the
// server could not be asked at all.
func (r *Runtime) ask(ctx context.Context, home Home, cwd string, env []string, hooks bool, s *Status) (loggedIn bool, why string) {
	p, err := rt.StartProc(ctx, rt.ProcOptions{
		Binary: r.binary(), Args: []string{"app-server", "--listen", "stdio://", "--strict-config"},
		Dir: cwd, Env: env, CloseGrace: 5 * time.Second,
	})
	if err != nil {
		return false, "codex app-server: " + err.Error()
	}
	conn := NewConn(p.Stdout(), p.Stdin(), nil)
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		_ = conn.Run(func(m Message) {
			if len(m.ID) > 0 { // nothing a check starts asks anything; answer at once so nothing waits
				_ = conn.RespondError(m.ID, -32601, "athena: not handled during a check")
			}
		})
	}()
	defer func() {
		p.CloseStdin()
		<-ran
		p.Stdout().Close()
	}()
	failed := func(step string, err error) (bool, string) {
		msg := fmt.Sprintf("codex app-server %s: %v", step, err)
		if errors.Is(err, ErrConnClosed) {
			// The server stopped: most often --strict-config refusing config.toml, which it says on stderr.
			if tail := lastLine(p.Stderr()); tail != "" {
				msg += ": " + redact.String(tail)
			}
		}
		return false, msg
	}
	if err := conn.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "athena", "version": "check"}}, nil); err != nil {
		return failed("initialize", err)
	}
	if err := conn.Notify("initialized", nil); err != nil {
		return failed("initialized", err)
	}
	var acct struct {
		Account *struct {
			Type string `json:"type"`
		} `json:"account"`
	}
	if err := conn.Call(ctx, "account/read", nil, &acct); err != nil {
		return failed("account/read", err)
	}
	s.AccountAsked = true
	if acct.Account != nil {
		loggedIn, s.Account = true, acct.Account.Type
	}
	if !hooks {
		return loggedIn, ""
	}
	if err := TrustHooks(ctx, conn, cwd, HookSpecs(home)); err != nil {
		var hu ErrHooksUncontrolled
		if !errors.As(err, &hu) {
			return failed("hooks", err)
		}
		s.Hooks = hu.Why
	} else {
		s.Hooks = "trusted"
	}
	return loggedIn, ""
}

// Login runs `codex login` against Athena's CODEX_HOME, attached to stdin, stdout and stderr: Codex itself asks for
// the account and writes the credential (auth.json) there. withAPIKey adds --with-api-key, for which Codex reads the
// key from stdin; Athena never sees or prints it. config.toml is written first, so the login runs with Athena's
// settings (with KeepHome only when there is none; a differing one is left as it is and noted on stderr), and the
// cached Check is dropped after. The error is the command's (an *exec.ExitError when it failed).
func (r *Runtime) Login(ctx context.Context, withAPIKey bool, stdin io.Reader, stdout, stderr io.Writer) error {
	if r.Home == "" {
		return errors.New("codex: Athena's CODEX_HOME is not configured")
	}
	home := r.home()
	if abs, err := filepath.Abs(home.Dir); err == nil {
		home.Dir = abs
	}
	stale, err := r.writeHome(home)
	if err != nil {
		return err
	}
	if stale {
		fmt.Fprintf(stderr, "note: %s: %s\n", filepath.Join(home.Dir, configName), StaleHome)
	}
	args := []string{"login"}
	if withAPIKey {
		args = append(args, "--with-api-key")
	}
	cmd := exec.CommandContext(ctx, r.binary(), args...)
	cmd.Env = Env(os.Environ(), home.Dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err = cmd.Run()
	r.Invalidate()
	if herr := EnsureHomeDir(home.Dir); err == nil {
		err = herr
	}
	return err
}

// lastLine is the last non-empty line of s, at most 300 bytes of it.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	if len(l) > 300 {
		l = l[:300]
	}
	return l
}

// Env is the environment of a Codex process Athena starts: rt.ChildEnv of parent keeping Codex's own variables (CODEX_,
// OPENAI_), with CODEX_HOME set to home whatever the parent's was.
func Env(parent []string, home string) []string {
	var env []string
	for _, kv := range rt.ChildEnv(parent, nil, "CODEX_", "OPENAI_") {
		if !strings.HasPrefix(kv, "CODEX_HOME=") {
			env = append(env, kv)
		}
	}
	return append(env, "CODEX_HOME="+home)
}
