package node

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// `athena git-credential` and `athena gh`: the only ways a worker session reaches its token. Both ask the node socket
// with the session's ATHENA_TICKET and ATHENA_ASSIGNMENT; the node answers only a live session, and only for its own
// Ticket's repository.

const (
	// ShimsDir is the directory, under the state directory, of the wrappers put first on a GitHub session's PATH.
	ShimsDir = "shims"
	// shimHead and shimTail frame the gh shim: #!/bin/sh, then exec "<athena bin>" gh "$@".
	shimHead = "#!/bin/sh\nexec \""
	shimTail = "\" gh \"$@\"\n"
)

// ghRefused are gh's subcommands `athena gh` refuses: they would show, replace or outlive the token (auth), or run
// commands of the session's choosing with the token in their environment (alias, extension, config).
var ghRefused = map[string]bool{"auth": true, "alias": true, "extension": true, "ext": true, "config": true}

// RunGitCredential implements `athena git-credential [--node <nonce>] <get|store|erase>`, git's credential helper in
// GitHub sessions and, with --node, of the node's own clone and fetch.
// get reads git's request from stdin and prints username=x-access-token and the token when the node serves it for
// that request; otherwise it prints nothing, so git fails to authenticate. store and erase do nothing. It always
// exits 0, as git expects of a helper.
func RunGitCredential(args []string, stdin io.Reader, stdout, stderr io.Writer, env []string) int {
	getenv := envLookup(env)
	nonce := ""
	if len(args) >= 2 && args[0] == "--node" {
		nonce, args = args[1], args[2:]
	}
	op := ""
	if len(args) > 0 {
		op = args[0]
	}
	req := map[string]string{}
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			req[k] = v
		}
	}
	if op != "get" {
		return 0
	}
	creq := CredentialRequest{
		Op: OpCredential, Ticket: getenv("ATHENA_TICKET"), Assignment: getenv("ATHENA_ASSIGNMENT"),
		Purpose: CredentialGit, Protocol: req["protocol"], Host: req["host"], Path: req["path"],
	}
	if nonce != "" {
		creq.Ticket, creq.Assignment, creq.Purpose, creq.Nonce = "", "", CredentialNode, nonce
	}
	var resp CredentialResponse
	err := callNode(getenv("ATHENA_SOCKET"), creq, &resp)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "athena git-credential: the athena node is unreachable: %v\n", err)
	case !resp.OK:
		fmt.Fprintf(stderr, "athena git-credential: %s\n", resp.Error)
	default:
		fmt.Fprintf(stdout, "username=x-access-token\npassword=%s\n", resp.Token)
	}
	return 0
}

// RunGh implements `athena gh <args>`: it runs the real gh with the session's token in gh's environment only
// (GH_TOKEN, GH_HOST=github.com, GH_CONFIG_DIR=<TicketDir>/.athena/gh, GH_PROMPT_DISABLED=1). It refuses gh auth and
// the other subcommands in ghRefused, and returns gh's exit code. A `gh pr create` asks the node first: in a repository
// in shadow mode it is held for the owner as a Proposal of its title, body, base and draft (parsePull: any other option
// is refused), and gh does not run (exit 0). env is the session's environment.
func RunGh(args []string, stdin io.Reader, stdout, stderr io.Writer, env []string) int {
	getenv := envLookup(env)
	if sub := firstArg(args); ghRefused[sub] {
		fmt.Fprintf(stderr, "athena gh: gh %s is not available in an Athena session; Athena authenticates gh for this ticket's repository\n", sub)
		return 1
	}
	// A pull request in a repository in shadow mode is held for the owner. In a live one gh runs as it always did: the
	// same arguments, and whatever stdin a body file of - consumed given back to it.
	var consumed bytes.Buffer
	if pr, ok, perr := parsePull(args, io.TeeReader(stdin, &consumed)); ok {
		var pull *HeldPull
		if perr == nil {
			pull = &pr // else the node holds nothing; a live gh reports its own errors
		}
		var held ProposeResponse
		err := callNode(getenv("ATHENA_SOCKET"), ProposeRequest{Op: OpPropose, Ticket: getenv("ATHENA_TICKET"),
			Assignment: getenv("ATHENA_ASSIGNMENT"), Pull: pull}, &held)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "athena gh: the athena node is unreachable: %v\n", err)
			return 1
		case !held.OK && perr != nil:
			fmt.Fprintf(stderr, "athena gh: %v\n", perr)
			return 1
		case !held.OK:
			fmt.Fprintf(stderr, "athena gh: %s\n", held.Error)
			return 1
		case held.Held:
			fmt.Fprintln(stdout, shadowPullHeld)
			return 0
		}
		stdin = io.MultiReader(&consumed, stdin)
	}
	real, err := realGh(getenv("PATH"))
	if err != nil {
		fmt.Fprintf(stderr, "athena gh: %v\n", err)
		return 1
	}
	var resp CredentialResponse
	err = callNode(getenv("ATHENA_SOCKET"), CredentialRequest{
		Op: OpCredential, Ticket: getenv("ATHENA_TICKET"), Assignment: getenv("ATHENA_ASSIGNMENT"), Purpose: CredentialGh,
	}, &resp)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "athena gh: the athena node is unreachable: %v\n", err)
		return 1
	case !resp.OK:
		fmt.Fprintf(stderr, "athena gh: %s\n", resp.Error)
		return 1
	}
	cfgDir := filepath.Join(resp.Dir, ".athena", "gh")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "athena gh: %v\n", err)
		return 1
	}
	cmd := exec.Command(real, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = ghEnv(env, resp.Token, cfgDir)
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		fmt.Fprintf(stderr, "athena gh: run gh: %v\n", err)
		return 1
	}
	return 0
}

// ghEnv is the environment of the real gh: the session's, without other GitHub tokens, plus the token and settings
// that keep gh non-interactive and keep what it starts (a pager, an editor, git hooks) from being the session's choice.
func ghEnv(base []string, tok, cfgDir string) []string {
	drop := map[string]bool{"GH_TOKEN": true, "GITHUB_TOKEN": true, "GH_ENTERPRISE_TOKEN": true, "GITHUB_ENTERPRISE_TOKEN": true,
		"GH_HOST": true, "GH_CONFIG_DIR": true, "GH_PROMPT_DISABLED": true, "GH_PAGER": true, "GH_EDITOR": true, "GH_BROWSER": true}
	var env []string
	for _, kv := range base {
		if name, _, _ := strings.Cut(kv, "="); !drop[name] {
			env = append(env, kv)
		}
	}
	env = withGitConfig(env, [][2]string{{"core.hooksPath", os.DevNull}, {"core.fsmonitor", "false"}})
	return append(env, "GH_TOKEN="+tok, "GH_HOST="+githubHost, "GH_CONFIG_DIR="+cfgDir, "GH_PROMPT_DISABLED=1",
		"GH_PAGER=cat", "GH_EDITOR=false", "GH_BROWSER=false", "GH_NO_UPDATE_NOTIFIER=1")
}

// envLookup returns the value of a NAME=value list's last entry for a name.
func envLookup(env []string) func(string) string {
	return func(name string) string {
		v := ""
		for _, kv := range env {
			if k, val, ok := strings.Cut(kv, "="); ok && k == name {
				v = val
			}
		}
		return v
	}
}

// firstArg is gh's subcommand: the first argument that is not a flag.
func firstArg(args []string) string {
	for _, a := range args {
		if a == "--" {
			return ""
		}
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// realGh finds gh on PATH, skipping Athena's shims.
func realGh(path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, "gh")
		st, err := os.Stat(p)
		if err != nil || st.IsDir() || st.Mode()&0o111 == 0 || isShim(p) {
			continue
		}
		return p, nil
	}
	return "", errors.New("gh is not installed on this node (not found on PATH)")
}

func isShim(p string) bool {
	b, err := os.ReadFile(p)
	if err != nil || len(b) > 4096 {
		return false
	}
	s := string(b)
	return strings.HasPrefix(s, shimHead) && strings.HasSuffix(s, shimTail)
}

// writeShims writes <stateDir>/shims/gh, a 0700 script that runs `<athenaBin> gh`.
func writeShims(stateDir, athenaBin string) error {
	if strings.ContainsAny(athenaBin, "\"$`\\\n") {
		return fmt.Errorf("athena binary path %q cannot go in a shell script", athenaBin)
	}
	dir := filepath.Join(stateDir, ShimsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(dir, "gh")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(shimHead+athenaBin+shimTail), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o700); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
