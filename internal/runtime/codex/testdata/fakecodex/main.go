// Command fakecodex stands in for the `codex` binary in tests. It speaks the app-server protocol over stdio
// (`fakecodex app-server ...`) from fixtures: it answers the requests Athena makes, and replays a fixture of server
// notifications (the lines the real server printed, one JSON object per line) when a turn starts. `fakecodex --version`
// prints "codex-cli <version>", and `fakecodex login [--with-api-key]` logs its arguments (ATHENA_FAKE_CODEX_ARGS_LOG),
// reads and drops the key from stdin with --with-api-key, and says it logged in. Any other command exits 2.
//
// Behaviour is driven by environment variables so other packages can reuse it, as fakeclaude does:
//
//	ATHENA_FAKE_CODEX_FIXTURE        path of the fixture replayed on the first turn/start ({ticket} is ATHENA_TICKET,
//	                                 then "default"; a path that does not exist replays nothing)
//	ATHENA_FAKE_CODEX_LATER_FIXTURE  path of the fixture replayed on every later turn/start, and on turn/steer (a steer
//	                                 replays it into the running turn, without its turn/started and turn/completed)
//	ATHENA_FAKE_CODEX_HOLD_RESULT    path of a file: a fixture's turn/completed waits until the file exists
//	ATHENA_FAKE_CODEX_RPC_LOG        file every line the client sent is appended to: requests, notifications, and its
//	                                 answers to the server requests below; and one {"fake":"hook",...} line per hook run
//	ATHENA_FAKE_CODEX_ARGS_LOG       file receiving {"args":[...],"codexHome":"...","env":{ATHENA_*}} as JSON
//	ATHENA_FAKE_CODEX_EXIT_MID_TURN  when set, the first turn replays half its fixture, then the process exits 3
//	ATHENA_FAKE_CODEX_HOOKS          what hooks/list says of the hooks in $CODEX_HOME/config.toml: "trusted" (the
//	                                 default); "untrusted" (untrusted until config/batchWrite records their trust, as
//	                                 Codex does, in config.toml; "modified" when a recorded hash is not theirs);
//	                                 "never-trusted" (untrusted whatever is written); or "foreign" (trusted, plus an
//	                                 enabled hook of a repository's .codex). A hook that is not trusted does not run.
//	ATHENA_FAKE_CODEX_LOGGED_OUT     when set, account/read says there is no account and the rate limits are refused
//	ATHENA_FAKE_CODEX_VERSION        the version --version prints (default 0.160.1)
//	ATHENA_FAKE_CODEX_PROTOCOL       path of protocol.json (default: testdata/codex/protocol.json of the source tree it
//	                                 was built from); every request it knows is checked against it, as the real server
//	                                 does: params must be an object where the method needs params, with the required
//	                                 fields present, or the answer is -32600. When the file cannot be read, app-server
//	                                 exits 5 rather than run unchecked, unless the variable is "off".
//
// It answers initialize, thread/start (thread id thr_fixture_<ATHENA_TICKET>, announced by a thread/started
// notification), thread/resume, turn/start (during a turn it steers into it, as the real server does), turn/steer,
// turn/interrupt (the turn ends "interrupted"), hooks/list, config/batchWrite (it records only the
// hooks.state."<key>".trusted_hash edits), account/read (acct_fixture) and account/rateLimits/read; any other request
// gets a -32601 error.
//
// Hooks are listed as Codex lists them: key "<config path>:<snake_event>:<group>:<handler>" (the path with the home's
// symlinks resolved, as in sourcePath), source "user", their timeout (600 when the handler names none), async flag and
// group matcher, and a currentHash of all of these. Trust is recorded as Codex records it, at the end of config.toml:
//
//	[hooks.state."<key>"]
//	trusted_hash = "<currentHash>"
//
// Fixtures are real server lines with the placeholder ids thr_fixture_0001 and turn_fixture_0001, which fakecodex
// replaces by the thread's and the turn's. Its own thread/started replaces the fixture's. The text and client id of
// the fixture's userMessage items become those of the turn/start or turn/steer that caused the replay, as the real
// server echoes them. Lines with a "fake" key are directives, not sent to the client:
//
//	{"fake":"request","method":"item/commandExecution/requestApproval","params":{...}}
//	    sends that server request (ids from 1000), waits for the client's answer and logs it
//	{"fake":"hook","event":"PreToolUse","input":{...}}
//	    runs the trusted commands config.toml names for that event (it parses only `[[hooks.<Event>]]` matcher groups
//	    holding `[[hooks.<Event>.hooks]]` handlers with `type = "command"` and `command = "..."`; with --strict-config
//	    any other hooks shape stops the process at start, as Codex does) with the input on stdin, and drops the next
//	    item (its item/started and item/completed) when a hook denies it: exit code 2, or stdout with
//	    "permissionDecision":"deny" or "decision":"block"
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	fixtureThread = "thr_fixture_0001"
	fixtureTurn   = "turn_fixture_0001"
	version       = "0.160.1"
)

func main() {
	args := os.Args[1:]
	logArgs(args)
	switch {
	case len(args) == 1 && args[0] == "--version":
		v := os.Getenv("ATHENA_FAKE_CODEX_VERSION")
		if v == "" {
			v = version
		}
		fmt.Printf("codex-cli %s\n", v)
		return
	case len(args) > 0 && args[0] == "login":
		login(args[1:])
		return
	case len(args) > 0 && args[0] == "app-server":
	default:
		fmt.Fprintf(os.Stderr, "fakecodex: unsupported command %q\n", strings.Join(args, " "))
		os.Exit(2)
	}
	if _, err := readHooks(os.Getenv("CODEX_HOME")); err != nil && slices.Contains(args, "--strict-config") {
		fmt.Fprintf(os.Stderr, "fakecodex: invalid config.toml: %v\n", err)
		os.Exit(1)
	}
	newServer().serve(os.Stdin)
}

// login is `codex login [--with-api-key]`: its arguments are already logged; with --with-api-key it reads the key from
// stdin, as Codex does, and drops it.
func login(args []string) {
	for _, a := range args {
		if a != "--with-api-key" {
			fmt.Fprintf(os.Stderr, "fakecodex: login: unknown argument %q\n", a)
			os.Exit(2)
		}
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			os.Exit(1)
		}
	}
	fmt.Println("Successfully logged in")
}

func logArgs(args []string) {
	rec := struct {
		Args      []string          `json:"args"`
		CodexHome string            `json:"codexHome"`
		Env       map[string]string `json:"env"`
	}{Args: args, CodexHome: os.Getenv("CODEX_HOME"), Env: map[string]string{}}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "ATHENA_") {
			rec.Env[k] = v
		}
	}
	b, _ := json.Marshal(rec)
	if p := ticketPath(os.Getenv("ATHENA_FAKE_CODEX_ARGS_LOG")); p != "" {
		_ = os.WriteFile(p, b, 0o600)
	}
}

func ticketPath(p string) string {
	return strings.ReplaceAll(p, "{ticket}", ticketName())
}

func ticketName() string {
	if t := os.Getenv("ATHENA_TICKET"); t != "" {
		return t
	}
	return "default"
}

// fixture is the path a fixture variable names, its {ticket} resolved; "" for none.
func fixture(name string) string {
	p := os.Getenv(name)
	if !strings.Contains(p, "{ticket}") {
		return p
	}
	for _, t := range []string{os.Getenv("ATHENA_TICKET"), "default"} {
		if t == "" {
			continue
		}
		if q := strings.ReplaceAll(p, "{ticket}", t); fileExists(q) {
			return q
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

type server struct {
	mu      sync.Mutex // guards everything below and serialises writes to out
	out     *bufio.Writer
	rpcLog  *os.File
	thread  string
	turns   int
	active  *turn
	nextReq int
	pending map[string]chan json.RawMessage
	proto   *protocol
}

// turn is a running turn: its replay stops when cancelled is closed.
type turn struct {
	id        string
	cancelled chan struct{}
	done      bool // guarded by server.mu: the turn has ended
}

func newServer() *server {
	s := &server{out: bufio.NewWriterSize(os.Stdout, 1<<16), nextReq: 1000, pending: map[string]chan json.RawMessage{}, thread: "thr_fixture_" + ticketName()}
	s.proto = loadProtocol()
	if p := ticketPath(os.Getenv("ATHENA_FAKE_CODEX_RPC_LOG")); p != "" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			os.Exit(4)
		}
		s.rpcLog = f
	}
	return s
}

// wire is any line of the protocol.
type wire struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (s *server) serve(in io.Reader) {
	r := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			s.handleLine(line)
		}
		if err != nil {
			break
		}
	}
	s.mu.Lock()
	if s.active != nil && !s.active.done {
		s.cancelLocked(s.active)
	}
	s.mu.Unlock()
}

func (s *server) logRPC(line []byte) {
	if s.rpcLog == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.rpcLog.Write(append(append([]byte(nil), line...), '\n'))
}

func (s *server) handleLine(line []byte) {
	var w wire
	if json.Unmarshal(line, &w) != nil {
		return
	}
	s.logRPC(line)
	if w.Method == "" { // the client's answer to one of our requests
		s.mu.Lock()
		ch := s.pending[string(w.ID)]
		delete(s.pending, string(w.ID))
		s.mu.Unlock()
		if ch != nil {
			ch <- line
		}
		return
	}
	if len(w.ID) == 0 { // a notification (initialized): nothing to do
		return
	}
	s.request(w)
}

func (s *server) request(w wire) {
	var p struct {
		Model          string      `json:"model"`
		Cwd            string      `json:"cwd"`
		ThreadID       string      `json:"threadId"`
		TurnID         string      `json:"turnId"`
		ExpectedTurnID string      `json:"expectedTurnId"`
		ClientMsgID    string      `json:"clientUserMessageId"`
		Input          []inputItem `json:"input"`
		Cwds           []string    `json:"cwds"`
	}
	_ = json.Unmarshal(w.Params, &p)
	home := os.Getenv("CODEX_HOME")
	if msg := s.proto.check(w.Method, w.Params); msg != "" {
		s.replyError(w.ID, -32600, "Invalid request: "+msg)
		return
	}
	switch w.Method {
	case "initialize":
		osName := "linux"
		if runtime.GOOS == "darwin" {
			osName = "macos"
		}
		s.reply(w.ID, map[string]any{"userAgent": "athena-fakecodex/" + version, "codexHome": home, "platformFamily": "unix", "platformOs": osName})
	case "thread/start", "thread/resume":
		model := p.Model
		if model == "" {
			model = "gpt-5-codex"
		}
		id := s.thread
		if w.Method == "thread/resume" {
			id = p.ThreadID
			if id == "" {
				s.replyError(w.ID, -32602, "thread/resume: missing threadId")
				return
			}
			s.mu.Lock()
			s.thread = id
			s.mu.Unlock()
		}
		th := threadObject(id, model, p.Cwd)
		s.reply(w.ID, map[string]any{
			"approvalPolicy": "on-request", "approvalsReviewer": "user", "cwd": p.Cwd, "model": model, "modelProvider": "openai",
			"sandbox": map[string]any{"type": "workspaceWrite"}, "thread": th,
		})
		if w.Method == "thread/start" {
			s.send(map[string]any{"method": "thread/started", "params": map[string]any{"thread": th}})
		}
	case "turn/start":
		s.mu.Lock()
		if t := s.active; t != nil && !t.done { // the real server steers a turn/start into the running turn
			s.mu.Unlock()
			s.reply(w.ID, map[string]any{"turn": map[string]any{"id": t.id, "items": []any{}, "status": "inProgress"}})
			go s.replay(t, fixture("ATHENA_FAKE_CODEX_LATER_FIXTURE"), userEcho{text: joinText(p.Input), clientID: p.ClientMsgID}, true, false)
			return
		}
		s.turns++
		t := &turn{id: fmt.Sprintf("turn_fixture_%04d", s.turns), cancelled: make(chan struct{})}
		s.active = t
		first := s.turns == 1
		s.mu.Unlock()
		s.reply(w.ID, map[string]any{"turn": map[string]any{"id": t.id, "items": []any{}, "status": "inProgress"}})
		name := "ATHENA_FAKE_CODEX_LATER_FIXTURE"
		if first {
			name = "ATHENA_FAKE_CODEX_FIXTURE"
		}
		go s.replay(t, fixture(name), userEcho{text: joinText(p.Input), clientID: p.ClientMsgID}, false, first && os.Getenv("ATHENA_FAKE_CODEX_EXIT_MID_TURN") != "")
	case "turn/steer":
		s.mu.Lock()
		t := s.active
		if t == nil || t.done {
			s.mu.Unlock()
			s.replyError(w.ID, -32600, "no active turn to steer")
			return
		}
		if p.ExpectedTurnID != t.id {
			s.mu.Unlock()
			s.replyError(w.ID, -32600, "expectedTurnId does not match the active turn")
			return
		}
		s.mu.Unlock()
		s.reply(w.ID, map[string]any{"turnId": t.id})
		go s.replay(t, fixture("ATHENA_FAKE_CODEX_LATER_FIXTURE"), userEcho{text: joinText(p.Input), clientID: p.ClientMsgID}, true, false)
	case "turn/interrupt":
		s.reply(w.ID, map[string]any{})
		s.mu.Lock()
		if t := s.active; t != nil && !t.done {
			s.cancelLocked(t)
			s.writeLocked(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": s.thread, "turn": map[string]any{
				"id": t.id, "items": []any{}, "status": "interrupted", "durationMs": 1}}})
		}
		s.mu.Unlock()
	case "hooks/list":
		cwd := p.Cwd
		if len(p.Cwds) > 0 {
			cwd = p.Cwds[0]
		}
		s.reply(w.ID, map[string]any{"data": []any{map[string]any{"cwd": cwd, "errors": []any{}, "warnings": []any{}, "hooks": hookList(home, cwd)}}})
	case "config/batchWrite":
		if err := applyTrustEdits(home, w.Params); err != nil {
			s.replyError(w.ID, -32603, "config/batchWrite: "+err.Error())
			return
		}
		s.reply(w.ID, map[string]any{"status": "ok", "version": "fixture", "filePath": configPath(home)})
	case "account/read":
		if os.Getenv("ATHENA_FAKE_CODEX_LOGGED_OUT") != "" {
			s.reply(w.ID, map[string]any{"account": nil, "requiresOpenaiAuth": true})
			return
		}
		s.reply(w.ID, map[string]any{"account": map[string]any{"type": "chatgpt", "email": nil, "planType": "plus"}, "requiresOpenaiAuth": true})
	case "account/rateLimits/read":
		if os.Getenv("ATHENA_FAKE_CODEX_LOGGED_OUT") != "" {
			s.replyError(w.ID, -32600, "codex account authentication required to read rate limits")
			return
		}
		const t0 = 1893456000
		s.reply(w.ID, map[string]any{"accountId": "acct_fixture", "rateLimits": map[string]any{
			"limitId": "codex", "planType": "plus",
			"primary":   map[string]any{"usedPercent": 12, "windowDurationMins": 300, "resetsAt": t0 + 18000},
			"secondary": map[string]any{"usedPercent": 4, "windowDurationMins": 10080, "resetsAt": t0 + 604800},
		}})
	default:
		s.replyError(w.ID, -32601, "method not found: "+w.Method)
	}
}

func threadObject(id, model, cwd string) map[string]any {
	return map[string]any{
		"id": id, "sessionId": id, "cliVersion": version, "createdAt": 1893456000, "updatedAt": 1893456000, "cwd": cwd,
		"ephemeral": false, "modelProvider": "openai", "model": model, "preview": "", "projectId": nil,
		"source": "appServer", "status": map[string]any{"type": "idle"}, "turns": []any{},
	}
}

type inputItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func joinText(in []inputItem) string {
	var parts []string
	for _, i := range in {
		if i.Type == "text" {
			parts = append(parts, i.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// userEcho is what the fixture's userMessage items are rewritten to.
type userEcho struct{ text, clientID string }

func (s *server) reply(id json.RawMessage, result any) {
	s.send(map[string]any{"id": id, "result": result})
}

func (s *server) replyError(id json.RawMessage, code int, msg string) {
	s.send(map[string]any{"id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (s *server) send(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeLocked(v)
}

func (s *server) writeLocked(v any) {
	b, _ := json.Marshal(v)
	s.writeLineLocked(b)
}

func (s *server) writeLineLocked(b []byte) {
	_, _ = s.out.Write(append(b, '\n'))
	_ = s.out.Flush()
}

// cancelLocked ends the turn's replay.
func (s *server) cancelLocked(t *turn) {
	t.done = true
	close(t.cancelled)
}

// replay sends the fixture's lines for turn t. steer: into a running turn, so without its framing.
func (s *server) replay(t *turn, path string, echo userEcho, steer, exitMid bool) {
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		os.Exit(6)
	}
	data = bytes.ReplaceAll(data, []byte(fixtureThread), []byte(s.threadID()))
	data = bytes.ReplaceAll(data, []byte(fixtureTurn), []byte(t.id))
	var lines [][]byte
	for _, l := range bytes.Split(data, []byte("\n")) {
		if l = bytes.TrimSpace(l); len(l) > 0 {
			lines = append(lines, l)
		}
	}
	if exitMid {
		lines = lines[:len(lines)/2]
	}
	dropping, dropID := false, ""
	for _, l := range lines {
		var line struct {
			Fake   string          `json:"fake"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Event  string          `json:"event"`
			Input  json.RawMessage `json:"input"`
		}
		if json.Unmarshal(l, &line) != nil {
			continue
		}
		switch line.Fake {
		case "request":
			if !s.serverRequest(t, line.Method, line.Params) {
				return
			}
			continue
		case "hook":
			if s.runHook(line.Event, line.Input) {
				dropping = true
			}
			continue
		}
		if line.Method == "thread/started" || (steer && (line.Method == "turn/started" || line.Method == "turn/completed")) {
			continue
		}
		if id := itemID(line.Params); id != "" {
			if dropping && line.Method == "item/started" {
				dropping, dropID = false, id
			}
			if id == dropID {
				continue
			}
		}
		if line.Method == "turn/completed" && !s.hold(t) {
			return
		}
		if (line.Method == "item/started" || line.Method == "item/completed") && echo.text != "" {
			l = rewriteUser(l, echo)
		}
		if !s.emit(t, l, line.Method == "turn/completed") {
			return
		}
	}
	if exitMid {
		os.Exit(3)
	}
}

func (s *server) threadID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.thread
}

// emit writes a fixture line unless the turn was interrupted; a turn/completed ends the turn first, so a client that
// reacts to it finds the turn over.
func (s *server) emit(t *turn, line []byte, end bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-t.cancelled:
		return false
	default:
	}
	if end {
		t.done = true
	}
	s.writeLineLocked(line)
	return true
}

// hold waits, before a turn's end, until ATHENA_FAKE_CODEX_HOLD_RESULT names a file that exists; false when the turn
// was interrupted meanwhile.
func (s *server) hold(t *turn) bool {
	p := ticketPath(os.Getenv("ATHENA_FAKE_CODEX_HOLD_RESULT"))
	for p != "" && !fileExists(p) {
		select {
		case <-t.cancelled:
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
	return true
}

// serverRequest sends a server request and waits for the client's answer (which handleLine logs); false when the turn
// was interrupted meanwhile.
func (s *server) serverRequest(t *turn, method string, params json.RawMessage) bool {
	s.mu.Lock()
	id := strconv.Itoa(s.nextReq)
	s.nextReq++
	ch := make(chan json.RawMessage, 1)
	s.pending[id] = ch
	b, _ := json.Marshal(map[string]any{"id": json.Number(id), "method": method, "params": params})
	s.writeLineLocked(b)
	s.mu.Unlock()
	select {
	case <-ch:
		return true
	case <-t.cancelled:
		return false
	}
}

// itemID is the id of the item a notification is about.
func itemID(params json.RawMessage) string {
	var p struct {
		Item   struct{ ID string } `json:"item"`
		ItemID string              `json:"itemId"`
	}
	_ = json.Unmarshal(params, &p)
	if p.Item.ID != "" {
		return p.Item.ID
	}
	return p.ItemID
}

// rewriteUser makes a userMessage item of the line carry the text and client id the client sent.
func rewriteUser(line []byte, echo userEcho) []byte {
	var m map[string]any
	if json.Unmarshal(line, &m) != nil {
		return line
	}
	params, _ := m["params"].(map[string]any)
	item, _ := params["item"].(map[string]any)
	if item == nil || item["type"] != "userMessage" {
		return line
	}
	item["content"] = []any{map[string]any{"type": "text", "text": echo.text, "text_elements": []any{}}}
	if echo.clientID != "" {
		item["clientId"] = echo.clientID
	} else {
		delete(item, "clientId")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return line
	}
	return b
}

// hook is one handler of $CODEX_HOME/config.toml: its event, command, and where it is (its matcher group among the
// event's, and its place in the group), which make its key.
type hook struct {
	event, command string
	group, handler int
	timeout        int     // seconds (Codex's default, 600, when the handler names none)
	async          bool    // async = true
	matcher        *string // the matcher group's matcher, if any
}

// key is the hook's key as Codex makes it: "<config path>:<snake_event>:<group>:<handler>".
func (h hook) key(home string) string {
	return fmt.Sprintf("%s:%s:%d:%d", configPath(home), snake(h.event), h.group, h.handler)
}

// configPath is $CODEX_HOME/config.toml as Codex reports it (sourcePath, and in hook keys): with the home's symlinks
// resolved (/var/folders/... is /private/var/folders/... on macOS).
func configPath(home string) string {
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	return filepath.Join(home, "config.toml")
}

// hash is the hook's currentHash: a hash of its event, command, timeout, async flag and matcher (what Codex hashes
// is its own; any change to the handler changes it).
func (h hook) hash() string {
	m := "<none>"
	if h.matcher != nil {
		m = *h.matcher
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d\n%t\n%s", h.event, h.command, h.timeout, h.async, m)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// readHooks parses the hooks of $CODEX_HOME/config.toml, in the shape Codex loads: `[[hooks.<Event>]]` matcher groups,
// each followed by `[[hooks.<Event>.hooks]]` handlers with `type = "command"` and `command = "…"`. It returns the valid
// hooks, and an error for the first piece of any other shape (which Codex's --strict-config refuses).
func readHooks(home string) ([]hook, error) {
	f, err := os.Open(filepath.Join(home, "config.toml"))
	if err != nil {
		return nil, nil
	}
	defer f.Close()
	var hooks []hook
	var firstErr error
	fail := func(format string, a ...any) {
		if firstErr == nil {
			firstErr = fmt.Errorf(format, a...)
		}
	}
	group := ""
	var matcher *string        // the current group's matcher
	groups := map[string]int{} // matcher groups seen per event
	handlers := 0              // handlers seen in the current group
	var cur *hook
	curType := ""
	finish := func() {
		if cur == nil {
			return
		}
		if curType != "command" || cur.command == "" {
			fail("hooks.%s.hooks: a handler needs type = \"command\" and a command", cur.event)
		} else {
			hooks = append(hooks, *cur)
		}
		cur, curType = nil, ""
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "[[hooks.") && strings.HasSuffix(line, ".hooks]]"):
			finish()
			event := strings.TrimSuffix(strings.TrimPrefix(line, "[[hooks."), ".hooks]]")
			if group != event {
				fail("[[hooks.%s.hooks]] has no [[hooks.%s]] matcher group before it", event, event)
			}
			cur = &hook{event: event, group: groups[event] - 1, handler: handlers, timeout: 600, matcher: matcher}
			handlers++
		case strings.HasPrefix(line, "[[hooks.") && strings.HasSuffix(line, "]]"):
			finish()
			group = strings.TrimSuffix(strings.TrimPrefix(line, "[[hooks."), "]]")
			groups[group]++
			handlers, matcher = 0, nil
		case strings.HasPrefix(line, "["):
			finish()
			group = ""
		case cur != nil && strings.HasPrefix(line, "command = "):
			if cmd, err := strconv.Unquote(strings.TrimPrefix(line, "command = ")); err == nil {
				cur.command = cmd
			}
		case cur != nil && strings.HasPrefix(line, "type = "):
			curType, _ = strconv.Unquote(strings.TrimPrefix(line, "type = "))
		case cur != nil && strings.HasPrefix(line, "timeout = "):
			if n, err := strconv.Atoi(strings.TrimPrefix(line, "timeout = ")); err == nil {
				cur.timeout = n
			}
		case cur != nil && strings.HasPrefix(line, "async = "):
			cur.async = strings.TrimPrefix(line, "async = ") == "true"
		case cur == nil && group != "" && strings.HasPrefix(line, "matcher = "):
			if m, err := strconv.Unquote(strings.TrimPrefix(line, "matcher = ")); err == nil {
				matcher = &m
			}
		}
	}
	finish()
	return hooks, firstErr
}

const stateHeader = `[hooks.state.`

// readTrust returns the trusted hashes config.toml records, by hook key, and the file's lines without those tables.
func readTrust(home string) (trust map[string]string, rest []string) {
	trust = map[string]string{}
	b, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		return trust, nil
	}
	key, inState := "", false
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, stateHeader) && strings.HasSuffix(t, "]"):
			inState = true
			key, _ = strconv.Unquote(strings.TrimSuffix(strings.TrimPrefix(t, stateHeader), "]"))
			continue
		case strings.HasPrefix(t, "["):
			inState = false
		case inState && strings.HasPrefix(t, "trusted_hash = "):
			if v, err := strconv.Unquote(strings.TrimPrefix(t, "trusted_hash = ")); err == nil && key != "" {
				trust[key] = v
			}
			continue
		}
		if !inState {
			rest = append(rest, line)
		}
	}
	return trust, rest
}

// applyTrustEdits records the hooks.state."<key>".trusted_hash edits of a config/batchWrite in config.toml, as Codex
// does (in "never-trusted" mode they are dropped); other edits are ignored.
func applyTrustEdits(home string, params json.RawMessage) error {
	var p struct {
		Edits []struct {
			KeyPath string          `json:"keyPath"`
			Value   json.RawMessage `json:"value"`
		} `json:"edits"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return err
	}
	trust, rest := readTrust(home)
	changed := false
	for _, e := range p.Edits {
		if !strings.HasPrefix(e.KeyPath, "hooks.state.") || !strings.HasSuffix(e.KeyPath, ".trusted_hash") {
			continue
		}
		key, err := strconv.Unquote(strings.TrimSuffix(strings.TrimPrefix(e.KeyPath, "hooks.state."), ".trusted_hash"))
		if err != nil {
			return fmt.Errorf("keyPath %q: the hook key must be a quoted string", e.KeyPath)
		}
		var v string
		if err := json.Unmarshal(e.Value, &v); err != nil {
			return fmt.Errorf("keyPath %q: the value must be a string", e.KeyPath)
		}
		if hooksMode() != "never-trusted" {
			trust[key], changed = v, true
		}
	}
	if !changed {
		return nil
	}
	keys := make([]string, 0, len(trust))
	for k := range trust {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := strings.Join(rest, "\n") + "\n"
	for _, k := range keys {
		out += fmt.Sprintf("\n%s%s]\ntrusted_hash = %s\n", stateHeader, strconv.Quote(k), strconv.Quote(trust[k]))
	}
	return os.WriteFile(filepath.Join(home, "config.toml"), []byte(out), 0o600)
}

func hooksMode() string {
	switch m := os.Getenv("ATHENA_FAKE_CODEX_HOOKS"); m {
	case "untrusted", "never-trusted", "foreign":
		return m
	}
	return "trusted"
}

// trustStatus is what hooks/list says of a configured hook's trust in the current mode.
func trustStatus(h hook, home string, trust map[string]string) string {
	switch hooksMode() {
	case "trusted", "foreign":
		return "trusted"
	case "never-trusted":
		return "untrusted"
	}
	switch v, ok := trust[h.key(home)]; {
	case !ok:
		return "untrusted"
	case v == h.hash():
		return "trusted"
	default:
		return "modified"
	}
}

// hookList is what hooks/list says: the hooks of config.toml, and with "foreign" one of the repository's.
func hookList(home, cwd string) []any {
	list := []any{} // [] rather than null when there is none, as Codex sends it
	configured, _ := readHooks(home)
	trust, _ := readTrust(home)
	for i, h := range configured {
		list = append(list, map[string]any{
			"key": h.key(home), "eventName": lowerFirst(h.event), "handlerType": "command", "async": h.async,
			"command": h.command, "enabled": true, "isManaged": false, "source": "user",
			"sourcePath": configPath(home), "currentHash": h.hash(),
			"displayOrder": i, "timeoutSec": h.timeout, "trustStatus": trustStatus(h, home, trust), "matcher": h.matcher,
			"statusMessage": nil, "pluginId": nil, "additionalContextLimit": nil,
		})
	}
	if hooksMode() == "foreign" {
		src := filepath.Join(cwd, ".codex", "config.toml")
		list = append(list, map[string]any{
			"key": src + ":pre_tool_use:0:0", "eventName": "preToolUse", "handlerType": "command", "async": false,
			"command": "./scripts/audit.sh", "enabled": true, "isManaged": false, "source": "project", "sourcePath": src,
			"currentHash": "sha256:foreign", "displayOrder": len(list), "timeoutSec": 30, "trustStatus": "trusted",
			"matcher": nil, "statusMessage": nil, "pluginId": nil, "additionalContextLimit": nil,
		})
	}
	return list
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// runHook runs the trusted commands configured for event with input on stdin, as Codex does at that point of a turn,
// and reports whether one denied: exit code 2, or an output saying "permissionDecision":"deny" or "decision":"block".
// A hook that is not trusted does not run.
func (s *server) runHook(event string, input json.RawMessage) (denied bool) {
	rec := map[string]any{"fake": "hook", "event": event}
	defer func() {
		rec["denied"] = denied
		b, _ := json.Marshal(rec)
		s.logRPC(b)
	}()
	home := os.Getenv("CODEX_HOME")
	configured, _ := readHooks(home)
	trust, _ := readTrust(home)
	for _, h := range configured {
		if h.event != event {
			continue
		}
		if st := trustStatus(h, home, trust); st != "trusted" {
			rec["skipped"] = st
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", h.command)
		cmd.Stdin = bytes.NewReader(input)
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		err := cmd.Run()
		cancel()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		out := stdout.String()
		rec["exit"], rec["stdout"] = code, out
		var o struct {
			Decision string `json:"decision"`
			Specific struct {
				PermissionDecision string `json:"permissionDecision"`
			} `json:"hookSpecificOutput"`
		}
		_ = json.Unmarshal([]byte(out), &o)
		if code == 2 || o.Specific.PermissionDecision == "deny" || o.Decision == "block" {
			return true
		}
	}
	return false
}

// protocol is protocol.json: what the real server checks of a request.
type protocol struct {
	Methods        map[string][]string `json:"methods"`
	ParamsRequired []string            `json:"paramsRequired"`
}

// loadProtocol reads ATHENA_FAKE_CODEX_PROTOCOL, or the protocol.json beside the fixtures of the source tree this
// binary was built from. It exits 5 when the file cannot be read (a -trimpath build, say), so the fake never runs
// unchecked by accident; ATHENA_FAKE_CODEX_PROTOCOL=off turns the checks off (nil: every request passes).
func loadProtocol() *protocol {
	path := os.Getenv("ATHENA_FAKE_CODEX_PROTOCOL")
	if path == "off" {
		return nil
	}
	if path == "" {
		_, file, _, _ := runtime.Caller(0)
		path = filepath.Join(filepath.Dir(file), "..", "codex", "protocol.json")
	}
	b, err := os.ReadFile(path)
	var p protocol
	if err == nil {
		err = json.Unmarshal(b, &p)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakecodex: protocol.json not usable at %s (%v); set ATHENA_FAKE_CODEX_PROTOCOL to its path, or to off\n", path, err)
		os.Exit(5)
	}
	return &p
}

// check returns why a request of a known method is invalid ("" when it is not, or the method is unknown).
func (p *protocol) check(method string, params json.RawMessage) string {
	if p == nil {
		return ""
	}
	required, known := p.Methods[method]
	if !known {
		return ""
	}
	var obj map[string]json.RawMessage
	present := len(bytes.TrimSpace(params)) > 0 && string(bytes.TrimSpace(params)) != "null"
	if present && json.Unmarshal(params, &obj) != nil {
		return method + ": params must be an object"
	}
	if !present && (slices.Contains(p.ParamsRequired, method) || len(required) > 0) {
		return method + ": params is required (an object)"
	}
	for _, f := range required {
		if _, ok := obj[f]; !ok {
			return fmt.Sprintf("%s: missing field `%s`", method, f)
		}
	}
	return ""
}
