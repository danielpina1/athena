package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/runtime/rt"
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

func TestParseUserEcho(t *testing.T) {
	evs, err := ParseEvents([]byte(`{"type":"user","message":{"role":"user","content":"say BANANA"},"isReplay":true,"session_id":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0] != (UserEcho{Text: "say BANANA"}) {
		t.Fatalf("events = %#v, want the echo", evs)
	}
	// A user message that is not a replay is no echo.
	evs, _ = ParseEvents([]byte(`{"type":"user","message":{"role":"user","content":"hi"}}`))
	if _, ok := evs[0].(UserEcho); ok {
		t.Fatal("a non-replay user line parsed as an echo")
	}
	// The echo carries the uuid the message was sent with, whatever shape its content comes back in.
	evs, err = ParseEvents([]byte(`{"type":"user","uuid":"u-1","isReplay":true,` +
		`"message":{"role":"user","content":[{"type":"text","text":"say"},{"type":"text","text":"BANANA"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0] != (UserEcho{Text: "say\nBANANA", UUID: "u-1"}) {
		t.Fatalf("events = %#v, want the echo with its uuid", evs)
	}
}

// rtOf parses a fixture line and translates its events for the node, as the runtime's sessions do.
func rtOf(t *testing.T, line []byte) []rt.Event {
	t.Helper()
	evs, err := ParseEvents(line)
	if err != nil {
		t.Fatal(err)
	}
	var out []rt.Event
	for _, ev := range evs {
		out = append(out, toRT(ev))
	}
	return out
}

func fixtureLine(t *testing.T, name, typ string) []byte {
	t.Helper()
	for _, l := range readFixture(t, name) {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(l, &head) == nil && head.Type == typ {
			return l
		}
	}
	t.Fatalf("%s has no %s line", name, typ)
	return nil
}

func TestClaudeResultBecomesTurnEnd(t *testing.T) {
	evs := rtOf(t, fixtureLine(t, "simple-turn.jsonl", "result"))
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	want := rt.TurnEnd{SessionID: "27abadfa-c58f-4e97-aaf3-64a8fdaff179", OK: true, Subtype: "success", Text: "Done.",
		CostUSD: 0.0227632, InputTokens: 18, OutputTokens: 215, CacheReadTokens: 51902, NumTurns: 2,
		Duration: 4973 * time.Millisecond}
	if got, ok := evs[0].(rt.TurnEnd); !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", evs[0], want)
	}

	failed, ok := rtOf(t, fixtureLine(t, "max-turns.jsonl", "result"))[0].(rt.TurnEnd)
	if !ok || failed.OK || failed.Subtype != "error_max_turns" || len(failed.Errors) != 1 || failed.Limit != nil {
		t.Fatalf("max turns = %#v", failed)
	}
	// Not OK: an error flag, or any subtype but success.
	for line, wantOK := range map[string]bool{
		`{"type":"result","subtype":"success","is_error":false}`:                true,
		`{"type":"result","is_error":false}`:                                    true,
		`{"type":"result","subtype":"success","is_error":true}`:                 false,
		`{"type":"result","subtype":"error_during_execution","is_error":false}`: false,
	} {
		if e := rtOf(t, []byte(line))[0].(rt.TurnEnd); e.OK != wantOK {
			t.Errorf("%s: OK = %v, want %v", line, e.OK, wantOK)
		}
	}
}

func TestClaudeEventsTranslate(t *testing.T) {
	for _, c := range []struct {
		in   Event
		want rt.Event
	}{
		{Init{SessionID: "s", Model: "m", CWD: "/w", Tools: []string{"Bash"}}, rt.Init{SessionID: "s", Model: "m"}},
		{AssistantText{Text: "hi"}, rt.AssistantText{Text: "hi"}},
		{ToolUse{ID: "t1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}, rt.ToolUse{ID: "t1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}},
		{ToolResult{ToolUseID: "t1", IsError: true}, rt.ToolResult{ID: "t1", IsError: true}},
		{UserEcho{Text: "go", UUID: "u"}, rt.UserEcho{Text: "go", UUID: "u"}},
		{Unknown{Type: "stream_event", Raw: json.RawMessage(`{}`)}, rt.Unknown{Kind: "stream_event"}},
	} {
		if got := toRT(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("toRT(%#v) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestClaudeRateLimitEventBecomesQuota(t *testing.T) {
	evs := rtOf(t, fixtureLine(t, "simple-turn.jsonl", "rate_limit_event"))
	q, ok := evs[0].(rt.QuotaUpdate)
	if len(evs) != 1 || !ok {
		t.Fatalf("got %#v, want one QuotaUpdate", evs)
	}
	reset := time.Unix(1794085200, 0)
	if len(q.Windows) != 1 {
		t.Fatalf("windows = %#v", q.Windows)
	}
	w := q.Windows[0]
	if w.Name != "7d" || w.Used != 0.12 || w.ResetsAt == nil || !w.ResetsAt.Equal(reset) {
		t.Fatalf("window = %+v (resets %v), want 7d at 0.12 resetting %v", w, w.ResetsAt, reset)
	}
	// Every fixture says overageStatus "rejected": that is the overage's state, not the window's.
	if w.Limited {
		t.Fatal("an allowed window reads as limited")
	}
}

// Only rate_limit_info.status "rejected" limits, and only the window rateLimitType names.
func TestClaudeRateLimitRejectedLimitsItsWindow(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1794000000,"rateLimitType":"five_hour",` +
		`"overageStatus":"rejected","unifiedWindows":{"five_hour":{"utilization":1,"resetsAt":1794000000},"seven_day":{"utilization":0.4,"resetsAt":1794085200}}}}`
	q := rtOf(t, []byte(line))[0].(rt.QuotaUpdate)
	if len(q.Windows) != 2 {
		t.Fatalf("windows = %+v", q.Windows)
	}
	five, seven := q.Windows[0], q.Windows[1]
	if five.Name != "5h" || !five.Limited || five.Used != 1 || five.ResetsAt == nil || five.ResetsAt.Unix() != 1794000000 {
		t.Fatalf("5h = %+v", five)
	}
	if seven.Name != "7d" || seven.Limited || seven.Used != 0.4 {
		t.Fatalf("7d = %+v", seven)
	}
	// A rejection with no utilization for its window: the window is used up, resetting when the event says.
	bare := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1794000000,"rateLimitType":"five_hour"}}`
	q = rtOf(t, []byte(bare))[0].(rt.QuotaUpdate)
	if len(q.Windows) != 1 || q.Windows[0].Name != "5h" || !q.Windows[0].Limited || q.Windows[0].Used != 1 ||
		q.Windows[0].ResetsAt == nil || q.Windows[0].ResetsAt.Unix() != 1794000000 {
		t.Fatalf("bare rejection = %+v", q.Windows)
	}
	// Utilization stays a fraction; a window Athena does not name is left out; an event with nothing usable is Unknown.
	odd := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":1.7},"seven_day_opus":{"utilization":0.2}}}}`
	q = rtOf(t, []byte(odd))[0].(rt.QuotaUpdate)
	if len(q.Windows) != 1 || q.Windows[0].Used != 1 || q.Windows[0].ResetsAt != nil {
		t.Fatalf("odd = %+v", q.Windows)
	}
	if u, ok := rtOf(t, []byte(`{"type":"rate_limit_event","x":1}`))[0].(rt.Unknown); !ok || u.Kind != "rate_limit_event" {
		t.Fatalf("an event without rate_limit_info = %#v", u)
	}
}

// turnsOf translates lines as one session does (a limit seen mid-turn belongs to the turn's end), and returns the
// TurnEnds.
func turnsOf(t *testing.T, lines ...string) []rt.TurnEnd {
	t.Helper()
	var w limitWatch
	var out []rt.TurnEnd
	for _, l := range lines {
		evs, err := ParseEvents([]byte(l))
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			if te, ok := w.translate(ev).(rt.TurnEnd); ok {
				out = append(out, te)
			}
		}
	}
	return out
}

const (
	errorResult   = `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","session_id":"s"}`
	successResult = `{"type":"result","subtype":"success","is_error":false,"result":"Done.","session_id":"s"}`
)

// A turn that saw a rejected rate_limit_event and failed stopped at its usage limit: its TurnEnd carries the limit,
// resetting when the event said, whichever window it names (one Athena does not map too).
func TestRejectedRateLimitSetsTurnLimit(t *testing.T) {
	rejected := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1794000000,"rateLimitType":"five_hour"}}`
	ends := turnsOf(t, string(fixtureLine(t, "simple-turn.jsonl", "system")), rejected, errorResult)
	if len(ends) != 1 || ends[0].Limit == nil {
		t.Fatalf("ends = %+v, want one with a limit", ends)
	}
	l := ends[0].Limit
	if l.Runtime != "claude" || l.ResetsAt == nil || l.ResetsAt.Unix() != 1794000000 || l.Transient {
		t.Fatalf("limit = %+v", l)
	}
	// A window Athena does not map (per model) still limits, with the event's reset.
	opus := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1794000100,"rateLimitType":"seven_day_opus"}}`
	if ends = turnsOf(t, opus, errorResult); len(ends) != 1 || ends[0].Limit == nil || ends[0].Limit.ResetsAt.Unix() != 1794000100 {
		t.Fatalf("unmapped window: %+v", ends)
	}
	// The window's own reset, when the event gives only that.
	unified := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"seven_day","unifiedWindows":{"seven_day":{"utilization":1,"resetsAt":1794000200}}}}`
	if ends = turnsOf(t, unified, errorResult); len(ends) != 1 || ends[0].Limit == nil || ends[0].Limit.ResetsAt.Unix() != 1794000200 {
		t.Fatalf("window reset: %+v", ends)
	}
	// No reset at all: a limit all the same.
	bare := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`
	if ends = turnsOf(t, bare, errorResult); len(ends) != 1 || ends[0].Limit == nil || ends[0].Limit.ResetsAt != nil {
		t.Fatalf("no reset: %+v", ends)
	}
	// An allowed event is no limit, and a rejection does not outlive its turn: the next failed turn is not limited.
	allowed := string(fixtureLine(t, "simple-turn.jsonl", "rate_limit_event"))
	if ends = turnsOf(t, allowed, errorResult, rejected, successResult, errorResult); len(ends) != 3 ||
		ends[0].Limit != nil || ends[1].Limit != nil || ends[2].Limit != nil {
		t.Fatalf("allowed, then a rejection in a turn that ended well, then a failed turn: %+v", ends)
	}
}

// A failed turn whose result says it hit a limit is limited, without a rate_limit_event; another failure, or a turn
// that ended well whatever it says, is not. A rate limit that names no usage limit is transient (a rate of requests);
// Claude Code's "usage limit reached|<unix time>" says when it resets.
func TestUsageLimitResultTextSetsLimit(t *testing.T) {
	type want struct {
		limited, transient bool
		reset              int64
	}
	for line, w := range map[string]want{
		`{"type":"result","subtype":"success","is_error":true,"result":"Claude AI usage limit reached|1794000000"}`:        {true, false, 1794000000},
		`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["API Error: Rate limit exceeded"]}`: {true, true, 0},
		`{"type":"result","subtype":"success","is_error":true,"result":"API Error: 429 rate limit · usage limit reached"}`: {true, false, 0},
		`{"type":"result","subtype":"success","is_error":true,"result":"You've hit your limit · resets 3pm"}`:              {true, false, 0},
		`{"type":"result","subtype":"success","is_error":true,"result":"5-hour limit reached ∙ resets 3pm"}`:               {true, false, 0},
		`{"type":"result","subtype":"error_max_turns","is_error":true,"errors":["Reached maximum number of turns (1)"]}`:   {},
		`{"type":"result","subtype":"success","is_error":false,"result":"I added a rate limit to the API."}`:               {},
	} {
		ends := turnsOf(t, line)
		if len(ends) != 1 {
			t.Fatalf("%s: %d ends", line, len(ends))
		}
		l := ends[0].Limit
		if got := l != nil; got != w.limited {
			t.Errorf("%s: limited = %v, want %v", line, got, w.limited)
			continue
		}
		if l == nil {
			continue
		}
		var reset int64
		if l.ResetsAt != nil {
			reset = l.ResetsAt.Unix()
		}
		if l.Runtime != "claude" || l.Message == "" || l.Transient != w.transient || reset != w.reset {
			t.Errorf("%s: limit = %+v (reset %d), want transient %v reset %d", line, l, reset, w.transient, w.reset)
		}
	}
	// A rejected rate_limit_event's reset wins over the text's.
	rejected := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1794000500,"rateLimitType":"five_hour"}}`
	ends := turnsOf(t, rejected, `{"type":"result","subtype":"success","is_error":true,"result":"Claude AI usage limit reached|1794000000"}`)
	if l := ends[0].Limit; l == nil || l.ResetsAt == nil || l.ResetsAt.Unix() != 1794000500 || l.Transient {
		t.Fatalf("rejected then text: %+v", l)
	}
}
