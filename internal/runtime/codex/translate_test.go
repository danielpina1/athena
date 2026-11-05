package codex

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/runtime/rt"
)

// translateAll runs a fixture through a fresh Translator.
func translateAll(t *testing.T, name string) []rt.Event {
	t.Helper()
	tr := NewTranslator()
	var evs []rt.Event
	for _, m := range fixtureMessages(t, name) {
		evs = append(evs, tr.Translate(m)...)
	}
	return evs
}

// at is a time of the fixtures: they count from 2030-01-01T00:00:00Z.
func at(sec int64) *time.Time {
	t := time.Unix(1893456000+sec, 0).UTC()
	return &t
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestTranslateToolTurn(t *testing.T) {
	got := translateAll(t, "tool-turn")
	want := []rt.Event{
		rt.Init{SessionID: "thr_fixture_0001", Model: "gpt-5-codex"},
		rt.Unknown{Kind: "turn/started"},
		rt.Unknown{Kind: "userMessage"},
		rt.UserEcho{Text: "Create hi.txt, then notes.md, and report.", UUID: "c0ffee00-0000-4000-8000-000000000002"},
		rt.Unknown{Kind: "reasoning"},
		rt.Unknown{Kind: "reasoning"},
		rt.ToolUse{ID: "item_0003", Name: "exec_command", Input: raw(`{"command":"/bin/zsh -lc 'printf hi \u003e hi.txt'","cwd":"/home/user/work"}`)},
		rt.ToolResult{ID: "item_0003", IsError: false},
		rt.ToolUse{ID: "item_0004", Name: "exec_command", Input: raw(`{"command":"/bin/zsh -lc 'cat missing.txt'","cwd":"/home/user/work"}`)},
		rt.ToolResult{ID: "item_0004", IsError: true},
		rt.ToolUse{ID: "item_0006", Name: "apply_patch", Input: raw(`{"changes":[{"path":"/home/user/work/notes.md","kind":"add"},{"path":"/home/user/work/old.txt","kind":"update"}]}`)},
		rt.ToolResult{ID: "item_0006", IsError: false},
		rt.ToolUse{ID: "item_0007", Name: "mcp__athena__report", Input: raw(`{"summary":"all done"}`)},
		rt.ToolResult{ID: "item_0007", IsError: false},
		rt.ToolUse{ID: "item_0008", Name: "web_search", Input: raw(`{"query":"go json rpc"}`)},
		rt.ToolResult{ID: "item_0008", IsError: false},
		rt.Unknown{Kind: "agentMessage"},
		rt.AssistantText{Text: "Created the files and reported."},
		rt.QuotaUpdate{Windows: []rt.Window{
			{Name: "5h", Used: 0.42, ResetsAt: at(18000)},
			{Name: "7d", Used: 0.08, ResetsAt: at(604800)},
		}},
		rt.TurnEnd{
			SessionID: "thr_fixture_0001", OK: true, Text: "Created the files and reported.",
			// This turn's tokens: the change of the thread's totals over the turn; input without its cached part.
			InputTokens: 908, OutputTokens: 240, CacheReadTokens: 8192,
			NumTurns: 1, Duration: 7300 * time.Millisecond,
		},
	}
	if len(got) != len(want) {
		t.Fatalf("%d events, want %d:\n%+v", len(got), len(want), got)
	}
	for i := range want {
		if !eventsEqual(got[i], want[i]) {
			t.Errorf("event %d = %+v\nwant     %+v", i, got[i], want[i])
		}
	}
}

// eventsEqual is DeepEqual with a ToolUse's Input compared as JSON.
func eventsEqual(a, b rt.Event) bool {
	ua, ok1 := a.(rt.ToolUse)
	ub, ok2 := b.(rt.ToolUse)
	if ok1 && ok2 {
		var x, y any
		return ua.ID == ub.ID && ua.Name == ub.Name && json.Unmarshal(ua.Input, &x) == nil && json.Unmarshal(ub.Input, &y) == nil && reflect.DeepEqual(x, y)
	}
	return reflect.DeepEqual(a, b)
}

func TestTranslateUserEchoCarriesClientID(t *testing.T) {
	tr := NewTranslator()
	evs := tr.Translate(Message{Method: "item/completed", Params: raw(`{"threadId":"t","turnId":"u","completedAtMs":1,"item":{
		"id":"item_1","type":"userMessage","clientId":"11111111-2222-4333-8444-555555555555",
		"content":[{"type":"text","text":"first","text_elements":[]},{"type":"image","url":"https://example.invalid/a.png"},{"type":"text","text":"second","text_elements":[]}]}}`)})
	want := []rt.Event{rt.UserEcho{Text: "first\nsecond", UUID: "11111111-2222-4333-8444-555555555555"}}
	if !reflect.DeepEqual(evs, want) {
		t.Fatalf("events = %+v, want %+v", evs, want)
	}
	// A message sent without a client id echoes with none.
	evs = tr.Translate(Message{Method: "item/completed", Params: raw(`{"item":{"id":"i","type":"userMessage","content":[{"type":"text","text":"x"}]}}`)})
	if !reflect.DeepEqual(evs, []rt.Event{rt.UserEcho{Text: "x"}}) {
		t.Fatalf("events = %+v", evs)
	}
}

func TestTranslateUsageLimitSetsLimitWithReset(t *testing.T) {
	evs := translateAll(t, "usage-limit-turn")
	end, ok := evs[len(evs)-1].(rt.TurnEnd)
	if !ok {
		t.Fatalf("last event = %+v", evs[len(evs)-1])
	}
	want := rt.TurnEnd{
		SessionID: "thr_fixture_0001", OK: false, Subtype: "usageLimitExceeded",
		Text:   "You've hit your usage limit. Try again later.",
		Errors: []string{"You've hit your usage limit. Try again later."},
		// Only the 5h window is used up (100%); the 7d one has room, so its later reset does not count.
		Limit:    &rt.LimitHit{Runtime: "codex", ResetsAt: at(7200), Message: "You've hit your usage limit. Try again later."},
		NumTurns: 1, Duration: 900 * time.Millisecond,
	}
	if !reflect.DeepEqual(end, want) {
		t.Fatalf("TurnEnd = %+v\nwant      %+v", end, want)
	}
	// The windows of that turn say it was limited.
	q := evs[len(evs)-2].(rt.QuotaUpdate)
	if !q.Windows[0].Limited || q.Windows[0].Used != 1 || q.Windows[1].Limited {
		t.Fatalf("quota = %+v", q)
	}

	// Both windows used up: the limit lasts until the later reset. Neither: no reset is known.
	for _, c := range []struct {
		name, primary, secondary string
		want                     *time.Time
	}{
		{"both", `{"usedPercent":100,"windowDurationMins":300,"resetsAt":1893463200}`, `{"usedPercent":100,"windowDurationMins":10080,"resetsAt":1893715200}`, at(259200)},
		{"neither", `{"usedPercent":90,"windowDurationMins":300,"resetsAt":1893463200}`, `{"usedPercent":10,"windowDurationMins":10080,"resetsAt":1893715200}`, nil},
		{"unknown reset", `{"usedPercent":100,"windowDurationMins":300}`, `null`, nil},
		{"no windows", `null`, `null`, nil},
	} {
		tr := NewTranslator()
		tr.Translate(Message{Method: "account/rateLimits/updated", Params: raw(`{"rateLimits":{"primary":` + c.primary + `,"secondary":` + c.secondary + `}}`)})
		evs := tr.Translate(Message{Method: "turn/completed", Params: raw(`{"threadId":"t","turn":{"id":"u","items":[],"status":"failed","error":{"message":"limit","codexErrorInfo":"usageLimitExceeded"}}}`)})
		end := evs[0].(rt.TurnEnd)
		if end.Limit == nil || end.Limit.Runtime != "codex" || !reflect.DeepEqual(end.Limit.ResetsAt, c.want) || end.Limit.Transient {
			t.Errorf("%s: Limit = %+v, want reset %v", c.name, end.Limit, c.want)
		}
	}

	// A short rate limit is transient and has no reset.
	tr := NewTranslator()
	end = tr.Translate(Message{Method: "turn/completed", Params: raw(`{"threadId":"t","turn":{"id":"u","items":[],"status":"failed","error":{"message":"slow down","codexErrorInfo":"rateLimitExceeded"}}}`)})[0].(rt.TurnEnd)
	if end.Limit == nil || !end.Limit.Transient || end.Limit.ResetsAt != nil || end.Subtype != "rateLimitExceeded" {
		t.Fatalf("TurnEnd = %+v", end)
	}
}

func TestTranslateRateLimitWindowsNormalized(t *testing.T) {
	msgs := fixtureMessages(t, "rate-limits")
	tr := NewTranslator()
	want := [][]rt.Window{
		{{Name: "5h", Used: 0.42, ResetsAt: at(18000)}, {Name: "7d", Used: 0.08, ResetsAt: at(604800)}},
		// Limited goes on the window that is used up, not on the 7d one that has room.
		{{Name: "5h", Used: 1, ResetsAt: at(7200), Limited: true}, {Name: "7d", Used: 0.63, ResetsAt: at(259200)}},
		{{Name: "45m", Used: 0.05, ResetsAt: at(2700)}},
	}
	if len(msgs) != len(want) {
		t.Fatalf("%d messages", len(msgs))
	}
	for i, m := range msgs {
		evs := tr.Translate(m)
		if len(evs) != 1 {
			t.Fatalf("message %d: %+v", i, evs)
		}
		q, ok := evs[0].(rt.QuotaUpdate)
		if !ok || !reflect.DeepEqual(q.Windows, want[i]) {
			t.Errorf("message %d: %+v\nwant %+v", i, evs[0], want[i])
		}
	}
	// A limit reached with no window used up (credits depleted) limits them all, conservatively.
	evs := tr.Translate(Message{Method: "account/rateLimits/updated", Params: raw(`{"rateLimits":{"primary":{"usedPercent":30,"windowDurationMins":300},"secondary":{"usedPercent":10,"windowDurationMins":10080},"rateLimitReachedType":"workspace_member_credits_depleted"}}`)})
	if w := evs[0].(rt.QuotaUpdate).Windows; !w[0].Limited || !w[1].Limited {
		t.Fatalf("windows = %+v, want both limited", w)
	}
	// A window that gives no length is named by its position, and no reset is nil.
	evs = tr.Translate(Message{Method: "account/rateLimits/updated", Params: raw(`{"rateLimits":{"primary":{"usedPercent":150},"secondary":{"usedPercent":0,"windowDurationMins":60}}}`)})
	got := evs[0].(rt.QuotaUpdate).Windows
	if got[0].Name != "primary" || got[0].Used != 1 || got[0].ResetsAt != nil || got[1].Name != "60m" {
		t.Fatalf("windows = %+v", got)
	}
}

func TestTranslateUnknownItemIsUnknown(t *testing.T) {
	tr := NewTranslator()
	for _, c := range []struct {
		m    Message
		kind string
	}{
		{Message{Method: "item/completed", Params: raw(`{"item":{"id":"i","type":"holographicTool"}}`)}, "holographicTool"},
		{Message{Method: "item/started", Params: raw(`{"item":{"id":"i","type":"imageView","path":"/x"}}`)}, "imageView"},
		{Message{Method: "item/agentMessage/delta", Params: raw(`{}`)}, "item/agentMessage/delta"},
		{Message{Method: "thread/futureThing", Params: raw(`{}`)}, "thread/futureThing"},
		{Message{Method: "item/completed", Params: raw(`not json`)}, "item/completed"},
		{Message{Method: "error", Params: raw(`{"error":{"message":"transient"},"willRetry":true}`)}, "error"},
	} {
		got := tr.Translate(c.m)
		if !reflect.DeepEqual(got, []rt.Event{rt.Unknown{Kind: c.kind}}) {
			t.Errorf("%s: events = %+v, want Unknown{%s}", c.m.Method, got, c.kind)
		}
	}
}

func TestTranslateSteerAndApprovalTurns(t *testing.T) {
	// The steer fixture is a later turn of the same thread: no thread/started; the user echo carries the steer's client
	// id. Its tokens are the change of the totals since simple-turn's end (last is 5300/30/5120, the change 5290/30/5120).
	tr := NewTranslator()
	for _, m := range fixtureMessages(t, "simple-turn") {
		tr.Translate(m)
	}
	var got []rt.Event
	for _, m := range fixtureMessages(t, "steer-turn") {
		got = append(got, tr.Translate(m)...)
	}
	want := []rt.Event{
		rt.Unknown{Kind: "turn/started"},
		rt.Unknown{Kind: "userMessage"},
		rt.UserEcho{Text: "Also mention the weather.", UUID: "c0ffee00-0000-4000-8000-000000000003"},
		rt.Unknown{Kind: "agentMessage"},
		rt.AssistantText{Text: "Noted: it is sunny."},
		rt.TurnEnd{SessionID: "thr_fixture_0001", OK: true, Text: "Noted: it is sunny.", InputTokens: 170, OutputTokens: 30, CacheReadTokens: 5120, NumTurns: 1, Duration: 2100 * time.Millisecond},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("steer:\n%+v\nwant\n%+v", got, want)
	}

	// The approval fixture's tool item is one ToolUse and one ToolResult, whatever happened in between.
	var uses, results int
	for _, e := range translateAll(t, "approval-turn") {
		switch e.(type) {
		case rt.ToolUse:
			uses++
		case rt.ToolResult:
			results++
		}
	}
	if uses != 1 || results != 1 {
		t.Fatalf("approval turn: %d ToolUse, %d ToolResult", uses, results)
	}
}

func TestTranslateToolResultsAndTurnEndDetails(t *testing.T) {
	tr := NewTranslator()
	done := func(item string) []rt.Event {
		return tr.Translate(Message{Method: "item/completed", Params: raw(`{"item":` + item + `}`)})
	}
	// A completed item whose start was never seen still gets its ToolUse first.
	got := done(`{"id":"c1","type":"commandExecution","command":"ls","cwd":"/w","status":"declined","exitCode":null}`)
	if len(got) != 2 || got[0].(rt.ToolUse).ID != "c1" || !got[1].(rt.ToolResult).IsError {
		t.Fatalf("declined command: %+v", got)
	}
	// Exit status and status decide a command's error; a failed patch and an MCP call with an error are errors.
	for _, c := range []struct {
		item string
		bad  bool
	}{
		{`{"id":"a","type":"commandExecution","command":"x","cwd":"/w","status":"completed","exitCode":0}`, false},
		{`{"id":"b","type":"commandExecution","command":"x","cwd":"/w","status":"completed","exitCode":2}`, true},
		{`{"id":"c","type":"commandExecution","command":"x","cwd":"/w","status":"failed"}`, true},
		{`{"id":"d","type":"fileChange","status":"failed","changes":[]}`, true},
		{`{"id":"e","type":"fileChange","status":"declined","changes":[]}`, true},
		{`{"id":"f","type":"mcpToolCall","server":"s","tool":"t","status":"failed","arguments":{},"error":{"message":"boom"}}`, true},
		{`{"id":"g","type":"mcpToolCall","server":"s","tool":"t","status":"completed","arguments":{},"error":null}`, false},
	} {
		evs := done(c.item)
		if res := evs[len(evs)-1].(rt.ToolResult); res.IsError != c.bad {
			t.Errorf("%s: IsError = %v, want %v", c.item, res.IsError, c.bad)
		}
	}

	// An interrupted turn, and a failed one with an object-shaped error code; errors not retried reach the TurnEnd.
	tr.Translate(Message{Method: "error", Params: raw(`{"threadId":"t","turnId":"u","willRetry":false,"error":{"message":"stream died"}}`)})
	end := tr.Translate(Message{Method: "turn/completed", Params: raw(`{"threadId":"t","turn":{"id":"u","items":[],"status":"failed","error":{"message":"bad","codexErrorInfo":{"httpConnectionFailed":{"httpStatusCode":502}}}}}`)})[0].(rt.TurnEnd)
	if end.OK || end.Subtype != "httpConnectionFailed" || end.Text != "bad" || !reflect.DeepEqual(end.Errors, []string{"stream died"}) || end.Limit != nil {
		t.Fatalf("failed TurnEnd = %+v", end)
	}
	end = tr.Translate(Message{Method: "turn/completed", Params: raw(`{"threadId":"t","turn":{"id":"u","items":[],"status":"interrupted"}}`)})[0].(rt.TurnEnd)
	if end.OK || end.Subtype != "interrupted" || end.Errors != nil || end.InputTokens != 0 {
		t.Fatalf("interrupted TurnEnd = %+v (the previous turn's errors and tokens must not carry over)", end)
	}
	end = tr.Translate(Message{Method: "turn/completed", Params: raw(`{"threadId":"t","turn":{"id":"u","items":[],"status":"failed"}}`)})[0].(rt.TurnEnd)
	if end.OK || end.Subtype != "failed" {
		t.Fatalf("a failed turn with no error info: %+v", end)
	}
}

// A turn's tokens are the change of the thread's totals over it (Codex's `last` is only the latest model call), and
// input leaves out the cached part, as Claude's does.
func TestTranslateTokensAreTheTurnsDeltaWithoutCachedInput(t *testing.T) {
	tr := NewTranslator()
	usage := func(lastIn, lastOut, lastCached, totIn, totOut, totCached int) {
		b := func(i, o, c int) string {
			return fmt.Sprintf(`{"inputTokens":%d,"outputTokens":%d,"cachedInputTokens":%d,"reasoningOutputTokens":0,"totalTokens":%d}`, i, o, c, i+o)
		}
		tr.Translate(Message{Method: "thread/tokenUsage/updated", Params: raw(`{"threadId":"t","turnId":"u","tokenUsage":{"last":` + b(lastIn, lastOut, lastCached) + `,"total":` + b(totIn, totOut, totCached) + `}}`)})
	}
	start := func() {
		tr.Translate(Message{Method: "turn/started", Params: raw(`{"threadId":"t","turn":{"id":"u","items":[],"status":"inProgress"}}`)})
	}
	end := func() rt.TurnEnd {
		return tr.Translate(Message{Method: "turn/completed", Params: raw(`{"threadId":"t","turn":{"id":"u","items":[],"status":"completed"}}`)})[0].(rt.TurnEnd)
	}
	// The first turn of a thread: two model calls.
	start()
	usage(1000, 10, 400, 1000, 10, 400)
	usage(1500, 20, 900, 2500, 30, 1300)
	if e := end(); e.InputTokens != 1200 || e.OutputTokens != 30 || e.CacheReadTokens != 1300 {
		t.Fatalf("turn 1: %+v", e)
	}
	// The next: one call, whose own usage (last) is not the turn's change in the totals either.
	start()
	usage(1500, 20, 600, 4000, 50, 1900)
	usage(1700, 25, 800, 5700, 75, 2700)
	if e := end(); e.InputTokens != 1800 || e.OutputTokens != 45 || e.CacheReadTokens != 1400 {
		t.Fatalf("turn 2: %+v (want 3200 input - 1400 cached, 45 output)", e)
	}
	// A turn with no usage update has no tokens; cached above input does not make input negative.
	start()
	if e := end(); e.InputTokens != 0 || e.OutputTokens != 0 || e.CacheReadTokens != 0 {
		t.Fatalf("turn without usage: %+v", e)
	}
	start()
	usage(10, 1, 50, 5710, 76, 2750)
	if e := end(); e.InputTokens != 0 {
		t.Fatalf("input = %d, want 0 (cached 50 of 10)", e.InputTokens)
	}

	// R47 N2: an update outside a turn (a resumed thread reporting its totals, say) belongs to no turn: the next turn
	// counts only its own change, even on a fresh Translator whose first update came between turns.
	tr = NewTranslator()
	usage(900, 9, 0, 90000, 900, 40000)
	start()
	usage(1000, 10, 400, 91000, 910, 40400)
	if e := end(); e.InputTokens != 600 || e.OutputTokens != 10 || e.CacheReadTokens != 400 {
		t.Fatalf("turn after an update outside a turn: %+v (want 600/10/400)", e)
	}
	// Totals that go down (Codex reset them) count as nothing, never negative, in every column.
	usage(1, 1, 1, 100, 1, 1)
	start()
	usage(1, 1, 1, 50, 0, 0)
	if e := end(); e.InputTokens != 0 || e.OutputTokens != 0 || e.CacheReadTokens != 0 {
		t.Fatalf("totals that went down: %+v", e)
	}
}
