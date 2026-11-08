package schedule

import (
	"encoding/json"
	"time"
)

// stateKey is the KV key of an entry's state document.
func stateKey(slug string) string { return "schedule.state." + slug }

// state is what the Source remembers of one entry, so a restart neither refires nor forgets.
type state struct {
	// Spec is the Cron or Event the document was made for; an edit starts it over.
	Spec string `json:"spec"`
	// Cursor is the latest clock firing handled (fired or dropped): the next walk starts after it.
	Cursor time.Time `json:"cursor"`
	// Base is the head an event entry last acted on.
	Base string `json:"base,omitempty"`
	// RedSince is when an event entry first saw its head red.
	RedSince time.Time `json:"redSince,omitzero"`
	// Episode names the red episode already fired for.
	Episode string `json:"episode,omitempty"`
	// LastFired is a clock firing's due time, or an event firing's time.
	LastFired time.Time `json:"lastFired,omitzero"`
	// Date and Fires count the firings of the local day Date.
	Date  string `json:"date,omitempty"`
	Fires int    `json:"fires,omitempty"`
}

func (s *Source) load(slug string) (*state, error) {
	raw, ok, err := s.kv.KVGet(stateKey(slug))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	var st state
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		s.log.Warn("schedule state unreadable; starting over", "slug", slug, "error", err)
		return nil, nil
	}
	return &st, nil
}

func (s *Source) save(slug string, st *state) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.kv.KVSet(stateKey(slug), string(raw))
}
