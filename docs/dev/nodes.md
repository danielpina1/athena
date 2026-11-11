# Nodes: more machines for the same server

A node is a machine that runs Athena's sessions for your server. The server always has one node, its own, named after
its host ("this machine"). You can add more: a second Mac, a Linux box under the desk. Each one runs `athena node run`,
links to the server, and takes Tickets the server places on it. You still talk only to the server: its CLI and its
dashboard show every node's Tickets.

What stays where:

- **The server** keeps the database, the Policy, the GitHub App key, the router and the dashboard.
- **A node** keeps its own key and certificate, its own `claude` and `codex` logins, its worktrees and its sessions.
  It gets a short-lived GitHub token per Assignment, held in memory, and never the App key.

## 1. Before you start

- The server runs the dashboard with TLS (`athena web setup`, mode `ca` or `acme`; see [Dashboard setup](dashboard.md)).
  Nodes join through the dashboard and link on a port of their own; with `--tls off` the server serves no nodes.
- The node reaches the server over your private network. With the UniFi WireGuard VPN, the node connects to the VPN
  like your phone does and resolves the dashboard's host name (for example `athena.home.arpa`) to the server's VPN
  address.
- The node needs **two ports** on the server: the dashboard's (`:8443` by default) for the join, and the node port,
  which is the dashboard's plus one (`:8444`). Nodes link on it with their certificate, so browsers on the dashboard
  port are never asked for one. Set another with `web.nodeListen` in the server's `~/.athena/config.json`, for example
  `"nodeListen": "10.8.0.1:8444"`. If a firewall sits between them, open both ports on the VPN side only.
- `athena` is installed on the node (the same version as the server), with `git`, `claude` and, if you use it, `codex`.

## 2. Enroll a node

On the dashboard, open **Nodes › Add a node** (it asks for your passkey again), or on the server's machine:

    athena node token

Either gives one line, ready to paste on the new machine:

    athena node join https://athena.home.arpa:8443 <token> --ca-sha256 3A:F1:...:9C

- The token is good once, for an hour (`--ttl 30m`, up to `24h`). It is shown only there (the dashboard's sheet
  forgets it when it closes) and never logged.
- `--ca-sha256` is the dashboard CA's fingerprint. The node checks the server's certificate against it before it
  sends anything. In mode `acme` the flag is left out: the system's roots verify the server.

On the new machine, paste the line. Add `--name` to pick its name; the default is the host name, in lower case.

    athena node join https://athena.home.arpa:8443 <token> --ca-sha256 3A:F1:...:9C --name studio

- The node makes its key here and sends only a certificate request. The key stays in `~/.athena-node/node.key`
  (mode 0600).
- If the fingerprint does not match, the join stops with "The server's certificate does not match --ca-sha256; check
  the command you copied." and nothing is written.
- `--state DIR` puts the node's state somewhere other than `~/.athena-node`.

Then run it, in a terminal or under launchd or systemd:

    athena node run

Its flags are those of `athena server` that concern sessions: `--slots N` (default 4), `--model M`, `--blocked a,b`,
`--approval-timeout D`, `--claude PATH`, `--codex PATH`.

- It waits for the server, fetches the prompts, and only then serves sessions.
- Its socket, its logs (`logs/athena.log`) and its worktrees (`work/`) live under its state directory. One
  `athena node run` uses a state directory at a time; a second is refused.
- An optional `config.json` there sets the node's own `runtimes` and `sandbox`, in the server's format. Its `github`
  and `web` parts belong to the server and are ignored.
- If the server is unreachable, the node keeps trying. What its sessions do meanwhile is kept and sent once the link
  is back, in order and once.

Check it from the server:

    athena node list

It shows each node's name, id, state (online, offline, draining or revoked), slots in use, runtimes and when it was
last seen.

## 3. Logins stay on each node

Every node runs `claude` and `codex` with its own logins, on its own subscription or keys. Log in on the node itself:

- `claude`: run `claude` once as the user that runs `athena node run`, and log in.
- `codex`: `athena codex login --state-dir ~/.athena-node`. Athena keeps the node's Codex login in its state
  directory, not in `~/.codex`. `athena codex status --state-dir ~/.athena-node` checks it.

A node whose runtime is not logged in shows it as not ready, and Tickets that need that runtime go elsewhere.

## 4. Drain, undrain, revoke

On the server's machine, name a node by its name or its id:

    athena node drain studio      # takes no new Assignments; running work finishes there
    athena node undrain studio    # takes new work again
    athena node revoke studio     # asks "Revoke studio? Its sessions stop and it must join again. [y/N]"

- Drain before you take a machine down for a while.
- Revoke when a machine is lost or retired: its certificate no longer counts and its link closes at once. `--yes`
  skips the question. The revoked machine's `athena node run` stops with "This node was revoked; run athena node join
  again."
- To bring a revoked machine back, make a new token and join again, with a fresh `--state` directory (or remove the
  old one). The old name stays with the revoked node, so pick a new `--name`.
- The server's own node can be drained, but never revoked.

## 5. When a node goes away

When a node drops off the VPN, the server notices within about 10 seconds. After two minutes its Tickets show
"<name> is offline". If the node comes back, its sessions carry on where they were.

If it will not come back soon, **Retry elsewhere** (the dashboard's action on the Ticket, or
`athena retry-elsewhere <ticket>`) starts the Ticket again on another node:

- **Work in a GitHub repository moves.** At the end of every turn, a node sends a checkpoint of the Ticket's worktree
  to the server. The new node starts from the latest checkpoint the server has, and gets the checkpoint's whole tree
  as uncommitted changes on the Ticket's branch: new files untracked, deleted files deleted, nothing staged. The
  session is told it moved, and from which node. Work done after the last checkpoint (the turn that was running) is
  not there.
- **If the Ticket later moves back** to a node that still has its old worktree, that node first saves its own older
  state on a side ref, `refs/athena/<ticket>/superseded-<time>`, and then takes the newer checkpoint. Nothing is
  lost; the newest work wins. Those refs go when the Ticket ends.
- **Work in a folder stays on its machine.** A Ticket started with `--dir` works in a folder only that node has, so it
  cannot move. Retry elsewhere refuses it with "This Ticket works in a folder on <name>; it continues when <name> is
  back."

## 6. Protocol versions

A node says its protocol in `NodeHello.protocol`, and the server refuses a node whose number differs from its own. The
number is `Protocol` in `internal/nodelink/client.go`.

The bump rule, copied from the comment there:

> Any change to an Args struct (proxies.go, serve.go) or to the fields of a call bumps Protocol. Both ends decode args
> strictly, and a fact the server cannot decode is refused as a UserError, which still advances the applied-fact mark:
> without the bump a node and a server of different versions would drop facts silently instead of refusing the link.

- Plan 6 shipped Protocol 1.
- Plan 7 made it **2**, once, for every Args change in that plan: the agent calls, child Tickets, the fork seed and
  `importBranch`. Update the node and the server together, or the node is refused.
- A dashboard client's `Hello.protocol` is a different number and stays 1, since only fields were added to it.
