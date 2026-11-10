package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/fleet"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/nodelink"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/internal/web"
)

// remoteNode is a second node in this process: its own state directory under /tmp, enrolled through the join and
// linked to the server's node listener over mutual TLS, running fakeclaude.
type remoteNode struct {
	t      *testing.T
	id     string
	name   string
	dir    string
	cert   tls.Certificate
	roots  *x509.CertPool
	server atomic.Pointer[webServer] // the server whose node listener it dials
	n      *node.Node
	c      *nodelink.Client
	stop   func()
	// closeGrace is the node's Config.CloseGrace (0: its default).
	closeGrace time.Duration
}

// joinRemote enrolls a node named name with the server, as `athena node join` would, and returns it not yet running.
func joinRemote(t *testing.T, w *webServer, name string) *remoteNode {
	t.Helper()
	tok, err := web.IssueEnrollToken(w.ep.st, store.EnrollNode, 0)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"token": tok, "name": name, "csr": csr})
	resp, err := w.client.Post(w.base+"/node/join", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var ans struct {
		NodeID string `json:"nodeId"`
		Cert   []byte `json:"cert"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &ans) != nil || ans.NodeID == "" {
		t.Fatalf("join: %d %s", resp.StatusCode, raw)
	}
	r := &remoteNode{t: t, id: ans.NodeID, name: name, dir: newStateDir(t),
		cert:  tls.Certificate{Certificate: [][]byte{ans.Cert}, PrivateKey: key},
		roots: w.client.Transport.(*http.Transport).TLSClientConfig.RootCAs}
	r.server.Store(w)
	return r
}

// start runs the node with slots sessions at once and waits until the fleet sees it online with its status.
func (r *remoteNode) start(slots int) {
	r.t.Helper()
	t := r.t
	c, err := nodelink.New(&nodelink.Identity{NodeID: r.id, Name: r.name}, nodelink.Options{
		StateDir: r.dir, Version: "test", MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
		Dial: func(ctx context.Context) (link.Conn, error) {
			w := r.server.Load()
			_, port, _ := net.SplitHostPort(w.ep.nodeL.Addr().String())
			cfg := &tls.Config{RootCAs: r.roots, ServerName: webHost, MinVersion: tls.VersionTLS13,
				Certificates: []tls.Certificate{r.cert}}
			return link.Dial(ctx, "wss://127.0.0.1:"+port+"/node", cfg)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	n := node.New(node.Config{
		WorkRoot: filepath.Join(r.dir, "work"), StateDir: r.dir, AthenaBin: "/usr/bin/true",
		SocketPath: filepath.Join(r.dir, "node.sock"), ClaudeBinary: fakeClaude(t), ApprovalTimeout: 5 * time.Second,
		Name: r.name, Slots: slots, CloseGrace: r.closeGrace,
		Credentials: c.Credentials(), Policy: c.Policy(),
		WorkerPrompt: c.WorkerPrompt, ReviewerPrompt: c.ReviewerPrompt, OnStatus: c.StatusChanged,
	}, c.Engine())
	ctx, cancel := context.WithCancel(context.Background())
	served, ran := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(served)
		_ = n.Serve(ctx)
	}()
	c.Attach(n)
	go func() {
		defer close(ran)
		_ = c.Run(ctx)
	}()
	r.n, r.c = n, c
	stopped := false
	r.stop = func() {
		if stopped {
			return
		}
		stopped = true
		n.Close()
		cancel()
		<-ran
		<-served
		c.Close()
	}
	t.Cleanup(r.stop)
	r.waitOnline(slots)
}

// waitOnline waits until the current server's fleet sees the node online with its slots.
func (r *remoteNode) waitOnline(slots int) {
	r.t.Helper()
	eventually(r.t, r.name+" online in the fleet", func() bool {
		for _, v := range r.server.Load().d.fleet.Nodes() {
			if v.ID == r.id {
				return v.State == fleet.StateOnline && v.Status.Slots == slots
			}
		}
		return false
	})
}

// report files the session's report through the node socket of the node that runs it.
func report(t *testing.T, stateDir, ticketID, assignmentID string) {
	t.Helper()
	var resp node.Response
	nodeCall(t, stateDir, node.ReportRequest{Op: node.OpReport, Ticket: ticketID, Assignment: assignmentID, Status: "ready",
		Summary: "done"}, &resp)
	if !resp.OK {
		t.Fatalf("report on %s: %+v", stateDir, resp)
	}
}

// TestTwoNodesShareLoad: the server's own node and one remote, two slots each: four Tickets run two on each node, and
// every one reaches its ready report.
func TestTwoNodesShareLoad(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	dir := newStateDir(t)
	w := startWebWith(t, dir, func(c *Config) { c.Node.Slots = 2 })
	r := joinRemote(t, w, "mini")
	r.start(2)

	var ids []string
	for range 4 {
		ids = append(ids, w.submit("Share the load"))
	}
	on := map[string]int{}
	for _, id := range ids {
		a := w.running(id)
		on[a.Node]++
	}
	if on["local"] != 2 || on[r.id] != 2 {
		t.Fatalf("placed %v, want two on local and two on %s", on, r.id)
	}
	if got := r.n.Status().SlotsInUse; got != 2 {
		t.Errorf("remote runs %d sessions, want 2", got)
	}
	if got := w.d.node.Status().SlotsInUse; got != 2 {
		t.Errorf("local runs %d sessions, want 2", got)
	}
	for _, id := range ids {
		a := w.running(id)
		sock := dir
		if a.Node == r.id {
			sock = r.dir
		}
		report(t, sock, id, a.ID)
	}
	for _, id := range ids {
		eventually(t, id+" ready", func() bool {
			d := w.detail(id)
			rep := d.Assignments[len(d.Assignments)-1].Report
			return rep != nil && rep.State == core.ReportReady
		})
	}
}

// TestRecoveryLeavesRemoteAssignments: a server that restarts while a remote node runs an Assignment records no
// Interruption for it and starts nothing locally; when the node connects, reconcile starts it there.
func TestRecoveryLeavesRemoteAssignments(t *testing.T) {
	fake := newFakeEnv(t, openTurnFixture(t))
	dir := newStateDir(t)
	w := startWeb(t, dir)
	r := joinRemote(t, w, "mini")
	w.stop()

	var id string
	offline(t, dir, func(e *core.Engine) {
		tk, err := e.SubmitOwnerTask("Runs on mini", core.TaskOptions{})
		if err != nil {
			t.Fatal(err)
		}
		id = tk.ID
		a := tk.LatestAssignment()
		if err := e.AssignmentPlaced(id, a.ID, r.id); err != nil {
			t.Fatal(err)
		}
		if err := e.AssignmentStarted(id, a.ID, fixtureSessionID, "", core.RuntimeClaude); err != nil {
			t.Fatal(err)
		}
	})

	w = startWeb(t, dir)
	r.server.Store(w)
	// Recovery has run by the time the control socket answers; give a stray local Start time to show.
	time.Sleep(300 * time.Millisecond)
	d := w.detail(id)
	a := d.Assignments[len(d.Assignments)-1]
	if a.State != core.AssignmentRunning || a.Node != r.id || len(a.Interruptions) != 0 {
		t.Fatalf("after restart: %+v", a)
	}
	if invs := fake.invocations(t); len(invs) != 0 {
		t.Fatalf("claude started %d times before the node connected: %v", len(invs), invs)
	}
	if slices.Contains(w.d.node.Status().LiveTicketIDs, id) {
		t.Fatal("the local node runs the remote's Assignment")
	}

	r.start(4)
	eventually(t, "reconcile starts it on the node", func() bool { return slices.Contains(r.n.Status().LiveTicketIDs, id) })
	eventually(t, "a claude start", func() bool { return len(fake.invocations(t)) == 1 })
	if rid, ok := argValue(fake.invocations(t)[0], "--resume"); !ok || rid != fixtureSessionID {
		t.Fatalf("start args = %v, want --resume %s", fake.invocations(t)[0], fixtureSessionID)
	}
	if slices.Contains(w.d.node.Status().LiveTicketIDs, id) {
		t.Fatal("the local node runs the remote's Assignment")
	}
	if d := w.detail(id); len(d.Assignments[len(d.Assignments)-1].Interruptions) != 0 {
		t.Fatalf("interruptions after reconcile: %+v", d.Assignments[len(d.Assignments)-1].Interruptions)
	}
}

// parkedRemote runs a server whose own node is drained and one remote, whose session parks under the pause at the end
// of its first turn, after filing a progress Report when report is set.
func parkedRemote(t *testing.T, report bool) (*webServer, fakeEnv, string) {
	t.Helper()
	fake := newFakeEnv(t, claudeFixture(t, "simple-turn.jsonl"))
	hold := filepath.Join(t.TempDir(), "turn-may-end")
	t.Setenv("ATHENA_FAKE_HOLD_RESULT", hold)
	dir := newStateDir(t)
	w := startWebWith(t, dir, fastClose)
	if err := w.d.fleet.SetDraining(fleet.LocalID, true); err != nil {
		t.Fatal(err)
	}
	r := joinRemote(t, w, "mini")
	r.closeGrace = 300 * time.Millisecond
	r.start(2)
	id := w.submit("Task\nbody-r3s4")
	a := w.running(id)
	if a.Node != r.id {
		t.Fatalf("placed on %q, want the remote", a.Node)
	}
	if report {
		var resp node.Response
		nodeCall(t, r.dir, node.ReportRequest{Op: node.OpReport, Ticket: id, Assignment: a.ID, Status: "progress", Summary: "going"}, &resp)
		if !resp.OK {
			t.Fatalf("report = %+v", resp)
		}
	}
	w.ok(Request{Op: OpPause}, nil)
	if err := os.WriteFile(hold, nil, 0o600); err != nil { // the turn ends
		t.Fatal(err)
	}
	eventually(t, "the remote session parked", func() bool {
		got := w.interruptions(id)
		return len(got) == 1 && got[0].Reason == core.InterruptPaused && got[0].ResumedAt == nil && got[0].ReportOwed == !report
	})
	return w, fake, id
}

// Ruling R20: the resume reaches a remote's parked sessions as the local node's. One that owed its Report is started
// again on its node; an idle one gets its pause lifted and waits for its next Input.
func TestResumeReachesRemoteParked(t *testing.T) {
	t.Run("owing its report", func(t *testing.T) {
		w, fake, id := parkedRemote(t, false)
		w.ok(Request{Op: OpResume}, nil)
		eventually(t, "the session started again", func() bool { return len(fake.invocations(t)) == 2 })
		if rid, ok := argValue(fake.invocations(t)[1], "--resume"); !ok || rid != fixtureSessionID {
			t.Fatalf("second start args = %v, want --resume %s", fake.invocations(t)[1], fixtureSessionID)
		}
		eventually(t, "the pause closed by the start", func() bool {
			got := w.interruptions(id)
			return len(got) == 1 && got[0].ResumedAt != nil
		})
	})
	t.Run("idle", func(t *testing.T) {
		w, fake, id := parkedRemote(t, true)
		w.ok(Request{Op: OpResume}, nil)
		eventually(t, "the pause lifted", func() bool {
			got := w.interruptions(id)
			return len(got) == 1 && got[0].ResumedAt != nil
		})
		time.Sleep(300 * time.Millisecond)
		if n := len(fake.invocations(t)); n != 1 {
			t.Fatalf("claude started %d times after the resume of an idle session, want 1", n)
		}
	})
}

// recordingDropper records the repositories whose tokens holdSwitched drops.
type recordingDropper struct{ repos []string }

func (d *recordingDropper) DropTokens(repo string) { d.repos = append(d.repos, repo) }

// TestHoldDropsTokensOnEveryNode: a repository switched to shadow has its write tokens dropped through the fleet, which
// sends them to every node (the fleet's own tests cover a remote getting the call; nodelink's, the node acting on it).
func TestHoldDropsTokensOnEveryNode(t *testing.T) {
	w := startWeb(t, newStateDir(t))
	if got := w.d.tokenDropper(); got != tokenDropper(w.d.fleet) {
		t.Fatalf("tokens dropped through %T, want the fleet", got)
	}
	var d recordingDropper
	from := policy.Settings{Repos: map[string]policy.RepoSettings{"o/r": {}}}
	to := policy.Settings{Repos: map[string]policy.RepoSettings{"o/r": {Mode: policy.RepoShadow}}}
	if err := holdSwitched(w.d.eng, &d, from, to); err != nil {
		t.Fatal(err)
	}
	if len(d.repos) != 1 || d.repos[0] != "o/r" {
		t.Fatalf("dropped %v, want [o/r]", d.repos)
	}
}

func doCommand(w *webServer, sensitive bool, id string, c *athenav1.Command) *athenav1.CommandResult {
	c.Id = id
	return w.d.Do(context.Background(), web.Origin{ClientID: "phone", Sensitive: sensitive}, c)
}

// A join token made from the dashboard needs a fresh passkey confirmation, and is good for an hour.
func TestCreateNodeTokenNeedsReauth(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	w := startWebWith(t, newStateDir(t), nil)
	mk := func() *athenav1.Command {
		return &athenav1.Command{Action: &athenav1.Command_CreateNodeToken{CreateNodeToken: &athenav1.CreateNodeToken{Name: "ignored"}}}
	}
	if r := doCommand(w, false, "t1", mk()); r.GetCode() != web.CodeReauthRequired || r.GetNodeToken() != nil {
		t.Fatalf("without reauth = %v", r)
	}
	before := time.Now()
	r := doCommand(w, true, "t1", mk())
	nt := r.GetNodeToken()
	if !r.GetOk() || nt == nil || !strings.HasPrefix(nt.GetCommand(), "athena node join "+w.origin+" ") {
		t.Fatalf("with reauth = %v", r)
	}
	if exp := nt.GetExpiresAt().AsTime(); exp.Before(before.Add(59*time.Minute)) || exp.After(before.Add(61*time.Minute)) {
		t.Errorf("expires %v, want in an hour", exp)
	}
	// Dashboard commands for nodes: draining needs no reauth, revoking does.
	rn := joinRemote(t, w, "mini")
	drain := &athenav1.Command{Action: &athenav1.Command_SetNodeDraining{SetNodeDraining: &athenav1.SetNodeDraining{NodeId: rn.id, Draining: true}}}
	if r := doCommand(w, false, "d1", drain); !r.GetOk() || !w.d.fleet.Draining(rn.id) {
		t.Fatalf("drain = %v", r)
	}
	revoke := func() *athenav1.Command {
		return &athenav1.Command{Action: &athenav1.Command_RevokeNode{RevokeNode: &athenav1.RevokeNode{NodeId: rn.id}}}
	}
	if r := doCommand(w, false, "v1", revoke()); r.GetCode() != web.CodeReauthRequired {
		t.Fatalf("revoke without reauth = %v", r)
	}
	if r := doCommand(w, true, "v1", revoke()); !r.GetOk() {
		t.Fatalf("revoke = %v", r)
	}
	for _, v := range w.d.fleet.Nodes() {
		if v.ID == rn.id && v.State != fleet.StateRevoked {
			t.Errorf("state = %s", v.State)
		}
	}
}

func submitOn(w *webServer, id, nodeID string) *athenav1.CommandResult {
	return doCommand(w, false, id, &athenav1.Command{Action: &athenav1.Command_SubmitTask{SubmitTask: &athenav1.SubmitTask{
		Text: "Pinned task", Node: nodeID}}})
}

func TestSubmitPinsNode(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	w := startWebWith(t, newStateDir(t), nil)
	rn := joinRemote(t, w, "mini")
	r := submitOn(w, "s1", rn.id)
	if !r.GetOk() || r.GetTicketId() == "" {
		t.Fatalf("submit = %v", r)
	}
	tk, _, err := w.d.eng.Ticket(r.GetTicketId())
	if err != nil || tk.Execution == nil || tk.Execution.Node != rn.id {
		t.Fatalf("pin = %+v, %v", tk.Execution, err)
	}
}

func TestSubmitUnknownNodeRefused(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	w := startWebWith(t, newStateDir(t), nil)
	r := submitOn(w, "s1", "nope")
	if r.GetOk() || r.GetCode() != web.CodeInvalid || !strings.Contains(r.GetMessage(), "No node has the id nope") {
		t.Fatalf("unknown = %v", r)
	}
	rn := joinRemote(t, w, "mini")
	if err := w.d.fleet.Revoke(rn.id); err != nil {
		t.Fatal(err)
	}
	if r := submitOn(w, "s2", rn.id); r.GetOk() || !strings.Contains(r.GetMessage(), "revoked") {
		t.Fatalf("revoked = %v", r)
	}
	if ts, _ := w.d.eng.Tickets(false); len(ts) != 0 {
		t.Errorf("%d Tickets were made", len(ts))
	}
}

// The join token appears once: no command_results row holds it, and the same command sent again mints a fresh one.
func TestNodeTokenResultNotStored(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	w := startWebWith(t, newStateDir(t), nil)
	mk := func() *athenav1.Command {
		return &athenav1.Command{Action: &athenav1.Command_CreateNodeToken{CreateNodeToken: &athenav1.CreateNodeToken{}}}
	}
	a := doCommand(w, true, "tok", mk()).GetNodeToken().GetCommand()
	if _, ok, err := w.d.st.CommandResult("phone", "tok"); err != nil || ok {
		t.Fatalf("a result was stored: ok=%v err=%v", ok, err)
	}
	b := doCommand(w, true, "tok", mk()).GetNodeToken().GetCommand()
	if a == "" || a == b {
		t.Fatalf("a resend replayed the token: %q %q", a, b)
	}
}

// A Ticket pinned to a node that is offline shows that node, and why it waits.
func TestPinnedUnplacedSummaryShowsNode(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	w := startWebWith(t, newStateDir(t), nil)
	rn := joinRemote(t, w, "mini") // never started: offline
	r := submitOn(w, "s1", rn.id)
	tk, _, err := w.d.eng.Ticket(r.GetTicketId())
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "it waits for mini", func() bool {
		id, name, why := (hubSource{d: w.d}).Placement(&tk)
		return id == rn.id && name == "mini" && why == "Waiting for mini"
	})
}
