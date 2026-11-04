package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/redact"
)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func key(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

func writeKey(t *testing.T, mode os.FileMode, pkcs8 bool) string {
	t.Helper()
	k := key(t)
	var block *pem.Block
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	} else {
		block = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// clock is a settable clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)} }

func newApp(t *testing.T, srv *httptest.Server, clk *clock) *App {
	t.Helper()
	app, err := NewApp(AppConfig{AppID: 12345, Slug: "athena-bot", KeyFile: writeKey(t, 0o600, false), InstallationID: 777, APIURL: srv.URL}, srv.Client(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func verifyJWT(t *testing.T, jwt string) (header, claims map[string]any) {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key(t).PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	for i, dst := range []*map[string]any{&header, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatal(err)
		}
	}
	return header, claims
}

func TestJWTVerifiesWithPublicKey(t *testing.T) {
	clk := newClock()
	app := newApp(t, httptest.NewServer(http.NotFoundHandler()), clk)
	jwt, err := app.JWT()
	if err != nil {
		t.Fatal(err)
	}
	header, claims := verifyJWT(t, jwt)
	if !reflect.DeepEqual(header, map[string]any{"alg": "RS256", "typ": "JWT"}) {
		t.Errorf("header = %v", header)
	}
	now := clk.now().Unix()
	if got := int64(claims["iat"].(float64)); got != now-60 {
		t.Errorf("iat = %d, want %d", got, now-60)
	}
	if got := int64(claims["exp"].(float64)); got != now+9*60 {
		t.Errorf("exp = %d, want %d", got, now+9*60)
	}
	if claims["iss"] != "12345" {
		t.Errorf("iss = %v, want the App ID \"12345\"", claims["iss"])
	}
	if len(claims) != 3 {
		t.Errorf("claims = %v", claims)
	}
}

func TestKeyFileAccepted(t *testing.T) {
	for _, c := range []struct {
		name  string
		mode  os.FileMode
		pkcs8 bool
	}{{"pkcs1 0600", 0o600, false}, {"pkcs1 0400", 0o400, false}, {"pkcs8 0600", 0o600, true}} {
		t.Run(c.name, func(t *testing.T) {
			app, err := NewApp(AppConfig{AppID: 1, KeyFile: writeKey(t, c.mode, c.pkcs8)}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			jwt, err := app.JWT()
			if err != nil {
				t.Fatal(err)
			}
			verifyJWT(t, jwt)
		})
	}
}

func TestKeyFileModeRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o644, 0o604, 0o660, 0o666, 0o601} {
		path := writeKey(t, mode, false)
		_, err := NewApp(AppConfig{AppID: 1, KeyFile: path}, nil, nil)
		if err == nil {
			t.Errorf("mode %o: want an error", mode)
			continue
		}
		if !strings.Contains(err.Error(), "0600") {
			t.Errorf("mode %o: error %q should say the mode must be 0600", mode, err)
		}
	}
}

func TestKeyFileInvalid(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "a.pem")
	os.WriteFile(notPEM, []byte("garbage"), 0o600)
	ec := filepath.Join(dir, "b.pem")
	os.WriteFile(ec, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte{1, 2}}), 0o600)
	for name, cfg := range map[string]AppConfig{
		"missing file": {AppID: 1, KeyFile: filepath.Join(dir, "none.pem")},
		"not pem":      {AppID: 1, KeyFile: notPEM},
		"bad key":      {AppID: 1, KeyFile: ec},
		"no key file":  {AppID: 1},
		"no app id":    {KeyFile: writeKey(t, 0o600, false)},
	} {
		if _, err := NewApp(cfg, nil, nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// installationServer answers the token endpoint, counting requests.
type installationServer struct {
	*httptest.Server
	mints  atomic.Int64
	bodies chan []byte
	expiry func() time.Time
	delay  time.Duration
	status atomic.Int64
}

func newInstallationServer(t *testing.T, clk *clock) *installationServer {
	s := &installationServer{bodies: make(chan []byte, 100)}
	s.expiry = func() time.Time { return clk.now().Add(time.Hour) }
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/777/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		n := s.mints.Add(1)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		verifyJWT(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		body, _ := io.ReadAll(r.Body)
		s.bodies <- body
		time.Sleep(s.delay)
		if st := s.status.Load(); st != 0 {
			w.WriteHeader(int(st))
			fmt.Fprint(w, `{"message":"boom"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_secret%d","expires_at":%q}`, n, s.expiry().UTC().Format(time.RFC3339))
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestInstallationTokenCachedUntilNearExpiry(t *testing.T) {
	clk := newClock()
	srv := newInstallationServer(t, clk)
	app := newApp(t, srv.Server, clk)
	ctx := context.Background()

	t1, err := app.InstallationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if t1.Value != "ghs_secret1" || !t1.ExpiresAt.Equal(clk.now().Add(time.Hour).Truncate(time.Second)) {
		t.Fatalf("token = %+v", t1)
	}
	clk.add(54 * time.Minute) // 6 minutes before expiry: still cached
	t2, _ := app.InstallationToken(ctx)
	if t2.Value != t1.Value || srv.mints.Load() != 1 {
		t.Fatalf("token was refreshed too early: %q, %d mints", t2.Value, srv.mints.Load())
	}
	clk.add(2 * time.Minute) // 4 minutes before expiry: refresh
	t3, err := app.InstallationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if t3.Value != "ghs_secret2" || srv.mints.Load() != 2 {
		t.Fatalf("token was not refreshed: %q, %d mints", t3.Value, srv.mints.Load())
	}
	// An installation token request asks for the installation's full permissions: no body fields.
	if body := string(<-srv.bodies); strings.Contains(body, "repositories") {
		t.Errorf("installation token body = %q", body)
	}
}

func TestInstallationTokenSingleFlight(t *testing.T) {
	clk := newClock()
	srv := newInstallationServer(t, clk)
	srv.delay = 100 * time.Millisecond
	app := newApp(t, srv.Server, clk)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := app.InstallationToken(context.Background())
			if err != nil || tok.Value != "ghs_secret1" {
				t.Errorf("token = %q, err = %v", tok.Value, err)
			}
		}()
	}
	wg.Wait()
	if n := srv.mints.Load(); n != 1 {
		t.Fatalf("%d mint requests, want 1", n)
	}
}

func TestInstallationTokenRefreshFailure(t *testing.T) {
	clk := newClock()
	srv := newInstallationServer(t, clk)
	app := newApp(t, srv.Server, clk)
	ctx := context.Background()
	t1, _ := app.InstallationToken(ctx)
	srv.status.Store(http.StatusBadGateway)
	clk.add(56 * time.Minute) // inside the refresh window but not expired: a failed refresh keeps the old token
	t2, err := app.InstallationToken(ctx)
	if err != nil || t2.Value != t1.Value {
		t.Fatalf("want the unexpired token on a failed refresh, got %q, %v", t2.Value, err)
	}
	clk.add(10 * time.Minute) // expired: the failure surfaces, classified
	if _, err := app.InstallationToken(ctx); !isClass(err, ErrTransient) {
		t.Fatalf("err = %v, want ErrTransient", err)
	}
}

func TestInstallationTokenNeedsInstallationID(t *testing.T) {
	app, err := NewApp(AppConfig{AppID: 1, KeyFile: writeKey(t, 0o600, false)}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.InstallationToken(context.Background()); err == nil {
		t.Fatal("want an error without an installation id")
	}
}

func TestScopedTokenRequestBody(t *testing.T) {
	clk := newClock()
	srv := newInstallationServer(t, clk)
	app := newApp(t, srv.Server, clk)
	tok, err := app.ScopedToken(context.Background(), "widgets", WorkerPermissions())
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value != "ghs_secret1" {
		t.Fatalf("token = %q", tok.Value)
	}
	var got map[string]any
	if err := json.Unmarshal(<-srv.bodies, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"repositories": []any{"widgets"},
		"permissions": map[string]any{
			"contents": "write", "pull_requests": "write", "issues": "write",
			"checks": "read", "actions": "read", "metadata": "read",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("body = %v\nwant   %v", got, want)
	}
	// Scoped tokens are minted every time and never share the installation token's cache.
	app.ScopedToken(context.Background(), "owner/widgets", map[string]string{"contents": "read"})
	if srv.mints.Load() != 2 {
		t.Fatalf("%d mints, want 2", srv.mints.Load())
	}
	body := string(<-srv.bodies)
	if !strings.Contains(body, `"repositories":["widgets"]`) {
		t.Errorf("owner/name should send the bare name: %s", body)
	}
	inst, _ := app.InstallationToken(context.Background())
	if inst.Value != "ghs_secret3" {
		t.Errorf("installation token = %q, want a fresh mint", inst.Value)
	}
	if _, err := app.ScopedToken(context.Background(), "", nil); err == nil {
		t.Error("want an error for an empty repository")
	}
}

func TestWorkerPermissionsIsACopy(t *testing.T) {
	p := WorkerPermissions()
	p["contents"] = "admin"
	if WorkerPermissions()["contents"] != "write" {
		t.Fatal("WorkerPermissions shares state")
	}
}

func TestBotAndRepos(t *testing.T) {
	clk := newClock()
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/777/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"token":"ghs_inst","expires_at":%q}`, clk.now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /users/athena-bot[bot]", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghs_inst" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"login":"athena-bot[bot]","id":4242}`)
	})
	mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghs_inst" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `{"total_count":2,"repositories":[{"id":2,"name":"b","full_name":"o/b","default_branch":"trunk","private":false}]}`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/installation/repositories?per_page=100&page=2>; rel="next"`, srvURL))
		fmt.Fprint(w, `{"total_count":2,"repositories":[{"id":1,"name":"a","full_name":"o/a","default_branch":"main","private":true}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL
	app := newApp(t, srv, clk)

	login, id, err := app.Bot(context.Background())
	if err != nil || login != "athena-bot[bot]" || id != 4242 {
		t.Fatalf("Bot = %q, %d, %v", login, id, err)
	}
	repos, err := app.Repos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Repo{
		{ID: 1, Name: "a", FullName: "o/a", DefaultBranch: "main", Private: true},
		{ID: 2, Name: "b", FullName: "o/b", DefaultBranch: "trunk"},
	}
	if !reflect.DeepEqual(repos, want) {
		t.Fatalf("repos = %+v", repos)
	}
}

func TestBotNeedsSlug(t *testing.T) {
	app, _ := NewApp(AppConfig{AppID: 1, KeyFile: writeKey(t, 0o600, false), InstallationID: 1}, nil, nil)
	if _, _, err := app.Bot(context.Background()); err == nil {
		t.Fatal("want an error without a slug")
	}
}

func TestTokenNeverFormatted(t *testing.T) {
	const secret = "ghs_SuperSecretValue123"
	tok := Token{Value: secret, ExpiresAt: time.Unix(1, 0)}
	type holder struct {
		Name  string
		Token Token
		Ptr   *Token
	}
	h := holder{"x", tok, &tok}
	var out []string
	for _, v := range []any{tok, &tok, h, &h} {
		out = append(out, fmt.Sprintf("%v|%+v|%s|%#v|%q|%d", v, v, v, v, v, v))
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("minted", "token", tok, "ptr", &tok, "holder", h, slog.Any("hp", &h))
	out = append(out, buf.String())
	text, _ := json.Marshal(h)
	out = append(out, string(text))
	for _, s := range out {
		if strings.Contains(s, secret) || strings.Contains(s, "SuperSecret") {
			t.Fatalf("token leaked: %s", s)
		}
	}
	if tok.String() != "ghs_…redacted" {
		t.Errorf("String = %q", tok.String())
	}
	if tok.Value != secret {
		t.Error("Value must stay readable")
	}
	if got := redact.String("token " + secret + " failed"); strings.Contains(got, "ghs_") {
		t.Errorf("redact.String left the token: %q", got)
	}
}
