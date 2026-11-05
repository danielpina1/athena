// Command fakecodex stands in for the `codex` binary in tests. It speaks the app-server protocol over stdio
// (`fakecodex app-server ...`) from fixtures: it answers the requests Athena makes, and replays a fixture of server
// notifications (the lines the real server printed, one JSON object per line) when a turn starts. Other commands
// (`--version`, `login`) are added by the tests that need them.
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
//	                                 default), "untrusted" (they also do not run), or "foreign" (trusted, plus an
//	                                 enabled hook of a repository's .codex)
//	ATHENA_FAKE_CODEX_LOGGED_OUT     when set, account/read says there is no account and the rate limits are refused
//
// It answers initialize, thread/start (thread id thr_fixture_<ATHENA_TICKET>, announced by a thread/started
// notification), thread/resume, turn/start, turn/steer, turn/interrupt (the turn ends "interrupted"), hooks/list,
// config/batchWrite, account/read (acct_fixture) and account/rateLimits/read; any other request gets a -32601 error.
//
// Fixtures are real server lines with the placeholder ids thr_fixture_0001 and turn_fixture_0001, which fakecodex
// replaces by the thread's and the turn's. Its own thread/started replaces the fixture's. The text and client id of
// the fixture's userMessage items become those of the turn/start or turn/steer that caused the replay, as the real
// server echoes them. Lines with a "fake" key are directives, not sent to the client:
//
//	{"fake":"request","method":"item/commandExecution/requestApproval","params":{...}}
//	    sends that server request (ids from 1000), waits for the client's answer and logs it
//	{"fake":"hook","event":"PreToolUse","input":{...}}
//	    runs the commands config.toml names for that event (the exact lines `[[hooks.<Event>.hooks]]` and
//	    `command = "..."`) with the input on stdin, and drops the next item (its item/started and item/completed) when
//	    a hook denies it: exit code 2, or stdout with "permissionDecision":"deny" or "decision":"block"
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	if len(args) == 0 || args[0] != "app-server" {
		fmt.Fprintf(os.Stderr, "fakecodex: unsupported command %q\n", strings.Join(args, " "))
		os.Exit(2)
	}
	newServer().serve(os.Stdin)
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
}

// turn is a running turn: its replay stops when cancelled is closed.
type turn struct {
	id        string
	cancelled chan struct{}
	done      bool // guarded by server.mu: the turn has ended
}

func newServer() *server {
	s := &server{out: bufio.NewWriterSize(os.Stdout, 1<<16), nextReq: 1000, pending: map[string]chan json.RawMessage{}, thread: "thr_fixture_" + ticketName()}
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
		if s.active != nil && !s.active.done {
			s.mu.Unlock()
			s.replyError(w.ID, -32600, "a turn is already running")
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
		s.reply(w.ID, map[string]any{"status": "ok", "version": "fixture", "filePath": filepath.Join(home, "config.toml")})
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

// hook is one command of $CODEX_HOME/config.toml.
type hook struct{ event, command string }

// readHooks parses the hooks of $CODEX_HOME/config.toml: only the lines `[[hooks.<Event>.hooks]]` and `command = "…"`.
func readHooks(home string) []hook {
	f, err := os.Open(filepath.Join(home, "config.toml"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var hooks []hook
	event := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "[[hooks.") && strings.HasSuffix(line, ".hooks]]"):
			event = strings.TrimSuffix(strings.TrimPrefix(line, "[[hooks."), ".hooks]]")
		case strings.HasPrefix(line, "["):
			event = ""
		case event != "" && strings.HasPrefix(line, "command = "):
			if cmd, err := strconv.Unquote(strings.TrimPrefix(line, "command = ")); err == nil {
				hooks = append(hooks, hook{event: event, command: cmd})
			}
		}
	}
	return hooks
}

func hooksMode() string {
	switch m := os.Getenv("ATHENA_FAKE_CODEX_HOOKS"); m {
	case "untrusted", "foreign":
		return m
	}
	return "trusted"
}

// hookList is what hooks/list says: the hooks of config.toml, and with "foreign" one of the repository's.
func hookList(home, cwd string) []any {
	var list []any
	trust := "trusted"
	if hooksMode() == "untrusted" {
		trust = "untrusted"
	}
	for i, h := range readHooks(home) {
		list = append(list, map[string]any{
			"key": fmt.Sprintf("user:%s:%d", h.event, i), "eventName": lowerFirst(h.event), "handlerType": "command",
			"command": h.command, "enabled": true, "isManaged": false, "source": "user",
			"sourcePath": filepath.Join(home, "config.toml"), "currentHash": fmt.Sprintf("fixture-hash-%d", i),
			"displayOrder": i, "timeoutSec": 30, "trustStatus": trust, "matcher": nil, "statusMessage": nil, "pluginId": nil,
		})
	}
	if hooksMode() == "foreign" {
		list = append(list, map[string]any{
			"key": "project:preToolUse:0", "eventName": "preToolUse", "handlerType": "command", "command": "./scripts/audit.sh",
			"enabled": true, "isManaged": false, "source": "project", "sourcePath": filepath.Join(cwd, ".codex", "hooks.json"),
			"currentHash": "fixture-hash-foreign", "displayOrder": len(list), "timeoutSec": 30, "trustStatus": "trusted",
			"matcher": nil, "statusMessage": nil, "pluginId": nil,
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

// runHook runs the commands configured for event with input on stdin, as Codex does at that point of a turn, and
// reports whether one denied: exit code 2, or an output saying "permissionDecision":"deny" or "decision":"block". An
// untrusted hook does not run.
func (s *server) runHook(event string, input json.RawMessage) (denied bool) {
	rec := map[string]any{"fake": "hook", "event": event}
	defer func() {
		rec["denied"] = denied
		b, _ := json.Marshal(rec)
		s.logRPC(b)
	}()
	if hooksMode() == "untrusted" {
		rec["skipped"] = "untrusted"
		return false
	}
	for _, h := range readHooks(os.Getenv("CODEX_HOME")) {
		if h.event != event {
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
