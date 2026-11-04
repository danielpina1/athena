package github

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// References: the issues and pull requests a message names, as Thread keys, so the router can show the Tickets of the
// pull requests and issues a candidate links (reference 007 "Messages delivered to Sessions": referenced Tickets).

var (
	refURL  = regexp.MustCompile(`https?://github\.com/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)/(?:issues|pull)/(\d+)`)
	refFull = regexp.MustCompile(`([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)#(\d+)`)
	refBare = regexp.MustCompile(`#(\d+)`)
)

// References implements the router's optional sources.References for GitHub.
func (s *Source) References(text, thread string) []string { return ParseReferences(text, thread) }

// ParseReferences returns, in the order they appear and once each, the canonical Thread keys (ThreadKey) of the issues
// and pull requests text names: GitHub issue and pull request URLs, owner/name#N, and #N, which is in the repository of
// thread (the Trigger's primary Thread); a bare #N on a Thread of no repository is left out.
func ParseReferences(text, thread string) []string {
	type ref struct {
		at  int
		key string
	}
	var refs []ref
	add := func(at int, repo, n string) {
		num, err := strconv.Atoi(n)
		if err != nil || num <= 0 || repo == "" {
			return
		}
		refs = append(refs, ref{at, ThreadKey(repo, num)})
	}
	wordAt := func(i int) bool {
		if i < 0 || i >= len(text) {
			return false
		}
		c := text[i]
		return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	for _, m := range refURL.FindAllStringSubmatchIndex(text, -1) {
		if !wordAt(m[1]) {
			add(m[0], text[m[2]:m[3]]+"/"+text[m[4]:m[5]], text[m[6]:m[7]])
		}
	}
	for _, m := range refFull.FindAllStringSubmatchIndex(text, -1) {
		if before := m[0] - 1; before >= 0 && (wordAt(before) || strings.ContainsRune("/.:-", rune(text[before]))) {
			continue
		}
		if !wordAt(m[1]) {
			add(m[0], text[m[2]:m[3]]+"/"+text[m[4]:m[5]], text[m[6]:m[7]])
		}
	}
	repo := repoOf(thread)
	for _, m := range refBare.FindAllStringSubmatchIndex(text, -1) {
		if before := m[0] - 1; before >= 0 && (wordAt(before) || strings.ContainsRune("/#&", rune(text[before]))) {
			continue
		}
		if !wordAt(m[1]) {
			add(m[0], repo, text[m[2]:m[3]])
		}
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].at < refs[j].at })
	var out []string
	seen := map[string]bool{}
	for _, r := range refs {
		if !seen[r.key] {
			seen[r.key] = true
			out = append(out, r.key)
		}
	}
	return out
}

// repoOf returns the owner/name of a GitHub Thread key (an issue's, a pull request's or a branch's), or "".
func repoOf(thread string) string {
	key, ok := strings.CutPrefix(thread, SourceName+":")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(key, "#@"); i >= 0 {
		key = key[:i]
	}
	if strings.Count(key, "/") != 1 {
		return ""
	}
	return key
}
