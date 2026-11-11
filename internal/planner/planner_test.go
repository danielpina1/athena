package planner

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNewUnknownStrategy(t *testing.T) {
	_, err := New(Config{Strategy: "oracle"})
	if err == nil || !strings.Contains(err.Error(), `"oracle"`) || !strings.Contains(err.Error(), "llm") ||
		!strings.Contains(err.Error(), "off") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewDefaults(t *testing.T) {
	s, err := New(Config{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	l, ok := s.(LLM)
	if !ok || s.Name() != "llm" {
		t.Fatalf("strategy = %#v", s)
	}
	c, ok := l.C.(*Claude)
	if !ok || c.Model != "haiku" || c.Binary != "claude" {
		t.Fatalf("completer = %#v", l.C)
	}
}

func TestOff(t *testing.T) {
	s, err := New(Config{Strategy: "off"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Plan(context.Background(), Request{}); !errors.Is(err, ErrOff) || s.Name() != "off" {
		t.Fatalf("Plan = %v", err)
	}
	if ErrOff.Error() != "planning is off in this server's configuration" {
		t.Errorf("ErrOff = %q", ErrOff)
	}
}
