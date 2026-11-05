package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const fixtureDir = "testdata/codex"

var fixtureNames = []string{"simple-turn", "tool-turn", "steer-turn", "usage-limit-turn", "approval-turn", "rate-limits"}

// fixtureLines returns the lines of a fixture, as written.
func fixtureLines(t *testing.T, name string) [][]byte {
	t.Helper()
	f, err := os.Open(filepath.Join(fixtureDir, name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines [][]byte
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		l, err := r.ReadBytes('\n')
		if l = bytes.TrimSpace(l); len(l) > 0 {
			lines = append(lines, l)
		}
		if err != nil {
			return lines
		}
	}
}

// fixtureMessages returns the server messages of a fixture; the fakecodex directives ({"fake": ...}) are not messages.
func fixtureMessages(t *testing.T, name string) []Message {
	t.Helper()
	var ms []Message
	for _, l := range fixtureLines(t, name) {
		var w wire
		if err := json.Unmarshal(l, &w); err != nil {
			t.Fatalf("%s: %v: %s", name, err, l)
		}
		if w.Method != "" {
			ms = append(ms, Message{ID: w.ID, Method: w.Method, Params: w.Params})
		}
	}
	return ms
}

type protocolFile struct {
	CodexVersion string              `json:"codexVersion"`
	Methods      map[string][]string `json:"methods"`
	// ParamsRequired lists the methods whose params must be present, as an object.
	ParamsRequired []string `json:"paramsRequired"`
	// RequestFields are the fields each client request Athena sends may carry.
	RequestFields map[string][]string `json:"requestFields"`
	// ServerRequestAnswers says how the session answers each server request: "approve", "decline" or "error".
	ServerRequestAnswers map[string]string `json:"serverRequestAnswers"`
}

func loadProtocol(t *testing.T) protocolFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, "protocol.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p protocolFile
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// Every method a fixture uses is one protocol.json (extracted from the installed Codex's schema) knows, and its
// params carry the fields the schema requires, so a hand-written fixture cannot drift from the protocol.
func TestFixturesUseKnownMethodsAndRequiredFields(t *testing.T) {
	proto := loadProtocol(t)
	if proto.CodexVersion != VerifiedVersion {
		t.Errorf("protocol.json is for codex %s, VerifiedVersion is %s", proto.CodexVersion, VerifiedVersion)
	}
	if len(proto.Methods) == 0 {
		t.Fatal("protocol.json lists no methods")
	}
	// Every server request has an answer, the four of the protocol Athena does not model among them (a turn must never
	// wait on a request nobody answers), and the methods that need params say so.
	for _, m := range []string{"item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval",
		"item/tool/requestUserInput", "mcpServer/elicitation/request", "item/tool/call", "account/chatgptAuthTokens/refresh",
		"attestation/generate", "applyPatchApproval", "execCommandApproval"} {
		if a := proto.ServerRequestAnswers[m]; a != "approve" && a != "decline" && a != "error" {
			t.Errorf("server request %s: answer %q, want approve, decline or error", m, a)
		}
		if _, ok := proto.Methods[m]; !ok {
			t.Errorf("server request %s is not in protocol.json", m)
		}
	}
	if !slices.Contains(proto.ParamsRequired, "account/read") || !slices.Contains(proto.ParamsRequired, "hooks/list") || slices.Contains(proto.ParamsRequired, "initialized") {
		t.Errorf("paramsRequired = %v", proto.ParamsRequired)
	}
	for _, name := range fixtureNames {
		for i, l := range fixtureLines(t, name) {
			var line struct {
				Fake   string          `json:"fake"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(l, &line); err != nil {
				t.Fatalf("%s:%d: %v", name, i+1, err)
			}
			switch line.Fake {
			case "hook":
				continue // a directive for fakecodex, not a protocol message
			case "", "request":
			default:
				t.Fatalf("%s:%d: unknown fakecodex directive %q", name, i+1, line.Fake)
			}
			required, ok := proto.Methods[line.Method]
			if !ok {
				t.Errorf("%s:%d: method %q is not in protocol.json", name, i+1, line.Method)
				continue
			}
			var params map[string]json.RawMessage
			if err := json.Unmarshal(line.Params, &params); err != nil {
				t.Errorf("%s:%d: %s params: %v", name, i+1, line.Method, err)
				continue
			}
			for _, f := range required {
				if _, has := params[f]; !has {
					t.Errorf("%s:%d: %s lacks the required param %q", name, i+1, line.Method, f)
				}
			}
		}
	}
}

// The repository is public: a fixture holds no account data, and its ids are the placeholders.
func TestFixturesCarryNoRealIDs(t *testing.T) {
	ids := regexp.MustCompile(`"(?:threadId|sessionId|session_id|accountId)":"([^"]*)"`)
	files, err := filepath.Glob(filepath.Join(fixtureDir, "*.jsonl"))
	captured, _ := filepath.Glob(filepath.Join(fixtureDir, "captured", "*.jsonl")) // what the capture script wrote
	files = append(files, captured...)
	if err != nil || len(files) < len(fixtureNames) {
		t.Fatalf("fixtures = %v, %v", files, err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		bads := []string{"@", "sk-", "/Users/"}
		if home := os.Getenv("HOME"); len(home) > 1 {
			bads = append(bads, home)
		}
		for _, bad := range bads {
			if strings.Contains(s, bad) {
				t.Errorf("%s contains %q", f, bad)
			}
		}
		for _, m := range ids.FindAllStringSubmatch(s, -1) {
			if !strings.HasPrefix(m[1], "thr_fixture") && !strings.HasPrefix(m[1], "acct_fixture") {
				t.Errorf("%s has the id %q, want thr_fixture… or acct_fixture", f, m[1])
			}
		}
		var thread struct {
			Params struct {
				Thread struct{ ID string } `json:"thread"`
			} `json:"params"`
		}
		for _, l := range bytes.Split(b, []byte("\n")) {
			if bytes.Contains(l, []byte(`"thread/started"`)) && json.Unmarshal(l, &thread) == nil && !strings.HasPrefix(thread.Params.Thread.ID, "thr_fixture") {
				t.Errorf("%s: thread id %q", f, thread.Params.Thread.ID)
			}
		}
	}
}
