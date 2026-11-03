package server

import (
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRenderFixture(t *testing.T) {
	data, err := os.ReadFile(claudeFixture(t, "simple-turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	fallback := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	r := newRenderer(fallback)
	var got []LogLine
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		got = append(got, r.render([]byte(line))...)
	}
	if len(got) == 0 || got[0].Kind != "init" || !strings.Contains(got[0].Text, fixtureSessionID) {
		t.Fatalf("first line = %+v, want init with the session id", got)
	}
	if !got[0].At.Equal(fallback) {
		t.Errorf("init time = %v, want the fallback %v (no timestamp before it)", got[0].At, fallback)
	}
	var tool, result *LogLine
	for i := range got {
		switch got[i].Kind {
		case "tool":
			tool = &got[i]
		case "result":
			result = &got[i]
		}
	}
	if tool == nil || !strings.HasPrefix(tool.Text, "Write ") || tool.At.Equal(fallback) {
		t.Fatalf("tool line = %+v, want Write with the line's timestamp", tool)
	}
	if result == nil || !strings.HasPrefix(result.Text, "success") {
		t.Fatalf("result line = %+v", result)
	}
}

func TestRenderToolInputSummary(t *testing.T) {
	r := newRenderer(time.Now())
	line := `{"type":"assistant","timestamp":"2026-11-03T10:11:12.000Z","message":{"content":[{"type":"tool_use","id":"x","name":"Custom","input":{"env":{"API_KEY":"s3cret-1"},"api_token":"s3cret-2","PASSWORD":"s3cret-3","target":"prod","count":3}}]}}`
	got := r.render([]byte(line))
	if len(got) != 1 || got[0].Kind != "tool" {
		t.Fatalf("got %+v", got)
	}
	text := got[0].Text
	if strings.Contains(text, "s3cret") {
		t.Fatalf("summary leaks a secret: %q", text)
	}
	for _, want := range []string{"Custom", "target=prod", "count=3", "api_token=***"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary %q lacks %q", text, want)
		}
	}
	if want := time.Date(2026, 11, 3, 10, 11, 12, 0, time.UTC); !got[0].At.Equal(want) {
		t.Errorf("At = %v, want %v", got[0].At, want)
	}

	long := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"y","name":"Bash","input":{"command":"` + strings.Repeat("echo hello; ", 30) + `"}}]}}`
	got = r.render([]byte(long))
	summary := strings.TrimPrefix(got[0].Text, "Bash ")
	if n := utf8.RuneCountInString(summary); n > 80 || !strings.HasPrefix(summary, "echo hello;") {
		t.Fatalf("summary (%d runes) = %q", n, summary)
	}
	// A later line without a timestamp keeps the last one seen.
	if !got[0].At.Equal(time.Date(2026, 11, 3, 10, 11, 12, 0, time.UTC)) {
		t.Errorf("At = %v, want the previous timestamp", got[0].At)
	}
}

func TestRenderOtherAndMalformed(t *testing.T) {
	r := newRenderer(time.Now())
	got := r.render([]byte(`{"type":"system","subtype":"thinking_tokens","secret":"zzz"}`))
	if len(got) != 1 || got[0].Kind != "other" || got[0].Text != "system/thinking_tokens" {
		t.Fatalf("got %+v", got)
	}
	got = r.render([]byte(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"private musings"}]}}`))
	if len(got) != 1 || got[0].Kind != "other" || got[0].Text != "assistant/thinking" {
		t.Fatalf("thinking = %+v", got)
	}
	got = r.render([]byte(`not json at all`))
	if len(got) != 1 || got[0].Kind != "other" || strings.Contains(got[0].Text, "not json") {
		t.Fatalf("malformed = %+v", got)
	}
	got = r.render([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello\n\nworld   there"}]}}`))
	if len(got) != 1 || got[0].Kind != "say" || got[0].Text != "Hello world there" {
		t.Fatalf("say = %+v", got)
	}
}
