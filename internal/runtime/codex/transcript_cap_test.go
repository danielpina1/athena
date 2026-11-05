package codex

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielpina1/athena/internal/render"
	"github.com/danielpina1/athena/internal/runtime/rt"
)

// nodeLineCap mirrors internal/node maxLineBytes: a longer transcript line reaches the dashboard as nil.
const nodeLineCap = 256 << 10

func jstr(s string) string { b, _ := json.Marshal(s); return string(b) }

// A 1 MiB command output (ANSI escapes, a token, a quoted password) goes through the envelope, the node's line cap
// and render.Items, and comes out a clipped, redacted tool_result, never "line too large to show".
func TestHugeOutputSurvivesEnvelopeAndLineCap(t *testing.T) {
	const token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	out := "\x1b[31mred\x1b[0m " + token + "\nPASSWORD=\"hunter2 hunter3\"\n" + strings.Repeat("line of output \x1b[1mbold\x1b[0m\n", 40<<10)
	raw := `{"method":"item/completed","params":{"threadId":"t","item":{"id":"c","type":"commandExecution","command":"make","cwd":"/w","status":"completed","aggregatedOutput":` + jstr(out) + `,"exitCode":0}}}`
	if len(raw) < 1<<20 {
		t.Fatalf("raw is %d bytes", len(raw))
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	var lines [][]byte
	tr, err := rt.OpenTranscript(path, func(_ int64, line []byte) { lines = append(lines, append([]byte(nil), line...)) })
	if err != nil {
		t.Fatal(err)
	}
	envelope(tr)([]byte(raw))
	tr.Close()
	if len(lines) != 1 {
		t.Fatalf("lines = %d", len(lines))
	}
	line := lines[0]
	if len(line) > nodeLineCap {
		t.Fatalf("line is %d bytes, node cap %d", len(line), nodeLineCap)
	}
	if !json.Valid(line) {
		t.Fatal("line is not valid JSON")
	}
	items := render.Items(line, 0)
	if len(items) != 1 || items[0].Kind != "tool_result" || strings.Contains(items[0].Text, "too large") {
		t.Fatalf("items = %+v", items)
	}
	txt := items[0].Text
	if strings.Contains(txt, "\x1b") || strings.Contains(txt, "ghp_abc") || strings.Contains(txt, "hunter") || !strings.HasPrefix(txt, "red ") {
		t.Errorf("text not stripped and redacted: %.100q", txt)
	}
	if !strings.Contains(string(line), "[cut: ") {
		t.Error("no cut mark in the transcript line")
	}
}

// Any other long string is cut too, when the line would still pass the cap.
func TestHugeOtherStringIsCut(t *testing.T) {
	raw := `{"method":"item/completed","params":{"item":{"id":"a","type":"agentMessage","text":` + jstr(strings.Repeat("word word\n", 80<<10)) + `}}}`
	got := capLine([]byte(raw))
	if len(got) > maxEnvelopeLine || !json.Valid(got) || !strings.Contains(string(got), "[cut: ") {
		t.Fatalf("got %d bytes valid=%v", len(got), json.Valid(got))
	}
	small := []byte(`{"method":"turn/started"}`)
	if string(capLine(small)) != string(small) {
		t.Error("a short line changed")
	}
}
