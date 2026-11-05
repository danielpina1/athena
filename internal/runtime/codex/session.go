package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielpina1/athena/internal/redact"
	"github.com/danielpina1/athena/internal/runtime/rt"
)

const (
	// startTimeout bounds a session's start: the handshake, the control check, the thread and the quota read.
	startTimeout = time.Minute
	// callTimeout bounds a request a running session makes (turn/start, turn/steer, turn/interrupt).
	callTimeout = 30 * time.Second
	// answerWait is how long Close lets the approvals it cancelled send their decline before it closes the input.
	answerWait = time.Second
)

// optOutNotifications are the streaming deltas a session never wants: every item's final state comes in its
// item/completed, and the transcript would otherwise hold each token twice.
var optOutNotifications = []string{
	"item/agentMessage/delta", "item/commandExecution/outputDelta", "item/fileChange/outputDelta",
	"item/reasoning/textDelta", "item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded",
	"item/plan/delta", "command/exec/outputDelta",
}

// ErrConfigUncontrolled is a session's refusal to start: the settings Codex would run with in the session's
// directory (config/read) are not what Athena's CODEX_HOME sets. Why says what is wrong.
type ErrConfigUncontrolled struct{ Why string }

func (e ErrConfigUncontrolled) Error() string {
	return "codex settings are not under Athena's control: " + e.Why
}

// session is a running `codex app-server` with one thread: an rt.Session.
type session struct {
	proc     *rt.Proc
	conn     *Conn
	q        *rt.Queue[rt.Event]
	approve  func(context.Context, rt.ApprovalRequest) rt.Verdict
	threadID string         // set before startSession returns, then never changed
	sandbox  map[string]any // every turn/start's sandboxPolicy

	// ctx lives until Close or the process's exit: the approvals waiting for Athena and the requests a session makes
	// end with it.
	ctx    context.Context
	cancel context.CancelFunc

	trMu sync.Mutex // the translator is fed by the reader, and seeded with the quota by startSession
	tr   *Translator

	mu         sync.Mutex
	activeTurn string              // the turn running now ("" between turns), from turn/started and turn/completed
	lastEnded  string              // the turn that ended last, so a late turn/start response does not revive it
	items      map[string]itemInfo // the tool items in flight, by id, for the approvals that name only the item
	inflight   int                 // server requests being answered

	sendMu    sync.Mutex // one Send at a time: a steer's fallback must not race another message
	closed    atomic.Bool
	closeOnce sync.Once
	closedCh  chan struct{} // closed once Close has returned
	done      chan struct{} // closed once the process has exited and Events is closed
}

// itemInfo is what an approval needs of the item it is about.
type itemInfo struct {
	command string
	paths   []string
}

// startSession starts a session (Runtime.Start).
func startSession(ctx context.Context, r *Runtime, o rt.StartOptions) (rt.Session, error) {
	switch {
	case o.Dir == "":
		return nil, errors.New("codex: StartOptions.Dir is required")
	case o.Transcript == nil:
		return nil, errors.New("codex: StartOptions.Transcript is required")
	case o.TicketID == "" || o.AssignmentID == "":
		return nil, errors.New("codex: StartOptions.TicketID and AssignmentID are required (Athena's MCP server needs them)")
	case r.Home == "":
		return nil, errors.New("codex: Athena's CODEX_HOME is not configured")
	case r.AthenaBin == "" || r.Socket == "":
		return nil, errors.New("codex: the athena binary and the node's socket are required")
	}
	home := r.home()
	if abs, err := filepath.Abs(home.Dir); err == nil {
		home.Dir = abs
	}
	p, err := rt.StartProc(ctx, rt.ProcOptions{
		Binary:     r.binary(),
		Args:       append([]string{"app-server", "--listen", "stdio://", "--strict-config"}, overrides(o, home.Socket)...),
		Dir:        o.Dir,
		Env:        sessionEnv(os.Environ(), o, home.Dir),
		CloseGrace: o.CloseGrace,
	})
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	sctx, cancel := context.WithCancel(context.Background())
	s := &session{
		proc: p, q: rt.NewQueue[rt.Event](), approve: o.Approve, sandbox: sandboxPolicy(o),
		ctx: sctx, cancel: cancel, tr: NewTranslator(), items: map[string]itemInfo{},
		closedCh: make(chan struct{}), done: make(chan struct{}),
	}
	s.conn = NewConn(p.Stdout(), p.Stdin(), envelope(o.Transcript))
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		_ = s.conn.Run(s.handle)
		p.Stdout().Close()
		s.q.Finish()
	}()
	go func() {
		<-p.Exited()
		cancel() // nothing waits for an answer any more
	}()

	hctx, hcancel := context.WithTimeout(ctx, startTimeout)
	defer hcancel()
	version := r.Version
	if version == "" {
		version = "dev"
	}
	if err := s.begin(hctx, o, home, version); err != nil {
		cancel()
		_ = p.KillGroup()
		<-p.Exited()
		<-ran
		if errors.Is(err, ErrConnClosed) {
			// The server stopped: most often --strict-config refusing a setting, which it says on stderr.
			if tail := lastLine(p.Stderr()); tail != "" {
				err = fmt.Errorf("%w: %s", err, redact.String(tail))
			}
		}
		return nil, err
	}
	pumped := make(chan struct{})
	go func() {
		s.q.Pump(s.closedCh)
		close(pumped)
	}()
	go func() {
		<-p.Exited()
		<-pumped
		close(s.done)
	}()
	return s, nil
}

// begin is a session's start on its own app-server: initialize, the control check, the thread (Init) and the quota.
func (s *session) begin(ctx context.Context, o rt.StartOptions, home Home, version string) error {
	failed := func(step string, err error) error {
		var hu ErrHooksUncontrolled
		var cu ErrConfigUncontrolled
		if errors.As(err, &hu) || errors.As(err, &cu) {
			return err
		}
		return fmt.Errorf("codex app-server %s: %w", step, err)
	}
	if err := s.conn.Call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "athena", "version": version},
		"capabilities": map[string]any{"optOutNotificationMethods": optOutNotifications},
	}, nil); err != nil {
		return failed("initialize", err)
	}
	if err := s.conn.Notify("initialized", nil); err != nil {
		return failed("initialized", err)
	}
	if err := checkControl(ctx, s.conn, o.Dir, home); err != nil {
		return failed("control check", err)
	}

	mode := "workspace-write"
	if o.ReadOnly {
		mode = "read-only"
	}
	params := map[string]any{"cwd": o.Dir, "approvalPolicy": "on-request", "approvalsReviewer": "user", "sandbox": mode}
	if o.Model != "" {
		params["model"] = o.Model
	}
	method := "thread/start"
	if o.ResumeID != "" {
		// A resumed thread keeps its instructions; its past turns are not needed (excludeTurns), only its id.
		method, params["threadId"], params["excludeTurns"] = "thread/resume", o.ResumeID, true
	} else {
		params["serviceName"] = "athena"
		if o.SystemPrompt != "" {
			params["developerInstructions"] = o.SystemPrompt
		}
	}
	var th struct {
		Model  string `json:"model"`
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := s.conn.Call(ctx, method, params, &th); err != nil {
		return failed(method, err)
	}
	if th.Thread.ID == "" {
		return fmt.Errorf("codex app-server %s: no thread id in the answer", method)
	}
	s.threadID = th.Thread.ID
	s.q.Push(rt.Init{SessionID: th.Thread.ID, Model: th.Model})

	// The quota is free to read; a session starts without it when Codex cannot say.
	var rl struct {
		RateLimits *rateLimitSnapshot `json:"rateLimits"`
	}
	if err := s.conn.Call(ctx, "account/rateLimits/read", nil, &rl); err == nil && rl.RateLimits != nil {
		ws := rl.RateLimits.windows()
		s.trMu.Lock()
		s.tr.windows = ws
		s.trMu.Unlock()
		s.q.Push(rt.QuotaUpdate{Windows: ws})
	} else if errors.Is(err, ErrConnClosed) {
		return failed("account/rateLimits/read", err)
	}
	return nil
}

// overrides are the session's -c overrides: Athena's MCP server's arguments (the Ticket, and the review role) and
// environment (the node's socket, the Assignment). They set nothing else: never hooks.* or features.*, which would
// change what the control check found.
func overrides(o rt.StartOptions, socket string) []string {
	args := []string{"mcp", "--ticket", o.TicketID}
	if o.Role == "review" {
		args = append(args, "--role", "review")
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = tomlString(a)
	}
	env := "{ATHENA_SOCKET=" + tomlString(socket) + ",ATHENA_ASSIGNMENT=" + tomlString(o.AssignmentID) + "}"
	return []string{
		"-c", "mcp_servers.athena.args=[" + strings.Join(quoted, ",") + "]",
		"-c", "mcp_servers.athena.env=" + env,
	}
}

// sessionEnv is the session process's environment: rt.ChildEnv of parent keeping Codex's own variables, with o.Env
// and o.SandboxEnv added, and CODEX_HOME set to home whatever the parent or o.Env said.
func sessionEnv(parent []string, o rt.StartOptions, home string) []string {
	extra := append(slices.Clone(o.Env), o.SandboxEnv...)
	var env []string
	for _, kv := range rt.ChildEnv(parent, extra, "CODEX_", "OPENAI_") {
		if !strings.HasPrefix(kv, "CODEX_HOME=") {
			env = append(env, kv)
		}
	}
	return append(env, "CODEX_HOME="+home)
}

// sandboxPolicy is the sandboxPolicy of every turn/start: read-only for a ReadOnly session, otherwise workspace-write
// with o.WritableRoots (the node's, as given) besides the working directory, which Codex always allows.
func sandboxPolicy(o rt.StartOptions) map[string]any {
	if o.ReadOnly {
		return map[string]any{"type": "readOnly"}
	}
	roots := o.WritableRoots
	if roots == nil {
		roots = []string{}
	}
	return map[string]any{"type": "workspaceWrite", "writableRoots": roots}
}

// envelope returns the Conn's onLine for a transcript: each notification and server request the server printed, as
// {"at":"<RFC3339Nano>","codex":<line>}. Responses never reach it (Conn), so neither the prompt thread/start echoes nor
// the account does.
func envelope(tr *rt.Transcript) func([]byte) {
	return func(raw []byte) {
		var b bytes.Buffer
		b.Grow(len(raw) + 64)
		b.WriteString(`{"at":"`)
		b.WriteString(time.Now().UTC().Format(time.RFC3339Nano))
		b.WriteString(`","codex":`)
		b.Write(raw)
		b.WriteByte('}')
		tr.Append(b.Bytes())
	}
}

// checkControl refuses a session in dir that Codex would run with anything Athena does not control, on the session's
// own app-server (a repository's .codex/ layers load only in its worktree): its hooks must be exactly Athena's, trusted
// and enabled (checkHooks: it never writes trust; Runtime.Check does), and its settings what Athena's CODEX_HOME sets
// (checkConfig).
func checkControl(ctx context.Context, c *Conn, dir string, home Home) error {
	entry, err := listHooks(ctx, c, dir)
	if err != nil {
		return err
	}
	if err := checkHooks(entry, HookSpecs(home)); err != nil {
		return err
	}
	return checkConfig(ctx, c, dir, home)
}

// configRead is what config/read says that checkConfig looks at.
type configRead struct {
	Config struct {
		Features   map[string]json.RawMessage `json:"features"`
		MCPServers map[string]struct {
			Command string `json:"command"`
		} `json:"mcp_servers"`
	} `json:"config"`
	Layers []struct {
		Name struct {
			Type           string `json:"type"`
			DotCodexFolder string `json:"dotCodexFolder"`
		} `json:"name"`
		DisabledReason *string `json:"disabledReason"`
	} `json:"layers"`
}

// checkConfig asks Codex for the settings it would run with in dir (config/read with its layers) and refuses, with
// ErrConfigUncontrolled, when one of the features Athena turns off is not off, when an MCP server other than Athena's
// is configured or Athena's runs another command, or when a repository's .codex/ layer is loaded at all (Codex loads one
// only for a project the user config trusts, and Athena's never does: such a layer could change the model provider,
// the sandbox or a notify command, which the effective features and servers do not show).
func checkConfig(ctx context.Context, c *Conn, dir string, home Home) error {
	var res configRead
	if err := c.Call(ctx, "config/read", map[string]any{"cwd": dir, "includeLayers": true}, &res); err != nil {
		return fmt.Errorf("codex config/read: %w", err)
	}
	for _, f := range disabledFeatures {
		switch v := strings.TrimSpace(string(res.Config.Features[f])); v {
		case "false":
		case "true":
			return ErrConfigUncontrolled{Why: fmt.Sprintf("the Codex feature %s is on, and Athena turns it off", f)}
		default:
			return ErrConfigUncontrolled{Why: fmt.Sprintf("Codex does not report the feature %s as off", f)}
		}
	}
	names := make([]string, 0, len(res.Config.MCPServers))
	for name := range res.Config.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name != "athena" {
			return ErrConfigUncontrolled{Why: fmt.Sprintf("an MCP server Athena did not configure is set up (%s)", name)}
		}
	}
	switch athena, ok := res.Config.MCPServers["athena"]; {
	case !ok:
		return ErrConfigUncontrolled{Why: "Athena's MCP server is not configured"}
	case athena.Command != home.AthenaBin:
		return ErrConfigUncontrolled{Why: fmt.Sprintf("Athena's MCP server runs %q, not %q", athena.Command, home.AthenaBin)}
	}
	for _, l := range res.Layers {
		if l.Name.Type == "project" && l.DisabledReason == nil {
			return ErrConfigUncontrolled{Why: fmt.Sprintf("a repository's Codex settings are loaded (%s)", l.Name.DotCodexFolder)}
		}
	}
	return nil
}

// handle is the Conn's handler: a server request is answered (on its own goroutine, Conn's), a notification updates
// the turn and item state and becomes events.
func (s *session) handle(m Message) {
	if len(m.ID) > 0 {
		s.serve(m)
		return
	}
	s.track(m)
	s.trMu.Lock()
	evs := s.tr.Translate(m)
	s.trMu.Unlock()
	// The session's Init comes from thread/start's (or thread/resume's) answer, once; thread/started repeats it.
	evs = slices.DeleteFunc(evs, func(e rt.Event) bool { _, init := e.(rt.Init); return init })
	s.q.Push(evs...)
}

// track follows the active turn and the tool items an approval may name.
func (s *session) track(m Message) {
	var p struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
		Item item `json:"item"`
	}
	switch m.Method {
	case "turn/started", "turn/completed", "item/started", "item/completed":
		if json.Unmarshal(m.Params, &p) != nil {
			return
		}
	default:
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch m.Method {
	case "turn/started":
		s.activeTurn = p.Turn.ID
	case "turn/completed":
		s.activeTurn, s.lastEnded = "", p.Turn.ID
	case "item/started":
		switch p.Item.Type {
		case "commandExecution":
			s.items[p.Item.ID] = itemInfo{command: p.Item.Command}
		case "fileChange":
			info := itemInfo{}
			for _, c := range p.Item.Changes {
				info.paths = append(info.paths, c.Path)
			}
			s.items[p.Item.ID] = info
		}
	case "item/completed":
		delete(s.items, p.Item.ID)
	}
}

// serve answers one server request, once.
func (s *session) serve(m Message) {
	s.mu.Lock()
	s.inflight++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()
	}()
	result, rerr := s.answer(m)
	if rerr != nil {
		_ = s.conn.RespondError(m.ID, rerr.Code, rerr.Message)
		return
	}
	_ = s.conn.Respond(m.ID, result)
}

// answer is the response to a server request, in the shape its method's response takes, or the JSON-RPC error it is
// refused with:
//
//   - the command, file-change and permissions approvals go to Approve; a declined, cancelled (Close) or unanswerable
//     (no Approve) one is declined;
//   - an MCP elicitation and a dynamic tool call are declined at once: no person waits inside a turn (the worker asks
//     through its report), and Athena gives the model no client-side tools;
//   - the legacy (v1) approvals are denied at once: Athena's sessions speak v2, so one is not Athena's to approve;
//   - a request for the user's input is refused at once, its response having no way to decline;
//   - a token refresh, an attestation and any method Athena does not know are refused as not found.
func (s *session) answer(m Message) (any, *RPCError) {
	switch m.Method {
	case "item/commandExecution/requestApproval":
		var p struct {
			ItemID  string  `json:"itemId"`
			Command *string `json:"command"`
			Cwd     *string `json:"cwd"`
			Reason  *string `json:"reason"`
			Network *struct {
				Host string `json:"host"`
			} `json:"networkApprovalContext"`
		}
		_ = json.Unmarshal(m.Params, &p)
		req := rt.ApprovalRequest{Kind: "command", Command: deref(p.Command), Cwd: deref(p.Cwd), Reason: deref(p.Reason)}
		if req.Command == "" {
			req.Command = s.item(p.ItemID).command
		}
		if p.Network != nil {
			req.Kind, req.Host = "network", p.Network.Host
		}
		return map[string]any{"decision": decision(s.decide(req))}, nil
	case "item/fileChange/requestApproval":
		var p struct {
			ItemID    string  `json:"itemId"`
			Reason    *string `json:"reason"`
			GrantRoot *string `json:"grantRoot"`
		}
		_ = json.Unmarshal(m.Params, &p)
		req := rt.ApprovalRequest{Kind: "file", Paths: slices.Clone(s.item(p.ItemID).paths), Reason: deref(p.Reason)}
		if root := deref(p.GrantRoot); root != "" {
			// Accepting also lets the session write under root for the rest of the session: the approver must see it.
			req.Paths = append(req.Paths, root)
			req.Reason = strings.TrimSpace(req.Reason + " (and writes under " + root + " for the rest of the session)")
		}
		return map[string]any{"decision": decision(s.decide(req))}, nil
	case "item/permissions/requestApproval":
		var p struct {
			Cwd         string          `json:"cwd"`
			Reason      *string         `json:"reason"`
			Permissions json.RawMessage `json:"permissions"`
		}
		_ = json.Unmarshal(m.Params, &p)
		req := rt.ApprovalRequest{Kind: "permissions", Cwd: p.Cwd, Reason: deref(p.Reason), Paths: writePaths(p.Permissions)}
		if !s.decide(req) || len(p.Permissions) == 0 {
			return map[string]any{"permissions": map[string]any{}}, nil // nothing granted
		}
		// Granted for this turn only: what was asked, as asked (the request and grant profiles have one shape).
		return map[string]any{"permissions": p.Permissions, "scope": "turn"}, nil
	case "mcpServer/elicitation/request":
		return map[string]any{"action": "decline"}, nil
	case "item/tool/call":
		return map[string]any{"contentItems": []any{}, "success": false}, nil
	case "execCommandApproval", "applyPatchApproval":
		return map[string]any{"decision": "denied"}, nil
	case "item/tool/requestUserInput":
		return nil, &RPCError{Code: -32000, Message: "athena: no person answers inside a turn; ask through Athena's report tool"}
	}
	return nil, &RPCError{Code: -32601, Message: "athena: " + m.Method + " is not supported"}
}

func (s *session) item(id string) itemInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.items[id]
}

// decide asks Approve. It is false when there is no Approve, when it denies, or when the session's context ends first
// (Close, the process's exit): an approval nobody can act on any more is declined.
func (s *session) decide(req rt.ApprovalRequest) bool {
	if s.approve == nil {
		return false
	}
	ch := make(chan rt.Verdict, 1)
	go func() { ch <- s.approve(s.ctx, req) }()
	select {
	case v := <-ch:
		return v.Allow && s.ctx.Err() == nil
	case <-s.ctx.Done():
		return false
	}
}

func decision(allow bool) string {
	if allow {
		return "accept"
	}
	return "decline"
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// writePaths are the paths a permissions request asks to write: its legacy write list and its write entries.
func writePaths(perms json.RawMessage) []string {
	var p struct {
		FileSystem *struct {
			Write   []string `json:"write"`
			Entries []struct {
				Access string `json:"access"`
				Path   struct {
					Path    string `json:"path"`
					Pattern string `json:"pattern"`
				} `json:"path"`
			} `json:"entries"`
		} `json:"fileSystem"`
	}
	if json.Unmarshal(perms, &p) != nil || p.FileSystem == nil {
		return nil
	}
	paths := slices.Clone(p.FileSystem.Write)
	for _, e := range p.FileSystem.Entries {
		if e.Access != "write" {
			continue
		}
		if e.Path.Path != "" {
			paths = append(paths, e.Path.Path)
		} else if e.Path.Pattern != "" {
			paths = append(paths, e.Path.Pattern)
		}
	}
	return paths
}

// Events returns the session's events in order (see rt.Session).
func (s *session) Events() <-chan rt.Event { return s.q.Out() }

// Send starts a turn with the message, or between a turn's start and its end steers it into the running turn
// (turn/steer); when Codex refuses the steer because the turn is not the one Athena saw or cannot take input, it falls
// back to turn/start (which Codex itself steers into a running turn). uuid comes back as the UserEcho's UUID.
func (s *session) Send(text, uuid string) error {
	if err := s.usable(); err != nil {
		return err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	input := []map[string]any{{"type": "text", "text": text}}
	s.mu.Lock()
	active := s.activeTurn
	s.mu.Unlock()
	if active != "" {
		params := map[string]any{"threadId": s.threadID, "expectedTurnId": active, "input": input}
		if uuid != "" {
			params["clientUserMessageId"] = uuid
		}
		err := s.call("turn/steer", params, nil)
		if err == nil || !steerRefused(err) {
			return err
		}
	}
	params := map[string]any{"threadId": s.threadID, "input": input, "sandboxPolicy": s.sandbox}
	if uuid != "" {
		params["clientUserMessageId"] = uuid
	}
	var res struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := s.call("turn/start", params, &res); err != nil {
		return err
	}
	s.mu.Lock()
	if res.Turn.ID != "" && res.Turn.ID != s.lastEnded {
		s.activeTurn = res.Turn.ID
	}
	s.mu.Unlock()
	return nil
}

// steerRefused reports whether a turn/steer failed because there was no turn to steer, it was not the expected one,
// or it cannot take input (Codex's activeTurnNotSteerable, during a review or compact turn).
func steerRefused(err error) bool {
	var re *RPCError
	if !errors.As(err, &re) {
		return false
	}
	msg := strings.ToLower(re.Message)
	return strings.Contains(msg, "no active turn") || strings.Contains(msg, "expected active turn") ||
		strings.Contains(msg, "cannot steer") || bytes.Contains(re.Data, []byte("activeTurnNotSteerable"))
}

// InterruptTurn interrupts the running turn (turn/interrupt), which then ends "interrupted"; with no turn running it
// does nothing.
func (s *session) InterruptTurn() error {
	if err := s.usable(); err != nil {
		return err
	}
	s.mu.Lock()
	active := s.activeTurn
	s.mu.Unlock()
	if active == "" {
		return nil
	}
	err := s.call("turn/interrupt", map[string]any{"threadId": s.threadID, "turnId": active}, nil)
	var re *RPCError
	if errors.As(err, &re) && strings.Contains(strings.ToLower(re.Message), "no active turn") {
		return nil // it ended meanwhile
	}
	return err
}

// usable is nil while the session can take input: rt.ErrExited once the process has exited, an error once closed.
func (s *session) usable() error {
	select {
	case <-s.proc.Exited():
		return rt.ErrExited
	default:
	}
	if s.closed.Load() {
		return errors.New("codex: session input is closed")
	}
	return nil
}

// call makes a request of a running session, bounded by callTimeout and the session's life; a server that is gone is
// rt.ErrExited.
func (s *session) call(method string, params, result any) error {
	ctx, cancel := context.WithTimeout(s.ctx, callTimeout)
	defer cancel()
	err := s.conn.Call(ctx, method, params, result)
	if err == nil {
		return nil
	}
	select {
	case <-s.proc.Exited():
		return rt.ErrExited
	default:
	}
	if errors.Is(err, ErrConnClosed) {
		return rt.ErrExited
	}
	if s.closed.Load() {
		return errors.New("codex: session input is closed")
	}
	return fmt.Errorf("codex %s: %w", method, err)
}

// Close declines the approvals still waiting (their Approve context ends), closes the input, waits up to the close
// grace for the process to exit, then kills its process group. It is idempotent.
func (s *session) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.cancel()
		s.waitAnswered(answerWait)
		s.proc.CloseStdin()
		close(s.closedCh)
	})
	return nil
}

// waitAnswered waits up to d for the server requests being answered to be answered.
func (s *session) waitAnswered(d time.Duration) {
	deadline := time.Now().Add(d)
	for {
		s.mu.Lock()
		n := s.inflight
		s.mu.Unlock()
		if n == 0 || time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Wait blocks until the process has exited and Events is closed, and returns the process's exit error.
func (s *session) Wait() error {
	<-s.done
	return s.proc.Wait()
}

// Killed reports whether Athena had to kill the process.
func (s *session) Killed() bool { return s.proc.Killed() }

// Stderr returns the last few KB of the process's stderr. It may contain anything; do not log it unfiltered.
func (s *session) Stderr() string { return s.proc.Stderr() }

// Pid is the process id of the app-server, which leads its own process group.
func (s *session) Pid() int { return s.proc.Pid() }
