// Package prompts embeds the system-prompt text Athena gives its sessions, so a node needs no repository files.
package prompts

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"slices"
	"strings"
)

// Part names, in the order Parts lists them.
const (
	PartWorker        = "worker"
	PartRouter        = "router"
	PartAdmission     = "admission"
	PartReplies       = "replies"
	PartSubscriptions = "subscriptions"
	PartReviewer      = "reviewer"
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
	//go:embed reviewer.md
	reviewer string
)

// Worker is the system prompt appended to every worker session.
var Worker = worker

// Router is the system prompt appended to the router session: its role and routing rules, then assessment, replies
// and subscriptions, without the files' comment headers.
var Router = compose(router, admission, replies, subscriptions)

// Reviewer is the system prompt appended to a review session (implement + review pairing): what to read, that it
// changes nothing, and the review tool's output format and verdict rule.
var Reviewer = compose1(reviewer) + "\n"

// routerParts are the parts the router prompt is composed of, in order.
var routerParts = []string{PartRouter, PartAdmission, PartReplies, PartSubscriptions}

// Parts returns the overridable parts in order: worker, router, admission, replies, subscriptions, reviewer. The
// router's and the reviewer's parts are their files without comment lines, as compose joins them.
func Parts() []Part {
	return []Part{
		{PartWorker, worker},
		{PartRouter, compose1(router)},
		{PartAdmission, compose1(admission)},
		{PartReplies, compose1(replies)},
		{PartSubscriptions, compose1(subscriptions)},
		{PartReviewer, Reviewer},
	}
}

// Effective returns the worker and router prompts with each part in over replacing that part's default before the
// router parts (routerParts, and only those) are composed. Names that are not parts are ignored.
func Effective(over map[string]string) (workerPrompt, routerPrompt string) {
	texts := make([]string, 0, len(routerParts))
	for _, p := range Parts() {
		if slices.Contains(routerParts, p.Name) {
			texts = append(texts, pick(over, p.Name, p.Default))
		}
	}
	return pick(over, PartWorker, worker), compose(texts...)
}

// EffectiveReviewer returns the reviewer prompt, the part in over replacing its default.
func EffectiveReviewer(over map[string]string) string { return pick(over, PartReviewer, Reviewer) }

// pick is over's text for the part name, or def when over has none.
func pick(over map[string]string, name, def string) string {
	if t, ok := over[name]; ok {
		return t
	}
	return def
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
