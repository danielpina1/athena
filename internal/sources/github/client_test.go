package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func staticToken(v string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return v, nil }
}

func isClass(err, class error) bool { return errors.Is(err, class) }

func TestRequestHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, want := range map[string]string{
			"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28",
			"User-Agent": "athena/1.2.3", "Authorization": "Bearer ghs_tok",
		} {
			if got := r.Header.Get(k); got != want {
				t.Errorf("%s = %q, want %q", k, got, want)
			}
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL+"/", staticToken("ghs_tok"), "1.2.3")
	if _, err := c.Get(context.Background(), "/x", nil); err != nil {
		t.Fatal(err)
	}
}

func TestETagNotModified(t *testing.T) {
	var inm []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inm = append(inm, r.Header.Get("If-None-Match"))
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		fmt.Fprint(w, `{"n":1}`)
	}))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL, staticToken("t"), "v")
	ctx := context.Background()

	var out struct{ N int }
	nm, err := c.Get(ctx, "/things", &out)
	if err != nil || nm || out.N != 1 {
		t.Fatalf("first Get = %v, %v, %+v", nm, err, out)
	}
	out.N = 99 // a 304 must leave out untouched
	nm, err = c.Get(ctx, "/things", &out)
	if err != nil || !nm || out.N != 99 {
		t.Fatalf("second Get = %v, %v, %+v", nm, err, out)
	}
	if inm[0] != "" || inm[1] != `"v1"` {
		t.Fatalf("If-None-Match sent: %q", inm)
	}
	// The last body stays available for a caller that needs it after a 304.
	var cached struct{ N int }
	if !c.Cached("/things", &cached) || cached.N != 1 {
		t.Fatalf("Cached = %+v", cached)
	}
	if c.Cached("/other", &cached) {
		t.Error("Cached reports a path never fetched")
	}
	// ETags are per path.
	nm, _ = c.Get(ctx, "/other", &out)
	if nm || inm[2] != "" {
		t.Errorf("a different path must not reuse the ETag: %v %q", nm, inm[2])
	}
}

func TestETagPerClient(t *testing.T) {
	var last atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last.Store(r.Header.Get("If-None-Match"))
		w.Header().Set("ETag", `"e"`)
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	a := NewClient(srv.Client(), srv.URL, staticToken("a"), "v")
	b := NewClient(srv.Client(), srv.URL, staticToken("b"), "v")
	a.Get(context.Background(), "/p", nil)
	b.Get(context.Background(), "/p", nil)
	if last.Load() != "" {
		t.Fatal("clients (token kinds) must not share ETags")
	}
}

func TestNotModifiedWithoutETagIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(304) }))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL, staticToken("t"), "v")
	if _, err := c.Get(context.Background(), "/p", nil); !isClass(err, ErrPermanent) {
		t.Fatalf("err = %v", err)
	}
}

func TestRateLimitedReset(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		status int
		hdr    map[string]string
		reset  time.Time
	}{
		{"403 remaining 0", 403, map[string]string{"x-ratelimit-remaining": "0", "x-ratelimit-reset": strconv.FormatInt(now.Add(5*time.Minute).Unix(), 10)}, now.Add(5 * time.Minute)},
		{"429 retry-after", 429, map[string]string{"Retry-After": "42"}, now.Add(42 * time.Second)},
		{"403 retry-after wins", 403, map[string]string{"Retry-After": "7", "x-ratelimit-remaining": "0", "x-ratelimit-reset": strconv.FormatInt(now.Add(time.Hour).Unix(), 10)}, now.Add(7 * time.Second)},
		{"429 without headers", 429, nil, now.Add(time.Minute)},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range c.hdr {
					w.Header().Set(k, v)
				}
				w.WriteHeader(c.status)
				fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
			}))
			defer srv.Close()
			cl := NewClient(srv.Client(), srv.URL, staticToken("t"), "v")
			cl.now = func() time.Time { return now }
			_, err := cl.Get(context.Background(), "/p", nil)
			var rl ErrRateLimited
			if !errors.As(err, &rl) {
				t.Fatalf("err = %v, want ErrRateLimited", err)
			}
			if !rl.Reset.Equal(c.reset) {
				t.Errorf("Reset = %v, want %v", rl.Reset, c.reset)
			}
			if !errors.Is(err, ErrRateLimited{}) {
				t.Error("errors.Is(err, ErrRateLimited{}) should match any rate limit")
			}
			if errors.Is(err, ErrTransient) || errors.Is(err, ErrPermanent) {
				t.Error("a rate limit is its own class")
			}
		})
	}
}

func TestForbiddenWithoutRateLimitIsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-remaining", "4000")
		w.WriteHeader(403)
		fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
	}))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL, staticToken("t"), "v")
	_, err := c.Get(context.Background(), "/p", nil)
	if !isClass(err, ErrPermanent) {
		t.Fatalf("err = %v", err)
	}
	if c.RateRemaining() != 4000 {
		t.Errorf("RateRemaining = %d", c.RateRemaining())
	}
}

func TestRateRemainingRecorded(t *testing.T) {
	reset := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	remaining := "4999"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-remaining", remaining)
		w.Header().Set("x-ratelimit-reset", strconv.FormatInt(reset.Unix(), 10))
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL, staticToken("t"), "v")
	if c.RateRemaining() != -1 {
		t.Errorf("before any response RateRemaining = %d, want -1", c.RateRemaining())
	}
	c.Get(context.Background(), "/p", nil)
	if c.RateRemaining() != 4999 || !c.RateReset().Equal(reset) {
		t.Errorf("RateRemaining = %d, reset %v", c.RateRemaining(), c.RateReset())
	}
	remaining = "12"
	c.Post(context.Background(), "/p", map[string]int{}, nil)
	if c.RateRemaining() != 12 {
		t.Errorf("RateRemaining after Post = %d", c.RateRemaining())
	}
}

func TestErrorClasses(t *testing.T) {
	for _, c := range []struct {
		method string
		status int
		body   string
		want   error
	}{
		{"GET", 500, "", ErrTransient},
		{"GET", 502, "", ErrTransient},
		{"GET", 503, "", ErrTransient},
		{"GET", 504, "", ErrTransient},
		{"GET", 408, "", ErrTransient},
		{"POST", 502, "", ErrTransient},
		{"GET", 404, `{"message":"Not Found"}`, ErrTransient}, // lost access reads as 404: retried
		{"GET", 410, "", ErrGone},
		{"POST", 410, "", ErrGone},
		{"POST", 404, "", ErrGone},
		{"PATCH", 404, "", ErrGone},
		{"POST", 422, `{"message":"Conversation is locked"}`, ErrGone},
		{"PATCH", 422, `{"message":"Issue was LOCKED"}`, ErrGone},
		{"POST", 422, `{"message":"Validation Failed"}`, ErrPermanent},
		{"GET", 400, "", ErrPermanent},
		{"GET", 401, "", ErrPermanent},
		{"GET", 403, `{"message":"forbidden"}`, ErrPermanent},
		{"POST", 409, "", ErrPermanent},
		{"POST", 451, "", ErrPermanent},
	} {
		t.Run(fmt.Sprintf("%s %d %s", c.method, c.status, c.body), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				fmt.Fprint(w, c.body)
			}))
			defer srv.Close()
			cl := NewClient(srv.Client(), srv.URL, staticToken("t"), "v")
			var err error
			switch c.method {
			case "GET":
				_, err = cl.Get(context.Background(), "/p", nil)
			case "POST":
				err = cl.Post(context.Background(), "/p", map[string]string{}, nil)
			case "PATCH":
				err = cl.Patch(context.Background(), "/p", map[string]string{}, nil)
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want class %v", err, c.want)
			}
			for _, other := range []error{ErrTransient, ErrGone, ErrPermanent} {
				if other != c.want && errors.Is(err, other) {
					t.Errorf("err also matches %v", other)
				}
			}
			var ae *APIError
			if !errors.As(err, &ae) || ae.Status != c.status {
				t.Errorf("errors.As(*APIError) = %+v", ae)
			}
		})
	}
}

func TestNetworkErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	c := NewClient(&http.Client{Timeout: time.Second}, url, staticToken("t"), "v")
	if _, err := c.Get(context.Background(), "/p", nil); !isClass(err, ErrTransient) {
		t.Fatalf("connection refused: err = %v", err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	c = NewClient(&http.Client{Timeout: 50 * time.Millisecond}, slow.URL, staticToken("t"), "v")
	if _, err := c.Get(context.Background(), "/p", nil); !isClass(err, ErrTransient) {
		t.Fatalf("client timeout: err = %v", err)
	}

	// A canceled context is the caller's decision, not a transient failure.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c = NewClient(slow.Client(), slow.URL, staticToken("t"), "v")
	err := func() error { _, err := c.Get(ctx, "/p", nil); return err }()
	if !errors.Is(err, context.Canceled) || isClass(err, ErrTransient) {
		t.Fatalf("canceled: err = %v", err)
	}
}

func TestTokenFuncErrorPropagates(t *testing.T) {
	boom := errors.New("no token")
	c := NewClient(http.DefaultClient, "http://127.0.0.1:1", func(context.Context) (string, error) { return "", boom }, "v")
	if _, err := c.Get(context.Background(), "/p", nil); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestErrorsNeverCarrySecrets(t *testing.T) {
	long := strings.Repeat("é", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		fmt.Fprintf(w, `{"message":"bad token ghs_LEAKEDsecret and Authorization: Bearer abc.def.ghi %s"}`, long)
	}))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL, staticToken("ghs_THETOKEN"), "v")
	err := c.Post(context.Background(), "/p", map[string]string{"a": "b"}, nil)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, bad := range []string{"ghs_THETOKEN", "ghs_LEAKED", "abc.def.ghi", "Bearer"} {
		if strings.Contains(msg, bad) {
			t.Errorf("error contains %q: %s", bad, msg)
		}
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatal("not an APIError")
	}
	if n := utf8.RuneCountInString(ae.Message); n > 300 {
		t.Errorf("message has %d runes, want <= 300", n)
	}
	if !utf8.ValidString(msg) {
		t.Error("truncation split a rune")
	}

	// Transport errors do not carry the token either.
	dead := httptest.NewServer(http.NotFoundHandler())
	u := dead.URL
	dead.Close()
	_, err = NewClient(http.DefaultClient, u, staticToken("ghs_THETOKEN"), "v").Get(context.Background(), "/p", nil)
	if err == nil || strings.Contains(err.Error(), "THETOKEN") {
		t.Fatalf("err = %v", err)
	}
}

func TestPostAndPatchBodies(t *testing.T) {
	type rec struct{ method, ctype, body string }
	var got []rec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, rec{r.Method, r.Header.Get("Content-Type"), string(b)})
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":5}`)
	}))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL, staticToken("t"), "v")
	var out struct{ ID int }
	if err := c.Post(context.Background(), "/issues/1/comments", map[string]string{"body": "hi"}, &out); err != nil || out.ID != 5 {
		t.Fatalf("Post: %v %+v", err, out)
	}
	if err := c.Patch(context.Background(), "/issues/1", map[string]string{"state": "closed"}, nil); err != nil {
		t.Fatal(err)
	}
	if got[0] != (rec{"POST", "application/json", `{"body":"hi"}`}) || got[1] != (rec{"PATCH", "application/json", `{"state":"closed"}`}) {
		t.Fatalf("requests = %+v", got)
	}
}

func TestPaginate(t *testing.T) {
	var srvURL string
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/items?page=2>; rel="next", <%s/items?page=3>; rel="last"`, srvURL, srvURL))
			fmt.Fprint(w, `[1,2]`)
		case "2":
			w.Header().Set("Link", fmt.Sprintf(`<%s/items?page=1>; rel="prev", <%s/items?page=3>; rel="next"`, srvURL, srvURL))
			fmt.Fprint(w, `[3]`)
		default:
			fmt.Fprint(w, `[4]`)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL
	c := NewClient(srv.Client(), srv.URL, staticToken("t"), "v")
	var all []int
	err := c.Paginate(context.Background(), "/items", func(raw json.RawMessage) error {
		var page []int
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		all = append(all, page...)
		return nil
	})
	if err != nil || fmt.Sprint(all) != "[1 2 3 4]" {
		t.Fatalf("all = %v, err = %v", all, err)
	}
	for _, a := range auths {
		if a != "Bearer t" {
			t.Errorf("Authorization = %q", a)
		}
	}

	stop := errors.New("stop")
	calls := 0
	err = c.Paginate(context.Background(), "/items", func(json.RawMessage) error { calls++; return stop })
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("callback error: %v after %d calls", err, calls)
	}
}

func TestPaginateRefusesForeignHost(t *testing.T) {
	var hit atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Store(true) }))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/steal>; rel="next"`, evil.URL))
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	c := NewClient(srv.Client(), srv.URL, staticToken("t"), "v")
	err := c.Paginate(context.Background(), "/items", func(json.RawMessage) error { return nil })
	if !isClass(err, ErrPermanent) || hit.Load() {
		t.Fatalf("err = %v, foreign host hit = %v", err, hit.Load())
	}
}
