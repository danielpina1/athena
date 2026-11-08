// Package node holds the pieces of the Athena node that sit between Claude
// Code sessions and the engine: hook rules, the hook client and the socket
// protocol types.
package node

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Decision is the outcome of evaluating one tool call against the hook rules.
// Verdict is allow, deny or escalate; the node turns escalate into an Approval.
type Decision struct {
	Verdict string
	Reason  string
	Rule    string
	// Also names the other rules an escalation triggers beyond Rule (a command can need approval for several reasons):
	// a standing allow rule lets the call through only when every one of them is allowed.
	Also []string
}

// merged is escalation d with the rules of escalation o added; d stays the one shown.
func (d Decision) merged(o Decision) Decision {
	for _, r := range append([]string{o.Rule}, o.Also...) {
		if r != d.Rule && r != "" && !slices.Contains(d.Also, r) {
			d.Also = append(slices.Clone(d.Also), r)
		}
	}
	return d
}

// RuleContext carries what the rules need to know about the worker.
type RuleContext struct {
	// TicketDir is the worker's working directory; writes outside it escalate.
	TicketDir string
	// Blocked lists commands denied when they are the first word of any
	// command segment (ATHENA_BLOCKED_COMMANDS). An entry with several words
	// (for example "git push") matches as a prefix of the segment.
	Blocked []string
	// Home is the owner's home directory, used to locate credential files.
	Home string
	// StateDir is the Athena state directory; reading it is denied, except for
	// the part of it that is TicketDir.
	StateDir string
	// AthenaBin is the athena binary sessions run their hooks with; its credential commands are blocked under its own
	// name too.
	AthenaBin string
	// VenvActive reports that the worker runs inside an active virtualenv, so
	// `pip install` is routine. Commands that activate or call a venv
	// themselves are recognised from the command text.
	VenvActive bool

	// Protected are paths no write may touch, each with everything below it (ProtectedPaths): set for a session whose
	// sandbox cannot keep them read-only (Codex's). A write to one is denied.
	Protected []string

	// ReadOnly is set for a session that may change nothing (a review): every write it names is denied (readOnlyRule).
	ReadOnly bool

	// Restricted is set for a session under the restricted tool policy (toolpolicy.go): what the rules escalate is
	// denied, since nobody is asked, and so are the web tools (restrictRule).
	Restricted bool

	// Shadow is set for a session whose work is held for the owner (shadow.go: its repository is in shadow mode, or it
	// is restricted): a git push is denied (shadowPushRule).
	Shadow bool

	// Secrets are the node's secret files (Config.Secrets: the GitHub App key): no session may read them, in any
	// spelling, nor search a directory that holds one.
	Secrets []string

	// ReadRoots are, for a Restricted session, the paths besides TicketDir its file tools may read: the system's and the
	// toolchains' (restrictedReadRule).
	ReadRoots []string

	// tainted names the expandable variables a Bash command assigns or may change; set by bashWriteRule.
	tainted map[string]bool
}

// Rule names reported in Decision.Rule.
const (
	ruleInput         = "input"
	ruleCredential    = "credential-read"
	ruleCredentialEnv = "credential-env"
	ruleBlocked       = "blocked-command"
	ruleWrite         = "write-outside-ticket"
	ruleSessionConfig = "session-config-write"
	ruleBashPrefix    = "bash-escalate:"
	ruleRouterTools   = "router-tools"
	ruleProtectedPath = "protected_path"
	ruleAttribution   = "attribution"
	ruleReadOnly      = "read_only"
	ruleRestricted    = "restricted"
)

// routerToolPrefix names the tools of Athena's MCP server, the only server the router session has.
const routerToolPrefix = "mcp__athena__"

// RouterRules is the router session's only rule: it may call Athena's router tools (mcp__athena__*) and nothing else.
// The session is started without built-in tools; this denies anything that slips through anyway, with a reason the
// model reads.
func RouterRules(tool string) Decision {
	if strings.HasPrefix(tool, routerToolPrefix) && len(tool) > len(routerToolPrefix) {
		return allow()
	}
	return deny(ruleRouterTools, fmt.Sprintf("the router has only Athena's tools (%s*); %q is not available to it: "+
		"route, read and reply with those tools, and leave work to the Tickets' workers", routerToolPrefix, tool))
}

func allow() Decision { return Decision{Verdict: VerdictAllow} }

func deny(rule, reason string) Decision {
	return Decision{Verdict: VerdictDeny, Reason: reason, Rule: rule}
}

func escalate(rule, reason string) Decision {
	return Decision{Verdict: VerdictEscalate, Reason: reason, Rule: rule}
}

// Evaluate applies the hook rules in order: credential reads (deny), any write of a
// read-only session (deny), a git push in shadow mode (deny), blocked commands (deny), an AI attribution in a commit or
// pull request (deny), writes to a protected path (deny) or outside the ticket dir (escalate; for Bash the
// redirections and file commands that name a target), risky Bash commands
// (escalate); anything else is allowed. Malformed input is denied. A Bash
// input's "workdir", when set, is the directory the command runs in. A restricted session's verdict then goes
// through restrictRule.
func Evaluate(ctx RuleContext, tool string, input json.RawMessage) Decision {
	return restrictRule(ctx, tool, evaluate(ctx, tool, input))
}

// webTools are the tools that reach the web outside the sandbox: Claude Code's and Codex's.
var webTools = map[string]bool{"WebFetch": true, "WebSearch": true, "web_search": true}

// restrictRule is the restricted tool policy over the rules' verdict d on a call of tool: a session that is not
// Restricted keeps d; a restricted one's web tool is denied, and an escalation becomes a deny of the same rule, since
// nobody is asked. A deny stays as it is.
func restrictRule(ctx RuleContext, tool string, d Decision) Decision {
	if !ctx.Restricted || d.Verdict == VerdictDeny {
		return d
	}
	if webTools[tool] {
		return deny(ruleRestricted, restrictedWebReason)
	}
	if d.Verdict == VerdictEscalate {
		return deny(d.Rule, restrictedEscalateReason)
	}
	return d
}

func evaluate(ctx RuleContext, tool string, input json.RawMessage) Decision {
	var in map[string]any
	if len(input) == 0 || json.Unmarshal(input, &in) != nil || in == nil {
		return deny(ruleInput, "tool input is not a JSON object")
	}
	if d, ok := credentialRule(ctx, tool, in); ok {
		return d
	}
	if d, ok := restrictedReadRule(ctx, tool, in); ok {
		return d
	}
	if d, ok := readOnlyRule(ctx, tool, in); ok {
		return d
	}
	if tool == "Bash" {
		if d, ok := shadowPushRule(ctx, str(in, "command")); ok {
			return d
		}
		if d, ok := blockedRule(ctx, str(in, "command")); ok {
			return d
		}
		if d, ok := attributionRule(ctx, str(in, "command"), str(in, "workdir")); ok {
			return d
		}
	}
	if writeTools[tool] {
		if d, ok := writeRule(ctx, in); ok {
			return d
		}
	}
	if tool == "Bash" {
		// Every escalation the command triggers, not only the first: an allow rule for one must not pass the others.
		w, wok := bashWriteRule(ctx, str(in, "command"), str(in, "workdir"))
		e, eok := bashEscalateRule(ctx, str(in, "command"))
		switch {
		case wok && w.Verdict == VerdictDeny:
			return w
		case wok && eok:
			return w.merged(e)
		case wok:
			return w
		case eok:
			return e
		}
	}
	return allow()
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// ---------------------------------------------------------------------------
// paths

// foldCase is true where the filesystem is case-insensitive by default
// (APFS on macOS, NTFS), so ~/.SSH is ~/.ssh. EvalSymlinks keeps the caller's
// spelling, so path comparisons must fold case there.
var foldCase = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// fold maps every rune to the smallest member of its unicode.SimpleFold orbit,
// the canonical form for filesystems that fold with simple case folding: "S",
// "s" and U+017F (long s) all become "S", so ".ſsh" folds to ".Ssh" like ".ssh".
// ToLower would miss such characters.
func fold(s string) string {
	if !foldCase {
		return s
	}
	return strings.Map(func(r rune) rune {
		min := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < min {
				min = f
			}
		}
		return min
	}, s)
}

// ciFlag is the regexp flag that makes text matching follow foldCase.
func ciFlag() string {
	if foldCase {
		return "(?i)"
	}
	return ""
}

// within reports whether p equals root or lies below it. Both must be clean.
func within(root, p string) bool {
	if root == "" || p == "" {
		return false
	}
	root, p = fold(root), fold(p)
	if p == root {
		return true
	}
	if root == string(filepath.Separator) {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}

// relWithin returns p relative to root, keeping p's spelling. It requires
// within(root, p) and avoids filepath.Rel, which compares case-sensitively.
func relWithin(root, p string) string {
	// Fold-equal spellings can differ in byte length (s vs long s), so skip
	// by runes, not bytes.
	n := utf8.RuneCountInString(root)
	for i := 0; i < n && len(p) > 0; i++ {
		_, size := utf8.DecodeRuneInString(p)
		p = p[size:]
	}
	return strings.TrimLeft(p, string(filepath.Separator))
}

// otherTilde reports a ~ form absPath does not understand (~user, ~+, ~-): it names a directory this process cannot
// resolve, so it must not be read as relative to the ticket directory.
func otherTilde(p string) bool {
	return strings.HasPrefix(p, "~") && p != "~" && !strings.HasPrefix(p, "~/")
}

// absPath expands ~ and makes p absolute relative to the ticket dir. It does
// not clean the result: ".." must be applied against the real filesystem (see
// realpath), because after a symlink it does not mean what it means lexically.
func (c RuleContext) absPath(p string) string {
	sep := string(filepath.Separator)
	switch {
	case p == "~" && c.Home != "":
		return c.Home
	case strings.HasPrefix(p, "~/") && c.Home != "":
		return c.Home + sep + p[2:]
	case !filepath.IsAbs(p) && c.TicketDir != "":
		return c.TicketDir + sep + p
	}
	return p
}

// realpath resolves p the way the kernel does: component by component, left
// to right, following symlinks as they are met and applying ".." to the
// already-resolved parent. Components that do not exist are kept as written,
// so a missing file under an existing directory resolves too. ok is false only
// when resolution cannot be completed (too many links, unreadable link).
func realpath(p string) (string, bool) {
	sep := string(filepath.Separator)
	if !filepath.IsAbs(p) {
		return "", false
	}
	rest := strings.Split(p, sep)
	cur := sep
	for links := 0; len(rest) > 0; {
		comp := rest[0]
		rest = rest[1:]
		switch comp {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, comp)
		fi, err := os.Lstat(next)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if links++; links > 40 {
			return "", false
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", false
		}
		if filepath.IsAbs(target) {
			cur = sep
		}
		rest = append(strings.Split(target, sep), rest...)
	}
	return cur, true
}

// variants returns the lexically cleaned p and, when different, its
// physical form (symlinks and ".." resolved against the filesystem).
func variants(p string) []string {
	if p == "" {
		return nil
	}
	out := []string{filepath.Clean(p)}
	if r, ok := realpath(p); ok && r != out[0] {
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------------------
// credential reads

var fileTools = map[string]bool{
	"Read": true, "Grep": true, "Glob": true, "Edit": true, "Write": true,
	"MultiEdit": true, "NotebookEdit": true, "NotebookRead": true,
}

var searchTools = map[string]bool{"Grep": true, "Glob": true}

var pathKeys = []string{"file_path", "path", "notebook_path"}

// credRel are credential locations relative to the home directory.
var credRel = []string{".ssh", ".aws", ".config/gh", ".netrc", ".codex/auth.json", ".claude.json", ".claude/.credentials.json",
	".git-credentials", ".config/git/credentials"}

// bareCredRe matches credential names mentioned in free text (a Bash command
// or a Glob pattern), however the home directory is spelled (~, $HOME, an
// absolute path) or even when it is implied by a prior cd.
var bareCredRe = regexp.MustCompile(`(?i)(?:^|[\s/"'=:~}])(?:\.ssh(?:/|\s|$|["';|&)])|\.aws/|\.config/gh(?:/|\s|$|["';|&)])|\.netrc(?:[\s"';|&)]|$)|\.codex/auth\.json|\.credentials\.json|\.claude\.json|\.git-credentials|\.config/git/credentials)`)

func credentialRule(ctx RuleContext, tool string, in map[string]any) (Decision, bool) {
	if fileTools[tool] {
		for _, k := range pathKeys {
			p := str(in, k)
			if p == "" {
				continue
			}
			if reason := ctx.protectedPath(ctx.absPath(p), searchTools[tool]); reason != "" {
				return deny(ruleCredential, reason), true
			}
		}
		if tool == "Glob" {
			if pat := str(in, "pattern"); pat != "" {
				if reason := ctx.protectedText(pat); reason != "" {
					return deny(ruleCredential, reason), true
				}
				if filepath.IsAbs(pat) || strings.HasPrefix(pat, "~") {
					if reason := ctx.protectedPath(ctx.absPath(globStem(pat)), true); reason != "" {
						return deny(ruleCredential, reason), true
					}
				}
			}
		}
	}
	if tool == "Bash" {
		cmd := str(in, "command")
		if reason := ctx.protectedText(cmd); reason != "" {
			return deny(ruleCredential, reason), true
		}
		if m := tokenEnvRe.FindString(cmd); m != "" {
			return deny(ruleCredentialEnv, "commands may not use "+m+": Athena authenticates git and gh for this ticket's "+
				"repository; run them plainly"), true
		}
	}
	return Decision{}, false
}

// readTools are the file tools that only read.
var readTools = map[string]bool{"Read": true, "Grep": true, "Glob": true, "NotebookRead": true}

// restrictedReadRule keeps a Restricted session's file tools to its own directory and ReadRoots: a path they name (or,
// for an absolute Glob pattern, its stem) that lies elsewhere in any of its spellings, a symbolic link out of the
// directory included, is denied. A search with no path runs in TicketDir.
func restrictedReadRule(ctx RuleContext, tool string, in map[string]any) (Decision, bool) {
	if !ctx.Restricted || !readTools[tool] {
		return Decision{}, false
	}
	var paths []string
	for _, k := range pathKeys {
		if p := str(in, k); p != "" {
			paths = append(paths, ctx.absPath(p))
		}
	}
	if pat := str(in, "pattern"); tool == "Glob" && (filepath.IsAbs(pat) || strings.HasPrefix(pat, "~")) {
		paths = append(paths, ctx.absPath(globStem(pat)))
	}
	for _, p := range paths {
		if !ctx.restrictedReadable(p) {
			return deny(ruleRestricted, restrictedReadReason), true
		}
	}
	return Decision{}, false
}

// restrictedReadable reports whether every spelling of the absolute path p lies in TicketDir or one of ReadRoots.
func (c RuleContext) restrictedReadable(p string) bool {
	var roots []string
	for _, r := range append([]string{c.TicketDir}, c.ReadRoots...) {
		roots = append(roots, variants(r)...)
	}
	for _, cand := range variants(p) {
		if !slices.ContainsFunc(roots, func(r string) bool { return within(r, cand) }) {
			return false
		}
	}
	return true
}

// tokenEnvRe names the variables that carry or fetch a GitHub or git token, or show one in a trace; a Bash command that
// names one is denied. None of them is set in a session; naming one is an attempt to supply or reveal a token.
var tokenEnvRe = regexp.MustCompile(`\b(?:GH_TOKEN|GH_ENTERPRISE_TOKEN|GITHUB_ENTERPRISE_TOKEN|GIT_ASKPASS|SSH_ASKPASS|` +
	`GIT_TRACE_REDACT|ATHENA_STATE)\b`)

// globStem is the longest leading part of a glob pattern without wildcards.
func globStem(pat string) string {
	if i := strings.IndexAny(pat, "*?[{"); i >= 0 {
		pat = pat[:i]
	}
	return strings.TrimRight(pat, "/")
}

// protectedPath returns a denial reason if the absolute path p (or what it
// resolves to) is a credential, one of the node's secrets (in any of their
// spellings) or an Athena state location. With ancestors set, a path that
// contains a credential location or a secret (a recursive search from ~) is
// protected too.
func (c RuleContext) protectedPath(p string, ancestors bool) string {
	var creds []string // roots that are always protected
	for _, h := range variants(c.Home) {
		for _, r := range credRel {
			creds = append(creds, filepath.Join(h, filepath.FromSlash(r)))
		}
	}
	state := variants(c.StateDir)
	ticket := variants(c.TicketDir)
	var secrets []string
	for _, k := range c.Secrets {
		secrets = append(secrets, variants(k)...)
	}

	for _, cand := range variants(p) {
		for _, k := range secrets {
			if within(k, cand) {
				return "reading Athena's secret files is not allowed"
			}
			if ancestors && within(cand, k) {
				return "searching a directory that holds Athena's secret files is not allowed"
			}
		}
		for _, r := range creds {
			if within(r, cand) {
				return "reading credential files is not allowed"
			}
			if ancestors && within(cand, r) {
				return "searching a directory that contains credential files is not allowed"
			}
		}
		if c.isClaudeCredential(cand) {
			return "reading credential files is not allowed"
		}
		exempt := false
		for _, t := range ticket {
			if within(t, cand) {
				exempt = true
			}
		}
		for _, s := range state {
			if within(s, cand) && !exempt {
				return "reading the athena state directory is not allowed"
			}
			if ancestors && within(cand, s) && cand != s {
				return "searching a directory that contains the athena state directory is not allowed"
			}
		}
	}
	return ""
}

// isClaudeCredential covers the ~/.claude* family: ~/.claude.json and its
// siblings, and .credentials.json inside any ~/.claude* directory.
func (c RuleContext) isClaudeCredential(p string) bool {
	for _, h := range variants(c.Home) {
		if !within(h, p) || fold(p) == fold(h) {
			continue
		}
		parts := strings.Split(fold(relWithin(h, p)), string(filepath.Separator))
		if !strings.HasPrefix(parts[0], fold(".claude")) {
			continue
		}
		if len(parts) == 1 && strings.HasPrefix(parts[0], fold(".claude.json")) {
			return true
		}
		if len(parts) >= 2 && (parts[1] == fold(".credentials.json") || parts[1] == fold("credentials.json")) {
			return true
		}
	}
	return false
}

// protectedText checks free text (Bash command, Glob pattern) for mentions of
// credential files, the node's secrets (by name, so any path to them) or the
// Athena state directory.
func (c RuleContext) protectedText(s string) string {
	if bareCredRe.MatchString(s) {
		return "reading credential files is not allowed"
	}
	for _, k := range c.Secrets {
		if name := filepath.Base(filepath.Clean(k)); name != "." && name != string(filepath.Separator) {
			re := regexp.MustCompile(ciFlag() + `(?:^|[^\w.\-])` + regexp.QuoteMeta(name) + `(?:$|[^\w.\-])`)
			if re.MatchString(s) {
				return "reading Athena's secret files is not allowed"
			}
		}
	}
	if c.StateDir == "" {
		return ""
	}
	// Mentions of the ticket dir (which normally lives under the state dir)
	// are fine.
	for _, t := range variants(c.TicketDir) {
		re := regexp.MustCompile(ciFlag() + regexp.QuoteMeta(t) + `(/|[^\w.\-]|$)`)
		s = re.ReplaceAllString(s, "$1")
	}
	for _, st := range c.stateSpellings() {
		re := regexp.MustCompile(ciFlag() + regexp.QuoteMeta(st) + `(?:/|[^\w.\-]|$)`)
		if re.MatchString(s) {
			return "reading the athena state directory is not allowed"
		}
	}
	return ""
}

// stateSpellings lists ways a command can name the state dir.
func (c RuleContext) stateSpellings() []string {
	var out []string
	for _, st := range variants(c.StateDir) {
		out = append(out, st)
		for _, h := range variants(c.Home) {
			if within(h, st) && st != h {
				rel := filepath.ToSlash(relWithin(h, st))
				out = append(out, "~/"+rel, "$HOME/"+rel, "${HOME}/"+rel)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// blocked commands

var (
	segmentSplit = regexp.MustCompile(`&&|\|\||[;|&\n()` + "`" + `]`)
	assignRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
)

var wrapperCmds = map[string]bool{
	"sudo": true, "env": true, "command": true, "exec": true, "nohup": true,
	"time": true, "nice": true, "xargs": true, "builtin": true,
}

// reservedWords can precede a command in a shell segment.
var reservedWords = map[string]bool{
	"do": true, "then": true, "else": true, "elif": true, "if": true,
	"while": true, "until": true, "!": true, "{": true, "}": true,
}

var shellCmds = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

func blockedRule(ctx RuleContext, command string) (Decision, bool) {
	var blocked [][]string
	for _, b := range ctx.Blocked {
		if f := strings.Fields(b); len(f) > 0 {
			blocked = append(blocked, f)
		}
	}
	athena := baseName(ctx.AthenaBin)
	match := func(words []string) (string, string) {
		for _, b := range blocked {
			if hasWordPrefix(words, b) {
				return strings.Join(b, " "), "command " + strings.Join(b, " ") + " is blocked for this assignment"
			}
		}
		if hit := credentialCommand(words, athena); hit != "" {
			return hit, "command " + hit + " is blocked: it would show or replace a credential; Athena authenticates git " +
				"and gh for this ticket's repository, so run them plainly"
		}
		return "", ""
	}
	if hit, reason := findBlocked(command, match, 0); hit != "" {
		return deny(ruleBlocked, reason), true
	}
	return Decision{}, false
}

// credentialCommand reports the credential command a segment's words run, if any: gh auth, git credential (and
// git-credential-<helper>), and athena's own git-credential and gh auth, under the name "athena" or the binary's own.
func credentialCommand(words []string, athena string) string {
	w0 := words[0]
	switch {
	case strings.HasPrefix(w0, "git-credential"):
		return w0
	case w0 == "git":
		if sub := gitSubcommand(words[1:]); strings.HasPrefix(sub, "credential") {
			return "git " + sub
		}
	case w0 == "gh":
		if ghAuth(words[1:]) {
			return "gh auth"
		}
	case w0 == "athena" || (athena != "" && w0 == athena):
		switch sub := firstArg(words[1:]); {
		case sub == "git-credential":
			return "athena git-credential"
		case sub == "gh":
			if i := slices.Index(words[1:], "gh") + 1; ghAuth(words[i+1:]) {
				return "athena gh auth"
			}
		}
	}
	return ""
}

// ghAuth reports whether gh's arguments run gh auth: auth is the first word that is not a flag, or the second when
// the first may be a flag's value.
func ghAuth(args []string) bool {
	firstMayBeValue := false
	seen := 0
	for i, a := range args {
		switch {
		case a == "--":
			return false
		case strings.HasPrefix(a, "-"):
			continue
		case seen == 0:
			if a == "auth" {
				return true
			}
			firstMayBeValue = i > 0 && !strings.Contains(args[i-1], "=")
		default:
			return a == "auth" && firstMayBeValue
		}
		seen++
	}
	return false
}

// gitSubcommand is the subcommand of git's arguments: the first word that is neither a global option nor its value.
func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch w := args[i]; {
		case w == "-C" || w == "-c" || w == "--git-dir" || w == "--work-tree" || w == "--namespace" || w == "--config-env":
			i++
		case strings.HasPrefix(w, "-"):
		default:
			return w
		}
	}
	return ""
}

// findBlocked splits command into segments and reports the first segment's
// hit by match, with its reason. Splitting ignores quoting on purpose:
// over-splitting only produces extra segments to check, which errs on the
// safe side.
func findBlocked(command string, match func(words []string) (string, string), depth int) (string, string) {
	if depth > 3 {
		return "", ""
	}
	for _, seg := range segmentSplit.Split(command, -1) {
		words := commandWords(seg)
		if len(words) == 0 {
			continue
		}
		if hit, reason := match(words); hit != "" {
			return hit, reason
		}
		// sh -c '<script>': check the script too.
		if shellCmds[words[0]] {
			for i, w := range words[1:] {
				if w == "-c" || (strings.HasPrefix(w, "-") && strings.HasSuffix(w, "c") && !strings.HasPrefix(w, "--")) {
					script := strings.Join(words[i+2:], " ")
					if hit, reason := findBlocked(script, match, depth+1); hit != "" {
						return hit, reason
					}
					break
				}
			}
		}
	}
	return "", ""
}

// commandWords returns the words of one command segment from the command that runs: env assignments, shell
// reserved words and wrappers (sudo, env, nohup ...) are skipped, and the command is reduced to its base name.
func commandWords(seg string) []string {
	words := cleanWords(strings.Fields(seg))
	for len(words) > 0 {
		w := words[0]
		switch {
		case assignRe.MatchString(w) || reservedWords[w]:
			words = words[1:]
		case wrapperCmds[baseName(w)]:
			words = words[1:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				words = words[1:]
			}
		default:
			words[0] = baseName(words[0])
			return words
		}
	}
	return nil
}

func hasWordPrefix(words, prefix []string) bool {
	if len(words) < len(prefix) {
		return false
	}
	for i := range prefix {
		if words[i] != prefix[i] {
			return false
		}
	}
	return true
}

func cleanWords(ws []string) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		w = strings.Trim(w, `"'`)
		w = strings.TrimPrefix(w, `\`)
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

func baseName(w string) string {
	if i := strings.LastIndex(w, "/"); i >= 0 {
		return w[i+1:]
	}
	return w
}

// ---------------------------------------------------------------------------
// writes

var writeTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true}

func writeRule(ctx RuleContext, in map[string]any) (Decision, bool) {
	p := str(in, "file_path")
	if p == "" {
		p = str(in, "notebook_path")
	}
	if p == "" {
		return escalate(ruleWrite, "write target is missing, ask the owner"), true
	}
	if ctx.TicketDir == "" {
		return escalate(ruleWrite, "no ticket directory to confine writes to"), true
	}
	if otherTilde(p) {
		return escalate(ruleWrite, "write target "+p+" cannot be resolved"), true
	}
	target, ok := realpath(ctx.absPath(p))
	if !ok {
		return escalate(ruleWrite, "write target "+p+" cannot be resolved"), true
	}
	if ctx.protectedTarget(target) {
		return deny(ruleProtectedPath, protectedReason(p)), true
	}
	base, ok := realpath(ctx.TicketDir)
	if !ok || !within(base, target) {
		return escalate(ruleWrite, "write to "+p+" is outside the ticket directory"), true
	}
	// Files that configure the session itself: a worker must not rewire its
	// own hooks or MCP config for a resumed session. Judged on the resolved
	// path so case, symlink and ".." tricks do not dodge it.
	rel := fold(relWithin(base, target))
	parts := strings.Split(rel, string(filepath.Separator))
	if parts[0] == fold(".athena") {
		return deny(ruleSessionConfig, "the .athena directory is managed by athena and cannot be written"), true
	}
	if len(parts) == 2 && parts[0] == fold(".claude") && (parts[1] == fold("settings.json") || parts[1] == fold("settings.local.json")) {
		return escalate(ruleSessionConfig, "writing Claude Code settings needs approval"), true
	}
	return Decision{}, false
}

// ---------------------------------------------------------------------------
// read-only sessions

// readOnlyGit are the git subcommands a read-only session may run: they read the repository and change nothing.
var readOnlyGit = map[string]bool{
	"status": true, "diff": true, "log": true, "show": true, "blame": true, "grep": true, "rev-parse": true,
	"ls-files": true, "merge-base": true, "cat-file": true,
}

// readOnlyCmds are the other commands a read-only session may run, besides the test and vet runners (testRunner).
var readOnlyCmds = map[string]bool{
	"cat": true, "ls": true, "rg": true, "grep": true, "find": true, "head": true, "tail": true, "wc": true,
	"file": true, "stat": true, "cd": true, "pwd": true, "echo": true, "true": true,
}

// findWrites are find's actions that run a program or write a file.
var findWrites = map[string]bool{"-delete": true, "-exec": true, "-execdir": true, "-ok": true, "-okdir": true,
	"-fprint": true, "-fprint0": true, "-fprintf": true, "-fls": true}

// readOnlyRule denies, in a read-only session (ctx.ReadOnly), what may change anything: a write tool, any Bash write
// target (bashWriteTargets), and any command but a short list (readOnlyCommand): read-only git subcommands, reading
// commands, and the test and vet runners. A test runner runs the repository's code, which may write; that residual is
// accepted (a review must be able to run the tests), and the session's sandbox, when it has one, bounds it.
func readOnlyRule(ctx RuleContext, tool string, in map[string]any) (Decision, bool) {
	if !ctx.ReadOnly {
		return Decision{}, false
	}
	const why = "this review session is read-only (" + ruleReadOnly + "): it changes nothing; report what should change " +
		"as a finding with the review tool"
	if writeTools[tool] {
		return deny(ruleReadOnly, why), true
	}
	if tool != "Bash" {
		return Decision{}, false
	}
	command := str(in, "command")
	if targets := ctx.bashWriteTargets(command, "", 0); len(targets) > 0 {
		return deny(ruleReadOnly, why+"; the command writes to "+targets[0].path), true
	}
	if bad := readOnlyScript(command, 0); bad != "" {
		return deny(ruleReadOnly, why+"; it may run only read-only git (status, diff, log, show, blame, grep, rev-parse, "+
			"ls-files, merge-base, cat-file), cat, ls, rg, grep, find, head, tail, wc, file, stat and the test and vet "+
			"runners, not "+bad), true
	}
	return Decision{}, false
}

// readOnlyScript is the first command of a shell script a read-only session may not run (readOnlyCommand), "" when
// it may run them all. The script splits into segments as findBlocked splits it (quoting ignored, so over-split), and
// the script of a shell's -c is judged the same way.
func readOnlyScript(command string, depth int) string {
	if depth > 3 {
		return "a script nested this deep"
	}
	// The & of a redirection (&>file, 2>&1) splits nothing, as in bashWriteTargets.
	command = strings.NewReplacer(">|", ">", "&>", ampOut+">", ">&", ">"+ampFd).Replace(command)
	for _, seg := range segmentSplit.Split(command, -1) {
		seg = strings.NewReplacer(ampOut, "&", ampFd, "&").Replace(seg)
		if a := readOnlyPrefix(seg); a != "" {
			return a
		}
		words := commandWords(seg)
		if len(words) == 0 {
			continue
		}
		if shellCmds[words[0]] {
			script, ok := shellScript(words)
			if !ok {
				return words[0]
			}
			if bad := readOnlyScript(script, depth+1); bad != "" {
				return bad
			}
			continue
		}
		if !readOnlyCommand(words) {
			return strings.Join(words[:min(len(words), 2)], " ")
		}
	}
	return ""
}

// readOnlyPrefix is the first word up to a segment's command that a read-only session may not run, "" when there is
// none: a variable assignment (GIT_PAGER=… git log, env PAGER=… git log), which can name a program an allowed
// command runs; sudo; and a wrapper or command named by a path outside the system's bin directories (./bin/cat,
// /tmp/x/git), which the allowlist judges by base name only (systemPath).
func readOnlyPrefix(seg string) string {
	words := cleanWords(strings.Fields(seg))
	for len(words) > 0 {
		w := words[0]
		switch {
		case assignRe.MatchString(w):
			return w
		case reservedWords[w]:
			words = words[1:]
			continue
		case strings.Contains(w, "/") && !systemPath(w), baseName(w) == "sudo":
			return w
		case wrapperCmds[baseName(w)]:
			words = words[1:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				words = words[1:]
			}
			continue
		}
		return ""
	}
	return ""
}

// systemBins are the directories a read-only session may name a command in by its path.
var systemBins = map[string]bool{"/bin": true, "/usr/bin": true, "/usr/local/bin": true, "/opt/homebrew/bin": true,
	"/usr/sbin": true, "/sbin": true}

// systemPath reports whether w is an absolute path to a program directly in one of systemBins.
func systemPath(w string) bool {
	return filepath.IsAbs(w) && filepath.Clean(w) == w && systemBins[filepath.Dir(w)]
}

// gitRuns are the git options that run a program a read-only session did not name as a command: a configuration
// value (-c, --config-env), another exec path, a pager for grep's matches, an external diff.
func gitRuns(a string) bool {
	return a == "-c" || strings.HasPrefix(a, "--config-env") || strings.HasPrefix(a, "--exec-path") ||
		strings.HasPrefix(a, "-O") || strings.HasPrefix(a, "--open-files-in-pager") || a == "--ext-diff"
}

// shellScript is the script of a shell's words (sh -c '<script>'); ok is false when they run no -c script.
func shellScript(words []string) (string, bool) {
	for i, w := range words[1:] {
		if w == "-c" || (strings.HasPrefix(w, "-") && strings.HasSuffix(w, "c") && !strings.HasPrefix(w, "--")) {
			return strings.Join(words[i+2:], " "), true
		}
	}
	return "", false
}

// readOnlyCommand reports whether a read-only session may run one command segment's words: a read-only git subcommand
// without an output file, a reading command (find without actions that run or write, rg without a preprocessor), or a
// test or vet runner.
func readOnlyCommand(words []string) bool {
	args := words[1:]
	has := func(pred func(string) bool) bool { return slices.ContainsFunc(args, pred) }
	switch w0 := words[0]; {
	case w0 == "git":
		return readOnlyGit[gitSubcommand(args)] && !has(func(a string) bool { return strings.HasPrefix(a, "--output") || gitRuns(a) })
	case w0 == "find":
		return !has(func(a string) bool { return findWrites[a] })
	case w0 == "rg":
		return !has(func(a string) bool {
			return a == "--pre" || strings.HasPrefix(a, "--pre=") || strings.HasPrefix(a, "--hostname-bin")
		})
	case w0 == "file":
		return !has(func(a string) bool { return a == "-C" || a == "--compile" })
	case readOnlyCmds[w0]:
		return true
	}
	return testRunner(words)
}

// testRunner reports whether words run a repository's tests or vet (go test|vet, cargo test|check, make test|check,
// npm|pnpm|yarn test, pytest, python -m pytest), without an option that names an output file, or a program or build
// file of its own (runnerRuns).
func testRunner(words []string) bool {
	if slices.ContainsFunc(words[1:], func(a string) bool {
		return a == "-o" || strings.HasPrefix(a, "-o=") || strings.HasPrefix(a, "-outputdir") ||
			(strings.HasPrefix(a, "-") && strings.Contains(a, "profile"))
	}) {
		return false
	}
	sub := firstArg(words[1:])
	ok := false
	switch words[0] {
	case "go":
		ok = sub == "test" || sub == "vet"
	case "cargo":
		ok = sub == "test" || sub == "check"
	case "make":
		ok = sub == "test" || sub == "check"
	case "npm", "pnpm", "yarn":
		ok = sub == "test" || (sub == "run" && len(words) > 2 && words[2] == "test")
	case "pytest":
		ok = true
	case "python", "python3":
		ok = len(words) > 2 && words[1] == "-m" && words[2] == "pytest"
	}
	return ok && !runnerRuns(words)
}

// runnerRuns reports whether a test runner's words carry an option that names a program for it to run or a build or
// configuration file of the session's choosing, in the --opt value and --opt=value forms (and short options run
// together, -fX): go's -exec, -toolexec and -vettool; make's NAME=value, -f, --file, --makefile, -e, --eval, -C and
// --directory; cargo's --config and -Z; npm's, pnpm's and yarn's --script-shell and --node-options; pytest's -p, -c,
// --config-file, -o and --override-ini.
func runnerRuns(words []string) bool {
	tool, args := words[0], words[1:]
	if tool == "python" || tool == "python3" {
		tool, args = "pytest", words[3:]
	}
	long := func(a string, names ...string) bool {
		name, _, _ := strings.Cut(a, "=")
		return slices.Contains(names, name)
	}
	short := func(a, letters string) bool { // a single-dash option bundle with one of letters
		return len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a[1:], letters)
	}
	return slices.ContainsFunc(args, func(a string) bool {
		switch tool {
		case "go": // Go's flags take one dash or two
			name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
			return strings.HasPrefix(a, "-") && (name == "exec" || name == "toolexec" || name == "vettool")
		case "make":
			return assignRe.MatchString(a) || short(a, "fCe") ||
				long(a, "--file", "--makefile", "--eval", "--directory", "--environment-overrides")
		case "cargo":
			return strings.HasPrefix(a, "-Z") || long(a, "--config")
		case "npm", "pnpm", "yarn":
			return long(a, "--script-shell", "--node-options")
		case "pytest":
			return short(a, "pco") || long(a, "--config-file", "--override-ini")
		}
		return false
	})
}

// ---------------------------------------------------------------------------
// risky shell commands

type bashPattern struct {
	name string
	re   *regexp.Regexp
}

var (
	bashEscalations = []bashPattern{
		{"env-staging-prod", regexp.MustCompile(`--env[= ](staging|prod\S*)`)},
		{"deploy", regexp.MustCompile(`\bdeploy\b`)},
		{"kubectl", regexp.MustCompile(`\bkubectl\b`)},
		{"terraform-apply", regexp.MustCompile(`\bterraform\s+apply\b`)},
		{"brew-install", regexp.MustCompile(`\bbrew\s+install\b`)},
		{"apt-install", regexp.MustCompile(`\bapt(-get)?\s+install\b`)},
		{"npm-global-install", regexp.MustCompile(`\bnpm\s+(i|install)\b[^;&|\n]*\s(-g|--global)\b|\bnpm\s+(-g|--global)\s+(i|install)\b`)},
		{"git-force-push", regexp.MustCompile(`\bgit\s+push\b[^;&|\n]*\s(--force(-with-lease)?|-f)\b`)},
		{"git-branch-delete", regexp.MustCompile(`\bgit\s+branch\b[^;&|\n]*\s-D\b`)},
		{"git-push-delete", regexp.MustCompile(`\bgit\s+push\b[^;&|\n]*\s(--delete|-d)\b`)},
		{"gh-pr-merge", regexp.MustCompile(`\bgh\s+pr\s+merge\b`)},
	}
	pipInstallRe = regexp.MustCompile(`\bpip3?\s+install\b|-m\s+pip3?\s+install\b`)
	venvInCmdRe  = regexp.MustCompile(`\bsource\s+\S*activate\b|(?:^|[;&|\s])\.\s+\S*activate\b|\bVIRTUAL_ENV=|\bvenv\S*/bin/`)
)

// Placeholders that keep the & of a redirection (&>file, >&2) from splitting a command into segments.
const (
	ampOut = "\x01" // the & of &>
	ampFd  = "\x02" // the & of >&
)

// redirectRe finds an output redirection and its target: >, >>, >|, &> and N>, as in `echo x > file` or `cmd 2>>log`.
// Its groups: the ampFd of a >& (if any), the space before the target, the target. A >& followed at once by a number or
// - names a file descriptor (2>&1, >&-); followed by a space, or by anything else, it names a file (`>& file`, >&file).
var redirectRe = regexp.MustCompile(`(?:\d*|` + ampOut + `)>>?(` + ampFd + `)?(\s*)(\S+)`)

// writeTargetCmds are commands whose arguments name the files they create or change: every argument for
// the first group, the last one for the second (cp a b, mv a b, ln -s a b).
var (
	writeAllArgs = map[string]bool{"tee": true, "touch": true, "mkdir": true, "truncate": true}
	writeLastArg = map[string]bool{"cp": true, "mv": true, "install": true, "ln": true}
)

// harmlessTarget reports the devices a command may always write to.
func harmlessTarget(p string) bool {
	return p == "/dev/null" || p == "/dev/stdout" || p == "/dev/stderr" || p == "/dev/tty" || strings.HasPrefix(p, "/dev/fd/")
}

// writeTarget is a path a command writes and the directory it was named from; dir is "" when a cd made the working
// directory unknown.
type writeTarget struct{ path, dir string }

// shellVarRe matches the variables a target may use: the ticket directory, the current directory and the home.
var shellVarRe = regexp.MustCompile(`\$\{(ATHENA_DIR|PWD|HOME)\}|\$(ATHENA_DIR|PWD|HOME)\b`)

// expandVars replaces exactly $ATHENA_DIR, $PWD (the effective directory dir) and $HOME (and their ${} forms);
// ok is false when the result still holds a variable, a command substitution or an unknown directory.
func (c RuleContext) expandVars(p, dir string) (string, bool) {
	ok := true
	out := shellVarRe.ReplaceAllStringFunc(p, func(m string) string {
		name := strings.Trim(m, "${}")
		v := ""
		if c.tainted[name] {
			ok = false
			return ""
		}
		switch name {
		case "ATHENA_DIR":
			v = c.TicketDir
		case "PWD":
			v = dir
		case "HOME":
			v = c.Home
		}
		if v == "" {
			ok = false
		}
		return v
	})
	return out, ok && !strings.ContainsAny(out, "$`")
}

// resolveFrom makes p (after variables and ~) absolute against dir and resolves it on the filesystem.
func (c RuleContext) resolveFrom(p, dir string) (string, bool) {
	p, ok := c.expandVars(p, dir)
	if !ok {
		return "", false
	}
	if otherTilde(p) {
		return "", false
	}
	p = c.absPathIn(p, dir)
	if !filepath.IsAbs(p) {
		return "", false
	}
	return realpath(p)
}

// absPathIn is absPath for a command that runs in dir; a relative p stays relative when dir is unknown.
func (c RuleContext) absPathIn(p, dir string) string {
	if !filepath.IsAbs(p) && !strings.HasPrefix(p, "~") {
		if dir == "" {
			return p
		}
		return dir + string(filepath.Separator) + p
	}
	return c.absPath(p)
}

// bashWriteTargets lists the paths a shell command visibly writes: redirection targets, the files named by tee,
// touch, mkdir, truncate, cp, mv, install, ln and `dd of=`. Segments run in order and cd/pushd change the directory
// later relative targets are named from. It is best effort: a script or program can write anywhere without naming the
// path (a sandbox is the only complete answer), but a worker that is denied a Write must not get the same write
// through `echo x > file`.
func (c RuleContext) bashWriteTargets(command, dir string, depth int) []writeTarget {
	command = strings.ReplaceAll(command, ">|", ">") // clobber: the | must not split the command
	command = strings.ReplaceAll(command, "&>", ampOut+">")
	command = strings.ReplaceAll(command, ">&", ">"+ampFd)
	var out []writeTarget
	add := func(p, d string) { out = append(out, writeTarget{p, d}) }
	for _, seg := range segmentSplit.Split(command, -1) {
		for _, m := range redirectRe.FindAllStringSubmatch(seg, -1) {
			// A descriptor follows >& at once (2>&1, >&2, >&-); after a space, or not a number, it is a file.
			t := m[3]
			if m[1] != "" && m[2] == "" && (t == "-" || strings.Trim(t, "0123456789") == "") {
				continue
			}
			if t = strings.Trim(t, `"'`); t != "" && !strings.HasPrefix(t, "(") && !harmlessTarget(t) {
				add(t, dir)
			}
		}
		words := commandWords(strings.NewReplacer(ampOut, "&", ampFd, "&").Replace(seg))
		if len(words) == 0 {
			continue
		}
		var args []string
		for _, w := range words[1:] {
			if !strings.HasPrefix(w, "-") || w == "-" {
				args = append(args, w)
			}
		}
		switch cmd := words[0]; {
		case cmd == "cd" || cmd == "pushd":
			dir = c.changeDir(args, dir)
		case cmd == "popd":
			dir = ""
		case writeAllArgs[cmd]:
			for _, a := range args {
				add(a, dir)
			}
		case writeLastArg[cmd] && len(args) > 0:
			add(args[len(args)-1], dir)
		case cmd == "dd":
			for _, w := range words[1:] {
				if v, ok := strings.CutPrefix(w, "of="); ok {
					add(v, dir)
				}
			}
		case shellCmds[cmd] && depth <= 3:
			for i, w := range words[1:] {
				if w == "-c" || (strings.HasPrefix(w, "-") && strings.HasSuffix(w, "c") && !strings.HasPrefix(w, "--")) {
					out = append(out, c.bashWriteTargets(strings.Join(words[i+2:], " "), dir, depth+1)...)
					break
				}
			}
			// Splitting ignores quotes, so the rest of a quoted script lands in the next segments, which then run in
			// whatever directory the script's cd chose: treat it as unknown.
			if slices.ContainsFunc(words[1:], func(w string) bool { return w == "cd" || w == "pushd" || w == "popd" }) {
				dir = ""
			}
		}
	}
	return out
}

// changeDir returns the directory after `cd args` from dir; "" when it cannot be known (cd -, a variable, no
// current directory).
func (c RuleContext) changeDir(args []string, dir string) string {
	target := "~"
	switch len(args) {
	case 0:
	case 1:
		target = args[0]
	default:
		return ""
	}
	if target == "-" {
		return ""
	}
	r, ok := c.resolveFrom(target, dir)
	if !ok {
		return ""
	}
	return r
}

var (
	expandable   = []string{"ATHENA_DIR", "PWD", "HOME"}
	varChangeCmd = `export|declare|typeset|local|readonly|read|unset|for|getopts|mapfile|readarray|eval|printf`
)

// taintedVars returns the expandable variables the command assigns (NAME=, NAME+=, also after env or export) or hands
// to a builtin that can change them: their value in the command is not the one Athena set.
func taintedVars(command string) map[string]bool {
	out := map[string]bool{}
	for _, n := range expandable {
		assign := regexp.MustCompile(`\b` + n + `\s*\+?=`)
		builtin := regexp.MustCompile(`(?:^|[\s;&|(])(?:` + varChangeCmd + `)\s[^;&|\n]*\b` + n + `\b`)
		if assign.MatchString(command) || builtin.MatchString(command) {
			out[n] = true
		}
	}
	return out
}

// bashWriteRule judges a Bash command's write targets like writeRule judges a Write tool call, but over all of them:
// a target in .athena denies the command even when another target only needs an approval. A target it cannot
// resolve (an unknown variable, a command substitution, a directory a cd made unknown) escalates.
func bashWriteRule(ctx RuleContext, command, workdir string) (Decision, bool) {
	ctx.tainted = taintedVars(command)
	var base string
	if ctx.TicketDir != "" {
		base, _ = realpath(ctx.TicketDir)
	}
	// The command runs in workdir when one is named; relative targets are named from there (from nowhere known when it
	// cannot be resolved, so they escalate).
	start := base
	if workdir != "" {
		start, _ = ctx.resolveFrom(workdir, base)
	}
	var esc *Decision
	note := func(d Decision) {
		if esc == nil {
			esc = &d
		} else {
			*esc = esc.merged(d)
		}
	}
	for _, t := range ctx.bashWriteTargets(command, start, 0) {
		p := strings.TrimSpace(t.path)
		if p == "" {
			continue
		}
		target, ok := ctx.resolveFrom(p, t.dir)
		switch {
		case ok && ctx.protectedTarget(target):
			return deny(ruleProtectedPath, protectedReason(p)), true
		case !ok:
			note(escalate(ruleWrite, "command writes to "+p+", which cannot be resolved"))
			continue
		case base == "" || !within(base, target):
			note(escalate(ruleWrite, "command writes to "+p+" outside the ticket directory"))
			continue
		}
		parts := strings.Split(fold(relWithin(base, target)), string(filepath.Separator))
		if parts[0] == fold(".athena") {
			return deny(ruleSessionConfig, "the .athena directory is managed by athena and cannot be written"), true
		}
		if len(parts) == 2 && parts[0] == fold(".claude") && (parts[1] == fold("settings.json") || parts[1] == fold("settings.local.json")) {
			note(escalate(ruleSessionConfig, "writing Claude Code settings needs approval"))
		}
	}
	if esc != nil {
		return *esc, true
	}
	return Decision{}, false
}

// bashEscalateRule is the escalation of the first risky pattern the command matches, naming the rest in Also.
func bashEscalateRule(ctx RuleContext, command string) (Decision, bool) {
	var out *Decision
	note := func(d Decision) {
		if out == nil {
			out = &d
		} else {
			*out = out.merged(d)
		}
	}
	for _, p := range bashEscalations {
		if p.re.MatchString(command) {
			note(escalate(ruleBashPrefix+p.name, "command needs approval ("+p.name+")"))
		}
	}
	if pipInstallRe.MatchString(command) && !ctx.VenvActive && !venvInCmdRe.MatchString(command) {
		note(escalate(ruleBashPrefix+"pip-install", "pip install outside a virtualenv needs approval"))
	}
	if out == nil {
		return Decision{}, false
	}
	return *out, true
}

// ---------------------------------------------------------------------------
// protected paths

// protectedTarget reports whether the resolved path target is one of ctx.Protected or lies below one.
func (c RuleContext) protectedTarget(target string) bool {
	for _, p := range c.Protected {
		for _, v := range variants(p) {
			if within(v, target) {
				return true
			}
		}
	}
	return false
}

func protectedReason(p string) string {
	return "writing " + p + " is not allowed: Athena's session files and the repository's git config and hooks are protected"
}
