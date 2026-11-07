// Package policy holds Athena's Policy: the gate that admits Triggers, the admission table, revalidation of
// Mandates, the outcome of an assessment, the Policy version, the effects of a settings change and the revisioned
// settings the Policy is built from. It is pure apart from the settings Store, and imports core, never the reverse.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/store"
)

// Accept modes.
const (
	ModeOff     = "off"
	ModeObserve = "observe"
	ModeAuto    = "auto"
)

// Admission cell choices.
const (
	ChoiceExplicit = "explicit"
	ChoiceInferred = "inferred"
	ChoiceIgnore   = "ignore"
)

// The admission table's cells. They name Row's fields and are what MandateBasis.Row records.
const (
	CellListedUnaddressed   = "listedUnaddressed"
	CellUnlistedAddressed   = "unlistedAddressed"
	CellUnlistedUnaddressed = "unlistedUnaddressed"
)

// The admission table's rows (receiving scopes) of a GitHub Source.
const (
	ScopeIssues   = "issues"
	ScopePulls    = "pulls"
	ScopeComments = "comments"
	ScopeCI       = "ci"
)

// scopeRows lists the rows in a stable order.
var scopeRows = []string{ScopeIssues, ScopePulls, ScopeComments, ScopeCI}

// GitHubSource is the name of the GitHub Source in Settings.Sources.
const GitHubSource = "github"

// Origins of a settings revision.
const (
	OriginCLI    = "cli"
	OriginRouter = "router"
	OriginSync   = "sync"
	// OriginWeb is the owner's dashboard.
	OriginWeb = "web"
)

// settingsBlock is the name of the Policy's block in the settings table.
const settingsBlock = "policy"

// Rule describes a kind of proactive work. Its Instructions reach the Executor of a Ticket it matched.
type Rule struct {
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
	Enabled      bool   `json:"enabled"`
}

// Row is one row of a Source's admission table. An empty cell means its default.
type Row struct {
	// ListedUnaddressed: on the list, not addressing the agent (inferred or ignore; default inferred).
	ListedUnaddressed string `json:"listedUnaddressed,omitempty"`
	// UnlistedAddressed: not on the list, addressing the agent (explicit, inferred or ignore; default ignore).
	UnlistedAddressed string `json:"unlistedAddressed,omitempty"`
	// UnlistedUnaddressed: not on the list, not addressing the agent (inferred or ignore; default ignore).
	UnlistedUnaddressed string `json:"unlistedUnaddressed,omitempty"`
}

// SourcePart is one Source's share of the Policy: who may start work, which bots may produce candidates and the
// admission table, whose rows are issues, pulls, comments and ci.
type SourcePart struct {
	Allowed   []string       `json:"allowed,omitempty"`
	Bots      []string       `json:"bots,omitempty"`
	Admission map[string]Row `json:"admission,omitempty"`
}

// Settings are what the Policy is built from: the accept mode, the rules and each Source's part; and the pairing of
// the Tickets that name none, which is no part of the Policy's version.
type Settings struct {
	Mode    string                `json:"mode"`
	Rules   []Rule                `json:"rules"`
	Sources map[string]SourcePart `json:"sources"`
	Pairing Pairing               `json:"pairing"`
	// Repos holds per-repository mode and WIP ceiling, keyed by lower-cased owner/name.
	Repos map[string]RepoSettings `json:"repos,omitempty"`
	// WIPCeiling is the ceiling of repositories without their own; 0 is DefaultWIPCeiling.
	WIPCeiling int `json:"wipCeiling,omitempty"`
	// Trust maps a trust class to its tool policy (standard or restricted); missing classes use DefaultTrust.
	Trust map[string]string `json:"trust,omitempty"`
	// Allow lists the owner's allow rules, sorted by repository, tool and rule.
	Allow []AllowRule `json:"allow,omitempty"`
	// PromoteAfter is how many approvals promote a rule; 0 is DefaultPromoteAfter.
	PromoteAfter int `json:"promoteAfter,omitempty"`
}

// Repository modes.
const (
	RepoLive   = "live"
	RepoShadow = "shadow"
)

// Tool policies.
const (
	ToolStandard   = core.ToolStandard
	ToolRestricted = core.ToolRestricted
)

// Defaults of the autonomy brakes.
const (
	DefaultWIPCeiling   = 3
	DefaultPromoteAfter = 5
)

// RepoSettings are one repository's mode (live or shadow; empty is live) and WIP ceiling (0 is the default).
type RepoSettings struct {
	Mode       string `json:"mode,omitempty"`
	WIPCeiling int    `json:"wipCeiling,omitempty"`
}

// AllowRule lets one tool through one rule's check in one repository without asking.
type AllowRule struct {
	Tool  string    `json:"tool"`
	Rule  string    `json:"rule"`
	Repo  string    `json:"repo"`
	Added time.Time `json:"added"`
}

// NeverPromote lists the rules no allow rule may name: the owner always decides these.
var NeverPromote = []string{"credential-read", "credential-env", "blocked-command", "session-config-write", "protected_path", "attribution",
	// What these escalate leaves out what the worker chose (a host, a path, any command): a rule could not say what it allows.
	"network_access", "codex_permissions", "codex_unsandboxed",
	// Their target is the worker's choice too: any path, or a command the rules cannot read.
	"write-outside-ticket", "input"}

// trustClasses are the trust classes a Trigger's sender falls into.
var trustClasses = []string{"owner", "allowed", "collaborator", "stranger", "bot", "ci"}

// DefaultTrust is the tool policy of each trust class when the owner set none: only strangers are restricted.
func DefaultTrust() map[string]string {
	m := make(map[string]string, len(trustClasses))
	for _, c := range trustClasses {
		m[c] = ToolStandard
	}
	m["stranger"] = ToolRestricted
	return m
}

func neverPromoted(rule string) bool {
	for _, r := range NeverPromote {
		if r == rule {
			return true
		}
	}
	return false
}

// Values of Pairing.Review.
const (
	PairingOn  = "on"
	PairingOff = "off"
)

// maxReviewRounds bounds Pairing.MaxRounds.
const maxReviewRounds = 10

// Pairing is the pairing default (spec §6.1a): Review on pairs every Ticket that names no pairing with a review of
// its ready reports, in at most MaxRounds rounds (1 to 10). Empty fields are their defaults: off, 2 rounds.
type Pairing struct {
	Review    string `json:"review"`
	MaxRounds int    `json:"maxRounds"`
}

// defaultPairing is the pairing of an installation nobody configured.
var defaultPairing = Pairing{Review: PairingOff, MaxRounds: 2}

// PairingDefaults are the Engine's pairing defaults these settings give (core.Options.Pairing).
func (s Settings) PairingDefaults() core.PairingDefaults {
	p := normalize(s).Pairing
	d := core.PairingDefaults{MaxRounds: p.MaxRounds}
	if p.Review == PairingOn {
		d.Mode = core.PairingReview
	}
	return d
}

// defaultRow is the reference's default admission row.
var defaultRow = Row{ListedUnaddressed: ChoiceInferred, UnlistedAddressed: ChoiceIgnore, UnlistedUnaddressed: ChoiceIgnore}

// Defaults returns the settings of an installation nobody configured: mode observe, no rules, and the GitHub part
// allowing the given logins with the reference's default admission table.
func Defaults(allowed []string) Settings {
	return normalize(Settings{
		Mode:    ModeObserve,
		Sources: map[string]SourcePart{GitHubSource: {Allowed: allowed}},
	})
}

// normalize returns a deep copy of s with trimmed names and logins, an admission row for every known scope and
// every empty cell set to its default.
func normalize(s Settings) Settings {
	out := Settings{Mode: s.Mode, Rules: make([]Rule, len(s.Rules)), Sources: make(map[string]SourcePart, len(s.Sources)),
		Pairing: s.Pairing, WIPCeiling: s.WIPCeiling, PromoteAfter: s.PromoteAfter}
	if len(s.Repos) > 0 {
		out.Repos = make(map[string]RepoSettings, len(s.Repos))
		for k, v := range s.Repos {
			out.Repos[strings.ToLower(strings.TrimSpace(k))] = v
		}
	}
	if len(s.Trust) > 0 {
		out.Trust = make(map[string]string, len(s.Trust))
		for k, v := range s.Trust {
			out.Trust[k] = v
		}
	}
	if len(s.Allow) > 0 {
		out.Allow = make([]AllowRule, len(s.Allow))
		for i, a := range s.Allow {
			a.Repo = strings.ToLower(strings.TrimSpace(a.Repo))
			out.Allow[i] = a
		}
		sort.SliceStable(out.Allow, func(i, j int) bool {
			a, b := out.Allow[i], out.Allow[j]
			if a.Repo != b.Repo {
				return a.Repo < b.Repo
			}
			if a.Tool != b.Tool {
				return a.Tool < b.Tool
			}
			return a.Rule < b.Rule
		})
	}
	if out.Pairing.Review == "" {
		out.Pairing.Review = defaultPairing.Review
	}
	if out.Pairing.MaxRounds == 0 {
		out.Pairing.MaxRounds = defaultPairing.MaxRounds
	}
	for i, r := range s.Rules {
		r.Name = strings.TrimSpace(r.Name)
		out.Rules[i] = r
	}
	for name, p := range s.Sources {
		np := SourcePart{Allowed: trimmed(p.Allowed), Bots: trimmed(p.Bots), Admission: make(map[string]Row, len(p.Admission)+len(scopeRows))}
		for scope, row := range p.Admission {
			np.Admission[scope] = row
		}
		for _, scope := range scopeRows {
			row := np.Admission[scope]
			// A CI result has no sender, so its row has only the last cell.
			if scope != ScopeCI && row.ListedUnaddressed == "" {
				row.ListedUnaddressed = defaultRow.ListedUnaddressed
			}
			if scope != ScopeCI && row.UnlistedAddressed == "" {
				row.UnlistedAddressed = defaultRow.UnlistedAddressed
			}
			if row.UnlistedUnaddressed == "" {
				row.UnlistedUnaddressed = defaultRow.UnlistedUnaddressed
			}
			np.Admission[scope] = row
		}
		out.Sources[name] = np
	}
	return out
}

func trimmed(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strings.TrimSpace(v)
	}
	return out
}

// Validate reports why s cannot be saved, or nil: the mode is off, observe or auto; rule names are non-empty and
// unique; logins are non-empty; admission rows are known and each cell holds one of its allowed choices (a ci row
// has only the last cell); the pairing's review is on or off and its rounds 1 to 10 (empty and 0 are the defaults).
func Validate(s Settings) error {
	switch s.Mode {
	case ModeOff, ModeObserve, ModeAuto:
	default:
		return fmt.Errorf("policy: mode %q is not off, observe or auto", s.Mode)
	}
	switch s.Pairing.Review {
	case "", PairingOn, PairingOff:
	default:
		return fmt.Errorf("policy: pairing review %q is not on or off", s.Pairing.Review)
	}
	if r := s.Pairing.MaxRounds; r < 0 || r > maxReviewRounds {
		return fmt.Errorf("policy: pairing maxRounds %d is not 1 to %d", r, maxReviewRounds)
	}
	if err := validateAutonomy(s); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, r := range s.Rules {
		name := strings.TrimSpace(r.Name)
		if name == "" {
			return errors.New("policy: a rule has no name")
		}
		if seen[name] {
			return fmt.Errorf("policy: rule %q appears twice", name)
		}
		seen[name] = true
	}
	for name, p := range s.Sources {
		if strings.TrimSpace(name) == "" {
			return errors.New("policy: a Source part has no name")
		}
		for _, l := range p.Allowed {
			if strings.TrimSpace(l) == "" {
				return fmt.Errorf("policy: Source %q lists an empty login", name)
			}
		}
		for _, l := range p.Bots {
			if strings.TrimSpace(l) == "" {
				return fmt.Errorf("policy: Source %q lists an empty bot", name)
			}
		}
		for scope, row := range p.Admission {
			if !knownScope(scope) {
				return fmt.Errorf("policy: Source %q has no admission row %q", name, scope)
			}
			if err := validateRow(name, scope, row); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateAutonomy checks the repositories (no two keys that name one repository once trimmed and lower-cased), ceilings,
// trust map, allow rules and promotion count.
func validateAutonomy(s Settings) error {
	if s.WIPCeiling < 0 {
		return fmt.Errorf("policy: wipCeiling %d is negative", s.WIPCeiling)
	}
	if s.PromoteAfter < 0 {
		return fmt.Errorf("policy: promoteAfter %d is negative", s.PromoteAfter)
	}
	seen := make(map[string]string, len(s.Repos))
	for _, repo := range slices.Sorted(maps.Keys(s.Repos)) {
		key := strings.ToLower(strings.TrimSpace(repo))
		if other, ok := seen[key]; ok {
			return fmt.Errorf("policy: repositories %q and %q are the same repository; keep one", other, repo)
		}
		seen[key] = repo
	}
	for repo, r := range s.Repos {
		switch r.Mode {
		case "", RepoLive, RepoShadow:
		default:
			return fmt.Errorf("policy: repository %q mode %q is not live or shadow", repo, r.Mode)
		}
		if r.WIPCeiling < 0 {
			return fmt.Errorf("policy: repository %q wipCeiling %d is negative", repo, r.WIPCeiling)
		}
	}
	for class, v := range s.Trust {
		if _, ok := DefaultTrust()[class]; !ok {
			return fmt.Errorf("policy: trust class %q is not one of %s", class, strings.Join(trustClasses, ", "))
		}
		if v != ToolStandard && v != ToolRestricted {
			return fmt.Errorf("policy: trust class %q policy %q is not standard or restricted", class, v)
		}
	}
	for _, a := range s.Allow {
		if strings.TrimSpace(a.Tool) == "" || strings.TrimSpace(a.Rule) == "" || strings.TrimSpace(a.Repo) == "" {
			return errors.New("policy: an allow rule needs a tool, a rule and a repository")
		}
		if neverPromoted(a.Rule) {
			return fmt.Errorf("policy: rule %q can never be allowed", a.Rule)
		}
	}
	return nil
}

func knownScope(scope string) bool {
	for _, s := range scopeRows {
		if s == scope {
			return true
		}
	}
	return false
}

func validateRow(source, scope string, r Row) error {
	check := func(cell, v string, allowed ...string) error {
		if v == "" {
			return nil
		}
		for _, a := range allowed {
			if v == a {
				return nil
			}
		}
		return fmt.Errorf("policy: %s admission %s.%s is %q; the choices are %s", source, scope, cell, v, strings.Join(allowed, ", "))
	}
	if scope == ScopeCI {
		// A CI result has no sender, so its row offers only the last cell.
		if r.ListedUnaddressed != "" || r.UnlistedAddressed != "" {
			return fmt.Errorf("policy: %s admission ci has only the %s cell", source, CellUnlistedUnaddressed)
		}
		return check(CellUnlistedUnaddressed, r.UnlistedUnaddressed, ChoiceInferred, ChoiceIgnore)
	}
	if err := check(CellListedUnaddressed, r.ListedUnaddressed, ChoiceInferred, ChoiceIgnore); err != nil {
		return err
	}
	if err := check(CellUnlistedAddressed, r.UnlistedAddressed, ChoiceExplicit, ChoiceInferred, ChoiceIgnore); err != nil {
		return err
	}
	return check(CellUnlistedUnaddressed, r.UnlistedUnaddressed, ChoiceInferred, ChoiceIgnore)
}

// canonical is what the Policy version hashes.
type canonical struct {
	Mode    string                     `json:"mode"`
	Rules   []Rule                     `json:"rules"`
	Sources map[string]canonicalSource `json:"sources"`
}

type canonicalSource struct {
	Allowed   []string       `json:"allowed"`
	Bots      []string       `json:"bots"`
	Admission map[string]Row `json:"admission"`
}

// Version identifies the Policy: the first 12 hex characters of the sha256 of the canonical JSON of the mode, the
// enabled rules and the Source parts. It does not depend on map key order, on the order of the rules or of the
// logins, on the case of a login, on disabled rules or on whether a default cell is written out.
func Version(s Settings) string {
	s = normalize(s)
	c := canonical{Mode: s.Mode, Rules: []Rule{}, Sources: make(map[string]canonicalSource, len(s.Sources))}
	for _, r := range s.Rules {
		if r.Enabled {
			c.Rules = append(c.Rules, r)
		}
	}
	sort.Slice(c.Rules, func(i, j int) bool { return c.Rules[i].Name < c.Rules[j].Name })
	for name, p := range s.Sources {
		c.Sources[name] = canonicalSource{Allowed: foldedSet(p.Allowed), Bots: foldedSet(p.Bots), Admission: p.Admission}
	}
	raw, err := json.Marshal(c) // encoding/json sorts map keys
	if err != nil {
		panic("policy: canonical settings do not encode: " + err.Error()) // plain strings and maps cannot fail
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:12]
}

// foldedSet returns the logins lower-cased, sorted and without duplicates (never nil).
func foldedSet(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// ErrStaleRevision is returned by Store.Save when the base revision is not the current one.
var ErrStaleRevision = store.ErrStaleRevision

// Revision is one stored revision of the settings.
type Revision struct {
	Revision int
	Origin   string
	At       time.Time
	Settings Settings
}

// revisionDoc is the stored document of a revision.
type revisionDoc struct {
	Origin   string    `json:"origin"`
	At       time.Time `json:"at"`
	Settings Settings  `json:"settings"`
}

// Store keeps the Policy's settings as numbered revisions in the settings table, block "policy".
type Store struct {
	st       *store.Store
	defaults Settings
}

// NewStore returns the settings Store over st. defaults are what Current returns until the first Save.
func NewStore(st *store.Store, defaults Settings) *Store {
	return &Store{st: st, defaults: normalize(defaults)}
}

// Current returns the newest settings and their revision; with nothing saved yet it returns the defaults at
// revision 0.
func (s *Store) Current() (Settings, int, error) {
	r, ok, err := s.st.CurrentSettings(settingsBlock)
	if err != nil {
		return Settings{}, 0, err
	}
	if !ok {
		return normalize(s.defaults), 0, nil
	}
	doc, err := decodeRevision(r)
	if err != nil {
		return Settings{}, 0, err
	}
	return doc.Settings, r.Revision, nil
}

// Save validates cur and stores it as the revision after baseRevision (0 when nothing was saved), recording the
// origin (cli, router, sync or web). A baseRevision that is not the current one is refused with ErrStaleRevision.
func (s *Store) Save(cur Settings, baseRevision int, origin string) (int, error) {
	switch origin {
	case OriginCLI, OriginRouter, OriginSync, OriginWeb:
	default:
		return 0, fmt.Errorf("policy: origin %q is not cli, router, sync or web", origin)
	}
	if err := Validate(cur); err != nil {
		return 0, err
	}
	raw, err := json.Marshal(revisionDoc{Origin: origin, At: s.st.Now().UTC(), Settings: normalize(cur)})
	if err != nil {
		return 0, fmt.Errorf("policy: encode settings: %w", err)
	}
	return s.st.SaveSettings(settingsBlock, baseRevision, string(raw))
}

// History returns the stored revisions, newest first; limit <= 0 returns all.
func (s *Store) History(limit int) ([]Revision, error) {
	rs, err := s.st.SettingsHistory(settingsBlock, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Revision, 0, len(rs))
	for _, r := range rs {
		doc, err := decodeRevision(r)
		if err != nil {
			return nil, err
		}
		out = append(out, Revision{Revision: r.Revision, Origin: doc.Origin, At: doc.At, Settings: doc.Settings})
	}
	return out, nil
}

func decodeRevision(r store.SettingsRevision) (revisionDoc, error) {
	var doc revisionDoc
	if err := json.Unmarshal([]byte(r.Doc), &doc); err != nil {
		return revisionDoc{}, fmt.Errorf("policy: decode settings revision %d: %w", r.Revision, err)
	}
	doc.Settings = normalize(doc.Settings)
	return doc, nil
}
