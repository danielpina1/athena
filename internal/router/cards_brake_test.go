package router

import (
	"testing"

	"github.com/danielpina1/athena/internal/core"
)

func TestReasonTextSaysTheBrake(t *testing.T) {
	got := reasonText([]core.Reason{{Code: core.CodeWIPBrake,
		Values: map[string]string{"repo": "acme/web", "owed": "3", "ceiling": "3"}}})
	if want := "Held: 3 things already wait on you in acme/web (limit 3)."; got != want {
		t.Fatalf("reasonText = %q, want %q", got, want)
	}
}

// One thing waiting is said in the singular.
func TestReasonTextSaysOneThing(t *testing.T) {
	got := reasonText([]core.Reason{{Code: core.CodeWIPBrake,
		Values: map[string]string{"repo": "acme/web", "owed": "1", "ceiling": "1"}}})
	if want := "Held: 1 thing already waits on you in acme/web (limit 1)."; got != want {
		t.Fatalf("reasonText = %q, want %q", got, want)
	}
}
