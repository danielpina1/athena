package node

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/render"
	"github.com/danielpina1/athena/internal/runtime/claude"
)

// echoLine is the transcript line the CLI writes for a message it took in (--replay-user-messages).
func echoLine(text string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "user", "isReplay": true, "message": map[string]any{"role": "user", "content": text}})
	return b
}

// The session view parses what the node sends: every shape the node composes reads as who sent it, with its text
// and none of the protocol. This ties internal/render's parser to the real formats.
func TestEchoedInputsRenderAsTheirSenders(t *testing.T) {
	at := time.Date(2026, 11, 5, 9, 46, 21, 0, time.UTC)
	tr := core.Trigger{ID: "github:issuecomment:991", Source: "github", Actor: core.ActorRef{Source: "github", ID: "octocat"}, Human: true,
		At: at, Text: "Please also cover the 429 path.", Kind: core.KindMessage, Threads: []string{"github:acme/api#7"},
		Meta: map[string]string{"repo": "acme/api", "number": "7", "url": "https://github.com/acme/api/issues/7#issuecomment-991"}}
	tk := core.Ticket{ID: "T-0007", Title: "Add rate limiting", Inputs: []core.Input{
		{ID: "i1", Kind: core.InputTrigger, Human: true, Text: "Add rate limiting to the bookings API."},
		{ID: "i2", Kind: core.InputTrigger, Human: true, TriggerID: tr.ID},
		{ID: "i3", Kind: core.InputNote, Text: "prefer small pull requests"},
		{ID: "i4", Kind: core.InputResumed},
		{ID: "i5", Kind: core.InputText, Text: "Asked for a status update"},
	}}
	trigger := func(id string) (core.Trigger, bool) { return tr, id == tr.ID }
	type said struct{ from, text string }
	read := func(msg string) []said {
		var out []said
		for _, it := range render.Items(echoLine(msg), 0) {
			if it.Kind != "user" {
				t.Fatalf("kind %q for %q", it.Kind, msg)
			}
			for _, leak := range []string{"[input", "trigger_id", tr.ID, "ATHENA_"} {
				if strings.Contains(it.Text+it.Detail, leak) {
					t.Errorf("%q shows in %+v", leak, it)
				}
			}
			out = append(out, said{it.Tool, it.Text})
		}
		return out
	}
	first := workerPreamble(tk, []string{"ATHENA_TICKET=T-0007", "ATHENA_DIR=/w/x"}) + "\n" + renderInputs(tk, []string{"i1"}, trigger)
	if got := read(first); !slices.Equal(got, []said{{"task", "Add rate limiting to the bookings API."}}) {
		t.Errorf("first message = %+v", got)
	}
	later := renderInputs(tk, []string{"i2", "i3", "i4", "i5"}, trigger)
	want := []said{{"github", "Please also cover the 429 path."}, {"athena", "Note: prefer small pull requests"},
		{"athena", "The Ticket was reopened"}, {"athena", "Asked for a status update"}}
	if got := read(later); !slices.Equal(got, want) {
		t.Errorf("later message = %+v, want %+v", got, want)
	}
	tk.Inputs[1] = core.Input{ID: "i2", Kind: core.InputTrigger, Human: true, Text: "Go ahead"}
	if got := read(renderInputs(tk, []string{"i2"}, trigger)); !slices.Equal(got, []said{{"owner", "Go ahead"}}) {
		t.Errorf("owner message = %+v", got)
	}
	for msg, want := range map[string]string{
		nudgeText:                            "Athena asked for a status report",
		recoveryNote + recoveryAdvice + "\n": "Athena restarted the session after its process ended",
		resumeNote + "\n":                    "Athena resumed the paused session",
	} {
		if got := read(msg); !slices.Equal(got, []said{{"athena", want}}) {
			t.Errorf("%q = %+v", msg, got)
		}
	}
}

// Worker sessions run with --replay-user-messages; an echo is not output, not a step and not a turn: a turn that
// ends without a Report after one is still nudged.
func TestWorkerEchoesAreNotOutput(t *testing.T) {
	h := newHarness(t, nil)
	t.Setenv("ATHENA_FAKE_FIXTURE", openTurnFixture(t))
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	h.eventually("claude started", func() bool { return len(h.invocations()) == 1 })
	if args := h.invocations()[0].Args; !slices.Contains(args, "--replay-user-messages") {
		t.Fatalf("worker args lack --replay-user-messages: %v", args)
	}
	s := h.n.lookup(tk.ID, a.ID)
	var sent []string
	r := &runner{n: h.n, s: s, eng: h.eng, log: s.log, sent: map[string]bool{}, started: true, initSeen: true,
		send: func(text string) error { sent = append(sent, text); return nil }}
	r.firstSent = time.Now()
	r.inputSent()
	r.onEvent(claude.UserEcho{Text: "[input x: message from the owner (not on GitHub)]\nGo", UUID: "u"})
	if r.outputSeen || r.usage.Turns != 0 || r.idle {
		t.Fatalf("an echo counted: outputSeen %v turns %d idle %v", r.outputSeen, r.usage.Turns, r.idle)
	}
	r.onEvent(claude.Result{Subtype: "success"})
	if len(sent) != 1 || sent[0] != nudgeText {
		t.Fatalf("sent %q, want one nudge", sent)
	}
}
