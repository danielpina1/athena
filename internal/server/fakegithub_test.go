package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub is a small stand-in for GitHub's REST API with one App installation (777) on the account "o" and one
// repository, o/r: enough for the App's checks, the Source's polls and the Outbox's comments. Lists honour since and
// come sorted by updated_at; there are no ETags. Every request is recorded.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	// down makes every request answer 503, as GitHub unreachable at boot.
	down atomic.Bool
	// failComments makes every comment post answer 503, so a reply waits to go out.
	failComments atomic.Bool

	mu       sync.Mutex
	issues   []map[string]any // the issues list: issues and pull requests
	comments []map[string]any // issue comments
	pulls    map[int]map[string]any
	posted   []postedComment
	requests []string // "METHOD path"
	// mints are the permissions each token was minted with (nil: the installation's own), with its value; revoked
	// the tokens revoked.
	mints   []fakeMint
	revoked []string
	// opened are the bodies of the pull requests created, with the token each was created with.
	opened []openedPull
	nextID int64
	// repos is what GET /installation/repositories lists.
	repos []string
}

type openedPull struct {
	Token string
	Body  map[string]any
}

type fakeMint struct {
	Token string
	Perms map[string]string
}

type postedComment struct {
	Number int
	Body   string
}

const (
	fakeBot   = "athena-bot[bot]"
	fakeBotID = 4242
	fakeRepo  = "o/r"
)

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{t: t, pulls: map[int]map[string]any{}, nextID: 5000, repos: []string{fakeRepo}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/777/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		g.record(r)
		var in struct {
			Permissions map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		tok := fmt.Sprintf("ghs_faketoken%d", time.Now().UnixNano())
		g.mu.Lock()
		g.mints = append(g.mints, fakeMint{Token: tok, Perms: in.Permissions})
		g.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, tok, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("DELETE /installation/token", func(w http.ResponseWriter, r *http.Request) {
		g.record(r)
		_, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		g.mu.Lock()
		g.revoked = append(g.revoked, tok)
		g.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /app/installations/777", func(w http.ResponseWriter, r *http.Request) {
		g.json(w, r, map[string]any{"id": 777, "account": map[string]any{"login": "o"}})
	})
	mux.HandleFunc("GET /users/athena-bot[bot]", func(w http.ResponseWriter, r *http.Request) {
		g.json(w, r, map[string]any{"login": fakeBot, "id": fakeBotID, "type": "Bot"})
	})
	mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		var list []map[string]any
		for i, name := range g.repos {
			list = append(list, map[string]any{"id": i + 1, "name": strings.Split(name, "/")[1], "full_name": name, "default_branch": "main"})
		}
		g.mu.Unlock()
		g.json(w, r, map[string]any{"total_count": len(list), "repositories": list})
	})
	mux.HandleFunc("GET /repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		g.json(w, r, map[string]any{"full_name": fakeRepo, "default_branch": "main", "html_url": "https://github.com/o/r"})
	})
	mux.HandleFunc("GET /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		g.list(w, r, func() []map[string]any { return g.issues })
	})
	mux.HandleFunc("GET /repos/o/r/issues/comments", func(w http.ResponseWriter, r *http.Request) {
		g.list(w, r, func() []map[string]any { return g.comments })
	})
	mux.HandleFunc("GET /repos/o/r/pulls/comments", func(w http.ResponseWriter, r *http.Request) {
		g.json(w, r, []any{})
	})
	mux.HandleFunc("GET /repos/o/r/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		var found map[string]any
		for _, it := range g.issues {
			if fmt.Sprint(it["number"]) == r.PathValue("n") {
				found = it
			}
		}
		g.mu.Unlock()
		if found == nil {
			g.notFound(w, r)
			return
		}
		g.json(w, r, found)
	})
	mux.HandleFunc("GET /repos/o/r/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		g.list(w, r, func() []map[string]any {
			var out []map[string]any
			for _, c := range g.comments {
				if strings.HasSuffix(c["issue_url"].(string), "/issues/"+strconv.Itoa(n)) {
					out = append(out, c)
				}
			}
			return out
		})
	})
	mux.HandleFunc("POST /repos/o/r/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		g.record(r)
		if g.failComments.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"message":"unavailable"}`)
			return
		}
		n, _ := strconv.Atoi(r.PathValue("n"))
		var in struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		now := time.Now().UTC().Format(time.RFC3339)
		g.mu.Lock()
		g.nextID++
		c := map[string]any{"id": g.nextID, "body": in.Body, "user": map[string]any{"login": fakeBot, "id": fakeBotID, "type": "Bot"},
			"issue_url":  "https://api.github.com/repos/o/r/issues/" + strconv.Itoa(n),
			"html_url":   fmt.Sprintf("https://github.com/o/r/issues/%d#issuecomment-%d", n, g.nextID),
			"created_at": now, "updated_at": now}
		g.comments = append(g.comments, c)
		g.posted = append(g.posted, postedComment{Number: n, Body: in.Body})
		g.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(c)
	})
	mux.HandleFunc("GET /repos/o/r/pulls/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		g.mu.Lock()
		p := g.pulls[n]
		g.mu.Unlock()
		if p == nil {
			g.notFound(w, r)
			return
		}
		g.json(w, r, p)
	})
	mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		g.record(r)
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		_, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		g.mu.Lock()
		g.opened = append(g.opened, openedPull{Token: tok, Body: in})
		g.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 21, "html_url": "https://github.com/o/r/pull/21"})
	})
	mux.HandleFunc("GET /repos/o/r/pulls/{n}/reviews", func(w http.ResponseWriter, r *http.Request) { g.json(w, r, []any{}) })
	mux.HandleFunc("GET /repos/o/r/branches/{b}", func(w http.ResponseWriter, r *http.Request) {
		g.json(w, r, map[string]any{"name": r.PathValue("b"), "commit": map[string]any{"sha": strings.Repeat("a", 40)}})
	})
	mux.HandleFunc("GET /repos/o/r/commits/{sha}/check-suites", func(w http.ResponseWriter, r *http.Request) {
		g.json(w, r, map[string]any{"total_count": 0, "check_suites": []any{}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fake github: unexpected %s %s", r.Method, r.URL.Path)
		g.notFound(w, r)
	})
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.down.Load() {
			g.record(r)
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"message":"unavailable"}`)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) record(r *http.Request) {
	g.mu.Lock()
	g.requests = append(g.requests, r.Method+" "+r.URL.Path)
	g.mu.Unlock()
}

func (g *fakeGitHub) json(w http.ResponseWriter, r *http.Request, v any) {
	if r.Method != http.MethodPost {
		g.record(r)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-ratelimit-remaining", "4321")
	_ = json.NewEncoder(w).Encode(v)
}

func (g *fakeGitHub) notFound(w http.ResponseWriter, r *http.Request) {
	g.record(r)
	w.WriteHeader(http.StatusNotFound)
	io.WriteString(w, `{"message":"Not Found"}`)
}

// list serves items updated since the since parameter, sorted by updated_at, in one page.
func (g *fakeGitHub) list(w http.ResponseWriter, r *http.Request, all func() []map[string]any) {
	since, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	g.mu.Lock()
	var out []map[string]any
	for _, it := range all() {
		u, _ := time.Parse(time.RFC3339, it["updated_at"].(string))
		if !u.Before(since) {
			out = append(out, it)
		}
	}
	g.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i]["updated_at"].(string) < out[j]["updated_at"].(string) })
	if out == nil {
		out = []map[string]any{}
	}
	g.json(w, r, out)
}

// addComment adds a person's comment on issue n and returns its id.
func (g *fakeGitHub) addComment(n int, login, body string) int64 {
	now := time.Now().UTC().Format(time.RFC3339)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextID++
	g.comments = append(g.comments, map[string]any{"id": g.nextID, "body": body,
		"user":       map[string]any{"login": login, "id": 100, "type": "User"},
		"issue_url":  "https://api.github.com/repos/o/r/issues/" + strconv.Itoa(n),
		"html_url":   fmt.Sprintf("https://github.com/o/r/issues/%d#issuecomment-%d", n, g.nextID),
		"created_at": now, "updated_at": now})
	return g.nextID
}

// mergePull makes pull request n, from the branch head of o/r opened by the bot, merged now.
func (g *fakeGitHub) mergePull(n int, head string) {
	now := time.Now().UTC().Format(time.RFC3339)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[n] = map[string]any{"number": n, "html_url": fmt.Sprintf("https://github.com/o/r/pull/%d", n),
		"head": map[string]any{"ref": head, "sha": strings.Repeat("b", 40), "repo": map[string]any{"full_name": fakeRepo}}}
	g.issues = append(g.issues, map[string]any{"id": 9000 + n, "number": n, "title": "Fix the flaky test", "state": "closed",
		"html_url":   fmt.Sprintf("https://github.com/o/r/pull/%d", n),
		"user":       map[string]any{"login": fakeBot, "id": fakeBotID, "type": "Bot"},
		"created_at": now, "updated_at": now, "closed_at": now,
		"pull_request": map[string]any{"merged_at": now}})
}

func (g *fakeGitHub) postedComments() []postedComment {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]postedComment(nil), g.posted...)
}

func (g *fakeGitHub) minted() []fakeMint {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]fakeMint(nil), g.mints...)
}

func (g *fakeGitHub) openedPulls() []openedPull {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]openedPull(nil), g.opened...)
}

func (g *fakeGitHub) revokedTokens() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.revoked...)
}

func (g *fakeGitHub) requestLog() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.requests...)
}

// appKey writes a fresh RSA key, PKCS#1 PEM, mode 0600, and returns its path.
func appKey(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// configureGitHub writes the state dir's config.json for the fake.
func (g *fakeGitHub) configure(t *testing.T, stateDir string, evalTimeoutSeconds int) {
	t.Helper()
	cfg := FileConfig{
		GitHub: &GitHubConfig{AppID: 1, Slug: "athena-bot", KeyFile: appKey(t), InstallationID: 777, Repos: []string{fakeRepo},
			IntervalSeconds: 1, APIURL: g.srv.URL},
		EvalTimeoutSeconds: evalTimeoutSeconds,
	}
	if err := SaveConfig(stateDir, cfg); err != nil {
		t.Fatal(err)
	}
}
