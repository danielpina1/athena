// Package prompts embeds the system-prompt text Athena gives its sessions, so a node needs no repository files.
package prompts

import _ "embed"

// Worker is the system prompt appended to every worker session.
//
//go:embed worker.md
var Worker string
