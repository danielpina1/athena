package codex

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

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

// The re-reviewer's probe: ANSI padding up to the cut, then a quoted password with no newline. No prefix of the secret
// is in the transcript line or in what render shows.
func TestCutNeverLeavesPartOfASecret(t *testing.T) {
	for _, pad := range []int{outputKeep/4 - 3, outputKeep/4 - 6, outputKeep/4 + 20} {
		out := strings.Repeat("\x1b[0m", pad) + `PASSWORD="hunter2 hunter3 hunter4"`
		out += strings.Repeat("\x1b[0m", 80<<10)
		raw := `{"method":"item/completed","params":{"item":{"id":"c","type":"commandExecution","command":"x","cwd":"/w","status":"completed","aggregatedOutput":` + jstr(out) + `,"exitCode":0}}}`
		got := capLine([]byte(raw))
		if len(got) > maxEnvelopeLine || !json.Valid(got) {
			t.Fatalf("pad %d: %d bytes valid=%v", pad, len(got), json.Valid(got))
		}
		for _, frag := range []string{"hunter", `PASSWORD="h`, `PASSWORD=\"h`} {
			if strings.Contains(string(got), frag) {
				t.Fatalf("pad %d: %q in the transcript line", pad, frag)
			}
		}
		for _, it := range render.Items([]byte(`{"at":"2030-01-01T00:00:00Z","codex":`+string(got)+`}`), 0) {
			if strings.Contains(it.Text, "hunter") || strings.Contains(it.Text, `PASSWORD="h`) {
				t.Fatalf("pad %d: shown %q", pad, it.Text)
			}
		}
	}
	// a long single line keeps its leading content (cut at a rune boundary), with a URL's userinfo redacted first.
	long := "https://user:pass@example.com/" + strings.Repeat("a", 300<<10)
	got := capLine([]byte(`{"method":"item/completed","params":{"item":{"id":"a","type":"agentMessage","text":` + jstr(long) + `}}}`))
	if strings.Contains(string(got), "user:pass") || !strings.Contains(string(got), "example.com/aaaa") || len(got) > maxEnvelopeLine {
		t.Errorf("single line: %.120s (%d)", got, len(got))
	}
}

// Many large fields with multibyte text at the cut share the cap in one pass: valid JSON, under the cap, one mark each.
func TestCutManyFieldsMultibyte(t *testing.T) {
	var fields []string
	for i := 0; i < 80; i++ {
		fields = append(fields, `"f`+string(rune('a'+i%26))+strings.Repeat("x", i)+`":`+jstr(strings.Repeat("日本語 line\n", 700+i)))
	}
	raw := `{"method":"item/completed","params":{"item":{"id":"a","type":"x",` + strings.Join(fields, ",") + `}}}`
	got := capLine([]byte(raw))
	if len(got) > maxEnvelopeLine || !json.Valid(got) || strings.Contains(string(got), "line-too-large") {
		t.Fatalf("%d bytes valid=%v", len(got), json.Valid(got))
	}
	var v struct {
		Params struct{ Item map[string]string } `json:"params"`
	}
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatal(err)
	}
	for k, s := range v.Params.Item {
		if strings.Count(s, "[cut: ") > 1 || strings.ContainsRune(s, utf8.RuneError) {
			t.Errorf("%s: marks %d, bad rune", k, strings.Count(s, "[cut: "))
		}
	}
	// the non-string part alone over the cap is the only reason for a stub
	var keys []string
	for i := 0; i < 30000; i++ {
		keys = append(keys, fmt.Sprintf(`"key%08d":1`, i))
	}
	big := `{"method":"m","params":{` + strings.Join(keys, ",") + `}}`
	if g := capLine([]byte(big)); !strings.Contains(string(g), "line-too-large") {
		t.Errorf("non-string overhead over the cap: %.60s", g)
	}
}

// The re-reviewer's probe: an ANSI code between the name and its value, a newline inside the quoted value, and the cut
// falling on that newline. Escapes are stripped before the redactor runs, so nothing of the secret is kept.
func TestCutAfterAnsiNeverLeavesPartOfASecret(t *testing.T) {
	secret := "\x1b[1mPASSWORD\x1b[0m=\"hunter2 a\nhunter3 b\""
	for _, pad := range []int{outputKeep - 40, outputKeep - 30, outputKeep - 20} {
		out := strings.Repeat("p", pad) + "\n" + secret + strings.Repeat("z", 90<<10)
		raw := `{"method":"item/completed","params":{"item":{"id":"c","type":"commandExecution","command":"x","cwd":"/w","status":"completed","aggregatedOutput":` + jstr(out) + `,"exitCode":0}}}`
		got := capLine([]byte(raw))
		if strings.Contains(string(got), "hunter") {
			t.Fatalf("pad %d: part of the secret in the transcript line", pad)
		}
		for _, it := range render.Items([]byte(`{"at":"2030-01-01T00:00:00Z","codex":`+string(got)+`}`), 0) {
			if strings.Contains(it.Text, "hunter") {
				t.Fatalf("pad %d: shown %q", pad, it.Text)
			}
		}
	}
}

// A cut is made once, and its mark counts the bytes of the original that are gone.
func TestCutMarkCountsTheRealCut(t *testing.T) {
	out := strings.Repeat("0123456789abcdef\n", 50<<10) // 850 KiB
	raw := `{"method":"item/completed","params":{"item":{"id":"c","type":"commandExecution","command":"x","cwd":"/w","status":"completed","aggregatedOutput":` + jstr(out) + `,"exitCode":0}}}`
	got := capLine([]byte(raw))
	var v struct {
		Params struct {
			Item struct{ AggregatedOutput string }
		} `json:"params"`
	}
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatal(err)
	}
	s := v.Params.Item.AggregatedOutput
	if strings.Count(s, "[cut: ") != 1 {
		t.Fatalf("marks in %.0q", s[len(s)-60:])
	}
	i := strings.LastIndex(s, "\n[cut: ")
	var n int
	if _, err := fmt.Sscanf(s[i:], "\n[cut: %d bytes]", &n); err != nil {
		t.Fatal(err)
	}
	if n != len(out)-i {
		t.Errorf("mark says %d, cut %d", n, len(out)-i)
	}
}
