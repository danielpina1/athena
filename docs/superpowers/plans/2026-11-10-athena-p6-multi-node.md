# Athena Plan 6: Multi-node Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A second machine runs Tickets. The owner enrolls it with a one-time join token, and it connects to the control server over mutual TLS. The server places each Assignment on a suitable node, and a node that drops keeps working and catches up when it returns. A Ticket stranded on an offline node can be retried elsewhere from its last checkpoint.

**Architecture:**
- **The remote node is the same `node.Node`.**
  - `athena node run` builds a `node.Node` exactly as the server builds its embedded one.
  - Its `EngineCallbacks`, `Credentials` and `Policy` are proxies over the link (`internal/nodelink`), not the Engine.
  - Hooks, MCP, runtimes, worktrees, sandboxing and transcripts stay node-local and unchanged.
- **The server gets a fleet.**
  - `internal/fleet` implements `core.Executor`. It holds the embedded local node and one `Remote` per connected node, and places each pending Assignment by the §7.2 rules.
  - Every per-Ticket call goes to the node that owns the Ticket: Deliver, Stop, Interrupt, Subscribe, diff, files and ApplyProposal.
  - The Engine stays pure. It learns the placement through one new command, `AssignmentPlaced`.
- **One RPC envelope on the existing link.**
  - Node ↔ server traffic is `internal/link` frames carrying `NodeCall`/`NodeResult`.
  - A call's arguments are the core types' existing JSON, decoded strictly per method.
  - Calls are of two kinds:
    - **Facts** (lifecycle callbacks) are durable, buffered in the node's SQLite outbox while offline, applied in order and at most once by a per-node high-water mark.
    - **Requests** (worker tools, Approvals, tokens, queries) are synchronous and fail fast with a retryable message while offline.
- **Recovery by reconcile, not replay.** Commands from server to node are not buffered. When a node connects, the fleet sends it the current Start/Stop/Deliver set for every Assignment placed on it. `node.Node.Start` is already idempotent for a queued or running Assignment.
- **Checkpoints travel as git bundles.** At every turn end the node bundles its checkpoint ref and sends it on `BLOB`. The server keeps them in a bare mirror per repository. *Retry elsewhere* seeds the target node's worktree from the mirror.

**Tech Stack:**
- **Already in the repo:** Go 1.27, `github.com/coder/websocket`, `modernc.org/sqlite`, protobuf via `buf` and protobuf-es, React 19 with Vite, Vitest and Playwright.
- **New:** none. No new Go modules and no new npm packages. The CSR, the client certificates and the mTLS listener use `crypto/x509` and `crypto/tls`. The bundles use the installed `git` (`git bundle`).

**Spec:** `docs/superpowers/specs/2026-11-03-athena-design.md`. Read:
- **Binding:** §7 (all), §8.1–8.2, and §11 "Device identity" and "GitHub access for workers on any node".
- **For context:** §2 (footprint and latency targets), §6 (Assignments, worktrees, hooks), §10 (Nodes view, New task, Needs you) and §10.3 (bench budgets).

Also read:
- `docs/superpowers/plans/2026-11-04-athena-p3-deferred.md`: "Carry into plan 6".
- `docs/superpowers/plans/2026-11-05-athena-p5-codex-runtime.md`: "Deferred to later plans", Plan 6.
- `docs/dev/dashboard.md`: the client contract.
- `internal/node/node.go:67-176`: `Config`, `EngineCallbacks`, and the concurrency contract.

## Global Constraints

- Every plan 1–5, 10 and 11 Global Constraint still holds:
  - the CSP, the Precision tokens and the client link and storage rules
  - the bundle budget: 200 KB JS / 30 KB CSS gzipped for the initial route, plus the lazy-chunk sentinels in `web/scripts/bundle-budget.mjs`
- `make check` passes at the end of every task. Every task that changes the UI also runs `make e2e`, with screenshots at 390, 820 and 1440 px, light and dark, and axe clean.
- **No new Go modules and no new npm packages.**
- **Secrets never cross the link** (§11):
  - The node's private key is made on the node and never leaves it. Enrollment sends a CSR.
  - The GitHub App key stays on the server. A node gets only per-Assignment installation tokens, held in memory: the existing `tokenStore`.
  - No token, key or join token appears in a log line, a URL query or a dashboard view. A join token appears once, in the enroll sheet and in the CLI output.
- **Footprint and latency (§2, §10.3):**
  - `athena node run` idles below 30 MB RSS and ~0% CPU.
  - Dispatch latency p99 is below 100 ms to a remote node on loopback, excluding `git worktree add`.
  - A dropped node is detected in under 10 s: node links use `Heartbeat = 3 s` and `DeadAfter = 9 s`. Dashboard links keep 15 s and 45 s.
- **Timing values** (named constants in `internal/fleet`):
  - `OfflineAfter = 2 * time.Minute` before a node's Tickets show the *node offline* person stage
  - join token TTL default 1 h, at most 24 h
  - node certificates valid 397 days (the existing `ClientCert` life)
  - request calls time out after 30 s
- **Protocol:**
  - The node endpoint is its own listener, `web.nodeListen`. It defaults to the dashboard's host with port + 1 (`:8444` for the default `:8443`).
  - The listener uses `tls.RequireAndVerifyClientCert` against the **node CA**, so browsers on the dashboard port are never asked for a certificate.
  - `NodeHello.protocol = 1`. A mismatch closes the link with `CloseProtocol` and the reason `node protocol N, server protocol M; update the node`.
- **Copy:**
  - A node is "node" in the UI, with its name.
  - The embedded node is named after the server's hostname, unless `--node-name` is given, and is marked "this machine".
  - The person stage reads "<name> is offline".
  - The retry action is "Retry elsewhere".
- **Tests never use the owner's machine state.**
  - Two-node tests run two state directories under `t.TempDir()`, on loopback, with `fakeclaude`/`fakecodex`.
  - The e2e harness builds to `bin/e2e/` and starts its own second node process.

## Review Focus

1. **Server restarts while a remote node works.** Its sessions keep running, and the Tickets must not get a "server restart" Interruption or a second launch. They reconcile when the node reconnects (Task 8 test `TestRecoveryLeavesRemoteAssignments`).
2. **A short network drop mid-turn** (20 s). No Report, TurnEnded or InputsRead is lost or applied twice, and they apply in the order the node made them (Task 6 `TestFactsReplayOnceInOrder`, Task 7 `TestFactsHighWaterSurvivesServerRestart`).
3. **A node is revoked while connected, or its certificate is replayed.** The live link closes at once, a reconnect is refused, and its Tickets show it offline (Task 5 `TestRevokedNodeRefused`, Task 7 `TestRevokeClosesLiveLink`).
4. **A join token is used twice or late, or the CA fingerprint is wrong.** The join is refused with a sentence that says which. No node row and no certificate are made (Task 5 `TestJoinRefusals`, Task 11 `TestJoinChecksFingerprint`).
5. **A Ticket is pinned to a node that is offline or draining.** It waits, its stage says "Waiting for <name>", and it is never placed elsewhere silently (Task 8 `TestPinnedTicketWaits`).

---

### Task 1: Link: client dial, the BLOB channel and the plan 3 carry-overs

**Files:**
- Modify: `internal/link/ws.go`, `internal/link/session.go`
- Test: `internal/link/session_test.go`, `internal/link/ws_test.go` (new)

**Interfaces:**
- Produces:
  - `func Dial(ctx context.Context, url string, tlsCfg *tls.Config) (Conn, error)`: the `athena.v1` subprotocol, permessage-deflate as `Accept` uses, read limit `MaxFrame+1`.
  - `Session.Send(BLOB, true, f)` is accepted. BLOB's default queue is 16 frames, and BLOB stays last in the writer order.
  - `Options.MaxUnackedBytes int`, default 64 MiB. Above it, `Send` returns `ErrBackpressure`, together with the existing 1024-frame cap.
  - `New` sets `initErr = ErrProtocol` when `Resume.PeerLastSeq` exceeds the highest seq in `Resume.Unacked` (or `Received`, when nothing is unacked).

- [ ] **Step 1: Write the failing tests.**
  - `TestDialAcceptRoundTrip`: an `httptest.NewTLSServer` running `Accept`, then `Dial` with its client config. A durable CONTROL frame and a durable BLOB frame both arrive, in order.
  - `TestBlobDoesNotStarveControl`: fill BLOB to its queue, then send one CONTROL frame. CONTROL is written first.
  - `TestUnackedBytesCap`: 70 frames of 1 MiB with `MaxUnackedBytes` at 64 MiB. The 65th `Send` returns `ErrBackpressure`.
  - `TestResumeBeyondSentIsProtocolError`.
  - `TestGapKeepsReceivedItems`: the node-side carry-over. Frames received before a gap are delivered to `OnFrame` before the session ends with `ErrProtocol`.
- [ ] **Step 2:** Run `go test ./internal/link -run 'Dial|Blob|UnackedBytes|ResumeBeyond|GapKeeps' -race`. Expected: FAIL.
- [ ] **Step 3:** Implement the changes above. `Dial` uses `websocket.Dial` with an `http.Client` whose transport carries `tlsCfg`. Also fix the orphaned `After` timers by stopping them in `Run`'s exit path. The Credit uint32 wrap stays, as it is per connection and nodes reconnect long before 4G items.
- [ ] **Step 4:** Run `go test ./internal/link -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(link): dial, the blob channel and a byte cap on unacked frames`.

### Task 2: Protocol for nodes

**Files:**
- Create: `proto/athena/v1/node.proto`
- Modify:
  - `proto/athena/v1/link.proto`: Frame body oneof
  - `proto/athena/v1/state.proto`: `NodeStatus`, `Snapshot`, `Delta`, `TicketSummary`
  - `proto/athena/v1/commands.proto`
- Generated: `internal/pb/athenav1/*`, `web/src/gen/athena/v1/*` (via `make proto`)
- Test: `internal/link/frame_test.go`

**Interfaces:**
- Produces, in `node.proto`:
  - `NodeHello{uint32 protocol; string node_id; string version; uint64 last_seq; Capabilities caps}`. `last_seq` is the last server frame the node received.
  - `NodeWelcome{string epoch; uint64 applied_fact_seq; uint32 heartbeat_ms; string node_name}`.
  - `Capabilities{string os; string arch; repeated RuntimeCap runtimes; repeated RepoCap repos; uint32 slots; repeated string dirs}`
    - `RuntimeCap{name, version, ready, why}`
    - `RepoCap{repo, path}`
    - `dirs` are the local repo roots the node will run Dir Tickets in.
  - `NodeCall{uint64 id; string method; bytes args; uint64 fact_seq}`. `fact_seq` is 0 for a request and is ≥ 1 for a fact, numbered by the node's outbox.
  - `NodeResult{uint64 id; bytes value; string error; bool user_error}`
  - `BlobChunk{string transfer_id; string kind; string ticket_id; bytes data; bool last}`, where `kind` is `checkpoint` or `seed`
  - `NodeStatusReport{NodeStatus status}`, which the node sends on change, deduplicated like the hub
- Frame oneof: `node_hello = 25`, `node_welcome = 26`, `node_call = 27`, `node_result = 28`, `blob_chunk = 29`, `node_status_report = 30`.
- `state.proto`:
  - `NodeStatus` gains `id`, `state` (`online|offline|draining|revoked`), `last_seen`, `os`, `arch`, `local bool` and `version`.
  - `Snapshot` gains `repeated NodeStatus nodes`, and the single `node` field is reserved.
  - `Delta` keeps a `NodeStatus node` change (an upsert keyed by id) and adds `string node_removed`.
  - `TicketSummary` gains `node_id`, `node_name` and `waiting_for_node`.
- `commands.proto`:
  - `SubmitTask.node = 7`, a node id; empty means any
  - Command oneof: `RetryElsewhere retry_elsewhere = 37`, `CreateNodeToken create_node_token = 38`, `SetNodeDraining set_node_draining = 39`, `RevokeNode revoke_node = 40`
  - `NodeToken{string command; time expires_at}` as a `CommandResult` value

- [ ] **Step 1:** Write `TestNodeFramesRoundTrip` in `frame_test.go`. Each new body encodes and decodes. A `NodeHello` with an unknown field is `ErrInvalid`.
- [ ] **Step 2:** Run it. Expected: FAIL to compile.
- [ ] **Step 3:** Write the protos. Run `make proto`. Update `hub.nodeStatus` and the web store compile errors minimally: one node becomes a one-element list. The real view work is Task 12.
- [ ] **Step 4:** Run `make check`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(proto): node link messages and a list of nodes`.

### Task 3: Core: placement, pins, offline stage and Retry elsewhere

**Files:**
- Modify:
  - `internal/core/model.go`: `Assignment.Node`, `Assignment.NodeOffline *time.Time`, `TaskOptions.Node`, reason `ReasonNodeOffline = "node offline"`
  - `internal/core/engine.go`: commands below
  - `internal/core/stage.go`: action `offline` before `delivery`
- Test: `internal/core/nodes_test.go` (new)

**Interfaces:**
- Produces, on `*core.Engine`:
  - `AssignmentPlaced(ticketID, assignmentID, nodeID string) error` records `Assignment.Node`. An ended Assignment is a no-op.
  - `NodeOffline(nodeID string, since time.Time) error` sets `NodeOffline` on every running or pending Assignment placed on the node, in one Tx, and emits `EventNodeOffline` per Ticket.
  - `NodeBack(nodeID string) error` clears the flag and emits `EventNodeBack`.
  - `RetryElsewhere(ticketID string) error`:
    - It is allowed only when the latest Assignment has `NodeOffline` set (otherwise a `UserError`: "this Ticket's node is not offline").
    - It ends that Assignment with `ReasonNodeOffline` and queues a new pending Assignment with `Avoid: []string{node}`.
  - `Assignment.Avoid []string` lists the nodes the fleet must not place this Assignment on.
- `SubmitOwnerTask` stores `TaskOptions.Node` in `Execution.Node` as the pin. A Ticket from a Source has no pin.
- `TicketStage`: `Stage{"person","offline"}` when the latest Assignment has `NodeOffline`. `CanRetry` stays as it is. Retry elsewhere is its own action.

- [ ] **Step 1: Write the failing tests.**
  - `TestAssignmentPlacedRecordsNode`.
  - `TestNodeOfflineStageAndBack`: the stage is `person/offline`, then `working` after `NodeBack`.
  - `TestRetryElsewhereEndsAndAvoids`: the new pending Assignment's `Avoid` is `["n2"]`, and the old one ended with `node offline`.
  - `TestRetryElsewhereRefusedWhenOnline`.
  - `TestPinStoredOnSubmit`.
- [ ] **Step 2:** Run `go test ./internal/core -run 'Placed|NodeOffline|RetryElsewhere|Pin' -race`. Expected: FAIL.
- [ ] **Step 3:** Implement. Every command follows the Engine's existing pattern: one Tx, events after commit, and Executor calls with the committed snapshot.
- [ ] **Step 4:** Run `go test ./internal/core -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(core): node placement, the offline stage and Retry elsewhere`.

### Task 4: Store: nodes, join tokens and the fact high-water mark

**Files:**
- Modify:
  - `internal/store/store.go`: migration 11
  - `internal/store/web.go`: token kind
- Create: `internal/store/nodes.go`
- Test: `internal/store/nodes_test.go`

**Interfaces:**
- Migration 11:
  - `nodes(id TEXT PK, name TEXT UNIQUE, cert_serial TEXT, created_at, last_seen, draining INT DEFAULT 0, revoked_at TEXT DEFAULT '', applied_fact_seq INT DEFAULT 0, caps BLOB)`
  - `ALTER TABLE enroll_tokens ADD COLUMN kind TEXT NOT NULL DEFAULT 'passkey'`
- Produces:
  - `type Node struct{ ID, Name, CertSerial string; CreatedAt, LastSeen time.Time; Draining bool; RevokedAt *time.Time; AppliedFactSeq uint64; Caps []byte }`
  - `(*Store).PutNode(n Node) error`, `GetNode(id string) (*Node, error)`, `Nodes() ([]Node, error)`
  - `SetNodeDraining(id string, on bool) error`, `RevokeNode(id string, at time.Time) error`
  - `TouchNode(id string, at time.Time) error`
  - `AdvanceFactSeq(id string, seq uint64) (bool, error)`: true when the mark moved. A seq ≤ the current mark is false.
  - `PutEnrollToken(hash []byte, kind string, expiresAt time.Time) error`
  - `UseEnrollToken(hash []byte, kind string, now time.Time) (bool, error)`
  - Passkey callers pass `"passkey"`, and nodes use `"node"`. A node token is never accepted by passkey enrollment, and the reverse.
- Also fix the plan 3 carry-over: `IssueEnrollToken` takes the store's injected clock.

- [ ] **Step 1: Write the failing tests.**
  - `TestNodeRoundTrip`.
  - `TestAdvanceFactSeqMonotonic`: 3 then 5 are true, then 5 and 4 are false.
  - `TestEnrollTokenKindsDoNotCross`.
  - `TestRevokedNodeKeepsRow`.
- [ ] **Step 2:** Run `go test ./internal/store -run 'Node|FactSeq|TokenKinds' -race`. Expected: FAIL.
- [ ] **Step 3:** Implement. Update the passkey call sites in `internal/web/auth.go` and `enroll_link.go`.
- [ ] **Step 4:** Run `go test ./internal/store ./internal/web -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(store): nodes and join tokens by kind`.

### Task 5: Node CA, join endpoint and the mTLS node listener

**Files:**
- Modify: `internal/pki/ca.go`, `internal/server/config.go` (`WebConfig.NodeListen`), `internal/server/wiring.go`
- Create: `internal/web/nodes.go` (join handler and node listener)
- Test: `internal/pki/ca_test.go`, `internal/web/nodes_test.go`

**Interfaces:**
- Produces:
  - `pki.NodeCAHost = "nodes.athena"`.
  - `func NodeCADir(stateDir string) string`, which returns `<stateDir>/pki/nodes`. The node CA is made with `LoadOrCreateCA(NodeCADir, NodeCAHost, nil)`, separate from the dashboard CA and present in every TLS mode, ACME included.
  - `(*CA).SignClientCSR(csrDER []byte, name string) (certDER []byte, serial string, err error)`:
    - It checks the CSR's signature and a P-256 key.
    - The subject CN and the DNS SAN are `name`, which must be `<node-id>.nodes.athena`.
    - It sets EKU clientAuth, uses the `ClientCert` life, and copies nothing else from the CSR.
  - `web.NodeJoin(st *store.Store, ca *pki.CA, now func() time.Time) http.Handler` serves `POST /node/join` on the **dashboard** listener, so a node with no certificate can reach it.
    - Body: JSON `{token, name, csr}` (base64 DER).
    - Answer: `{nodeId, name, cert, caCert, nodeURL}`.
    - Refusals are 400 or 403 with one of these exact sentences:
      - "This join token was already used; create a new one."
      - "This join token has expired; create a new one."
      - "This join token is not valid."
      - "A node named <name> already exists."
  - `web.NodeListener(cfg NodeListenerConfig) (*http.Server, error)` serves `GET /node` (the link) with `RequireAndVerifyClientCert`, `ClientCAs` set to the node CA, and the dashboard's server certificate.
    - `VerifyConnection` looks the node id from the cert CN up in `nodes`. A missing node, a revoked node or a serial mismatch is refused at the handshake.
  - `type NodeLinkHandler interface{ ServeNode(nodeID string, c link.Conn) }`. Task 7's fleet implements it.
- Node names: 1–32 characters of `[a-z0-9-]`, starting with a letter. The id is `n` followed by 10 random hex characters.

- [ ] **Step 1: Write the failing tests.**
  - `TestSignClientCSR`: the result verifies against the node CA with `ExtKeyUsageClientAuth`, and a CSR with an RSA key is refused.
  - `TestJoinIssuesCertOnce`.
  - `TestJoinRefusals`: four cases, each with its exact sentence, and none of them creates a row.
  - `TestRevokedNodeRefused`: the handshake fails after `RevokeNode`.
  - `TestDashboardPortAsksNoClientCert`: a `tls.Dial` to the dashboard listener sees no `CertificateRequest`.
- [ ] **Step 2:** Run `go test ./internal/pki ./internal/web -run 'CSR|Join|Revoked|AsksNoClientCert' -race`. Expected: FAIL.
- [ ] **Step 3:** Implement, and wire both into `wiring.go`. `NodeListen` follows the rule in Global Constraints and is refused when it equals `listen`.
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(web): enroll nodes and serve them over mutual TLS`.

### Task 6: `internal/nodelink`: the node's side of the link

**Files:**
- Create:
  - `internal/nodelink/identity.go`: `node.json`, the key and the certs
  - `internal/nodelink/outbox.go`: SQLite, `<nodeState>/node.db`
  - `internal/nodelink/client.go`: dial loop, Hello, calls
  - `internal/nodelink/proxies.go`: `EngineCallbacks`, `Credentials`, `Policy`
  - `internal/nodelink/serve.go`: server→node calls onto a `*node.Node`
- Test: `internal/nodelink/*_test.go`

**Interfaces:**
- Consumes: Task 1 `link.Dial`, `link.New` with `Resume`; Task 2 messages; `node.EngineCallbacks`, `node.Credentials`, `node.Policy`.
- Produces:
  - `type Identity struct{ NodeID, Name, NodeURL, ServerFingerprint string; Cert tls.Certificate; CAPool *x509.CertPool }`
  - `LoadIdentity(dir string) (*Identity, error)`
  - `Join(ctx context.Context, url, token, name, fingerprint, dir string) (*Identity, error)`
    - It makes the key (0600), the CSR and the POST.
    - `fingerprint` is the dashboard CA's SHA-256. Empty means the system roots, for ACME.
  - `type Client struct`, made by `New(id *Identity, n *node.Node, opts Options) *Client`:
    - `Run(ctx) error` reconnects with backoff from 1 s to 30 s, with jitter.
    - `Engine() node.EngineCallbacks`, `Credentials() node.Credentials`, `Policy() node.Policy`.
    - `Online() bool`.
  - **Facts** are methods that append to the outbox and return nil once the row is committed:
    - `AssignmentStarted`, `InputsDelivered`, `DeliveryFailed`, `InputsRead`, `TurnEnded`
    - `AssignmentInterrupted`, `AssignmentPaused`, `UsageLimited`
    - `RecordUsage`, `AllowedByRule`, `ExpireApproval`, `SettleProposal`
  - **Requests** are every other `EngineCallbacks` method, plus `Token`, `Revoke`, `Identity`, the four `Policy` methods and `Prompts`.
    - Each is a `NodeCall` with `fact_seq = 0`, a 30 s timeout, and `ErrOffline` at once when not connected.
    - `ErrOffline` reads "the control server is unreachable; wait a minute and try again".
  - **Offline answers.**
    - `Policy` fails closed while offline: `ToolPolicy` is restricted, `Allows` is false, `Shadow` is true and `Trust` is `stranger`.
    - `Prompts` returns the last cached prompts. It has none before the first connect, so `node run` waits for the first Welcome before it serves.
    - `Ticket(id)` while offline returns the last Ticket snapshot the server sent with a Start, Deliver or Stop (cached per Ticket). A Ticket never seen is `ErrOffline`.
  - Each method's args are the existing JSON of its Go parameters, as one exported struct per method in `proxies.go`, named `Args<Method>` (for example `ArgsFileReport{TicketID string; Report core.ReportInput}`). The fleet decodes them with `DisallowUnknownFields`.
  - **Outbox:** `outbox(seq INTEGER PK AUTOINCREMENT, method TEXT, args BLOB, made_at TEXT)`. Rows ≤ `NodeWelcome.applied_fact_seq` are deleted. Rows above it are resent in seq order before any new call.

- [ ] **Step 1: Write the failing tests**, against an in-process fake server built on `link.New` over `net.Pipe`-backed Conns.
  - `TestFactsReplayOnceInOrder`:
    - Record three facts while offline, then connect with `applied_fact_seq = 1`.
    - The server receives facts 2 and 3, in order, exactly once.
    - Reconnect again with 3, and nothing is resent.
  - `TestRequestOfflineFailsFast`: under 50 ms, with `ErrOffline`'s text.
  - `TestPolicyFailsClosedOffline`.
  - `TestTicketFromCacheOffline`.
  - `TestJoinWritesKey0600AndNeverSendsKey`: the POST body holds no `PRIVATE KEY`.
  - `TestJoinChecksFingerprint` is in Task 11, with the CLI.
- [ ] **Step 2:** Run `go test ./internal/nodelink -race`. Expected: FAIL.
- [ ] **Step 3:** Implement.
  - The fact methods never block on the network. A runner keeps working offline, which is §7.3.
  - `serve.go` maps the server's calls onto `*node.Node`: `start`, `deliver`, `stop`, `interrupt`, `setPaused`, `applyProposal`, `diff`, `files`, `openFile` (≤ 768 KiB, else "too large to show from another node"), `subscribe` (lines on STREAM) and `seed`.
- [ ] **Step 4:** Run `go test ./internal/nodelink -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(nodelink): a node's calls to the server, buffered while offline`.

### Task 7: `internal/fleet`: nodes on the server

**Files:**
- Create:
  - `internal/fleet/fleet.go`: the registry and `ServeNode`
  - `internal/fleet/remote.go`: a connected node
  - `internal/fleet/calls.go`: the node's calls applied to the Engine
- Test: `internal/fleet/*_test.go`

**Interfaces:**
- Consumes:
  - Task 3's Engine commands
  - Task 4's store
  - Task 5's `NodeLinkHandler`
  - Task 6's wire format, decoding the args structs from `nodelink` (exported there as `nodelink.Args*`)
- Produces:
  - `type Target interface`: the per-Ticket surface both the local `*node.Node` and a `*Remote` satisfy.
    - `Start(t core.Ticket, a core.Assignment)`, `Deliver(t, a)`, `Stop(t, a, reason)`
    - `Interrupt(ticketID string) error`
    - `Subscribe(ticketID string, from int64) ([]node.Line, <-chan node.Line, func(), error)`
    - `TicketDiff`, `TicketFiles`, `OpenTicketFile`
    - `ApplyProposal(ctx, p) error`
    - `SetPaused(bool)`
    - `Status() node.Status`
  - `type Fleet struct`, made by `New(cfg Config) *Fleet`. `Config{Local *node.Node; LocalID, LocalName string; Engine *core.Engine; Store *store.Store; Creds node.Credentials; Policy node.Policy; Prompts func() (worker, reviewer string); Mirror *Mirror; Now func() time.Time; Log *slog.Logger}`.
  - `(*Fleet).ServeNode(nodeID string, c link.Conn)`: Hello, then Welcome carrying `applied_fact_seq` from the store, then reconcile, then serve until the link ends.
  - `(*Fleet).Nodes() []NodeView`. `NodeView` holds the status, state, last seen, caps and whether it is local.
  - `(*Fleet).Revoke(id)` and `SetDraining(id, on)`. `Revoke` closes the live link with code 4003 and the reason "this node was revoked".
  - `(*Fleet).OnChange(func())`, which is the hub's hook.
  - **Facts are applied in `fact_seq` order** on one goroutine per node, as follows:
    1. A seq ≤ the mark is acknowledged and skipped.
    2. Otherwise the Engine method is called.
    3. Then `AdvanceFactSeq` runs.
    4. Engine `UserError`s are logged and still advance the mark, because a fact the Engine refuses would be refused again.
  - Requests are answered concurrently.
  - **Reconcile on connect.** For every open Ticket whose latest Assignment has `Node == nodeID`:
    - pending or running: `Start` with the snapshot, which also refreshes the node's Ticket cache
    - `Undelivered` present: `Deliver`
    - The node answers with its live Ticket ids. A live Ticket whose latest Assignment ended or moved gets `Stop`.

- [ ] **Step 1: Write the failing tests**, with a real Engine on a temp store, a remote driven by `nodelink.Client` over `net.Pipe`, and `fakeclaude` sessions where needed.
  - `TestFactsHighWaterSurvivesServerRestart`:
    - Apply facts 1–3.
    - Build a new Fleet on the same store, then reconnect with the outbox still holding 2–4.
    - Only 4 applies.
  - `TestReconcileStartsAndStops`.
  - `TestRevokeClosesLiveLink`: the link closes with 4003, and `Nodes()` shows `revoked`.
  - `TestRequestsRunConcurrently`: a blocked `WaitApproval` does not hold up a `Token`.
- [ ] **Step 2:** Run `go test ./internal/fleet -race`. Expected: FAIL.
- [ ] **Step 3:** Implement. `*node.Node` already has every `Target` method except a matching `Subscribe` signature. Adapt with a thin `localTarget` if needed, not by changing `node`.
- [ ] **Step 4:** Run `go test ./internal/fleet -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(fleet): connected nodes, their facts once and in order, reconcile on connect`.

### Task 8: Placement: the fleet is the Executor

**Files:**
- Create: `internal/fleet/place.go`
- Modify:
  - `internal/server/server.go`: the Engine's Executor becomes the Fleet; `recoverAssignments`/`resumeParked` act only on local Assignments
  - `internal/server/dispatch.go`: Interrupt and the artifacts through the Fleet
  - `internal/server/wiring.go`: `Artifacts: fleet`
  - `internal/cli/cli.go`: `--node-name`
- Test: `internal/fleet/place_test.go`, `internal/server/nodes_test.go` (new)

**Interfaces:**
- Produces:
  - `(*Fleet)` implements `core.Executor`. `Start` only queues; placement runs on the fleet's own goroutine and then calls `Engine.AssignmentPlaced` and `Target.Start`.
  - `func choose(t core.Ticket, a core.Assignment, nodes []candidate) (id string, waitReason string)`. It is pure and tested on its own, and applies these rules in order:
    1. **Hard rules.** A node qualifies only if it is:
       - online, not draining, not revoked, and not in `a.Avoid`
       - equal to the pin, when `Execution.Node` is set
       - has the runtime ready: for a pinned runtime that runtime; for `auto` any ready one; for `RoleReview` the reviewing runtime
       - has the directory, for a Dir Ticket. `Dir` belongs to the pinned node, or else to the local node.
    2. **Warmth.** Prefer the node of the Ticket's previous Assignment.
    3. **Load.** Otherwise pick the lowest `(slots_in_use + queued) / slots`, with ties going to the local node, then to the name.
    - When nothing qualifies, `waitReason` is "Waiting for <pinned name>" for a pinned Ticket, and "Waiting for a node with <runtime>" otherwise.
  - Waiting Assignments are re-placed whenever `OnChange` fires, which covers a connect, a status change and an undrain.
  - Their wait reason reaches `TicketSummary.waiting_for_node` through `(*Fleet).WaitReason(ticketID) string`.
  - Deliver, Stop, Interrupt, the artifacts and ApplyProposal route by the latest Assignment's `Node`. An Assignment with no Node, from before this plan, goes to the local node.
  - An offline remote's Deliver and Stop are dropped, because reconcile on reconnect redoes them. Its `Interrupt` and the artifacts return `ErrNodeOffline` ("<name> is offline").

- [ ] **Step 1: Write the failing tests.**
  - `TestChooseRules`, table-driven:
    - The pin wins over the load.
    - Draining is excluded.
    - `Avoid` is excluded.
    - Warmth beats the load.
    - A Dir Ticket goes only to its node.
    - `auto` with only codex ready picks the codex node.
  - `TestPinnedTicketWaits`: the pinned node is offline, so the Ticket waits with "Waiting for mac-studio". When the node connects, it is placed within 100 ms.
  - `TestRecoveryLeavesRemoteAssignments`:
    - Start the server with a running Assignment whose `Node` is a remote id.
    - No Interruption is recorded, and the local node gets no Start.
    - When the remote connects, reconcile starts it.
  - `TestTwoNodesShareLoad`: server plus one remote, four Tickets, two slots each. Both nodes run two sessions, and every Ticket reaches `ready`.
- [ ] **Step 2:** Run `go test ./internal/fleet ./internal/server -run 'Choose|Pinned|Recovery|ShareLoad' -race`. Expected: FAIL.
- [ ] **Step 3:** Implement. The kill switch reaches remotes too: `SetPaused` goes to every Target and is re-sent on connect.
- [ ] **Step 4:** Run `go test ./... -race`. Expected: PASS. The existing single-node server tests must pass unchanged, through the local Target.
- [ ] **Step 5:** Run `make check`, then commit `feat(fleet): place each Assignment on a node`.

### Task 9: Checkpoints to the server's mirror

**Files:**
- Modify:
  - `internal/node/checkpoint.go`: a checkpoint at every turn end, for Tickets with a Repo
  - `internal/node/node.go`: `Config.OnCheckpoint func(ticketID, repo, ref, sha string)`, called after a checkpoint
- Create: `internal/fleet/mirror.go`
- Modify: `internal/nodelink/serve.go` (bundle and send), `internal/fleet/calls.go` (receive)
- Test: `internal/node/checkpoint_test.go`, `internal/fleet/mirror_test.go`

**Interfaces:**
- Produces:
  - **Node side.** On `OnCheckpoint`, `nodelink` runs `git bundle create - <ref> ^<last-sent sha>`; the first one sends the whole ref.
    - The bundle goes as `BlobChunk{kind:"checkpoint"}` frames of ≤ 512 KiB.
    - It then sends the fact `CheckpointSent{ticketID, repo, sha}`.
    - The last-sent sha per Ticket lives in `node.db`.
  - `type Mirror struct`, made by `NewMirror(dir string) *Mirror`, rooted at `<stateDir>/mirrors`:
    - `Receive(repo, ticketID string, bundle io.Reader) error` fetches into `<dir>/<owner>/<name>.git`, which is bare and made on first use, at `refs/athena/<id>/checkpoint`.
    - `Bundle(repo, ticketID string, w io.Writer) (sha string, err error)`.
  - The embedded local node's `OnCheckpoint` writes straight into the Mirror. Its worktree is on the same machine, but the mirror makes Retry elsewhere uniform.
  - Mirror refs of an ended Ticket are deleted once its PR closes or after 14 days, whichever comes first, by a daily sweep.

- [ ] **Step 1: Write the failing tests.**
  - `TestCheckpointEveryTurnEnd`: with `fakeclaude`, two turns give two checkpoint calls, and the branch does not move.
  - `TestMirrorReceivesIncrementalBundles`: a full bundle and then an incremental one, after which the ref equals the node's sha.
  - `TestMirrorSweepsEnded`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement. All git calls run with no shell and use the `gitIn` pins and timeouts the node already uses.
- [ ] **Step 4:** Run `go test ./internal/node ./internal/fleet -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(fleet): keep each Ticket's latest checkpoint on the server`.

### Task 10: Offline nodes and Retry elsewhere

**Files:**
- Modify:
  - `internal/fleet/fleet.go`: the offline timer
  - `internal/fleet/place.go`
  - `internal/node/node.go` and `internal/node/repos.go`: seeding a worktree
  - `internal/server/dispatch.go`: the `RetryElsewhere` command
- Test: `internal/fleet/offline_test.go`, `internal/node/seed_test.go`

**Interfaces:**
- Produces:
  - A remote whose link ended is `offline` at once in `Nodes()`. After `OfflineAfter` with no reconnect, the fleet calls `Engine.NodeOffline(id, since)`, and on reconnect `Engine.NodeBack(id)`.
  - `Dispatcher` handles `RetryElsewhere{ticket_id}` and the control socket op `retry-elsewhere` by calling `Engine.RetryElsewhere`. The new Assignment is placed by Task 8 with `Avoid`.
  - **Seeding.** When the chosen target is not the node of the previous Assignment and the Ticket has a Repo, the fleet streams `Mirror.Bundle` as `BlobChunk{kind:"seed"}` before `Start`.
    - The node's `prepare` sees a seed and builds the worktree: it fetches the bundle into its clone, creates branch `athena/<id>` at the checkpoint's parent, then runs `git checkout <checkpoint> -- .` so the work in progress is back as uncommitted changes.
    - The session starts with the existing recovery note, plus one line: "This Ticket moved from <old node>, which went offline; the work below was restored from its last checkpoint at <time>."
    - With no checkpoint in the mirror, the worktree starts from the remote branch if it was pushed, or else from base. The note says so.
  - A Dir Ticket (no Repo) is never offered Retry elsewhere: `RetryElsewhere` refuses it with "This Ticket works in a folder on <name>; it continues when <name> is back."

- [ ] **Step 1: Write the failing tests.**
  - `TestOfflineAfterThreshold`: on a fake clock, the stage is `person/offline` after 2 minutes and not at 1:59. A reconnect clears it.
  - `TestRetryElsewhereRestoresWork`, with two nodes:
    - A turn on n1 edits `a.txt`, uncommitted, and the checkpoint reaches the mirror.
    - Kill n1, then RetryElsewhere.
    - On n2, the worktree has the edited `a.txt`, the branch is `athena/<id>`, and the first prompt has the moved line.
  - `TestRetryElsewhereWithoutCheckpoint`.
  - `TestDirTicketNotMovable`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/fleet ./internal/node ./internal/server -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(fleet): offline nodes and Retry elsewhere from the checkpoint`.

### Task 11: CLI for nodes

**Files:**
- Create: `internal/cli/node.go`, `internal/cli/node_test.go`
- Modify:
  - `internal/cli/cli.go`: dispatch `node`
  - `internal/server/control.go`: ops `node-token`, `nodes`, `node-drain`, `node-undrain`, `node-revoke`
- Create: `docs/dev/nodes.md`

**Interfaces:**
- Produces, for use on the server's machine (through the control socket):
  - `athena node token [--ttl 1h]` prints exactly one line, ready to paste on the new machine: `athena node join <dashboard-url> <token> --ca-sha256 <fingerprint>`. In ACME mode `--ca-sha256` is left out.
  - `athena node list` prints a table of name, id, state, slots in use, runtimes and last seen.
  - `athena node drain|undrain|revoke <name|id>`. `revoke` asks "Revoke <name>? Its sessions stop and it must join again. [y/N]" unless `--yes` is given.
- Produces, for use on a node:
  - `athena node join <url> <token> [--name N] [--ca-sha256 FP] [--state DIR]`. The name defaults to the hostname, lowercased and sanitized. The state directory defaults to `~/.athena-node`.
    - A fingerprint mismatch refuses with "The server's certificate does not match --ca-sha256; check the command you copied." Nothing is written.
  - `athena node run [--state DIR] [--slots N] [--model M] [--blocked ...] [--claude BIN] [--codex BIN]` builds the `node.Node` with `nodelink` proxies and serves its socket. Its flags match `athena server`'s node flags.
- `docs/dev/nodes.md` covers enrolling a node over the UniFi VPN, the second port, draining, revoking, what Retry elsewhere restores, and that each node keeps its own `claude`/`codex` logins.

- [ ] **Step 1: Write the failing tests.**
  - `TestNodeTokenPrintsJoinCommand`.
  - `TestJoinChecksFingerprint`, against an `httptest` TLS server.
  - `TestRevokeAsksFirst`.
  - `TestNodeRunNeedsIdentity`: with no `node.json`, it says "This machine has not joined yet; run athena node join first."
- [ ] **Step 2:** Run `go test ./internal/cli -run Node -race`. Expected: FAIL.
- [ ] **Step 3:** Implement, and write `docs/dev/nodes.md`.
- [ ] **Step 4:** Run `go test ./internal/cli -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(cli): join, run and manage nodes`.

### Task 12: Hub and commands: nodes on the dashboard's link

**Files:**
- Modify:
  - `internal/hub/hub.go`: `publishNode` becomes per node, deduplicated and throttled per id
  - `internal/hub/views.go`: `nodeStatus`, plus `TicketSummary` node fields
  - `internal/server/hubsource.go`: `Nodes()` from the Fleet
  - `internal/server/dispatch.go`:
    - `SubmitTask.node`, checked as known and not revoked
    - `CreateNodeToken`, sensitive, behind the re-auth `EnrollLink` uses, TTL 1 h
    - `SetNodeDraining`
    - `RevokeNode`, sensitive
    - `RetryElsewhere`
- Test: `internal/hub/hub_test.go`, `internal/server/dispatch_test.go`

**Interfaces:**
- Produces:
  - Snapshot `nodes`: the local node first, then by name.
  - A `Delta.node` per change, and `node_removed` when a revoked node is forgotten. Revoked nodes stay listed, with state `revoked`, until `athena node forget` (deferred). So `node_removed` is defined but sent by nothing yet; say so in a proto comment.
  - `CreateNodeToken` returns `NodeToken{command, expires_at}`.

- [ ] **Step 1: Write the failing tests.**
  - `TestSnapshotListsNodes`.
  - `TestNodeDeltaPerNode`: a quota-only change on n2 is throttled without holding back n1's slot change.
  - `TestCreateNodeTokenNeedsReauth`.
  - `TestSubmitPinsNode`.
  - `TestSubmitUnknownNodeRefused`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/hub ./internal/server -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(hub): every node on the dashboard, and the node commands`.

### Task 13: Dashboard: Nodes, New task, Needs you and the Ticket detail

**Files:**
- Modify:
  - `web/src/features/nodes/Nodes.tsx`: a list of `NodeCard`, an "Add a node" sheet, and a per-card menu with Drain/Undrain and Revoke
  - `web/src/features/new-task/NewTask.tsx`, with a new `NodePicker.tsx` modeled on `RuntimePicker.tsx`
  - `web/src/lib/store/selectors.ts`: kind `offline`
  - `web/src/features/needs-you/NeedsYou.tsx`, with a new `OfflineCard.tsx`
  - `web/src/features/tickets/TicketDetail.tsx`: an "on <node>" chip
  - `web/src/features/settings/Execution.tsx`: one section per node
  - `web/src/lib/commands/actions.ts`: `retryElsewhere`, `createNodeToken`, `setNodeDraining` and `revokeNode`
- Test:
  - co-located `*.test.tsx`
  - `web/e2e/nodes.spec.ts` (new): the harness starts a second node process (`bin/e2e/athena node run`) joined through the real join flow

**Interfaces:**
- Consumes: Task 12's snapshot, deltas and commands.
- UI rules:
  - **Nodes view.**
    - The subtitle becomes "The machines that run sessions, with their load and health."
    - A card's state pill uses green for online, amber for draining and grey for offline or revoked. Orange stays reserved for what needs the owner.
    - "Add a node" opens a sheet with the command in a copyable code block, its expiry ("Works once, until 14:32"), and one sentence: "Run this on the new machine, connected to your VPN."
  - **New task.** The node picker shows only with ≥ 2 nodes that are not revoked. Its options are "Any node" (the default) and each node. An offline node is listed but disabled, with "offline".
  - **Needs you.** `OfflineCard` reads "<name> is offline since 14:02". For a repo Ticket it offers **Retry elsewhere** as the primary action and "Wait" as secondary, which dismisses it for this session view only. For a Dir Ticket it shows only the waiting sentence from Task 10.
  - **Revoke** asks for confirmation: "Revoke <name>? Its sessions stop, and it must join again to come back."
  - A Ticket row that is waiting shows `waiting_for_node` as its stage line.
- The JS budget: the Nodes view is already lazy. This task may add at most 3 KB gzipped to the initial route, which is NodePicker and OfflineCard.

- [ ] **Step 1:** Write the Vitest tests:
  - `Nodes.test.tsx`: two cards, the state pills, and the Add sheet showing the command once the result arrives
  - `NodePicker.test.tsx`: hidden with one node, offline disabled
  - `OfflineCard.test.tsx`: a repo Ticket versus a Dir Ticket
  - `selectors.test.ts`: kind `offline`
- [ ] **Step 2:** Run `cd web && npx vitest run src/features/nodes src/features/new-task src/features/needs-you src/lib/store`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Write `web/e2e/nodes.spec.ts`:
  1. Enroll a node from the sheet, pasting the command into the harness.
  2. Its card turns online.
  3. Submit a task pinned to it, and it runs.
  4. Kill the node process and advance the fleet's clock through the test hook `ATHENA_TEST_OFFLINE_AFTER=2s`.
  5. The Needs you card appears.
  6. Retry elsewhere, and the Ticket runs on the server's node.
  - Take screenshots per the Global Constraints.
- [ ] **Step 5:** Run `make check && make e2e`. Expected: PASS and axe clean.
- [ ] **Step 6:** Commit `feat(web): nodes, the node picker and Retry elsewhere`.

### Task 14: Bench, smoke test and the roadmap

**Files:**
- Modify:
  - `bench/`: `node_idle_rss`, measured on `athena node run` connected and idle for 10 s; and `dispatch_remote_p99`, from `Start` to the node's `AssignmentStarted` fact on loopback, with `fakeclaude`
  - the bench baseline file
  - `docs/dev/smoke-test.md`: a "Second machine" section
  - `docs/superpowers/plans/2026-11-03-athena-roadmap.md`: the "**Plan 6 is built**" note
- Create: `docs/superpowers/plans/2026-11-10-athena-p6-deferred.md`

- [ ] **Step 1:** Add both benchmarks with the §10.3 budgets: node idle RSS < 30 MB, remote dispatch p99 < 100 ms.
- [ ] **Step 2:** Run `make bench`. Expected: both within budget. Record the baseline.
- [ ] **Step 3:** Write the smoke section, run by the owner:
  1. Join the main server machine (the Ryzen box) as a node.
  2. Pin one task to it and leave one unpinned.
  3. Pull its network cable for 30 s, then plug it back in.
  4. Check that no Report is lost or doubled.
  5. Drain it and revoke it.
  - If the owner has not run it, record "not run" in the report.
- [ ] **Step 4:** Write the deferred file (below) and the roadmap note. Run `make check`. Commit `docs: plan 6 bench, smoke test and deferrals`.

---

## Deferred to later plans (decided here)

- **Quota per account across nodes** (plan 5's deferral) moves to **plan 9**, with the quota history. Two nodes on one account each learn its limit from their own sessions; the cost if wrong is one failed launch and a failover per node.
  - The per-account concurrency limit across nodes (§16.1 item 3) moves with it.
- **Labels** in capabilities and dispatch (§7.2 "owner pin/labels"): only the pin is built. Labels arrive when a second kind of machine needs them (plan 8's GPU node).
- **Node-to-node git** over the VPN (§7.4): every move goes through the server's mirror. A direct remote is an optimisation once repositories are large.
- **Tar moves of Dir Tickets** (§7.4): a Dir Ticket stays on its node, because its folder is the owner's folder on that machine.
- **Certificate renewal**: after 397 days a node must join again. `athena node list` shows the expiry from day 360.
- **`athena node forget`** for revoked rows, and the `node_removed` delta.
- **Editable per-node Execution settings** in the dashboard: they stay node flags, shown read-only per node.
- **Plan 7:** `ask_agent`/`hand_off` across nodes ride this plan's request calls.
- **Plan 8:** local inference capabilities in `Capabilities`.
- **Plan 9:** link metrics per node (RTT, buffered facts, replayed facts, bytes per channel).
