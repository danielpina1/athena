package policy

import "github.com/danielpina1/athena/internal/core"

// metaAssociation is the Meta key a Source sets to the sender's relationship to the repository, as the host names it.
const metaAssociation = "association"

// Trust is the trust class of the Trigger's sender. Precedence: owner, ci, bot, allowed, collaborator, stranger.
// A sender on the allowed list is allowed whatever the host says of them; otherwise a host-declared owner, member or
// collaborator is a collaborator, and everyone else, including a sender the host did not classify, is a stranger.
func (p *policy) Trust(t core.Trigger) string {
	switch {
	case t.Source == core.OwnerActor.Source:
		return core.TrustOwner
	case t.Source == ScheduleSource:
		return core.TrustOwner
	case t.Kind == core.KindCI || t.Scope == ScopeCI:
		return core.TrustCI
	case t.Bot:
		return core.TrustBot
	case inList(p.s.Sources[t.Source].Allowed, t.Actor.ID):
		return core.TrustAllowed
	}
	switch t.Meta[metaAssociation] {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return core.TrustCollaborator
	}
	return core.TrustStranger
}
