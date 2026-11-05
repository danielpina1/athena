package render

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// echo is the transcript line of a user message the CLI echoed back (--replay-user-messages).
func echo(text string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "user", "isReplay": true, "uuid": "u1",
		"message": map[string]any{"role": "user", "content": text}})
	return b
}

type said struct{ from, text, detail string }

func saidOf(items []*athenav1.StreamItem) []said {
	var out []said
	for _, it := range items {
		if it.Kind != "user" {
			out = append(out, said{"kind " + it.Kind, it.Text, ""})
			continue
		}
		out = append(out, said{it.Tool, it.Text, it.Detail})
	}
	return out
}

func eq(t *testing.T, got []said, want ...said) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d items %+v, want %+v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

const card = "[input 01HX: message from a person]\n" +
	"trigger_id: github:issuecomment:991\n" +
	"acknowledged: no; ack it before substantial work if it is a request you take up\n" +
	"from: octocat\n" +
	"at: 2026-11-05T09:46:21Z\n" +
	"thread: acme/api#7 (github:acme/api#7)\n" +
	"url: https://github.com/acme/api/issues/7#issuecomment-991\n" +
	"Please also cover the 429 path.\n\n"

const githubDetail = `{"from":"octocat","kind":"message","url":"https://github.com/acme/api/issues/7#issuecomment-991"}`

// The first message: the worker preamble goes, the request it started from is the Task.
func TestInputFirstMessageIsTheTask(t *testing.T) {
	first := "You are the worker of Athena Ticket T-0007: Add rate limiting\n\nExecution variables:\n" +
		"ATHENA_TICKET=T-0007\nATHENA_NAME=add-rate-limiting-t-0007\nATHENA_DIR=/w/add-rate-limiting-t-0007\n" +
		"ATHENA_BLOCKED_COMMANDS=\nATHENA_SOCKET=/s/node.sock\n\n" +
		"[input 5ad2: message from the owner (not on GitHub)]\nAdd rate limiting to the bookings API.\n\n- per IP\n- 429 over it\n\n"
	eq(t, saidOf(Items(echo(first), 0)), said{"task", "Add rate limiting to the bookings API.\n\n- per IP\n- 429 over it", ""})

	// Started from a GitHub issue: the Task carries who asked and where.
	issue := "You are the worker of Athena Ticket T-0008: Fix login\n\nExecution variables:\nATHENA_TICKET=T-0008\n\n" +
		strings.Replace(card, "message from a person", "issue from a person", 1)
	eq(t, saidOf(Items(echo(issue), 0)),
		said{"task", "Please also cover the 429 path.", strings.Replace(githubDetail, `"kind":"message"`, `"kind":"issue"`, 1)})

	// A preamble with no Input after it shows nothing.
	if got := Items(echo("You are the worker of Athena Ticket T-1: x\n\nExecution variables:\nATHENA_TICKET=T-1\n"), 0); len(got) != 0 {
		t.Fatalf("bare preamble = %+v", got)
	}
}

func TestInputOwnerGithubAndAthena(t *testing.T) {
	eq(t, saidOf(Items(echo("[input 01HY: message from the owner (not on GitHub)]\nGo ahead, keep it per IP\n\n"), 0)),
		said{"owner", "Go ahead, keep it per IP", ""})
	eq(t, saidOf(Items(echo(card), 0)), said{"github", "Please also cover the 429 path.", githubDetail})

	// Several Inputs in one message, a note and a reopening among them.
	many := "[input a1: the Ticket was resumed by the message that follows]\n\n" +
		"[input a2: message from the owner (not on GitHub)]\nOne more thing\n\n" +
		"[input a3: note]\nNote: prefer small pull requests\n\n" +
		"[input a4: message]\nAsked for a status update\n\n"
	eq(t, saidOf(Items(echo(many), 0)),
		said{"athena", "The Ticket was reopened", ""},
		said{"owner", "One more thing", ""},
		said{"athena", "Note: prefer small pull requests", ""},
		said{"athena", "Asked for a status update", ""})

	eq(t, saidOf(Items(echo("Your turn ended without a report. Call the report tool now with your status."), 0)),
		said{"athena", "Athena asked for a status report", ""})
	eq(t, saidOf(Items(echo("Your previous process ended unexpectedly; continue the task. Check your directory and git history before repeating any outside action.\n\n"+
		"[input b1: message from the owner (not on GitHub)]\nStill there?\n\n"), 0)),
		said{"athena", "Athena restarted the session after its process ended", ""},
		said{"owner", "Still there?", ""})
	eq(t, saidOf(Items(echo("Your session was paused while it waited; it resumes for the input that follows.\n\n"+card), 0)),
		said{"athena", "Athena resumed the paused session", ""},
		said{"github", "Please also cover the 429 path.", githubDetail})
}

// A card's protocol lines never show; a cut text loses its pointer line (the block links to the comment instead).
func TestInputCardDetails(t *testing.T) {
	long := "[input c1: review from a person]\ntrigger_id: github:review:5\nfrom: dependabot (bot)\nat: 2026-11-05T09:46:21Z\n" +
		"thread: acme/api#9 (github:acme/api#9)\nurl: https://github.com/acme/api/pull/9#pullrequestreview-5\nreplies_to: github:review:4\n" +
		"context: it reached you on a thread you follow for context; it is not a request to this ticket\nrouting note: see T-2\n" +
		"Looks good, but token=ghp_abcdefabcdefabcdef1234 leaked…\n[cut: 600 of 900 characters shown; read the rest at https://github.com/acme/api/pull/9 (gh can read it)]\n\n"
	got := Items(echo(long), 0)
	if len(got) != 1 || got[0].Tool != "github" {
		t.Fatalf("items = %+v", got)
	}
	if got[0].Text != "Looks good, but token=*** leaked…" {
		t.Errorf("text = %q", got[0].Text)
	}
	var d map[string]string
	if err := json.Unmarshal([]byte(got[0].Detail), &d); err != nil || d["from"] != "dependabot (bot)" || d["kind"] != "review" ||
		d["url"] != "https://github.com/acme/api/pull/9#pullrequestreview-5" {
		t.Errorf("detail = %s (%v)", got[0].Detail, err)
	}
	for _, leak := range []string{"trigger_id", "github:review:5", "routing note", "c1"} {
		if strings.Contains(got[0].Text+got[0].Detail, leak) {
			t.Errorf("protocol detail %q shows: %+v", leak, got[0])
		}
	}
}

// Old transcripts: a plain user message is the owner's, as before; an echo in text blocks reads the same.
func TestInputPlainAndBlocks(t *testing.T) {
	eq(t, saidOf(Items([]byte(`{"type":"user","message":{"role":"user","content":"Run the tests too"}}`), 0)), said{"owner", "Run the tests too", ""})
	blocks := `{"type":"user","isReplay":true,"uuid":"u2","message":{"role":"user","content":[{"type":"text","text":"[input d1: message from the owner (not on GitHub)]\nGo"}]}}`
	eq(t, saidOf(Items([]byte(blocks), 0)), said{"owner", "Go", ""})
	// A user line of text blocks that is no echo (the CLI's own nudges) stays a muted line.
	if got := Items([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"[Your previous response had no visible output.]"}]}}`), 0); len(got) != 1 || got[0].Kind != "other" {
		t.Fatalf("non-echo blocks = %+v", got)
	}
}

// `athena logs` shows an echoed message as who sent it and its first words, never its protocol.
func TestRenderEchoForTheCLI(t *testing.T) {
	r := New(time.Now())
	got := r.Render(echo("[input 01HY: message from the owner (not on GitHub)]\nGo ahead, keep it per IP\n\n"))
	if len(got) != 1 || got[0].Kind != "input" || got[0].Text != "owner: Go ahead, keep it per IP" {
		t.Fatalf("echo = %+v", got)
	}
	got = r.Render(echo(card))
	if len(got) != 1 || got[0].Text != "github: Please also cover the 429 path." {
		t.Fatalf("card = %+v", got)
	}
}
