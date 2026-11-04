package server

import (
	"google.golang.org/protobuf/types/known/timestamppb"
	"log/slog"

	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/prompts"
)

const (
	// maxPromptBytes bounds one prompt part's text.
	maxPromptBytes = 64 << 10
	// promptHistory is how many revisions of a part GetPrompt returns.
	promptHistory = 20
	// promptOriginWeb marks a revision saved from the dashboard.
	promptOriginWeb = "web"

	stalePrompts = "the prompts changed meanwhile; reload them and try again"
)

// overrideTexts returns the owner's current override text by part name and the latest prompt revision.
func overrideTexts(st *store.Store) (map[string]string, uint32, error) {
	over, rev, err := st.PromptOverrides()
	if err != nil {
		return nil, 0, err
	}
	texts := make(map[string]string, len(over))
	for name, o := range over {
		texts[name] = o.Text
	}
	return texts, rev, nil
}

// effectivePrompts returns the worker and router prompts a new session would start with.
func effectivePrompts(st *store.Store) (worker, router string, err error) {
	texts, _, err := overrideTexts(st)
	if err != nil {
		return "", "", err
	}
	worker, router = prompts.Effective(texts)
	return worker, router, nil
}

// promptsView says which parts are overridden and whether a default changed under an override.
func promptsView(st *store.Store) (*athenav1.PromptsView, error) {
	over, rev, err := st.PromptOverrides()
	if err != nil {
		return nil, err
	}
	v := &athenav1.PromptsView{Revision: rev}
	for _, p := range prompts.Parts() {
		pv := &athenav1.PromptPartView{Name: p.Name}
		if o, ok := over[p.Name]; ok {
			pv.Overridden = true
			pv.DefaultUpdated = o.DefaultHash != prompts.Hash(p.Default)
		}
		v.Parts = append(v.Parts, pv)
	}
	return v, nil
}

func promptPart(name string) (prompts.Part, error) {
	for _, p := range prompts.Parts() {
		if p.Name == name {
			return p, nil
		}
	}
	return prompts.Part{}, userErrorf("prompt part %q not found", name)
}

// getPrompt describes one part: its default, the owner's text, the default it was saved against when that has changed,
// the full prompts a new session would start with and the part's last revisions.
func (d *Dispatcher) getPrompt(name string) (*athenav1.PromptDetail, error) {
	part, err := promptPart(name)
	if err != nil {
		return nil, err
	}
	over, _, err := d.st.PromptOverrides()
	if err != nil {
		return nil, err
	}
	texts := make(map[string]string, len(over))
	for n, o := range over {
		texts[n] = o.Text
	}
	detail := &athenav1.PromptDetail{Name: part.Name, DefaultText: part.Default}
	detail.FullWorker, detail.FullRouter = prompts.Effective(texts)
	if o, ok := over[name]; ok {
		detail.Text = o.Text
		if o.DefaultHash != prompts.Hash(part.Default) {
			detail.PreviousDefaultText = o.DefaultText
		}
	}
	hist, err := d.st.PromptHistory(name, promptHistory)
	if err != nil {
		return nil, err
	}
	for _, r := range hist {
		detail.History = append(detail.History, &athenav1.PromptRevision{
			Revision: r.Revision, At: timestamppb.New(r.At), Origin: r.Origin, Reset_: r.Text == nil})
	}
	return detail, nil
}

// setPrompt overrides a part with text; a nil text resets it to the default. base is the revision the owner edited from.
func (d *Dispatcher) setPrompt(name string, base uint32, text *string) error {
	part, err := promptPart(name)
	if err != nil {
		return err
	}
	if text != nil {
		switch {
		case prompts.Blank(*text):
			return userErrorf("a prompt override needs text beyond comment lines; reset the part to use the default")
		case len(*text) > maxPromptBytes:
			return userErrorf("a prompt part is at most 64 KiB")
		}
	}
	_, err = d.st.SavePrompt(name, text, base, part.Default, promptOriginWeb)
	return err
}

// currentPrompt is the worker (or router) prompt a new session starts with. A store that cannot be read gives the
// defaults, so a session never fails to start over an override.
func currentPrompt(st *store.Store, log *slog.Logger, worker bool) string {
	w, r, err := effectivePrompts(st)
	if err != nil {
		log.Error("read prompt overrides; using the defaults", "error", err)
		w, r = prompts.Worker, prompts.Router
	}
	if worker {
		return w
	}
	return r
}
