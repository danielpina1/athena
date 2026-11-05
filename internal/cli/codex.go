package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/danielpina1/athena/internal/runtime/codex"
	"github.com/danielpina1/athena/internal/server"
)

// Stdin is what `athena codex login` hands to `codex login` (tests replace it).
var Stdin io.Reader = os.Stdin

// codex implements `athena codex login [--with-api-key]` and `athena codex status`, on Athena's own CODEX_HOME
// (<stateDir>/codex), never the owner's ~/.codex.
func (c *command) codex(ctx context.Context, args []string) int {
	if len(args) == 0 {
		c.fs.Usage()
		return 2
	}
	switch args[0] {
	case "login":
		return c.codexLogin(ctx, args[1:])
	case "status":
		return c.codexStatus(ctx, args[1:])
	}
	fmt.Fprintf(c.stderr, "athena codex: unknown subcommand %q\n", args[0])
	c.fs.Usage()
	return 2
}

// codexRuntime is the configuration's Codex runtime and whether the configuration enables it.
func (c *command) codexRuntime() (*codex.Runtime, bool, error) {
	cfg, err := server.LoadConfig(c.stateDir)
	if err != nil {
		return nil, false, err
	}
	bin, err := server.Executable()
	if err != nil {
		return nil, false, err
	}
	return server.CodexRuntime(c.stateDir, cfg, bin, "", 0), cfg.Runtimes.Codex.On(), nil
}

// codexLogin runs `codex login` with CODEX_HOME=<stateDir>/codex, attached to the terminal: Codex asks for the account
// and stores the credential there itself. With --with-api-key Codex reads the key from stdin; Athena prints nothing of
// it.
func (c *command) codexLogin(ctx context.Context, args []string) int {
	apiKey := c.fs.Bool("with-api-key", false, "read an OpenAI API key from stdin instead of signing in with ChatGPT")
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	r, _, err := c.codexRuntime()
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.stderr, "Logging Codex in for Athena (CODEX_HOME=%s)\n", r.Home)
	if err := r.Login(ctx, *apiKey, Stdin, c.stdout, c.stderr); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return 1
		}
		return c.fail(fmt.Errorf("athena codex login: %w", err))
	}
	return 0
}

// codexStatus prints what Check found: installed, version, tested, logged in (the account's kind only), hooks. It
// exits 1 when Codex is not ready (or disabled).
func (c *command) codexStatus(ctx context.Context, args []string) int {
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	r, on, err := c.codexRuntime()
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.stdout, "Codex for Athena (CODEX_HOME=%s, binary %s)\n", r.Home, r.Binary)
	if !on {
		fmt.Fprintln(c.stdout, "disabled:  runtimes.codex.enabled is false in config.json")
		return 1
	}
	s := r.Status(ctx)
	version, loggedIn, hooks := "-", "-", s.Hooks
	if s.Version != "" {
		version = s.Version + " (tested)"
		if !s.Tested {
			version = s.Version + " (untested: newer than the verified " + codex.VerifiedVersion + ")"
		}
	}
	switch {
	case s.Account != "":
		loggedIn = "yes (" + s.Account + ")"
	case s.Hooks != "": // Codex was asked, and has no account
		loggedIn = "no"
	}
	if hooks == "" {
		hooks = "-"
	}
	ready := "yes"
	if !s.Ready {
		ready = "no: " + s.Why
	}
	fmt.Fprintf(c.stdout, "installed: %s\nversion:   %s\nlogged in: %s\nhooks:     %s\nready:     %s\n",
		yesNo(s.Installed), version, loggedIn, hooks, ready)
	if !s.Ready {
		return 1
	}
	return 0
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
