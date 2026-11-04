// Package redact masks secrets in text shown to a person: assignments and flags whose name looks secret,
// Authorization headers, Bearer tokens, GitHub tokens, URL userinfo and sensitive query parameters.
package redact

import (
	"regexp"
	"strings"
	"unicode"
)

// Mask replaces a secret value.
const Mask = "***"

var (
	// authHeader also matches a header printed as a Go map (Authorization:[Bearer x]).
	authHeader = regexp.MustCompile(`(?i)\b(authorization)\s*:\s*\[?(?:(?:bearer|basic|token|digest)\s+)?[^\s"'\]]+`)
	// githubToken is a GitHub token by its prefix: installation (ghs_), user (ghu_), OAuth (gho_), personal (ghp_) and
	// refresh (ghr_) tokens and fine-grained personal access tokens (github_pat_).
	githubToken = regexp.MustCompile(`\b(?:gh[opsur]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+)`)
	bearer      = regexp.MustCompile(`(?i)\b(bearer)\s+[^\s"']+`)
	userinfo    = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s@"']+@`)
	// flagArg is a long flag followed by its value as the next word: --password hunter2.
	flagArg = regexp.MustCompile(`(--[A-Za-z][A-Za-z0-9_-]*)(\s+)("[^"]*"|'[^']*'|[^\s"'-][^\s"']*)`)
	// assignment is NAME=value: an environment variable, a --flag=value or a query parameter.
	assignment = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_.-]*)=("[^"]*"|'[^']*'|[^\s&"';]*)`)
)

// secretWords are the words of a name that make its value secret.
var secretWords = map[string]bool{
	"token": true, "secret": true, "password": true, "passwd": true, "pass": true, "pwd": true, "key": true,
	"apikey": true, "auth": true, "authorization": true, "credential": true, "session": true, "cookie": true,
	"signature": true, "sig": true,
}

// String returns s with secrets masked. It is idempotent.
func String(s string) string {
	s = githubToken.ReplaceAllString(s, Mask)
	s = authHeader.ReplaceAllString(s, "$1: "+Mask)
	s = bearer.ReplaceAllString(s, "$1 "+Mask)
	s = userinfo.ReplaceAllString(s, "${1}"+Mask+"@")
	s = replace(flagArg, s, func(m []string) string {
		if !SecretName(m[1]) {
			return m[0]
		}
		return m[1] + m[2] + Mask
	})
	s = replace(assignment, s, func(m []string) string {
		if m[2] == "" || !SecretName(m[1]) {
			return m[0]
		}
		return m[1] + "=" + Mask
	})
	return s
}

// SecretName reports whether a name (an environment variable, a flag, a key) looks like it holds a secret: one of its
// words, split on '_', '-', '.' and camelCase, is a secret word such as token, password or key. A word only
// containing one, such as KEYWORD or MONKEY, does not count.
func SecretName(name string) bool {
	for _, w := range words(name) {
		w = strings.ToLower(w)
		if secretWords[w] || (strings.HasSuffix(w, "s") && secretWords[strings.TrimSuffix(w, "s")]) {
			return true
		}
	}
	return false
}

// words splits a name on non-alphanumerics and on lower-to-upper case changes.
func words(name string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	var prev rune
	for _, r := range name {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
		prev = r
	}
	flush()
	return out
}

// replace replaces every match of re in s with fn of its submatches.
func replace(re *regexp.Regexp, s string, fn func(m []string) string) string {
	idx := re.FindAllStringSubmatchIndex(s, -1)
	if idx == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range idx {
		m := make([]string, len(loc)/2)
		for i := range m {
			if loc[2*i] >= 0 {
				m[i] = s[loc[2*i]:loc[2*i+1]]
			}
		}
		b.WriteString(s[last:loc[0]])
		b.WriteString(fn(m))
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}
