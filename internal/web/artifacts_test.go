package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func (f *fakeArtifacts) TicketFiles(string) ([]node.FileEntry, bool, error) {
	if f.filesErr != nil {
		return nil, false, f.filesErr
	}
	return []node.FileEntry{{Path: "out/report.txt", Size: 5, ModTime: time.Date(2026, 11, 4, 1, 2, 3, 0, time.UTC)}}, true, nil
}

func (f *fakeArtifacts) OpenTicketFile(id, rel string) (*os.File, error) {
	f.opened = append(f.opened, [2]string{id, rel})
	if f.openErr != nil {
		return nil, f.openErr
	}
	return os.Open(filepath.Join(f.dir, "report.txt"))
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

var artifactRoutes = []string{"/api/tickets/T-0001/diff", "/api/tickets/T-0001/files", "/api/tickets/T-0001/files/out/report.txt"}

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
	want = `{"files":[{"path":"out/report.txt","size":5,"modTime":"2026-11-04T01:02:03Z"}],"truncated":true}`
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
