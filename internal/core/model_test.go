package core

import (
	"encoding/json"
	"testing"
	"time"
)

// Plan 1 documents must keep their exact encoding: every field plan 2 added is omitted while empty.
func TestPlan1DocumentsEncodeUnchanged(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for name, c := range map[string]struct {
		v    any
		want string
	}{
		"decision": {Decision{ID: "d", Kind: "accept", At: at}, `{"id":"d","kind":"accept","at":"2026-01-02T03:04:05Z"}`},
		"trigger":  {Trigger{ID: "t", Source: "s", At: at}, `{"id":"t","source":"s","actor":{"source":"","id":""},"at":"2026-01-02T03:04:05Z"}`},
		"mandate":  {Mandate{Strength: "explicit", PolicyVersion: "v", Actor: ActorRef{Source: "s", ID: "a"}}, `{"strength":"explicit","policyVersion":"v","actor":{"source":"s","id":"a"}}`},
	} {
		raw, err := json.Marshal(c.v)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != c.want {
			t.Errorf("%s encodes as %s, want %s", name, raw, c.want)
		}
	}
	raw, err := json.Marshal(Ticket{ID: "T"})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"follows", "repo"} {
		if _, ok := keys[k]; ok {
			t.Errorf("an empty Ticket encodes %q: %s", k, raw)
		}
	}
}
