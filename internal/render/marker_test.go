package render

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMarkerRendersRuntimeItem(t *testing.T) {
	at := time.Date(2030, 1, 2, 3, 4, 5, 6, time.UTC)
	line := MarkerLine(Marker{At: at, Runtime: "codex", Model: "gpt-5-codex", Assignment: "a_1", Role: "review", Reason: "review"})
	if line[len(line)-1] == '\n' {
		t.Fatal("MarkerLine must not end in a newline")
	}
	var top map[string]any
	if err := json.Unmarshal(line, &top); err != nil || top["athena"] != "session" || top["runtime"] != "codex" ||
		top["model"] != "gpt-5-codex" || top["assignment"] != "a_1" || top["role"] != "review" || top["reason"] != "review" {
		t.Fatalf("line = %s (%v)", line, err)
	}
	got := Items(line, 42)
	if len(got) != 1 {
		t.Fatalf("items = %+v", got)
	}
	it := got[0]
	if it.Kind != "runtime" || it.Tool != "codex" || it.Text != "gpt-5-codex · review" || it.Offset != 42 || !it.At.AsTime().Equal(at) {
		t.Fatalf("item = %+v", it)
	}
	d := detailMap(t, it)
	if d["model"] != "gpt-5-codex" || d["reason"] != "review" || d["role"] != "review" {
		t.Errorf("detail = %v", d)
	}
	// no model: the text is the reason alone.
	none := Items(MarkerLine(Marker{At: at, Runtime: "claude", Reason: "start"}), 0)
	if len(none) != 1 || none[0].Text != "start" || none[0].Tool != "claude" {
		t.Fatalf("no model = %+v", none)
	}
}
