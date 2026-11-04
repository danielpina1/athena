package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

func readFixture(t *testing.T, name string) [][]byte {
	t.Helper()
	f, err := os.Open("testdata/claude/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines [][]byte
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			lines = append(lines, line)
		}
		if err != nil {
			break
		}
	}
	return lines
}

func parseAll(t *testing.T, name string) []Event {
	t.Helper()
	var evs []Event
	for i, l := range readFixture(t, name) {
		got, err := ParseEvents(l)
		if err != nil {
			t.Fatalf("%s line %d: %v", name, i+1, err)
		}
		evs = append(evs, got...)
	}
	return evs
}

func TestParseFixtures(t *testing.T) {
	evs := parseAll(t, "simple-turn.jsonl")

	var init *Init
	var toolUse *ToolUse
	var toolResult *ToolResult
	var result *Result
	var texts []string
	for _, ev := range evs {
		switch e := ev.(type) {
		case Init:
			if init == nil {
				init = &e
			}
		case ToolUse:
			if toolUse == nil {
				toolUse = &e
			}
		case ToolResult:
			toolResult = &e
		case AssistantText:
			texts = append(texts, e.Text)
		case Result:
			result = &e
		}
	}
	if init == nil || init.SessionID == "" || init.Model == "" || init.CWD != "/home/user/work" || len(init.Tools) == 0 {
		t.Fatalf("bad init: %+v", init)
	}
	if toolUse == nil || toolUse.Name != "Write" || toolUse.ID == "" {
		t.Fatalf("want a Write tool use, got %+v", toolUse)
	}
	var in struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(toolUse.Input, &in); err != nil || in.Content != "hi" {
		t.Fatalf("tool input %s: %v", toolUse.Input, err)
	}
	if toolResult == nil || toolResult.ToolUseID != toolUse.ID || toolResult.IsError {
		t.Fatalf("tool result does not pair with tool use: %+v", toolResult)
	}
	if len(texts) == 0 || texts[len(texts)-1] != "Done." {
		t.Fatalf("assistant text = %q", texts)
	}
	if result == nil || result.Subtype != "success" || result.IsError || result.SessionID != init.SessionID {
		t.Fatalf("bad result: %+v", result)
	}
	if result.CostUSD <= 0 || result.InputTokens <= 0 || result.OutputTokens <= 0 || result.CacheReadTokens <= 0 || result.NumTurns != 2 || result.DurationMS <= 0 {
		t.Fatalf("result metrics not parsed: %+v", result)
	}
	if result.Text != "Done." {
		t.Fatalf("result text = %q", result.Text)
	}
}

func TestParseResumeAndMidTurnFixtures(t *testing.T) {
	var resumeInit Init
	var resumeResult Result
	for _, ev := range parseAll(t, "resume-turn.jsonl") {
		switch e := ev.(type) {
		case Init:
			resumeInit = e
		case Result:
			resumeResult = e
		}
	}
	if resumeInit.SessionID == "" || resumeResult.Subtype != "success" || resumeResult.Text != "hi.txt" {
		t.Fatalf("resume: init=%+v result=%+v", resumeInit, resumeResult)
	}

	writes, results := 0, 0
	for _, ev := range parseAll(t, "mid-turn-input.jsonl") {
		switch e := ev.(type) {
		case ToolUse:
			if e.Name == "Write" {
				writes++
			}
		case Result:
			results++
		}
	}
	if writes != 2 || results != 1 {
		t.Fatalf("mid-turn input: writes=%d results=%d, want 2 and 1", writes, results)
	}
}

func TestParseUnknownIsNotError(t *testing.T) {
	for _, line := range []string{
		`{"type":"rate_limit_event","x":1}`,
		`{"type":"system","subtype":"thinking_tokens"}`,
		`{"type":"brand_new_thing","payload":{"a":[1,2]}}`,
		`{"no_type":true}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":""}]}}`,
		`{"type":"user","message":{"role":"user","content":"plain string"}}`,
	} {
		ev, err := ParseEvent([]byte(line))
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		u, ok := ev.(Unknown)
		if !ok {
			t.Fatalf("%s: got %T, want Unknown", line, ev)
		}
		if !json.Valid(u.Raw) {
			t.Fatalf("%s: Raw is not valid JSON: %q", line, u.Raw)
		}
	}
	u, _ := ParseEvent([]byte(`{"type":"brand_new_thing","payload":1}`))
	if u.(Unknown).Type != "brand_new_thing" {
		t.Fatalf("type = %q", u.(Unknown).Type)
	}
}

func TestParseInvalidJSONIsError(t *testing.T) {
	for _, line := range []string{"", "not json", `{"type":`} {
		if _, err := ParseEvent([]byte(line)); err == nil {
			t.Fatalf("%q: want error", line)
		}
	}
}

func TestParseMultipleBlocksAndErrorResult(t *testing.T) {
	evs, err := ParseEvents([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"a"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`))
	if err != nil || len(evs) != 2 {
		t.Fatalf("evs=%v err=%v", evs, err)
	}
	if evs[0].(AssistantText).Text != "a" || evs[1].(ToolUse).Name != "Bash" {
		t.Fatalf("evs = %+v", evs)
	}
	evs, err = ParseEvents([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"boom"}]}}`))
	if err != nil || len(evs) != 1 || !evs[0].(ToolResult).IsError || evs[0].(ToolResult).ToolUseID != "t1" {
		t.Fatalf("evs=%v err=%v", evs, err)
	}
	ev, err := ParseEvent([]byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r := ev.(Result); !r.IsError || r.Subtype != "error_during_execution" {
		t.Fatalf("result = %+v", r)
	}
}

// An error result from the real CLI (max-turns.jsonl: `--max-turns 1` and a task needing two tool calls) carries its
// reason in "errors", not in "result".
func TestParseErrorResultFixture(t *testing.T) {
	evs := parseAll(t, "max-turns.jsonl")
	var r *Result
	for _, ev := range evs {
		if res, ok := ev.(Result); ok {
			r = &res
		}
	}
	if r == nil {
		t.Fatal("no result in max-turns.jsonl")
	}
	if !r.IsError || r.Subtype != "error_max_turns" || r.Text != "" {
		t.Fatalf("result = %+v", *r)
	}
	if len(r.Errors) != 1 || r.Errors[0] != "Reached maximum number of turns (1)" {
		t.Fatalf("errors = %q", r.Errors)
	}
}
