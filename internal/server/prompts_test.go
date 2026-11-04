package server

import (
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/web"
	"github.com/danielpina1/athena/prompts"
)

func setPromptCmd(id, part string, base uint32, text string) *athenav1.Command {
	return &athenav1.Command{Id: id, Action: &athenav1.Command_SetPrompt{SetPrompt: &athenav1.SetPrompt{Part: part, BaseRevision: base, Text: text}}}
}

func resetPromptCmd(id, part string, base uint32) *athenav1.Command {
	return &athenav1.Command{Id: id, Action: &athenav1.Command_ResetPrompt{ResetPrompt: &athenav1.ResetPrompt{Part: part, BaseRevision: base}}}
}

func getPromptCmd(id, part string) *athenav1.Command {
	return &athenav1.Command{Id: id, Action: &athenav1.Command_GetPrompt{GetPrompt: &athenav1.GetPrompt{Part: part}}}
}

func partView(v *athenav1.PromptsView, name string) *athenav1.PromptPartView {
	for _, p := range v.GetParts() {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func TestDefaultUpdatedWhenDefaultHashDiffers(t *testing.T) {
	w := startWeb(t, newStateDir(t))
	mine := "my admission rules"
	if _, err := w.d.st.SavePrompt(prompts.PartAdmission, &mine, 0, "an older default", "web"); err != nil {
		t.Fatal(err)
	}
	v, err := promptsView(w.d.st)
	if err != nil {
		t.Fatal(err)
	}
	if p := partView(v, prompts.PartAdmission); !p.Overridden || !p.DefaultUpdated {
		t.Fatalf("admission = %v, want overridden with an updated default", p)
	}
	if p := partView(v, prompts.PartWorker); p.Overridden || p.DefaultUpdated {
		t.Fatalf("worker = %v, want untouched", p)
	}
	if len(v.Parts) != 5 || v.Revision != 1 {
		t.Fatalf("view = %v", v)
	}
	d, err := w.d.getPrompt(prompts.PartAdmission)
	if err != nil || d.Text != mine || d.PreviousDefaultText != "an older default" || d.DefaultText == "" {
		t.Fatalf("detail = %v, %v", d, err)
	}
}

func TestKeepMyTextClearsDefaultUpdated(t *testing.T) {
	w := startWeb(t, newStateDir(t))
	mine := "my replies"
	if _, err := w.d.st.SavePrompt(prompts.PartReplies, &mine, 0, "an older default", "web"); err != nil {
		t.Fatal(err)
	}
	b := w.login()
	c := w.connect(b.session(), phoneHello())
	c.welcome()
	if r := c.do(setPromptCmd("keep", prompts.PartReplies, 1, mine)); !r.GetOk() {
		t.Fatalf("keep my text = %v", r)
	}
	v, _ := promptsView(w.d.st)
	if p := partView(v, prompts.PartReplies); !p.Overridden || p.DefaultUpdated || v.Revision != 2 {
		t.Fatalf("after keep: %v at revision %d", p, v.Revision)
	}
	// "Use the new default" is a reset.
	other := "other"
	if _, err := w.d.st.SavePrompt(prompts.PartRouter, &other, 2, "old router default", "web"); err != nil {
		t.Fatal(err)
	}
	if r := c.do(resetPromptCmd("reset", prompts.PartRouter, 3)); !r.GetOk() {
		t.Fatalf("reset = %v", r)
	}
	v, _ = promptsView(w.d.st)
	if p := partView(v, prompts.PartRouter); p.Overridden || p.DefaultUpdated {
		t.Fatalf("router after reset = %v", p)
	}
}

func TestSetPromptIsSensitive(t *testing.T) {
	w := startWeb(t, newStateDir(t))
	b := w.login()
	w.skew.Store(int64(web.ReauthWindow + time.Minute)) // the login's assertion is too old now
	c := w.connect(b.session(), phoneHello())
	c.welcome()
	for _, cmd := range []*athenav1.Command{setPromptCmd("s1", "worker", 0, "x"), resetPromptCmd("s2", "worker", 0)} {
		if r := c.do(cmd); r.GetCode() != web.CodeReauthRequired {
			t.Fatalf("%s without reauth = %v", cmd.Id, r)
		}
	}
	if r := c.do(getPromptCmd("g1", "worker")); !r.GetOk() {
		t.Fatalf("get prompt needs no reauth: %v", r)
	}
	if _, rev, _ := w.d.st.PromptOverrides(); rev != 0 {
		t.Fatalf("revision %d after refused saves", rev)
	}
	b.reauth()
	if r := c.do(setPromptCmd("s1", "worker", 0, "x")); !r.GetOk() {
		t.Fatalf("set after reauth = %v", r)
	}
}

func TestSetPromptRefusals(t *testing.T) {
	w := startWeb(t, newStateDir(t))
	c := w.connect(w.login().session(), phoneHello())
	c.welcome()
	for _, tc := range []struct {
		cmd  *athenav1.Command
		code string
	}{
		{setPromptCmd("e1", "worker", 0, ""), web.CodeInvalid},
		{setPromptCmd("e2", "worker", 0, "   \n"), web.CodeInvalid},
		{setPromptCmd("e3", "worker", 0, strings.Repeat("x", 64<<10+1)), web.CodeInvalid},
		{setPromptCmd("e4", "nonsense", 0, "x"), web.CodeNotFound},
		{resetPromptCmd("e5", "nonsense", 0), web.CodeNotFound},
		{getPromptCmd("e6", "nonsense"), web.CodeNotFound},
		{setPromptCmd("e7", "worker", 5, "x"), web.CodeConflict},
	} {
		if r := c.do(tc.cmd); r.GetCode() != tc.code {
			t.Errorf("%s = %v, want %s", tc.cmd.Id, r, tc.code)
		}
	}
	if r := c.do(setPromptCmd("ok", "worker", 0, strings.Repeat("x", 64<<10))); !r.GetOk() {
		t.Errorf("a 64 KiB text = %v", r)
	}
}

func TestGetPromptReturnsFullAndHistory(t *testing.T) {
	w := startWeb(t, newStateDir(t))
	c := w.connect(w.login().session(), phoneHello())
	c.welcome()
	for i := uint32(0); i < 22; i++ {
		if r := c.do(setPromptCmd("w"+string(rune('a'+i)), "worker", i, "worker text "+string(rune('a'+i)))); !r.GetOk() {
			t.Fatalf("save %d = %v", i, r)
		}
	}
	if r := c.do(setPromptCmd("rt", "router", 22, "ROUTER OVERRIDE")); !r.GetOk() {
		t.Fatal(r)
	}
	if r := c.do(resetPromptCmd("rw", "worker", 23)); !r.GetOk() {
		t.Fatal(r)
	}
	r := c.do(getPromptCmd("g", "worker"))
	d := r.GetPrompt()
	if !r.GetOk() || d == nil || d.Name != "worker" || d.Text != "" || d.DefaultText != prompts.Worker {
		t.Fatalf("detail = %v", r)
	}
	if d.FullWorker != prompts.Worker || !strings.Contains(d.FullRouter, "ROUTER OVERRIDE") {
		t.Error("the full prompts do not apply the overrides")
	}
	if len(d.History) != 20 || d.History[0].Revision != 24 || !d.History[0].Reset_ || d.History[1].Reset_ || d.History[1].Origin != "web" ||
		d.History[0].At == nil {
		t.Fatalf("history = %v", d.History)
	}
	r = c.do(getPromptCmd("g2", "router"))
	if d := r.GetPrompt(); d.Text != "ROUTER OVERRIDE" || len(d.History) != 1 || d.PreviousDefaultText != "" {
		t.Fatalf("router detail = %v", r)
	}
}

func TestPromptsReachTheHubOnSave(t *testing.T) {
	w := startWeb(t, newStateDir(t))
	snap, err := w.hub.Snapshot()
	if err != nil || snap.Prompts.GetRevision() != 0 || len(snap.Prompts.Parts) != 5 {
		t.Fatalf("snapshot prompts = %v, %v", snap.Prompts, err)
	}
	c := w.connect(w.login().session(), phoneHello())
	c.welcome()
	if r := c.do(setPromptCmd("s", "worker", 0, "mine")); !r.GetOk() {
		t.Fatal(r)
	}
	f := c.next("prompts delta", func(f *athenav1.Frame) bool { return f.GetDelta().GetPrompts() != nil })
	if p := f.GetDelta().GetPrompts(); p.Revision != 1 || !partView(p, "worker").Overridden {
		t.Fatalf("delta = %v", p)
	}
}
