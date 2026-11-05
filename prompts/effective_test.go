package prompts

import (
	"strings"
	"testing"
)

func TestPartsOrderAndDefaults(t *testing.T) {
	var names []string
	for _, p := range Parts() {
		names = append(names, p.Name)
		if strings.TrimSpace(p.Default) == "" || strings.Contains(p.Default, "<!--") {
			t.Errorf("part %s default is empty or keeps a comment", p.Name)
		}
	}
	if got := strings.Join(names, ","); got != "worker,router,admission,replies,subscriptions" {
		t.Fatalf("parts = %s", got)
	}
	w, r := Effective(nil)
	if w != Worker || r != Router {
		t.Error("Effective(nil) differs from the defaults' compositions")
	}
}

func TestEffectiveReplacesOnePart(t *testing.T) {
	w, r := Effective(map[string]string{PartAdmission: "ADMISSION OVERRIDE"})
	if w != Worker {
		t.Error("the worker prompt changed with a router part")
	}
	if !strings.Contains(r, "ADMISSION OVERRIDE") {
		t.Fatal("router prompt lacks the override")
	}
	for _, p := range Parts() {
		switch p.Name {
		case PartWorker, PartAdmission:
		default:
			if !strings.Contains(r, p.Default) {
				t.Errorf("router prompt lost the %s part", p.Name)
			}
		}
	}
	if strings.Contains(r, Parts()[2].Default) {
		t.Error("router prompt kept the overridden default")
	}
	w, r = Effective(map[string]string{PartWorker: "W", "nonsense": "x"})
	if w != "W" || r != Router {
		t.Error("worker override or unknown part mishandled")
	}
}

func TestHash(t *testing.T) {
	if got := Hash("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("Hash = %s", got)
	}
}
