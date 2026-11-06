package policy

import (
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

func TestTrustClass(t *testing.T) {
	s := testSettings("auto")
	s.Sources[GitHubSource] = SourcePart{Allowed: []string{"Alice"}, Bots: []string{"dependabot[bot]"}}
	p := New(s, nil, time.Minute)
	gh := func(actor, assoc string) core.Trigger {
		tr := core.Trigger{Source: GitHubSource, Kind: core.KindMessage, Scope: "comments", Human: true,
			Actor: core.ActorRef{Source: GitHubSource, ID: actor}}
		if assoc != "" {
			tr.Meta = map[string]string{"association": assoc}
		}
		return tr
	}
	bot := gh("dependabot[bot]", "NONE")
	bot.Human, bot.Bot = false, true
	botCI := bot
	botCI.Kind = core.KindCI
	ci := gh("", "")
	ci.Human, ci.Scope = false, ScopeCI
	for name, tc := range map[string]struct {
		t    core.Trigger
		want string
	}{
		"owner":                     {core.Trigger{Source: core.OwnerActor.Source, Actor: core.OwnerActor, Human: true}, core.TrustOwner},
		"allowed, association NONE": {gh("alice", "NONE"), core.TrustAllowed},
		"allowed, no association":   {gh("ALICE", ""), core.TrustAllowed},
		"OWNER":                     {gh("bob", "OWNER"), core.TrustCollaborator},
		"MEMBER":                    {gh("bob", "MEMBER"), core.TrustCollaborator},
		"COLLABORATOR":              {gh("bob", "COLLABORATOR"), core.TrustCollaborator},
		"CONTRIBUTOR":               {gh("bob", "CONTRIBUTOR"), core.TrustStranger},
		"NONE":                      {gh("bob", "NONE"), core.TrustStranger},
		"FIRST_TIME_CONTRIBUTOR":    {gh("bob", "FIRST_TIME_CONTRIBUTOR"), core.TrustStranger},
		"missing":                   {gh("bob", ""), core.TrustStranger},
		"bot":                       {bot, core.TrustBot},
		"ci scope":                  {ci, core.TrustCI},
		"ci beats bot":              {botCI, core.TrustCI},
	} {
		if got := p.Trust(tc.t); got != tc.want {
			t.Errorf("%s: Trust = %q, want %q", name, got, tc.want)
		}
	}
}
