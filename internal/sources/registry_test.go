package sources

import (
	"context"
	"testing"
	"time"
)

const (
	issue = "github:o/r#1"
	pull  = "github:o/r#2"
)

type fakeSource struct{ deaf map[string]bool }

func (s *fakeSource) Name() string                  { return "github" }
func (s *fakeSource) Listens(thread string) bool    { return !s.deaf[thread] }
func (s *fakeSource) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (s *fakeSource) Health() error                 { return nil }

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

// The Registry routes a Thread key to its Source by prefix; an unknown prefix does not listen.
func TestRegistryRoutesByPrefix(t *testing.T) {
	reg := NewRegistry()
	src := &fakeSource{deaf: map[string]bool{}}
	src.deaf[pull] = true
	reg.Register(src)
	eq(t, "listens", reg.Listens(issue), true)
	eq(t, "deaf", reg.Listens(pull), false)
	eq(t, "unknown prefix", reg.Listens("feishu:chat"), false)
	eq(t, "no prefix", reg.Listens("nocolon"), false)
	eq(t, "lookup", reg.Source("github") == Source(src), true)
	eq(t, "unknown", reg.Source("feishu") == nil, true)
	eq(t, "not running before Start", reg.Running("github"), false)
	ctx, cancel := context.WithCancel(context.Background())
	reg.Start(ctx)
	eq(t, "running", reg.Running("github"), true)
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for reg.Running("github") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	eq(t, "stopped once Run returned", reg.Running("github"), false)
	defer func() {
		if recover() == nil {
			t.Error("a second registration did not panic")
		}
	}()
	reg.Register(&fakeSource{deaf: map[string]bool{}})
}
