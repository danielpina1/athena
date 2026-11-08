package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/danielpina1/athena/internal/sources/schedule"
)

var _ schedule.Repos = (*Watcher)(nil)

func watcherOver(t *testing.T, h http.HandlerFunc, slug string) *Watcher {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewWatcher(newTestClient(srv.Client(), srv.URL+"/", staticToken("tok"), "test"), slug)
}

func TestHeadReadsDefaultBranch(t *testing.T) {
	w := watcherOver(t, func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r":
			fmt.Fprint(rw, `{"default_branch":"trunk"}`)
		case "/repos/o/r/branches/trunk":
			fmt.Fprint(rw, `{"commit":{"sha":"abc123"}}`)
		default:
			http.NotFound(rw, r)
		}
	}, "")
	branch, sha, err := w.Head(context.Background(), "o/r")
	if err != nil || branch != "trunk" || sha != "abc123" {
		t.Fatalf("Head = %q, %q, %v", branch, sha, err)
	}
}

func TestHeadUsesCachedBodyOn304(t *testing.T) {
	var conditional int
	w := watcherOver(t, func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			conditional++
			rw.WriteHeader(http.StatusNotModified)
			return
		}
		rw.Header().Set("ETag", `"v1"`)
		if strings.HasSuffix(r.URL.Path, "/r") {
			fmt.Fprint(rw, `{"default_branch":"main"}`)
			return
		}
		fmt.Fprint(rw, `{"commit":{"sha":"def456"}}`)
	}, "")
	for i := 0; i < 2; i++ {
		branch, sha, err := w.Head(context.Background(), "o/r")
		if err != nil || branch != "main" || sha != "def456" {
			t.Fatalf("Head #%d = %q, %q, %v", i, branch, sha, err)
		}
	}
	if conditional != 2 {
		t.Fatalf("conditional requests = %d, want 2", conditional)
	}
}

func TestCI(t *testing.T) {
	suite := func(app, status, conclusion string) string {
		c := "null"
		if conclusion != "" {
			c = `"` + conclusion + `"`
		}
		return fmt.Sprintf(`{"id":1,"status":%q,"conclusion":%s,"app":{"slug":%q}}`, status, c, app)
	}
	done := func(c string) string { return suite("ci", "completed", c) }
	running := suite("ci", "in_progress", "")
	cases := []struct {
		name   string
		suites []string
		want   schedule.CIState
	}{
		{"failure", []string{done("failure")}, schedule.CIRed},
		{"timed_out", []string{done("timed_out")}, schedule.CIRed},
		{"success", []string{done("success")}, schedule.CIGreen},
		{"cancelled and success", []string{done("cancelled"), done("success")}, schedule.CIGreen},
		{"in_progress and success", []string{running, done("success")}, schedule.CIUnknown},
		{"failure and in_progress", []string{done("failure"), running}, schedule.CIRed},
		{"none", nil, schedule.CIUnknown},
		{"own failure ignored", []string{suite("athena-bot", "completed", "failure")}, schedule.CIUnknown},
		{"own failure ignored beside success", []string{suite("Athena-Bot", "completed", "failure"), done("success")}, schedule.CIGreen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := watcherOver(t, func(rw http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/o/r/commits/abc/check-suites" {
					http.NotFound(rw, r)
					return
				}
				fmt.Fprintf(rw, `{"total_count":%d,"check_suites":[%s]}`, len(tc.suites), strings.Join(tc.suites, ","))
			}, "athena-bot")
			got, err := w.CI(context.Background(), "o/r", "abc")
			if err != nil || got != tc.want {
				t.Fatalf("CI = %v, %v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestChangedFilesAcrossPages(t *testing.T) {
	var srvURL string
	w := watcherOver(t, func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/compare/aaa...bbb" {
			http.NotFound(rw, r)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(rw, `{"files":[{"filename":"web/package.json"}]}`)
			return
		}
		rw.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/compare/aaa...bbb?per_page=100&page=2>; rel="next"`, srvURL))
		fmt.Fprint(rw, `{"files":[{"filename":"README.md"},{"filename":"go.mod"}]}`)
	}, "")
	srvURL = strings.TrimSuffix(w.c.base.String(), "/")
	got, err := w.ChangedFiles(context.Background(), "o/r", "aaa", "bbb")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"README.md", "go.mod", "web/package.json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
}

func TestChangedFiles404IsErrNoCompare(t *testing.T) {
	w := watcherOver(t, func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusNotFound)
		fmt.Fprint(rw, `{"message":"Not Found"}`)
	}, "")
	_, err := w.ChangedFiles(context.Background(), "o/r", "aaa", "bbb")
	if !errors.Is(err, schedule.ErrNoCompare) {
		t.Fatalf("err = %v, want ErrNoCompare", err)
	}
}
