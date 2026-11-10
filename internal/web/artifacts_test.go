package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
)

type fakeArtifacts struct {
	diffErr, filesErr, openErr error
	dir                        string
	opened                     [][2]string
}

func (f *fakeArtifacts) TicketDiff(_ context.Context, id string) (node.Diff, error) {
	if f.diffErr != nil {
		return node.Diff{}, f.diffErr
	}
	return node.Diff{Base: "abc", Branch: "athena/" + strings.ToLower(id), Truncated: true, Patch: "+new line\n",
		Files: []node.DiffFile{{Path: "b", OldPath: "a", Status: "R", Added: 2, Deleted: 1}, {Path: "bin", Status: "A", Binary: true}}}, nil
}

func (f *fakeArtifacts) TicketFiles(context.Context, string) ([]node.FileEntry, bool, error) {
	if f.filesErr != nil {
		return nil, false, f.filesErr
	}
	return []node.FileEntry{
		{Path: "out/report.txt", Size: 5, ModTime: time.Date(2026, 11, 4, 1, 2, 3, 0, time.UTC)},
		{Path: "REPORT.md", Status: "?", Size: 9, ModTime: time.Date(2026, 11, 4, 1, 2, 3, 0, time.UTC)},
	}, true, nil
}

// OpenTicketFile opens the file of rel's base name in dir: the node's own guard is tested in internal/node.
func (f *fakeArtifacts) OpenTicketFile(_ context.Context, id, rel string) (*os.File, error) {
	f.opened = append(f.opened, [2]string{id, rel})
	if f.openErr != nil {
		return nil, f.openErr
	}
	return os.Open(filepath.Join(f.dir, filepath.Base(rel)))
}

func artifactsEnv(t *testing.T) (*AuthEnv, *fakeArtifacts, *bytes.Buffer) {
	t.Helper()
	e, _ := signedIn(t)
	fa := &fakeArtifacts{dir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(fa.dir, "report.txt"), []byte("<b>hi</b>"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	mountArtifacts(e.Mux, e.Auth, fa, slog.New(slog.NewTextHandler(&logs, nil)))
	return e, fa, &logs
}

var artifactRoutes = []string{"/api/tickets/T-0001/diff", "/api/tickets/T-0001/files", "/api/tickets/T-0001/files/out/report.txt", "/api/tickets/T-0001/files/out/report.txt?view=1"}

func TestArtifactsNeedSession(t *testing.T) {
	e, fa, _ := artifactsEnv(t)
	e.DropCookies()
	for _, p := range artifactRoutes {
		rec := e.Do("GET", p, nil)
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusUnauthorized || body["code"] != "unauthenticated" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s without a session: %d %s (Cache-Control %q)", p, rec.Code, rec.Body, rec.Header().Get("Cache-Control"))
		}
	}
	if len(fa.opened) != 0 {
		t.Fatalf("a file was opened without a session: %v", fa.opened)
	}
}

func TestArtifactsJSON(t *testing.T) {
	e, _, _ := artifactsEnv(t)
	rec := e.Do("GET", "/api/tickets/T-0001/diff", nil)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("diff: %d %q", rec.Code, rec.Header())
	}
	want := `{"base":"abc","branch":"athena/t-0001","files":[{"path":"b","oldPath":"a","status":"R","added":2,"deleted":1,"binary":false},` +
		`{"path":"bin","status":"A","added":0,"deleted":0,"binary":true}],"patch":"+new line\n","truncated":true}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("diff body\n got %s\nwant %s", got, want)
	}
	rec = e.Do("GET", "/api/tickets/T-0001/files", nil)
	want = `{"files":[{"path":"out/report.txt","size":5,"modTime":"2026-11-04T01:02:03Z"},` +
		`{"path":"REPORT.md","status":"?","size":9,"modTime":"2026-11-04T01:02:03Z"}],"truncated":true}`
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != 200 || got != want || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("files: %d %s", rec.Code, got)
	}
}

func TestArtifactsNotFound(t *testing.T) {
	e, fa, logs := artifactsEnv(t)
	for _, err := range []error{node.ErrNoWorkspace, node.ErrNotFound} {
		fa.diffErr, fa.filesErr, fa.openErr = err, err, err
		for _, p := range artifactRoutes {
			rec := e.Do("GET", p, nil)
			if rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != `{"code":"not_found","error":"not found"}` || rec.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("%s with %v: %d %s", p, err, rec.Code, rec.Body)
			}
		}
	}
	fa.diffErr, fa.filesErr, fa.openErr = errors.New("git exploded: /secret/path"), errors.New("boom"), errors.New("boom")
	for _, p := range artifactRoutes {
		rec := e.Do("GET", p, nil)
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 500 || body["code"] != "internal" || strings.Contains(rec.Body.String(), "secret") {
			t.Errorf("%s with another error: %d %s", p, rec.Code, rec.Body)
		}
	}
	if !strings.Contains(logs.String(), "git exploded") {
		t.Errorf("the failure was not logged: %q", logs)
	}
}

// TestArtifactsNodeOffline: a Ticket whose node is offline answers 503 with "<name> is offline".
func TestArtifactsNodeOffline(t *testing.T) {
	e, fa, _ := artifactsEnv(t)
	err := &core.UserError{Msg: "mini is offline", Err: core.ErrNodeOffline}
	fa.diffErr, fa.filesErr, fa.openErr = err, err, err
	for _, p := range artifactRoutes {
		rec := e.Do("GET", p, nil)
		if rec.Code != http.StatusServiceUnavailable ||
			strings.TrimSpace(rec.Body.String()) != `{"code":"unavailable","error":"mini is offline"}` {
			t.Errorf("%s with the node offline: %d %s", p, rec.Code, rec.Body)
		}
	}
}

func TestFileDownloadHeaders(t *testing.T) {
	e, fa, _ := artifactsEnv(t)
	rec := e.Do("GET", "/api/tickets/T-0001/files/out/report.txt", nil)
	h := rec.Header()
	if rec.Code != 200 || rec.Body.String() != "<b>hi</b>" {
		t.Fatalf("%d %q", rec.Code, rec.Body)
	}
	for k, v := range map[string]string{
		"Content-Type":           "application/octet-stream",
		"X-Content-Type-Options": "nosniff",
		"Content-Disposition":    `attachment; filename=report.txt`,
		"Cache-Control":          "no-store",
	} {
		if h.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, h.Get(k), v)
		}
	}
	if got := fa.opened[len(fa.opened)-1]; got != [2]string{"T-0001", "out/report.txt"} {
		t.Errorf("opened %v", got)
	}
	// An encoded .. reaches the node as it is; the node refuses it.
	e.Do("GET", "/api/tickets/T-0001/files/%2e%2e/x", nil)
	if got := fa.opened[len(fa.opened)-1]; got[1] != "../x" {
		t.Errorf("encoded dotdot arrived as %q", got[1])
	}
}

func writeIn(t *testing.T, dir, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// ?view=1 serves a file to read in the page: text as text/plain whatever it holds (HTML and SVG included, never to be
// rendered), the allowed images as themselves, inline, under a sandbox policy added to the app's own.
func TestFileViewHeaders(t *testing.T) {
	e, fa, _ := artifactsEnv(t)
	const html = "<!doctype html><script>alert(1)</script>"
	const svg = `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><script>alert(2)</script></svg>`
	writeIn(t, fa.dir, "page.html", []byte(html))
	writeIn(t, fa.dir, "logo.svg", []byte(svg))
	writeIn(t, fa.dir, "notes.md", []byte("# Notes"))
	png := []byte("\x89PNG\r\n\x1a\nnot really")
	for _, name := range []string{"shot.png", "a.JPG", "b.jpeg", "c.gif", "d.webp", "e.pdf", "f.bin"} {
		writeIn(t, fa.dir, name, png)
	}
	cases := []struct{ name, ctype, body string }{
		{"page.html", "text/plain; charset=utf-8", html},
		{"logo.svg", "text/plain; charset=utf-8", svg},
		{"notes.md", "text/plain; charset=utf-8", "# Notes"},
		{"shot.png", "image/png", string(png)},
		{"a.JPG", "image/jpeg", string(png)},
		{"b.jpeg", "image/jpeg", string(png)},
		{"c.gif", "image/gif", string(png)},
		{"d.webp", "image/webp", string(png)},
		{"e.pdf", "text/plain; charset=utf-8", string(png)},
		{"f.bin", "text/plain; charset=utf-8", string(png)},
	}
	for _, c := range cases {
		rec := e.Do("GET", "/api/tickets/T-0001/files/docs/"+c.name+"?view=1", nil)
		h := rec.Header()
		if rec.Code != 200 || rec.Body.String() != c.body {
			t.Errorf("%s: %d %q", c.name, rec.Code, rec.Body)
			continue
		}
		for k, v := range map[string]string{
			"Content-Type":           c.ctype,
			"X-Content-Type-Options": "nosniff",
			"Content-Disposition":    "inline; filename=" + c.name,
			"Cache-Control":          "no-store",
			"X-Athena-Size":          strconv.Itoa(len(c.body)),
			"X-Athena-Truncated":     "",
		} {
			if h.Get(k) != v {
				t.Errorf("%s: %s = %q, want %q", c.name, k, h.Get(k), v)
			}
		}
		if csp := h.Values("Content-Security-Policy"); !slices.Contains(csp, "sandbox; default-src 'none'") {
			t.Errorf("%s: Content-Security-Policy = %q", c.name, csp)
		}
		if got := fa.opened[len(fa.opened)-1]; got != [2]string{"T-0001", "docs/" + c.name} {
			t.Errorf("opened %v", got)
		}
	}
}

// The policy is added to the app's own, never put in its place: the page's frame-ancestors still holds.
func TestFileViewKeepsTheAppPolicy(t *testing.T) {
	e, fa, _ := artifactsEnv(t)
	writeIn(t, fa.dir, "notes.md", []byte("# Notes"))
	req := httptest.NewRequest("GET", "/api/tickets/T-0001/files/notes.md?view=1", nil)
	for _, c := range e.Request().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	securityHeaders(e.Mux, true).ServeHTTP(rec, req)
	csp := rec.Header().Values("Content-Security-Policy")
	if rec.Code != 200 || !slices.Equal(csp, []string{contentSecurityPolicy, "sandbox; default-src 'none'"}) {
		t.Fatalf("%d Content-Security-Policy = %q", rec.Code, csp)
	}
}

// Text past 1 MiB is cut, never in the middle of a character, and says so; an image is whole or not served.
func TestFileViewCapsSize(t *testing.T) {
	e, fa, _ := artifactsEnv(t)
	big := append(bytes.Repeat([]byte("a"), maxViewText-1), []byte("é and more")...) // é straddles the cut
	writeIn(t, fa.dir, "big.log", big)
	rec := e.Do("GET", "/api/tickets/T-0001/files/big.log?view=1", nil)
	h := rec.Header()
	if rec.Code != 200 || rec.Body.Len() != maxViewText-1 || h.Get("X-Athena-Truncated") != "true" || h.Get("X-Athena-Size") != strconv.Itoa(len(big)) {
		t.Fatalf("%d len=%d truncated=%q size=%q", rec.Code, rec.Body.Len(), h.Get("X-Athena-Truncated"), h.Get("X-Athena-Size"))
	}
	exact := bytes.Repeat([]byte("b"), maxViewText)
	writeIn(t, fa.dir, "exact.txt", exact)
	rec = e.Do("GET", "/api/tickets/T-0001/files/exact.txt?view=1", nil)
	if rec.Code != 200 || rec.Body.Len() != maxViewText || rec.Header().Get("X-Athena-Truncated") != "" {
		t.Fatalf("exactly 1 MiB: %d len=%d truncated=%q", rec.Code, rec.Body.Len(), rec.Header().Get("X-Athena-Truncated"))
	}
	writeIn(t, fa.dir, "huge.png", bytes.Repeat([]byte{0}, maxViewImage+1))
	rec = e.Do("GET", "/api/tickets/T-0001/files/huge.png?view=1", nil)
	if rec.Code != http.StatusRequestEntityTooLarge || strings.TrimSpace(rec.Body.String()) != `{"code":"too_large","error":"too large to view"}` {
		t.Fatalf("huge image: %d %s", rec.Code, rec.Body)
	}
	// Without view, the whole file downloads.
	rec = e.Do("GET", "/api/tickets/T-0001/files/big.log", nil)
	if rec.Code != 200 || rec.Body.Len() != len(big) || rec.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("download: %d len=%d %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Type"))
	}
}
