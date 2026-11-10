package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
)

// Artifacts is what a Ticket left behind, as the node reads it; *node.Node is one.
type Artifacts interface {
	TicketDiff(ctx context.Context, ticketID string) (node.Diff, error)
	TicketFiles(ctx context.Context, ticketID string) ([]node.FileEntry, bool, error)
	OpenTicketFile(ctx context.Context, ticketID, rel string) (*os.File, error)
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
	Status  string    `json:"status,omitempty"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

const (
	maxViewText  = 1 << 20
	maxViewImage = 8 << 20
)

// mountArtifacts registers the Ticket diff and file routes. Each needs a session and answers with Cache-Control:
// no-store; a Ticket (or file) that is not there is 404 not_found, any other failure 500 internal, logged without
// file contents.
//
//	GET /api/tickets/{id}/diff           the Ticket's branch against its base
//	GET /api/tickets/{id}/files          the files of a Ticket without a repository, or those a Ticket's worktree
//	                                     added or changed (each with its status)
//	GET /api/tickets/{id}/files/{path...} one of them, as an attachment; with ?view=1 to read in the page (serveView)
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
		if msg, ok := nodeOffline(err); ok {
			fail(w, http.StatusServiceUnavailable, CodeUnavailable, msg)
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
		files, truncated, err := a.TicketFiles(r.Context(), r.PathValue("id"))
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
		f, err := a.OpenTicketFile(r.Context(), r.PathValue("id"), r.PathValue("path"))
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
		name := path.Base(r.PathValue("path"))
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		if r.URL.Query().Get("view") == "1" {
			serveView(w, r, f, st.Size(), name, failure)
			return
		}
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", disposition("attachment", name))
		http.ServeContent(w, r, "", st.ModTime(), f)
	}))
}

// nodeOffline is the sentence of an error from a node that is not connected ("<name> is offline").
func nodeOffline(err error) (string, bool) {
	if !errors.Is(err, core.ErrNodeOffline) {
		return "", false
	}
	var ue *core.UserError
	if errors.As(err, &ue) && ue.Msg != "" {
		return ue.Msg, true
	}
	return "the ticket's node is offline", true
}

// disposition is a Content-Disposition of kind (attachment or inline) with the file's name, when it can be quoted.
func disposition(kind, name string) string {
	if d := mime.FormatMediaType(kind, map[string]string{"filename": name}); d != "" {
		return d
	}
	return kind
}

// viewImages are the images a page may show inline, by extension. SVG is not one: it can carry script, so it is
// served as text like HTML.
var viewImages = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

// serveView serves a Ticket's file to read in the page (GET …/files/{path...}?view=1): an allowed image as itself, up to
// 8 MiB (past that 413 too_large), and anything else as text/plain, its first 1 MiB (cut at a character boundary;
// X-Athena-Truncated: true says it was cut). X-Athena-Size is the file's whole size. The response is inline, never
// sniffed, and carries a sandbox policy with nothing allowed, added to the app's own: opened by itself, it runs and
// loads nothing.
func serveView(w http.ResponseWriter, r *http.Request, f io.Reader, size int64, name string, failure func(http.ResponseWriter, *http.Request, error)) {
	h := w.Header()
	h.Add("Content-Security-Policy", "sandbox; default-src 'none'")
	ctype, image := viewImages[strings.ToLower(path.Ext(name))]
	limit := int64(maxViewText)
	if image {
		if size > maxViewImage {
			fail(w, http.StatusRequestEntityTooLarge, "too_large", "too large to view")
			return
		}
		limit = maxViewImage
	} else {
		ctype = "text/plain; charset=utf-8"
	}
	b, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		failure(w, r, err)
		return
	}
	if int64(len(b)) < size && !image {
		// Do not end in the middle of a character.
		for i := 0; i < utf8.UTFMax && len(b) > 0; i++ {
			if r, n := utf8.DecodeLastRune(b); r != utf8.RuneError || n != 1 {
				break
			}
			b = b[:len(b)-1]
		}
		h.Set("X-Athena-Truncated", "true")
	}
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", disposition("inline", name))
	h.Set("X-Athena-Size", strconv.FormatInt(size, 10))
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
