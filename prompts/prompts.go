// Package prompts embeds the system-prompt text Athena gives its sessions, so a node needs no repository files.
package prompts

import (
	_ "embed"
	"strings"
)

// Worker is the system prompt appended to every worker session.
//
//go:embed worker.md
var Worker string

var (
	//go:embed router.md
	router string
	//go:embed admission.md
	admission string
	//go:embed replies.md
	replies string
	//go:embed subscriptions.md
	subscriptions string
)

// Router is the system prompt appended to the router session: its role and routing rules, then assessment, replies
// and subscriptions, without the files' comment headers.
var Router = compose(router, admission, replies, subscriptions)

// compose joins prompt files, one sentence per line, with a blank line between files and their HTML comment lines
// left out.
func compose(parts ...string) string {
	var out []string
	for _, p := range parts {
		var lines []string
		for _, l := range strings.Split(strings.TrimSpace(p), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "<!--") {
				continue
			}
			lines = append(lines, l)
		}
		out = append(out, strings.TrimSpace(strings.Join(lines, "\n")))
	}
	return strings.Join(out, "\n\n") + "\n"
}
