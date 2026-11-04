package web

import (
	"context"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/danielpina1/athena/internal/node"
)

// Artifacts is what a Ticket left behind, as the node reads it; *node.Node is one.
type Artifacts interface {
	TicketDiff(ctx context.Context, ticketID string) (node.Diff, error)
	TicketFiles(ticketID string) ([]node.FileEntry, bool, error)
	OpenTicketFile(ticketID, rel string) (*os.File, error)
}

type diffFileJSON struct {
	Path    string `json:"path"`
	OldPath string `json:"oldPath,omitempty"`
	Status  string `json:"status"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary"`
}

type diffJSON struct {
	Base      string         `json:"base"`
	Branch    string         `json:"branch"`
	Files     []diffFileJSON `json:"files"`
	Patch     string         `json:"patch"`
	Truncated bool           `json:"truncated"`
}

type fileEntryJSON struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

// mountArtifacts registers the Ticket diff and file routes. Each needs a session and answers with Cache-Control:
// no-store; a Ticket (or file) that is not there is 404 not_found, any other failure 500 internal, logged without
// file contents.
//
//	GET /api/tickets/{id}/diff           the Ticket's branch against its base
//	GET /api/tickets/{id}/files          the files of a Ticket without a repository
//	GET /api/tickets/{id}/files/{path...} one of them, as an attachment
func mountArtifacts(mux *http.ServeMux, auth *Auth, a Artifacts, log *slog.Logger) {
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if _, ok := auth.Session(r); !ok {
				fail(w, http.StatusUnauthorized, "unauthenticated", "sign in first")
				return
			}
			h(w, r)
		}
	}
	failure := func(w http.ResponseWriter, r *http.Request, err error) {
		if errors.Is(err, node.ErrNoWorkspace) || errors.Is(err, node.ErrNotFound) {
			fail(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		log.Error("ticket artifact failed", "method", r.Method, "ticket", r.PathValue("id"), "error", err)
		fail(w, http.StatusInternalServerError, "internal", "internal error")
	}

	mux.HandleFunc("GET /api/tickets/{id}/diff", guard(func(w http.ResponseWriter, r *http.Request) {
		d, err := a.TicketDiff(r.Context(), r.PathValue("id"))
		if err != nil {
			failure(w, r, err)
			return
		}
		out := diffJSON{Base: d.Base, Branch: d.Branch, Patch: d.Patch, Truncated: d.Truncated, Files: make([]diffFileJSON, 0, len(d.Files))}
		for _, f := range d.Files {
			out.Files = append(out.Files, diffFileJSON(f))
		}
		writeJSON(w, http.StatusOK, out)
	}))

	mux.HandleFunc("GET /api/tickets/{id}/files", guard(func(w http.ResponseWriter, r *http.Request) {
		files, truncated, err := a.TicketFiles(r.PathValue("id"))
		if err != nil {
			failure(w, r, err)
			return
		}
		out := struct {
			Files     []fileEntryJSON `json:"files"`
			Truncated bool            `json:"truncated"`
		}{Files: make([]fileEntryJSON, 0, len(files)), Truncated: truncated}
		for _, f := range files {
			out.Files = append(out.Files, fileEntryJSON(f))
		}
		writeJSON(w, http.StatusOK, out)
	}))

	mux.HandleFunc("GET /api/tickets/{id}/files/{path...}", guard(func(w http.ResponseWriter, r *http.Request) {
		f, err := a.OpenTicketFile(r.PathValue("id"), r.PathValue("path"))
		if err != nil {
			failure(w, r, err)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			failure(w, r, err)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(r.PathValue("path"))}))
		http.ServeContent(w, r, "", st.ModTime(), f)
	}))
}
