// Package node holds the pieces of the Athena node that sit between Claude
// Code sessions and the engine: hook rules, the hook client and the socket
// protocol types.
package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
	// VenvActive reports that the worker runs inside an active virtualenv, so
	// `pip install` is routine. Commands that activate or call a venv
	// themselves are recognised from the command text.
	VenvActive bool
}

// Rule names reported in Decision.Rule.
const (
	ruleInput         = "input"
	ruleCredential    = "credential-read"
	ruleBlocked       = "blocked-command"
	ruleWrite         = "write-outside-ticket"
	ruleSessionConfig = "session-config-write"
	ruleBashPrefix    = "bash-escalate:"
)

func allow() Decision { return Decision{Verdict: VerdictAllow} }

func deny(rule, reason string) Decision {
	return Decision{Verdict: VerdictDeny, Reason: reason, Rule: rule}
}

func escalate(rule, reason string) Decision {
	return Decision{Verdict: VerdictEscalate, Reason: reason, Rule: rule}
}

// Evaluate applies the hook rules in order: credential reads (deny), blocked
// commands (deny), writes outside the ticket dir (escalate; for Bash the
// redirections and file commands that name a target), risky Bash commands
// (escalate); anything else is allowed. Malformed input is denied.
func Evaluate(ctx RuleContext, tool string, input json.RawMessage) Decision {
	var in map[string]any
	if len(input) == 0 || json.Unmarshal(input, &in) != nil || in == nil {
		return deny(ruleInput, "tool input is not a JSON object")
	}
	if d, ok := credentialRule(ctx, tool, in); ok {
		return d
	}
	if tool == "Bash" {
		if d, ok := blockedRule(ctx, str(in, "command")); ok {
			return d
		}
	}
	if writeTools[tool] {
		if d, ok := writeRule(ctx, in); ok {
			return d
		}
	}
	if tool == "Bash" {
		if d, ok := bashWriteRule(ctx, str(in, "command")); ok {
			return d
		}
		if d, ok := bashEscalateRule(ctx, str(in, "command")); ok {
			return d
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
var credRel = []string{".ssh", ".aws", ".config/gh", ".netrc", ".codex/auth.json", ".claude.json", ".claude/.credentials.json"}

// bareCredRe matches credential names mentioned in free text (a Bash command
// or a Glob pattern), however the home directory is spelled (~, $HOME, an
// absolute path) or even when it is implied by a prior cd.
var bareCredRe = regexp.MustCompile(`(?i)(?:^|[\s/"'=:~}])(?:\.ssh(?:/|\s|$|["';|&)])|\.aws/|\.config/gh(?:/|\s|$|["';|&)])|\.netrc(?:[\s"';|&)]|$)|\.codex/auth\.json|\.credentials\.json|\.claude\.json)`)

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
		if reason := ctx.protectedText(str(in, "command")); reason != "" {
			return deny(ruleCredential, reason), true
		}
	}
	return Decision{}, false
}

// globStem is the longest leading part of a glob pattern without wildcards.
func globStem(pat string) string {
	if i := strings.IndexAny(pat, "*?[{"); i >= 0 {
		pat = pat[:i]
	}
	return strings.TrimRight(pat, "/")
}

// protectedPath returns a denial reason if the absolute path p (or what it
// resolves to) is a credential or Athena state location. With ancestors set,
// a path that contains a credential location (a recursive search from ~) is
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

	for _, cand := range variants(p) {
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
// credential files or the Athena state directory.
func (c RuleContext) protectedText(s string) string {
	if bareCredRe.MatchString(s) {
		return "reading credential files is not allowed"
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
	if len(ctx.Blocked) == 0 {
		return Decision{}, false
	}
	var blocked [][]string
	for _, b := range ctx.Blocked {
		if f := strings.Fields(b); len(f) > 0 {
			blocked = append(blocked, f)
		}
	}
	if hit := findBlocked(command, blocked, 0); hit != "" {
		return deny(ruleBlocked, "command "+hit+" is blocked for this assignment"), true
	}
	return Decision{}, false
}

// findBlocked splits command into segments and reports the blocked entry that
// starts any of them. Splitting ignores quoting on purpose: over-splitting
// only produces extra segments to check, which errs on the safe side.
func findBlocked(command string, blocked [][]string, depth int) string {
	if depth > 3 {
		return ""
	}
	for _, seg := range segmentSplit.Split(command, -1) {
		words := commandWords(seg)
		if len(words) == 0 {
			continue
		}
		for _, b := range blocked {
			if hasWordPrefix(words, b) {
				return strings.Join(b, " ")
			}
		}
		// sh -c '<script>': check the script too.
		if shellCmds[words[0]] {
			for i, w := range words[1:] {
				if w == "-c" || (strings.HasPrefix(w, "-") && strings.HasSuffix(w, "c") && !strings.HasPrefix(w, "--")) {
					script := strings.Join(words[i+2:], " ")
					if hit := findBlocked(script, blocked, depth+1); hit != "" {
						return hit
					}
					break
				}
			}
		}
	}
	return ""
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
	target, ok := realpath(ctx.absPath(p))
	if !ok {
		return escalate(ruleWrite, "write target "+p+" cannot be resolved"), true
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

// redirectRe finds an output redirection and its target: >, >>, >|, &> and N>, as in `echo x > file` or `cmd 2>>log`.
// A target that starts with & is a file descriptor (2>&1) unless it names a file (>&file).
var redirectRe = regexp.MustCompile(`(?:\d*|&)>>?\|?\s*(\S+)`)

// writeTargetCmds are commands whose arguments name the files they create or change: every argument for
// the first group, the last one for the second (cp a b, mv a b, ln -s a b).
var (
	writeAllArgs = map[string]bool{"tee": true, "touch": true, "mkdir": true, "truncate": true}
	writeLastArg = map[string]bool{"cp": true, "mv": true, "install": true, "ln": true}
)

// harmlessTargets are the devices a command may always write to.
func harmlessTarget(p string) bool {
	return p == "/dev/null" || p == "/dev/stdout" || p == "/dev/stderr" || p == "/dev/tty" || strings.HasPrefix(p, "/dev/fd/")
}

// bashWriteTargets lists the paths a shell command visibly writes: redirection targets, the files named by tee,
// touch, mkdir, truncate, cp, mv, install, ln and `dd of=`. It is best effort: a script or program can write anywhere
// without naming the path (a sandbox is the only complete answer), but a worker that is denied a Write must not get
// the same write through `echo x > file`.
func bashWriteTargets(command string, depth int) []string {
	var out []string
	for _, m := range redirectRe.FindAllStringSubmatch(command, -1) {
		t := m[1]
		if strings.HasPrefix(t, "&") {
			t = t[1:]
			if t == "-" || (t != "" && strings.Trim(t, "0123456789") == "") {
				continue
			}
		}
		if t = strings.Trim(t, `"'`); t != "" && !strings.HasPrefix(t, "(") && !harmlessTarget(t) {
			out = append(out, t)
		}
	}
	if depth > 3 {
		return out
	}
	for _, seg := range segmentSplit.Split(command, -1) {
		words := commandWords(seg)
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
		case writeAllArgs[cmd]:
			out = append(out, args...)
		case writeLastArg[cmd] && len(args) > 0:
			out = append(out, args[len(args)-1])
		case cmd == "dd":
			for _, w := range words[1:] {
				if v, ok := strings.CutPrefix(w, "of="); ok {
					out = append(out, v)
				}
			}
		case shellCmds[cmd]:
			for i, w := range words[1:] {
				if w == "-c" || (strings.HasPrefix(w, "-") && strings.HasSuffix(w, "c") && !strings.HasPrefix(w, "--")) {
					out = append(out, bashWriteTargets(strings.Join(words[i+2:], " "), depth+1)...)
					break
				}
			}
		}
	}
	return out
}

// bashWriteRule escalates a Bash command that writes outside the ticket directory, like writeRule does for the
// Write tools. A target it cannot resolve (a variable, a command substitution) is escalated too.
func bashWriteRule(ctx RuleContext, command string) (Decision, bool) {
	var base string
	if ctx.TicketDir != "" {
		base, _ = realpath(ctx.TicketDir)
	}
	for _, t := range bashWriteTargets(command, 0) {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if strings.ContainsAny(t, "$`") {
			return escalate(ruleWrite, "command writes to "+t+", which cannot be resolved"), true
		}
		target, ok := realpath(ctx.absPath(t))
		if base == "" || !ok || !within(base, target) {
			return escalate(ruleWrite, "command writes to "+t+" outside the ticket directory"), true
		}
		// The worker must not rewire its own session through the shell either.
		rel := fold(relWithin(base, target))
		if parts := strings.Split(rel, string(filepath.Separator)); parts[0] == fold(".athena") {
			return deny(ruleSessionConfig, "the .athena directory is managed by athena and cannot be written"), true
		}
	}
	return Decision{}, false
}

func bashEscalateRule(ctx RuleContext, command string) (Decision, bool) {
	for _, p := range bashEscalations {
		if p.re.MatchString(command) {
			return escalate(ruleBashPrefix+p.name, "command needs approval ("+p.name+")"), true
		}
	}
	if pipInstallRe.MatchString(command) && !ctx.VenvActive && !venvInCmdRe.MatchString(command) {
		return escalate(ruleBashPrefix+"pip-install", "pip install outside a virtualenv needs approval"), true
	}
	return Decision{}, false
}
