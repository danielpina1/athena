package render

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	r := New(fallback)
	var got []LogLine
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		got = append(got, r.Render([]byte(line))...)
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
	r := New(time.Now())
	line := `{"type":"assistant","timestamp":"2026-11-03T10:11:12.000Z","message":{"content":[{"type":"tool_use","id":"x","name":"Custom","input":{"env":{"API_KEY":"s3cret-1"},"api_token":"s3cret-2","PASSWORD":"s3cret-3","target":"prod","count":3}}]}}`
	got := r.Render([]byte(line))
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
	got = r.Render([]byte(long))
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
	r := New(time.Now())
	got := r.Render([]byte(`{"type":"system","subtype":"thinking_tokens","secret":"zzz"}`))
	if len(got) != 1 || got[0].Kind != "other" || got[0].Text != "system/thinking_tokens" {
		t.Fatalf("got %+v", got)
	}
	got = r.Render([]byte(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"private musings"}]}}`))
	if len(got) != 1 || got[0].Kind != "other" || got[0].Text != "assistant/thinking" {
		t.Fatalf("thinking = %+v", got)
	}
	got = r.Render([]byte(`not json at all`))
	if len(got) != 1 || got[0].Kind != "other" || strings.Contains(got[0].Text, "not json") {
		t.Fatalf("malformed = %+v", got)
	}
	got = r.Render([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello\n\nworld   there"}]}}`))
	if len(got) != 1 || got[0].Kind != "say" || got[0].Text != "Hello world there" {
		t.Fatalf("say = %+v", got)
	}
}

func TestRenderRedactsSecretsInValues(t *testing.T) {
	r := New(time.Now())
	for _, line := range []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a","name":"Bash","input":{"command":"GITHUB_TOKEN=ghp_s3cret gh release create"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"b","name":"Bash","input":{"command":"curl -H 'Authorization: Bearer s3cret-tok' https://u:s3cret-pw@api.x/?access_token=s3cret-q"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"I ran mysql --password s3cret-db for you"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c","name":"Custom","input":{"note":"API_KEY=s3cret-k"}}]}}`,
	} {
		for _, l := range r.Render([]byte(line)) {
			if strings.Contains(l.Text, "s3cret") {
				t.Errorf("rendered line leaks a secret: %+v", l)
			}
		}
	}
}

const fixtureSessionID = "27abadfa-c58f-4e97-aaf3-64a8fdaff179"

func claudeFixture(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "runtime", "claude", "testdata", "claude", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRenderRedactsAndCaps(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a1B2", 10)
	say := strings.Repeat("x", SayMaxBytes+500) + " " + secret
	res := "token=" + secret + " " + strings.Repeat("é", ResultMaxBytes)
	line := func(typ, block string) []byte {
		return []byte(`{"type":"` + typ + `","timestamp":"2026-11-03T10:11:12.000Z","message":{"content":[` + block + `]}}`)
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return string(b) }

	items := Items(line("assistant", `{"type":"text","text":`+enc(secret+" "+say)+`}`), 42)
	if len(items) != 1 || items[0].Kind != "say" || items[0].Offset != 42 || items[0].At == nil {
		t.Fatalf("say = %+v", items)
	}
	if got := items[0].Text; len(got) > SayMaxBytes || !strings.HasSuffix(got, "…") || strings.Contains(got, "ghp_") || !utf8.ValidString(got) {
		t.Fatalf("say not redacted and capped: %d bytes, secret %v", len(got), strings.Contains(got, "ghp_"))
	}
	items = Items(line("assistant", `{"type":"thinking","thinking":`+enc(say)+`}`), 0)
	if items[0].Kind != "thinking" || len(items[0].Text) > SayMaxBytes {
		t.Fatalf("thinking = %d bytes kind %s", len(items[0].Text), items[0].Kind)
	}
	items = Items(line("assistant", `{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"ls -la","description":"x"}}`), 7)
	if it := items[0]; it.Kind != "tool_call" || it.Tool != "Bash" || it.ToolUseId != "tu1" || it.Text != "ls -la" {
		t.Fatalf("tool call = %+v", it)
	}
	items = Items(line("user", `{"type":"tool_result","tool_use_id":"tu1","is_error":true,"content":`+enc(res)+`}`), 9)
	if it := items[0]; it.Kind != "tool_result" || it.ToolUseId != "tu1" || !it.IsError || len(it.Text) > ResultMaxBytes ||
		strings.Contains(it.Text, "ghp_") || !utf8.ValidString(it.Text) || !strings.HasSuffix(it.Text, "…") {
		t.Fatalf("tool result = %d bytes %.60q", len(it.Text), it.Text)
	}
	items = Items(line("user", `{"type":"tool_result","tool_use_id":"tu2","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`), 0)
	if items[0].Text != "a\nb" {
		t.Fatalf("block content = %q", items[0].Text)
	}
	if got := Items([]byte("not json"), 5); len(got) != 1 || got[0].Kind != "other" || got[0].Offset != 5 {
		t.Fatalf("garbage = %+v", got)
	}
	if got := Items(nil, 5); len(got) != 1 || got[0].Kind != "other" {
		t.Fatalf("oversized = %+v", got)
	}
}

func TestItemsFixture(t *testing.T) {
	data, err := os.ReadFile(claudeFixture(t, "simple-turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	var off int64
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		for _, it := range Items([]byte(line), off) {
			kinds[it.Kind] = true
			if it.Offset != off {
				t.Fatalf("offset = %d, want %d", it.Offset, off)
			}
		}
		off += int64(len(line)) + 1
	}
	for _, k := range []string{"init", "tool_call", "result"} {
		if !kinds[k] {
			t.Errorf("no %s item in %v", k, kinds)
		}
	}
}

// Secrets are masked before the cut, however many there are: no part of one survives where the text is cut.
func TestRenderManySecretsNoFragment(t *testing.T) {
	tok := "abcdefghijklmnopqrstuvwxyz0123456789ABCDEF"
	text := strings.Repeat("API_TOKEN="+tok+" and plain words\n", 6000)
	b, _ := json.Marshal(text)
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":` + string(b) + `}]}}`)
	for _, it := range Items(line, 0) {
		if strings.Contains(it.Text, "abcdefghij") || strings.Contains(it.Text, "API_TOKEN="+tok[:6]) || len(it.Text) > SayMaxBytes {
			t.Fatalf("text keeps a secret fragment or is not capped (%d bytes)", len(it.Text))
		}
	}
}
