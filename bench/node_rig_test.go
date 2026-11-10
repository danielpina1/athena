package bench

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/server"
)

// The remote-node rig is two real processes of the built binary: an `athena server` on loopback whose own node cannot
// launch a session (no runtime), so every Ticket is placed on the second process, an `athena node run` joined to it
// over mutual TLS (fakeclaude, no Codex).

var joinCmd = regexp.MustCompile(`^athena node join (\S+) (\S+)(?: --ca-sha256 (\S+))?$`)

type remoteRig struct {
	tb      testing.TB
	dir     string // the server's state directory
	nodeDir string
	node    *exec.Cmd
	nodeID  string // the node's id, which pins a task to it
}

func freePort(tb testing.TB) int {
	tb.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// runAthena runs the built binary to completion and returns its stdout.
func runAthena(tb testing.TB, args ...string) string {
	tb.Helper()
	bin := build(tb, "athena", "github.com/danielpina1/athena/cmd/athena")
	var out, errb bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		tb.Fatalf("athena %v: %v\n%s", args, err, errb.String())
	}
	return out.String()
}

// startRemoteRig starts the server and the joined, connected node and waits until the node serves its socket.
func startRemoteRig(tb testing.TB) *remoteRig {
	tb.Helper()
	bin := build(tb, "athena", "github.com/danielpina1/athena/cmd/athena")
	fake := fakeClaude(tb)
	dir, nodeDir := shortDir(tb), shortDir(tb)
	listen, nodeListen := fmt.Sprintf("127.0.0.1:%d", freePort(tb)), fmt.Sprintf("127.0.0.1:%d", freePort(tb))
	if err := server.SetupWeb(dir, server.WebConfig{Listen: listen, NodeListen: nodeListen, Host: "localhost", TLS: "ca"}); err != nil {
		tb.Fatal(err)
	}
	cfg, err := server.LoadConfig(dir)
	if err != nil {
		tb.Fatal(err)
	}
	cfg.LocalRepoRoots = &[]string{}
	if err := server.SaveConfig(dir, cfg); err != nil {
		tb.Fatal(err)
	}
	// The server's own node has no runtime to launch, so placement sends every Ticket to the remote one.
	srv := exec.Command(bin, "server", "--state-dir", dir, "--claude", "/nonexistent/claude", "--codex", "/nonexistent/codex")
	if err := srv.Start(); err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { stopServer(srv) })
	for deadline := time.Now().Add(startTimeout); ; time.Sleep(20 * time.Millisecond) {
		var st server.WebStatusResult
		if err := controlCall(dir, server.Request{Op: server.OpWebStatus}, &st); err == nil && st.Serving {
			break
		}
		if time.Now().After(deadline) {
			tb.Fatal("the server's endpoint did not come up")
		}
	}
	m := joinCmd.FindStringSubmatch(strings.TrimSpace(runAthena(tb, "node", "token", "--state-dir", dir)))
	if m == nil {
		tb.Fatal("node token printed no join command")
	}
	args := []string{"node", "join", m[1], m[2], "--name", "bench-node", "--state", nodeDir}
	if m[3] != "" {
		args = append(args, "--ca-sha256", m[3])
	}
	runAthena(tb, args...)
	nd := exec.Command(bin, "node", "run", "--state", nodeDir, "--claude", fake, "--codex", "/nonexistent/codex", "--slots", "4")
	if err := nd.Start(); err != nil {
		tb.Fatal(err)
	}
	r := &remoteRig{tb: tb, dir: dir, nodeDir: nodeDir, node: nd}
	tb.Cleanup(func() { stopServer(nd) })
	// Ready means bench-node's own row says online (the server's own node is always online) and the node serves its
	// socket, which it does only once it is linked and has the server's prompts.
	for deadline := time.Now().Add(startTimeout); ; time.Sleep(20 * time.Millisecond) {
		out := runAthena(tb, "node", "list", "--state-dir", dir)
		_, err := os.Stat(filepath.Join(nodeDir, server.NodeSocket))
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) >= 3 && f[0] == "bench-node" && f[2] == "online" {
				r.nodeID = f[1]
			}
		}
		if r.nodeID != "" && err == nil {
			break
		}
		if time.Now().After(deadline) {
			tb.Fatalf("the node did not come online; node list:\n%s", out)
		}
	}
	return r
}

// rssBytes is the current resident set size of a process in bytes, read with ps (the node has no metrics socket).
func rssBytes(tb testing.TB, pid int) uint64 {
	tb.Helper()
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		tb.Fatalf("ps: %v", err)
	}
	kb, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		tb.Fatalf("ps printed %q", out)
	}
	return kb << 10
}

// nodeIdleRSS leaves a connected `athena node run` idle for idle and returns its RSS.
func nodeIdleRSS(tb testing.TB, idle time.Duration) uint64 {
	tb.Helper()
	r := startRemoteRig(tb)
	time.Sleep(idle)
	return rssBytes(tb, r.node.Process.Pid)
}

// eventTimes reads the server's log and returns, per Ticket, the time of its ticket.created and of its first
// assignment.started event (the Engine emits an event after the change it names is applied).
func (r *remoteRig) eventTimes() (created, started map[string]time.Time) {
	created, started = map[string]time.Time{}, map[string]time.Time{}
	f, err := os.Open(filepath.Join(r.dir, "logs", "athena.log"))
	if err != nil {
		r.tb.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var l struct {
			Time   time.Time `json:"time"`
			Msg    string    `json:"msg"`
			Type   string    `json:"type"`
			Ticket string    `json:"ticket"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Msg != "event" {
			continue
		}
		switch l.Type {
		case "ticket.created":
			created[l.Ticket] = l.Time
		case "assignment.started":
			if _, ok := started[l.Ticket]; !ok {
				started[l.Ticket] = l.Time
			}
		}
	}
	return created, started
}

// measureRemoteDispatch returns n latencies of a Ticket with no repository placed on a loopback remote node: from the
// Engine's creating the Ticket (and so its pending Assignment, which the fleet places and Starts) to the node's
// AssignmentStarted fact applied on the server, both read from the server's event log. With nothing in between but
// placement, the link, the node launching fakeclaude and its fact coming back, the figure is the whole dispatch.
func measureRemoteDispatch(tb testing.TB, n int) []time.Duration {
	r := startRemoteRig(tb)
	var ids []string
	for i := 0; i < n+3; i++ {
		var out struct {
			ID string `json:"id"`
		}
		if err := controlCall(r.dir, server.Request{Op: server.OpSubmit, Text: "remote dispatch benchmark", Node: r.nodeID}, &out); err != nil {
			tb.Fatal(err)
		}
		deadline := time.Now().Add(startTimeout)
		for {
			if _, started := r.startedOf(out.ID); started {
				break
			}
			if time.Now().After(deadline) {
				tb.Fatalf("ticket %s did not start on the node", out.ID)
			}
			time.Sleep(10 * time.Millisecond)
		}
		ids = append(ids, out.ID)
		if err := controlCall(r.dir, server.Request{Op: server.OpStop, ID: out.ID, Reason: "done"}, nil); err != nil {
			tb.Fatal(err)
		}
	}
	created, started := r.eventTimes()
	var out []time.Duration
	for i, id := range ids {
		if i < 3 { // the first sessions warm the file cache
			continue
		}
		c, ok1 := created[id]
		s, ok2 := started[id]
		if !ok1 || !ok2 {
			tb.Fatalf("ticket %s: no created or started event in the log", id)
		}
		out = append(out, s.Sub(c))
	}
	return out
}

func (r *remoteRig) startedOf(id string) (time.Time, bool) {
	_, started := r.eventTimes()
	t, ok := started[id]
	return t, ok
}
