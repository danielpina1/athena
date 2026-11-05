// Package prompts embeds the system-prompt text Athena gives its sessions, so a node needs no repository files.
package prompts

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"strings"
)

// Part names, in the order Parts lists them.
const (
	PartWorker        = "worker"
	PartRouter        = "router"
	PartAdmission     = "admission"
	PartReplies       = "replies"
	PartSubscriptions = "subscriptions"
)

// Part is one overridable piece of a system prompt and its compiled-in default.
type Part struct {
	Name, Default string
}

//go:embed worker.md
var worker string

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

// Worker is the system prompt appended to every worker session.
var Worker = worker

// Router is the system prompt appended to the router session: its role and routing rules, then assessment, replies
// and subscriptions, without the files' comment headers.
var Router = compose(router, admission, replies, subscriptions)

// Parts returns the overridable parts in order: worker, router, admission, replies, subscriptions. The router's parts
// are their files without comment lines, as compose joins them.
func Parts() []Part {
	return []Part{
		{PartWorker, worker},
		{PartRouter, compose1(router)},
		{PartAdmission, compose1(admission)},
		{PartReplies, compose1(replies)},
		{PartSubscriptions, compose1(subscriptions)},
	}
}

// Effective returns the worker and router prompts with each part in over replacing that part's default before the
// router parts are composed. Names that are not parts are ignored.
func Effective(over map[string]string) (workerPrompt, routerPrompt string) {
	pick := func(name, def string) string {
		if t, ok := over[name]; ok {
			return t
		}
		return def
	}
	parts := Parts()
	texts := make([]string, 0, len(parts)-1)
	for _, p := range parts[1:] {
		texts = append(texts, pick(p.Name, p.Default))
	}
	return pick(PartWorker, worker), compose(texts...)
}

// Hash is the SHA-256 of s in hex: it tells whether a default changed under an override.
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// compose1 is one part without its comment lines or surrounding blank lines.
func compose1(p string) string { return strings.TrimSuffix(compose(p), "\n") }

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

// Blank reports whether text says nothing once comment lines and blank space are dropped, as compose drops them.
func Blank(text string) bool { return compose1(text) == "" }
