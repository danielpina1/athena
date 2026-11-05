package node

import (
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// ---------------------------------------------------------------------------
// attribution

// attributionRe matches the AI attribution a commit or pull request must not carry: a co-author trailer naming Codex
// or Claude, a "Generated with" footer, or a bot address of OpenAI or Anthropic.
var attributionRe = regexp.MustCompile(`(?i)co-authored-by:\s*(codex|claude)|generated with \[?(codex|claude code)|noreply@(openai|anthropic)\.com`)

const (
	attributionReason = "remove the AI attribution trailer or footer: commits and pull requests carry none"
	// maxMessageFile is how much of a commit message or pull request body file the rule reads.
	maxMessageFile = 64 << 10
)

// attributionRule denies a `git commit` or a `gh pr create|edit` (also through `athena gh`, or inside `sh -c`) whose
// message, trailers, title or body carries an AI attribution (attributionRe). It reads the -m/--message and --trailer
// values of a commit (or an abbreviation git accepts, --mess), the --title and --body of a pull request, and the
// message or body files they name (-F/--file/--body-file) when the file is in the ticket directory, up to 64 KiB,
// relative paths from where the command runs them (workdir, then each cd, or `git -C`). A message read from stdin
// (-F -, /dev/stdin), or from a file it cannot read there (outside the ticket directory, or not written yet), is
// looked for in the whole command, where a here-document carries it.
func attributionRule(ctx RuleContext, command, workdir string) (Decision, bool) {
	if !strings.Contains(command, "git") && !strings.Contains(command, "gh") {
		return Decision{}, false
	}
	dir := ctx.TicketDir
	if workdir != "" {
		dir = ctx.absPath(workdir)
	}
	if ctx.attributed(command, dir, 0) {
		return deny(ruleAttribution, attributionReason), true
	}
	return Decision{}, false
}

// attributed reports whether command carries an attribution, run from dir ("" when it cannot be known).
func (c RuleContext) attributed(command, dir string, depth int) bool {
	athena := baseName(c.AthenaBin)
	for _, words := range shellSegments(command) {
		words = commandStart(words)
		if len(words) == 0 {
			continue
		}
		var texts, files []string
		fdir := dir
		switch w0 := words[0]; {
		case w0 == "cd" || w0 == "pushd":
			var args []string
			for _, w := range words[1:] {
				if !strings.HasPrefix(w, "-") || w == "-" {
					args = append(args, w)
				}
			}
			dir = c.changeDir(args, dir)
		case w0 == "popd":
			dir = ""
		case w0 == "git":
			sub, rest, gdirs := gitSubcommandArgs(words[1:])
			for _, g := range gdirs {
				if fdir != "" {
					fdir = c.absPathIn(g, fdir)
				}
			}
			if sub == "commit" {
				texts, files = commitMessages(rest)
			}
		case w0 == "gh":
			texts, files = prMessages(words[1:])
		case (w0 == "athena" || (athena != "" && w0 == athena)) && len(words) > 1 && words[1] == "gh":
			texts, files = prMessages(words[2:])
		case shellCmds[w0] && depth < 3:
			for i, w := range words[1:] {
				if w == "-c" || (strings.HasPrefix(w, "-") && strings.HasSuffix(w, "c") && !strings.HasPrefix(w, "--")) {
					if i+2 < len(words) && c.attributed(words[i+2], dir, depth+1) {
						return true
					}
					break
				}
			}
		}
		for _, f := range files {
			if body, ok := c.messageFile(f, fdir); ok {
				texts = append(texts, body)
			} else {
				texts = append(texts, command)
			}
		}
		for _, t := range texts {
			if attributionRe.MatchString(t) {
				return true
			}
		}
	}
	return false
}

// stdinFiles are the paths a message file read from stdin is named by.
var stdinFiles = map[string]bool{"-": true, "/dev/stdin": true, "/dev/fd/0": true, "/proc/self/fd/0": true}

// messageFile is the start of a regular file in the ticket directory a commit message or pull request body is read
// from, p relative to dir; ok is false for stdin and for any other path, or one it cannot read.
func (c RuleContext) messageFile(p, dir string) (string, bool) {
	if p == "" || stdinFiles[p] || c.TicketDir == "" || otherTilde(p) {
		return "", false
	}
	base, ok := realpath(c.TicketDir)
	target, ok2 := realpath(c.absPathIn(p, dir))
	if !ok || !ok2 || !within(base, target) {
		return "", false
	}
	if st, err := os.Stat(target); err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	f, err := os.Open(target)
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxMessageFile))
	return string(b), err == nil
}

// commitMessages are the message texts (-m, --message, --trailer) and message files (-F, --file) of git commit's
// arguments.
func commitMessages(args []string) (texts, files []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "--":
			return texts, files
		case a == "-m":
			texts = append(texts, next())
		case a == "-F":
			files = append(files, next())
		case strings.HasPrefix(a, "--") && len(a) > 2:
			// git takes any unambiguous abbreviation of a long option (--mess).
			name, v, hasValue := strings.Cut(a[2:], "=")
			switch {
			case abbreviates(name, "message"), abbreviates(name, "trailer"):
				if !hasValue {
					v = next()
				}
				texts = append(texts, v)
			case abbreviates(name, "file"):
				if !hasValue {
					v = next()
				}
				files = append(files, v)
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// A cluster of short options (-am msg, -mmsg): -m and -F take the rest, or the next word; -C, -c and -t take a
			// value too.
		cluster:
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'm', 'F':
					v := a[j+1:]
					if v == "" {
						v = next()
					}
					if a[j] == 'm' {
						texts = append(texts, v)
					} else {
						files = append(files, v)
					}
					break cluster
				case 'C', 'c', 't':
					if a[j+1:] == "" {
						next()
					}
					break cluster
				}
			}
		}
	}
	return texts, files
}

// abbreviates reports whether name is option, or an abbreviation of it.
func abbreviates(name, option string) bool { return name != "" && strings.HasPrefix(option, name) }

// prMessages are the title and body texts and the body files of gh's arguments when they run `gh pr create` or
// `gh pr edit`; nothing otherwise.
func prMessages(args []string) (texts, files []string) {
	var pos []string
	for i := 0; i < len(args) && len(pos) < 2; i++ {
		switch a := args[i]; {
		case a == "-R" || a == "--repo":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) < 2 || pos[0] != "pr" || (pos[1] != "create" && pos[1] != "edit") {
		return nil, nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "--":
			return texts, files
		case a == "-t" || a == "--title" || a == "-b" || a == "--body":
			texts = append(texts, next())
		case a == "-F" || a == "--body-file":
			files = append(files, next())
		case strings.HasPrefix(a, "--title="), strings.HasPrefix(a, "--body="):
			_, v, _ := strings.Cut(a, "=")
			texts = append(texts, v)
		case strings.HasPrefix(a, "--body-file="):
			files = append(files, strings.TrimPrefix(a, "--body-file="))
		case len(a) > 2 && (strings.HasPrefix(a, "-t") || strings.HasPrefix(a, "-b")) && a[1] != '-':
			texts = append(texts, a[2:])
		case len(a) > 2 && strings.HasPrefix(a, "-F"):
			files = append(files, a[2:])
		}
	}
	return texts, files
}

// gitSubcommandArgs is git's subcommand (gitSubcommand), the arguments after it, and the directories of its -C
// options, in order.
func gitSubcommandArgs(args []string) (string, []string, []string) {
	var dirs []string
	for i := 0; i < len(args); i++ {
		switch w := args[i]; {
		case w == "-C":
			if i+1 < len(args) {
				dirs = append(dirs, args[i+1])
			}
			i++
		case w == "-c" || w == "--git-dir" || w == "--work-tree" || w == "--namespace" || w == "--config-env":
			i++
		case strings.HasPrefix(w, "-"):
		default:
			return w, args[i+1:], dirs
		}
	}
	return "", nil, dirs
}

// wrapperValueFlags are the short options of a wrapper that take the next word as their value; wrapperOperands, how
// many operands a wrapper takes before the command (timeout's duration).
var (
	wrapperValueFlags = map[string]string{"sudo": "ugChpDrtT", "nice": "n", "timeout": "sk", "xargs": "IPLnsdEa", "env": "uCS", "stdbuf": "ioe"}
	wrapperOperands   = map[string]int{"timeout": 1}
)

// commandStart drops what precedes a simple command's own name (variable assignments, reserved words, wrappers such as
// env, sudo or timeout with their options, option values and operands) and reduces the name to its base name.
func commandStart(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		switch b := baseName(w); {
		case assignRe.MatchString(w) || reservedWords[w]:
			words = words[1:]
		case wrapperCmds[b] || b == "timeout" || b == "stdbuf":
			words = words[1:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				f := words[0]
				words = words[1:]
				if f == "--" {
					break
				}
				if len(f) == 2 && strings.IndexByte(wrapperValueFlags[b], f[1]) >= 0 && len(words) > 0 {
					words = words[1:]
				}
			}
			words = words[min(wrapperOperands[b], len(words)):]
		default:
			out := slices.Clone(words)
			out[0] = baseName(out[0])
			return out
		}
	}
	return nil
}

// shellSegments splits a shell command into its simple commands' words, as a POSIX shell reads them: quotes ('…',
// "…", $'…') and backslashes are honoured and removed, and ;, &, |, newlines and parentheses outside quotes end a
// command. Expansions are left as written, and a here-document's lines read as commands of their own.
func shellSegments(cmd string) [][]string {
	var segs [][]string
	var words []string
	var cur strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	endSeg := func() {
		endWord()
		if len(words) > 0 {
			segs = append(segs, words)
			words = nil
		}
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == '\\':
			if i+1 < len(cmd) {
				i++
				if cmd[i] != '\n' {
					cur.WriteByte(cmd[i])
					inWord = true
				}
			}
		case c == '\'':
			end := strings.IndexByte(cmd[i+1:], '\'')
			if end < 0 {
				end = len(cmd) - i - 1
			}
			cur.WriteString(cmd[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case c == '$' && i+1 < len(cmd) && cmd[i+1] == '\'':
			for i += 2; i < len(cmd) && cmd[i] != '\''; i++ {
				if cmd[i] == '\\' && i+1 < len(cmd) {
					i++
					switch cmd[i] {
					case 'n':
						cur.WriteByte('\n')
					case 't':
						cur.WriteByte('\t')
					default:
						cur.WriteByte(cmd[i])
					}
					continue
				}
				cur.WriteByte(cmd[i])
			}
			inWord = true
		case c == '"':
			for i++; i < len(cmd) && cmd[i] != '"'; i++ {
				if cmd[i] == '\\' && i+1 < len(cmd) && strings.IndexByte("\"\\$`\n", cmd[i+1]) >= 0 {
					i++
					if cmd[i] == '\n' {
						continue
					}
				}
				cur.WriteByte(cmd[i])
			}
			inWord = true
		case c == ' ' || c == '\t':
			endWord()
		case c == '\n' || c == ';' || c == '&' || c == '|' || c == '(' || c == ')':
			endSeg()
		case c == '#' && !inWord:
			if nl := strings.IndexByte(cmd[i:], '\n'); nl >= 0 {
				i += nl - 1
			} else {
				i = len(cmd)
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	endSeg()
	return segs
}
