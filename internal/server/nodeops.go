package server

import (
	"errors"
	"time"

	"github.com/danielpina1/athena/internal/fleet"
	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/internal/web"
)

// The owner's node commands over the control socket (athena node token|list|drain|undrain|revoke).

// NodeTokenResult answers node-token: the dashboard's URL a node joins at, the single-use join token, the dashboard
// CA's fingerprint ("" with ACME, whose certificate the system roots verify) and when the token expires. The token
// is only in this answer: it is never logged.
type NodeTokenResult struct {
	URL         string    `json:"url"`
	Token       string    `json:"token"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Expires     time.Time `json:"expires"`
}

// NodeRow is one node of `athena node list`.
type NodeRow struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Local bool   `json:"local,omitempty"`
	// State is fleet.StateOnline, StateOffline, StateDraining or StateRevoked.
	State      string `json:"state"`
	Slots      int    `json:"slots"`
	SlotsInUse int    `json:"slotsInUse"`
	// Runtimes are the node's runtimes, by name; NotReady those of them that are not ready.
	Runtimes []string  `json:"runtimes,omitempty"`
	NotReady []string  `json:"notReady,omitempty"`
	LastSeen time.Time `json:"lastSeen"`
}

var errNoFleet = errors.New("nodes are not available")

// nodeToken issues a node join token good for ttl (0: fleet.JoinTokenTTL, at most fleet.MaxJoinTokenTTL). Nodes join
// on the dashboard's endpoint and link on its node listener, so both must serve, which needs TLS.
func (d *Dispatcher) nodeToken(ttl time.Duration) (NodeTokenResult, error) {
	switch {
	case ttl < 0:
		return NodeTokenResult{}, userErrorf("A join token's time must be above 0.")
	case ttl == 0:
		ttl = fleet.JoinTokenTTL
	case ttl > fleet.MaxJoinTokenTTL:
		return NodeTokenResult{}, userErrorf("A join token is good for at most 24h; ask for a shorter time.")
	}
	if d.web == nil {
		return NodeTokenResult{}, userErrorf("Nodes join through the dashboard, which this server does not serve: run athena web setup, then restart athena server.")
	}
	if d.web.nodeL == nil {
		return NodeTokenResult{}, userErrorf("Nodes need the dashboard on TLS: run athena web setup with --tls ca or --tls acme, then restart athena server.")
	}
	fp := ""
	if d.web.tls.Mode() == "ca" {
		ca := d.web.tls.CA()
		if ca == nil {
			return NodeTokenResult{}, errors.New("the dashboard has no CA in mode ca")
		}
		fp = ca.Fingerprint()
	}
	expires := d.st.Now().Add(ttl)
	token, err := web.IssueEnrollToken(d.st, store.EnrollNode, ttl)
	if err != nil {
		return NodeTokenResult{}, err
	}
	return NodeTokenResult{URL: d.web.origin, Token: token, Fingerprint: fp, Expires: expires}, nil
}

// nodeRows lists the nodes: the server's own first, then the enrolled ones by name, revoked ones included.
func (d *Dispatcher) nodeRows() ([]NodeRow, error) {
	if d.fleet == nil {
		return nil, errNoFleet
	}
	views := d.fleet.Nodes()
	out := make([]NodeRow, 0, len(views))
	for _, v := range views {
		r := NodeRow{ID: v.ID, Name: v.Name, Local: v.Local, State: v.State, LastSeen: v.LastSeen,
			Slots: v.Status.Slots, SlotsInUse: v.Status.SlotsInUse}
		for _, rt := range v.Status.Runtimes {
			r.Runtimes = append(r.Runtimes, rt.Name)
			if !rt.Ready {
				r.NotReady = append(r.NotReady, rt.Name)
			}
		}
		if len(v.Status.Runtimes) == 0 && v.Caps != nil {
			// No status yet (an offline node since the server started): what its last Hello said.
			for _, rt := range v.Caps.GetRuntimes() {
				r.Runtimes = append(r.Runtimes, rt.GetName())
				if !rt.GetReady() {
					r.NotReady = append(r.NotReady, rt.GetName())
				}
			}
		}
		if r.Slots == 0 && v.Caps != nil {
			r.Slots = int(v.Caps.GetSlots())
		}
		out = append(out, r)
	}
	return out, nil
}

// findNode is the node named by its id or its name.
func (d *Dispatcher) findNode(arg string) (fleet.NodeView, error) {
	if d.fleet == nil {
		return fleet.NodeView{}, errNoFleet
	}
	if arg == "" {
		return fleet.NodeView{}, userErrorf("Name a node: its name or its id (athena node list lists them).")
	}
	views := d.fleet.Nodes()
	for _, v := range views {
		if v.ID == arg {
			return v, nil
		}
	}
	for _, v := range views {
		if v.Name == arg {
			return v, nil
		}
	}
	return fleet.NodeView{}, userErrorf("No node is named %s; athena node list lists them.", arg)
}

// drainNode marks a node as taking no new Assignments (on), or as taking them again; its running work goes on.
func (d *Dispatcher) drainNode(arg string, on bool) (NodeRow, error) {
	v, err := d.findNode(arg)
	if err != nil {
		return NodeRow{}, err
	}
	if v.State == fleet.StateRevoked {
		return NodeRow{}, userErrorf("%s is revoked; it must join again.", v.Name)
	}
	if err := d.fleet.SetDraining(v.ID, on); err != nil {
		return NodeRow{}, err
	}
	return NodeRow{ID: v.ID, Name: v.Name, Local: v.Local}, nil
}

// revokeNode revokes a node's certificate and closes its link: its sessions stop, and it must join again.
func (d *Dispatcher) revokeNode(arg string) (NodeRow, error) {
	v, err := d.findNode(arg)
	if err != nil {
		return NodeRow{}, err
	}
	if v.State == fleet.StateRevoked {
		return NodeRow{}, userErrorf("%s is already revoked.", v.Name)
	}
	if err := d.fleet.Revoke(v.ID); err != nil {
		return NodeRow{}, err
	}
	return NodeRow{ID: v.ID, Name: v.Name, Local: v.Local, State: fleet.StateRevoked}, nil
}

// JoinCommand is the line a new machine runs to join: what `athena node token` prints and the dashboard shows.
// fingerprint is the dashboard CA's; with ACME ("") the system roots verify the server and the flag is left out.
func JoinCommand(url, token, fingerprint string) string {
	line := "athena node join " + url + " " + token
	if fingerprint != "" {
		line += " --ca-sha256 " + fingerprint
	}
	return line
}

// pinnable checks that a task may be pinned to the node with this id: it is known and not revoked.
func (d *Dispatcher) pinnable(id string) error {
	if d.fleet == nil {
		if id == fleet.LocalID {
			return nil
		}
		return userErrorf("No node has the id %s.", id)
	}
	for _, v := range d.fleet.Nodes() {
		if v.ID != id {
			continue
		}
		if v.State == fleet.StateRevoked {
			return userErrorf("%s is revoked; it must join again before it takes a task.", v.Name)
		}
		return nil
	}
	return userErrorf("No node has the id %s; pick one from the node list.", id)
}
