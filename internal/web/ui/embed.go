// Package ui embeds the dashboard's production build (web/, built by make web into dist/).
package ui

import (
	"embed"
	"io/fs"
)

// dist holds the Vite build; only .gitkeep is committed, so go build works without Node.
//
//go:embed all:dist
var dist embed.FS

// FS is the embedded build, rooted at dist/.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic("ui: " + err.Error()) // dist is embedded, so it exists
	}
	return sub
}

// Built reports whether the build is present (index.html exists).
func Built() bool { return IsBuilt(FS()) }

// IsBuilt reports whether fsys holds a built dashboard.
func IsBuilt(fsys fs.FS) bool {
	if fsys == nil {
		return false
	}
	st, err := fs.Stat(fsys, "index.html")
	return err == nil && !st.IsDir()
}
