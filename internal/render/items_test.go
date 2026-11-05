package render

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// The session's bookkeeping lines (system lines other than init, rate limit events) are not shown: they render to
// no items. Lines of an unknown type still say what they were.
func TestItemsDropTranscriptNoise(t *testing.T) {
	for _, line := range []string{
		`{"type":"system","subtype":"task_progress","task_id":"x","description":"Reading files"}`,
		`{"type":"system","subtype":"thinking_tokens","tokens":120}`,
		`{"type":"system","subtype":"hook_started","hook_name":"PreToolUse"}`,
		`{"type":"system","subtype":"hook_response","stdout":"{}"}`,
		`{"type":"system","subtype":"background_tasks_changed"}`,
		`{"type":"system","subtype":"vcs_state_changed"}`,
		`{"type":"system"}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1794085200}}`,
		`{"type":"tool_progress","tool_use_id":"toolu_1-heartbeat-0","tool_name":"Bash","elapsed_time_seconds":30,"heartbeat":true}`,
		`{"type":"command_lifecycle","command_uuid":"c1","state":"queued"}`,
	} {
		if got := Items([]byte(line), 10); len(got) != 0 {
			t.Errorf("%s rendered %d items (%q), want none", line, len(got), got[0].GetText())
		}
	}
	init := Items([]byte(`{"type":"system","subtype":"init","session_id":"s-1","model":"claude-haiku-4-5-20251001"}`), 0)
	if len(init) != 1 || init[0].Kind != "init" || init[0].Text != "session s-1 model claude-haiku-4-5-20251001" {
		t.Fatalf("init = %+v", init)
	}
	other := Items([]byte(`{"type":"brand_new_kind","x":1}`), 3)
	if len(other) != 1 || other[0].Kind != "other" || other[0].Text != "brand_new_kind" || other[0].Offset != 3 {
		t.Fatalf("unknown type = %+v", other)
	}
}

// Thinking arrives redacted upstream (empty text): such a block shows nothing, and a line of only that renders to
// no items, not an "assistant" placeholder.
func TestItemsDropEmptyThinking(t *testing.T) {
	empty := `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"","signature":"EoQJ"},{"type":"thinking","thinking":"  \n "}]}}`
	if got := Items([]byte(empty), 0); len(got) != 0 {
		t.Fatalf("empty thinking = %+v, want no items", got)
	}
	mixed := `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":""},{"type":"thinking","thinking":"Check the handler"},{"type":"text","text":"Done."}]}}`
	got := Items([]byte(mixed), 0)
	if len(got) != 2 || got[0].Kind != "thinking" || got[0].Text != "Check the handler" || got[1].Kind != "say" {
		t.Fatalf("mixed = %+v", got)
	}
}

// The owner's message is its own kind, its text plain and redacted.
func TestItemsUserMessage(t *testing.T) {
	got := Items([]byte(`{"type":"user","message":{"role":"user","content":"Run the tests too, token=ghp_abcdefabcdefabcdef"}}`), 4)
	if len(got) != 1 || got[0].Kind != "user" || !strings.HasPrefix(got[0].Text, "Run the tests too") || strings.Contains(got[0].Text, "ghp_") {
		t.Fatalf("user = %+v", got)
	}
}

// The result line carries the session's wall time, in the same key=value form as the rest.
func TestItemsResultDuration(t *testing.T) {
	line := `{"type":"result","subtype":"success","is_error":false,"num_turns":24,"total_cost_usd":0.43121,"duration_ms":372000,"result":"Shipped."}`
	got := Items([]byte(line), 0)
	if len(got) != 1 || got[0].Kind != "result" || got[0].Text != "success turns=24 cost=$0.4312 duration_ms=372000\nShipped." {
		t.Fatalf("result = %+v", got)
	}
}

func detailOf(t *testing.T, input string) (string, map[string]any) {
	t.Helper()
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu","name":"X","input":` + input + `}]}}`
	items := Items([]byte(line), 0)
	if len(items) != 1 || items[0].Kind != "tool_call" {
		t.Fatalf("items = %+v", items)
	}
	d := items[0].Detail
	var m map[string]any
	if err := json.Unmarshal([]byte(d), &m); err != nil {
		t.Fatalf("detail %q is not a JSON object: %v", d, err)
	}
	return d, m
}

// A tool call carries its input as compact JSON: strings redacted, secret keys masked, numbers as they were. Text
// stays the one-line summary.
func TestItemsToolDetail(t *testing.T) {
	in := `{"command":"GITHUB_TOKEN=ghp_s3cretvalue1234 gh pr create","description":"Open the pull request","timeout":120000,` +
		`"env":{"A":"s3cret-env"},"api_token":"s3cret-tok","run_in_background":false,` +
		`"todos":[{"content":"Write tests","status":"completed"},{"content":"Ship","status":"in_progress","password":"s3cret-n"}]}`
	d, m := detailOf(t, in)
	if strings.Contains(d, "s3cret") || strings.Contains(d, "ghp_") {
		t.Fatalf("detail leaks a secret: %s", d)
	}
	if strings.ContainsAny(d, "\n") || strings.Contains(d, ": ") {
		t.Errorf("detail is not compact: %s", d)
	}
	if m["description"] != "Open the pull request" || !strings.HasSuffix(m["command"].(string), "gh pr create") {
		t.Errorf("detail = %s", d)
	}
	if !strings.Contains(d, `"timeout":120000`) || m["run_in_background"] != false {
		t.Errorf("scalars changed: %s", d)
	}
	if m["env"] != "***" || m["api_token"] != "***" {
		t.Errorf("sensitive keys not masked: %s", d)
	}
	todos := m["todos"].([]any)
	if len(todos) != 2 || todos[0].(map[string]any)["status"] != "completed" || todos[1].(map[string]any)["password"] != "***" {
		t.Errorf("nested values: %s", d)
	}

	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu","name":"Bash","input":{"command":"ls -la","description":"List files"}}]}}`
	if it := Items([]byte(line), 0)[0]; it.Text != "ls -la" {
		t.Errorf("text = %q, want the summary", it.Text)
	}
	res := Items([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu","content":"ok"}]}}`), 0)
	if res[0].Detail != "" {
		t.Errorf("a tool result has a detail: %q", res[0].Detail)
	}
}

// Long values are clipped to about 2000 runes; their full line count is kept under "_lines", so +N −M stays true.
// Past 8 KiB in all the big values go and the keys stay.
func TestItemsToolDetailCaps(t *testing.T) {
	enc := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	content := strings.Repeat("é line of the new file\n", 400) // 400 lines, 9200 runes
	d, m := detailOf(t, `{"file_path":"/w/app.go","content":`+enc(content)+`}`)
	got := m["content"].(string)
	if n := utf8.RuneCountInString(got); n > DetailValueMaxRunes+1 || !strings.HasSuffix(got, "…") || !strings.HasPrefix(content, strings.TrimSuffix(got, "…")) {
		t.Fatalf("content not clipped: %d runes", n)
	}
	if lines := m["_lines"].(map[string]any); lines["content"] != float64(400) {
		t.Fatalf("_lines = %v, want content: 400", m["_lines"])
	}
	if len(d) > DetailMaxBytes {
		t.Fatalf("detail is %d bytes", len(d))
	}

	big := strings.Repeat("x", 3000)
	d, m = detailOf(t, `{"file_path":"/w/a.go","old_string":`+enc(big+"\n"+big)+`,"new_string":`+enc(big)+`,"edits":[{"old_string":`+enc(big)+`,"new_string":`+enc(big)+`}],"replace_all":true}`)
	if len(d) > DetailMaxBytes || !utf8.ValidString(d) {
		t.Fatalf("detail is %d bytes, over the cap", len(d))
	}
	if m["file_path"] != "/w/a.go" || m["replace_all"] != true {
		t.Fatalf("small values lost: %s", d)
	}
	for _, k := range []string{"old_string", "new_string", "edits"} {
		if _, ok := m[k]; !ok {
			t.Errorf("key %s dropped: %s", k, d)
		}
	}
	if lines := m["_lines"].(map[string]any); lines["old_string"] != float64(2) || lines["edits.0.new_string"] != float64(1) {
		t.Errorf("_lines = %v", m["_lines"])
	}
}
