# Athena Plan 3: Link Protocol and Dashboard Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `athena server` serves an HTTPS endpoint, reachable over the VPN, that a dashboard (plan 4) uses through one WebSocket per device. A passkey login protects it. Over that connection the client gets:
- a bounded snapshot of the system, then live deltas: Tickets, Approvals, routing records, the node and health
- commands with exactly-once effects: submit, reply, stop, retry, approve, interrupt, routing actions, Policy and push
- live session output from any byte offset

Web Push notifies the owner's phone when a Ticket needs them.

**Architecture:**
- **Wire contract.** `proto/athena/v1` (protobuf, generated with `buf`).
- **Link layer.** `internal/link` frames messages on a WebSocket and adds sequence numbers, cumulative acks, resume, per-channel credit and heartbeats. Plan 6 reuses it for nodes.
- **TLS and PKI.** `internal/pki` is Athena's own CA. `internal/web/tlsmgr.go` serves either a CA-issued certificate or an ACME DNS-01 one, and swaps it on renewal without a restart.
- **Change feed.** The store reports what each committed transaction wrote. A **hub** turns those writes into deltas on a per-process ring (epoch, seq). A client whose cursor falls outside the ring gets a fresh snapshot.
- **Commands.** Dashboard commands run through the same handlers as the control socket (refactored into one dispatcher). Each command carries an id; submit and reply pass it into the Engine, so a retried command never acts twice.
- **Live output.** Each line a session writes is tapped as it is written, so output reaches a client in milliseconds, with no file polling.

**Tech Stack:** Go 1.27, `github.com/coder/websocket` v1.8.x, `google.golang.org/protobuf` v1.36.x (latest stable), `buf` 1.x and `protoc-gen-go` as `go tool` dependencies, `github.com/go-webauthn/webauthn` v0.18.x, `github.com/go-acme/lego/v4` v4.35.x (core plus the `cloudflare`, `exec` and `httpreq` DNS providers only). The standard library covers everything else, including the RFC 8291 and RFC 8292 Web Push crypto (`crypto/ecdh`, `crypto/hkdf`, `crypto/aes`, `crypto/ecdsa`).

**Spec:** `docs/superpowers/specs/2026-11-03-athena-design.md`. Read:
- §2 goals 5, 6 and 7, and the anti-goals
- §8.1–8.3
- §10: views 1–6 and §10.1 "instant feedback"; plan 4 builds the UI, this plan serves its data
- §11 and §16.1 item 4 (TLS: both paths)

Also read plan 2's deferred items (`docs/superpowers/plans/2026-11-03-athena-p2-deferred.md`). Plan 4 takes "Pause processing" and the router settings tools; this plan only exposes their data.

## Global Constraints

- Every plan 1 and plan 2 Global Constraint still holds. `make check` passes at the end of every task.
- **Dependencies:**
  - New Go modules are exactly: `github.com/coder/websocket`, `google.golang.org/protobuf`, `github.com/go-webauthn/webauthn` with its own dependencies, and `github.com/go-acme/lego/v4`.
  - From lego, import only `lego`, `certificate`, `challenge/dns01`, `registration`, `providers/dns/cloudflare`, `providers/dns/exec` and `providers/dns/httpreq`. Never import `providers/dns`, the all-providers factory.
  - Tools go in go.mod `tool` directives (`github.com/bufbuild/buf/cmd/buf`, `google.golang.org/protobuf/cmd/protoc-gen-go`). Generated Go code is committed, so `go build` never needs `buf`.
- **Wire:**
  - Protobuf binary frames only, with one WebSocket per device at `/ws` and subprotocol `athena.v1`.
  - A frame is at most 1 MiB. Compression is `permessage-deflate` with no context takeover.
  - A frame carrying unknown fields is rejected with error code `invalid`, and so is a `Hello.protocol` other than `1` (spec §11 "unknown fields/versions are rejected").
- **Channels** are `CONTROL=1`, `EVENTS=2`, `STREAM=3` and `BLOB=4`. `BLOB` is reserved: it is defined but unused until plan 6.
- **Timing:**
  - Heartbeat ping every 15 s; a peer is dead after 45 s of silence.
  - The delta ring holds 4096 deltas or 8 MiB, whichever is smaller.
  - The per-Ticket stream ring holds 2000 items or 1 MiB.
  - Stream credit starts at 256 items.
  - Command results are kept 24 h.
- **No polling on the dashboard path.** Deltas come from store commits and node events, and stream items from the session tap.
  - The one exception is a 15 s internal health probe that publishes only on change.
  - The browser never polls.
- **Budgets** (spec §2 goal 6), measured by `make bench`:
  - An Engine commit reaching a connected client: p95 < 250 ms.
  - A session line reaching a watching client: p95 < 250 ms.
  - A command to its result: p95 < 250 ms, not counting model time.
  - Server idle RSS with the web server running and one connected client: < 100 MB.
- **Auth:**
  - Single owner. Passkeys (WebAuthn, discoverable credentials, `userVerification: required`).
  - The session cookie is `athena_session`: `HttpOnly`, `Secure` (dropped only when TLS is off on loopback), `SameSite=Strict`, `Path=/`.
  - Session lifetime: 30 days idle and 90 days absolute.
  - The cookie value is 32 random bytes, stored only as its SHA-256.
  - Sensitive commands need a passkey assertion within the last 5 min: `SetPolicy`, passkey revoke and push-settings changes. Otherwise the answer is `reauth_required`.
- **Origin:** the WebSocket upgrade and every `POST /auth/*` require `Origin` to equal the configured origin `https://<host>[:<port>]`. Anything else is refused with HTTP 403.
- **TLS:**
  - Minimum TLS 1.2. Mode is `ca` (default), `acme` or `off`. `off` is accepted only when the listen address is loopback.
  - CA-issued server certificates are valid for 397 days and are re-issued when 30 days remain. ACME certificates are renewed when 30 days remain.
- **Secrets:**
  - These never appear in a frame, a log line, a URL query or the snapshot: installation tokens, private keys, cookie values, enrollment tokens, VAPID private keys and push subscription keys.
  - The enrollment token travels in the URL **fragment**.
  - An Approval's tool input goes to the client through `redact`.
- **Stable error codes** in `CommandResult.code`: `unauthenticated`, `reauth_required`, `invalid`, `not_found`, `conflict`, `unavailable` and `internal`.
- **Push copy** is the Ticket's title plus one stage phrase, exactly one of:
  - "needs your answer"
  - "needs approval"
  - "is ready for review"
  - "delivery failed"
  - "is blocked"
  - "failed"

## Review Focus

1. **The phone drops in the middle of a command** (network change, app backgrounded) and resends the same command id after reconnecting. The result is exactly one Ticket or one Input, and the client gets the original result. (Tasks 5, 8)
2. **A long-disconnected client, or a server restart.** A stale cursor or an old epoch gets a full snapshot, never a partial state. Deltas then arrive in seq order with no gap; a client that sees a gap asks for a resync. (Tasks 7, 8)
3. **A slow or stalled phone while sessions stream heavily.** Server memory stays bounded, other clients and the sessions themselves are unaffected, and that client receives a `StreamGap` and then the latest output. (Tasks 2, 9)
4. **A request from another site, or with a bad or missing session.** All of these are refused with no state change:
   - a WebSocket upgrade from another Origin
   - a missing, expired or revoked cookie
   - an enrollment token that is reused or expired

   A sensitive command without a recent re-auth answers `reauth_required`. (Tasks 6, 8)
5. **A server certificate near expiry.** It is replaced while the server keeps serving: new handshakes get the new certificate, with no restart. An ACME failure keeps the old certificate, reports health `tls`, and retries. A CA-mode certificate covers the configured host and IPs, and the CA's name constraints refuse anything else. (Tasks 3, 4)

---

## File Structure

```
proto/athena/v1/link.proto        Frame, Hello, Welcome, Ping/Pong, Ack, Credit, Error, Resync
proto/athena/v1/state.proto       TicketSummary, TicketDetail, ApprovalView, TriggerView, ThreadView, DecisionView, NodeStatus, Health, PolicyView, Snapshot, Delta
proto/athena/v1/commands.proto    Command (oneof actions), CommandResult
proto/athena/v1/stream.proto      StreamOpen, StreamItems, StreamItem, StreamGap, StreamClose
buf.yaml, buf.gen.yaml            lint + Go generation into internal/pb/athenav1
internal/pb/athenav1/*.pb.go      generated, committed
internal/link/frame.go            encode/decode, unknown-field rejection, size limit
internal/link/session.go          seq, cumulative ack, resend window, credit, heartbeat over a Conn
internal/link/ws.go               Conn adapter for coder/websocket
internal/pki/ca.go                CA create/load, server and client certificate issue, name constraints
internal/pki/mobileconfig.go      iOS/macOS profile for the CA root
internal/web/tlsmgr.go            ca / acme / off modes, GetCertificate, renewal loop, health
internal/web/acme.go              lego obtain/renew behind an Obtainer interface
internal/web/auth.go              WebAuthn ceremonies, enrollment tokens, sessions, step-up
internal/web/server.go            http.Server: routes, security headers, Origin check, static assets
internal/web/assets/              embedded placeholder pages: index.html (login), enroll.html (plan 4 replaces)
internal/web/conn.go              one dashboard connection: Hello/Welcome, snapshot, deltas, commands, streams
internal/web/push.go              VAPID, RFC 8291 encryption, RFC 8292 JWT, send
internal/web/notify.go            stage-entry notifications, preferences, visibility, reconciliation
internal/hub/hub.go               change feed → deltas, epoch/seq ring, subscribers, resync
internal/hub/views.go             core → proto view mapping, bounded snapshot
internal/server/dispatch.go       one command dispatcher shared by the control socket and the dashboard (implements web.Commander)
internal/store/web.go             migration 4: passkeys, web_sessions, enroll_tokens, command_results, push_subscriptions
internal/store/store.go           commit hook: written keys per Tx
internal/runtime/claude/session.go  Options.OnLine tap (offset, line)
internal/node/stream.go           per-Ticket stream rings, Subscribe(ticket, from) and Status()
internal/render/render.go         moved from internal/server/render.go: LogLine + rich StreamItem rendering (shared by server and web)
internal/cli/web.go               athena web setup | enroll | passkeys | revoke | status | ca
docs/dev/dashboard.md             setup: host, TLS modes, CA install on iPhone/macOS/Linux, enrolling a passkey
```

---

### Task 1: Wire contract and code generation

**Files:**
- Create: `proto/athena/v1/{link,state,commands,stream}.proto`, `buf.yaml`, `buf.gen.yaml`, `internal/pb/athenav1/` (generated), `internal/link/frame.go`
- Modify: `go.mod` (tool directives, protobuf), `Makefile` (`proto` target; `check` runs `go tool buf lint` and fails if `go tool buf generate` changes the tree)
- Test: `internal/link/frame_test.go`

**Interfaces:**
- Produces (proto, package `athena.v1`, `go_package = ".../internal/pb/athenav1;athenav1"`):
  - **Framing:**
    - `Frame{Channel channel; uint64 seq; uint64 ack; oneof body {...}}`. `seq` is 0 for a frame that is not durable.
    - `enum Channel {CHANNEL_UNSPECIFIED=0; CONTROL=1; EVENTS=2; STREAM=3; BLOB=4}`
  - **Session setup and upkeep:**
    - `Hello{uint32 protocol; string client_id; string epoch; uint64 cursor; uint64 last_seq}`
    - `Welcome{string epoch; string server_version; uint32 heartbeat_ms; bool snapshot_follows}`
    - `Ping{uint64 at_unix_ms}`, `Pong{uint64 at_unix_ms}`
    - `Credit{Channel channel; uint32 items}`
    - `Error{string code; string message}`
    - `Resync{}`, from the client: "send me a snapshot"
  - **State:**
    - `Snapshot{uint64 cursor; repeated TicketSummary tickets; repeated ApprovalView approvals; repeated TriggerView triggers; repeated ThreadView threads; repeated DecisionView decisions; NodeStatus node; Health health; PolicyView policy}`
    - `Delta{uint64 seq; oneof change {TicketSummary ticket; TicketDetail ticket_detail; ApprovalView approval; TriggerView trigger; ThreadView thread; DecisionView decision; NodeStatus node; Health health; PolicyView policy}}`
  - **Views:**
    - `TicketSummary{id, title, stage, action, repo, dir, model, created_at, updated_at, ended_at, outcome, needs_you bool, can_retry bool, pending_approval_id, pr_url, attention_question}`
    - `TicketDetail{TicketSummary summary; repeated TimelineEntry timeline; repeated string threads}`
    - `TimelineEntry{at, kind (trigger|decision|input|report|assignment|approval|reply), text, ref_id}`
    - `ApprovalView{id, ticket_id, tool, summary, rule, input_redacted, state, created_at, answered_at}`
    - `TriggerView`, `ThreadView` and `DecisionView` carry the fields of the server's existing JSON views (`internal/server/views.go`). DecisionView adds `would_accept`, `reasons` (code plus values) and `dismissed_at`; that is the routing-records view plan 4 shows.
    - `NodeStatus{name; uint32 slots; uint32 slots_in_use; repeated string live_ticket_ids; uint32 queued; string claude_version}`
    - `Health{repeated HealthItem items}`, where `HealthItem{name, ok, message, since}` covers the Sources, the router, effects kinds, `tls` and `push`
    - `PolicyView{uint32 revision; string version; string settings_json}`. The Policy travels as its canonical JSON, and `internal/policy` stays its only schema.
  - **Commands:**
    - `Command{string id; oneof action}`. The actions are:
      - `SubmitTask{text, repo, dir, model}`
      - `Reply{ticket_id, text}`, `Stop{ticket_id, reason}`, `Retry{ticket_id}`, `Interrupt{ticket_id}`
      - `AnswerApproval{approval_id, approve, note}`
      - `Reevaluate{trigger_id}`, `Dismiss{decision_id}`, `Unsubscribe{thread}`
      - `SetPolicy{base_revision, settings_json}`
      - `WatchTicket{ticket_id}`, `UnwatchTicket{ticket_id}`
      - `PushSubscribe{endpoint, p256dh, auth}`, `PushUnsubscribe{endpoint}`, `SetNotificationPrefs{repeated string muted}`
      - `Visibility{bool visible}`
      - `ListPasskeys{}`, `RevokePasskey{id}`
    - `CommandResult{id, ok, code, message, oneof result {string ticket_id; PasskeyList passkeys; uint32 policy_revision}}`
  - **Stream:**
    - `StreamOpen{ticket_id; int64 from_offset}`. `-1` means "the last 200 items".
    - `StreamItems{ticket_id; repeated StreamItem items}`
    - `StreamItem{int64 offset; at; kind (init|say|thinking|tool_call|tool_result|result|other); string text; string tool; string tool_use_id; bool is_error}`
    - `StreamGap{ticket_id; int64 from; int64 to}`, `StreamClose{ticket_id}`
- Produces (`internal/link`):
  - `Encode(f *athenav1.Frame) ([]byte, error)`
  - `Decode(b []byte) (*athenav1.Frame, error)`. It returns `ErrInvalid` for a frame over 1 MiB, unknown fields at any depth, or an unset body.
  - `const MaxFrame = 1 << 20`

- [ ] **Step 1: Write failing tests.**
  - `TestFrameRoundTrip`: every body type.
  - `TestDecodeRejectsUnknownFields`: append a field with an unused tag to an encoded Frame, and to a nested `Command`; both give `ErrInvalid`.
  - `TestDecodeRejectsOversize`
  - `TestDecodeRejectsEmptyBody`
- [ ] **Step 2: Run** `go test ./internal/link` → FAIL.
- [ ] **Step 3: Write the protos and `buf` config**, generate, and implement `Encode`/`Decode`. Unknown fields are found by walking `protoreflect` messages and checking `GetUnknown()`. `buf lint` uses the `STANDARD` rules, except that enum zero values are named `*_UNSPECIFIED`.
- [ ] **Step 4: Run** `make proto && make check` → PASS. The tree is clean after generating.
- [ ] **Step 5: Commit** `feat(proto): athena.v1 wire contract`.

### Task 2: Link session — seq, ack, resume, credit, heartbeat

**Files:**
- Create: `internal/link/session.go`, `internal/link/ws.go`
- Test: `internal/link/session_test.go`

**Interfaces:**
- Consumes: `Encode`/`Decode` (Task 1).
- Produces:
  - `type Conn interface { Read(ctx) ([]byte, error); Write(ctx, []byte) error; Close(code int, reason string) error }`
  - `NewWS(c *websocket.Conn) Conn`
  - `type Session`: `New(c Conn, o Options) *Session`, with `Options{Heartbeat, DeadAfter time.Duration; SendQueue map[athenav1.Channel]int; OnFrame func(*athenav1.Frame)}`.
  - `Send(ch athenav1.Channel, durable bool, f *athenav1.Frame) error`. It stamps `seq` and the piggybacked `ack`. It returns `ErrBackpressure` when that channel's queue is full; it never blocks the caller.
  - `Grant(ch, items uint32)`, which sends a `Credit`.
  - `Credit(ch) uint32`, the credit the peer has granted.
  - `Unacked() []*athenav1.Frame`, the durable frames sent and not yet acked, for a resume.
  - `LastReceived() uint64`
  - `Run(ctx) error`, which returns when the peer is dead, the context ends or the connection closes.

Behaviour (spec §8.2):
- Each direction numbers its durable frames from 1. A receiver acks cumulatively: it piggybacks on its next frame, or sends a standalone ack after 200 ms of silence.
- A frame whose `seq` is ≤ the last received is dropped as a duplicate.
- Each channel has its own bounded send queue, so a full `STREAM` queue never delays `CONTROL` or `EVENTS`. The writer serves `CONTROL` first, then `EVENTS`, then `STREAM`.
- Heartbeat: a `Ping` every 15 s. 45 s with no frame received closes the connection with code 4000.

- [ ] **Step 1: Write failing tests** (an in-memory `Conn` pair; one test over a real `httptest` WebSocket):
  - `TestDurableFramesNumberedAndAcked`
  - `TestDuplicateSeqDropped`
  - `TestResumeReplaysUnacked`: drop the connection, start a new Session, then send `Unacked()` after the peer's `Hello.last_seq`. Nothing is lost and nothing is duplicated.
  - `TestStreamQueueFullDoesNotBlockControl`
  - `TestCreditTracking`
  - `TestHeartbeatKillsSilentPeer`, using an injected clock
  - `TestWebSocketSubprotocolRequired`: a client without `athena.v1` is refused.
- [ ] **Step 2: Run** `go test -race ./internal/link` → FAIL.
- [ ] **Step 3: Implement.** One reader goroutine, one writer goroutine, and a timer for heartbeats.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(link): sequenced, acked, resumable sessions`.

### Task 3: Athena's CA

**Files:**
- Create: `internal/pki/ca.go`, `internal/pki/mobileconfig.go`
- Test: `internal/pki/ca_test.go`

**Interfaces:**
- Produces:
  - `LoadOrCreateCA(dir string, host string, ips []net.IP) (*CA, error)`. It writes `ca.key` (0600, ECDSA P-256, PKCS#8) and `ca.pem` under `dir` (0700).
  - CA certificate:
    - valid 10 years
    - CN `Athena CA (<host>)`
    - `IsCA`, MaxPathLen 0
    - **NameConstraints**, critical: permitted DNS `host` (subdomains included) and each configured IP as a /32 or /128
  - `(*CA).ServerCert(host string, ips []net.IP) (tls.Certificate, error)`: valid 397 days, SAN = host plus IPs, EKU serverAuth, ECDSA P-256.
  - `(*CA).ClientCert(name string) (tls.Certificate, error)`, for plan 6 nodes. The name must be a subdomain of the host; EKU is clientAuth.
  - `(*CA).Fingerprint() string`: the SHA-256 of the DER, colon-separated hex.
  - `MobileConfig(ca *CA) []byte`: an unsigned plist with one `com.apple.security.root` payload, deterministic UUIDs derived from the fingerprint, and PayloadDisplayName `Athena CA (<host>)`.

- [ ] **Step 1: Write failing tests.**
  - `TestCARoundTrip`: create, then load, gives the same fingerprint. The key file mode is 0600; refuse a key with group or other bits.
  - `TestServerCertVerifiesForHostAndIPs`: verify with an `x509.CertPool` holding only the CA.
  - `TestNameConstraintsRefuseOtherNames`: a leaf for `example.com` signed with the CA key fails verification.
  - `TestServerCertValidity397Days`
  - `TestMobileConfigParses`: decode the plist with a minimal XML reader and assert the payload type and the DER bytes.
- [ ] **Step 2: Run** `go test ./internal/pki` → FAIL.
- [ ] **Step 3: Implement** with `crypto/x509`.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(pki): athena CA with name constraints and iOS profile`.

### Task 4: TLS manager — ca, acme, off

**Files:**
- Create: `internal/web/tlsmgr.go`, `internal/web/acme.go`
- Test: `internal/web/tlsmgr_test.go`

**Interfaces:**
- Consumes: `internal/pki` (Task 3).
- Produces:
  - `TLSConfig{Mode string /* ca|acme|off */; Host string; IPs []net.IP; Listen string; ACME ACMEConfig}`
  - `ACMEConfig{Email, Provider string /* cloudflare|exec|httpreq */; Resolvers []string; Directory string}`. Directory defaults to Let's Encrypt production.
  - `NewTLSManager(stateDir string, c TLSConfig, o Obtainer, log *slog.Logger, now func() time.Time) (*TLSManager, error)`
  - `(*TLSManager).Config() *tls.Config`: nil for `off`. It sets `GetCertificate`, `MinVersion: tls.VersionTLS12` and NextProtos `h2` and `http/1.1`.
  - `Run(ctx)`: checks every 12 h and renews or re-issues when 30 days or fewer remain.
  - `Health() (ok bool, msg string, notAfter time.Time)`
  - `type Obtainer interface { Obtain(ctx, host string) (certPEM, keyPEM []byte, err error) }`
  - `NewLegoObtainer(stateDir string, c ACMEConfig) (Obtainer, error)`. The account key lives in `<stateDir>/tls/acme/account.key` (0600). The provider is built by name from the three allowed packages; its credentials come from lego's environment variables, including the `_FILE` variants.
- Rules:
  - `off` with a listen address that is not loopback → error `tls off is allowed only on a loopback address`.
  - Certificates and keys live under `<stateDir>/tls/` (0700 directory, 0600 keys).
  - On an ACME failure, the current certificate keeps serving, health reports `tls` failing, and retries back off from 1 h, doubling to 12 h.
  - Starting the server with no certificate yet and ACME failing → the server serves nothing on the web listener and reports health. The control socket still works.

- [ ] **Step 1: Write failing tests** (a fake `Obtainer`; injected clock):
  - `TestCAModeServesCAIssuedCert`, over a real TLS handshake from an `httptest` listener
  - `TestReissueBeforeExpiry`: at 29 days left, a new handshake gets a new serial with no restart.
  - `TestACMEFailureKeepsOldCert`
  - `TestOffRefusedOnNonLoopback`
  - `TestLegoObtainerRejectsUnknownProvider`
- [ ] **Step 2: Run** `go test ./internal/web -run TLS` → FAIL.
- [ ] **Step 3: Implement.** A real ACME issuance is not exercised in CI; `docs/dev/dashboard.md` (Task 11) gives the manual check.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(web): tls manager with own-CA and ACME DNS-01`.

### Task 5: Store migration 4, commit hook and idempotent commands

**Files:**
- Create: `internal/store/web.go`
- Modify: `internal/store/store.go` (commit hook), `internal/core/engine.go`, `internal/core/model.go` (`TaskOptions.RequestID`, `Ticket.RequestID`; `AddPersonInputWithID`)
- Test: `internal/store/web_test.go`, `internal/core/engine_test.go`

**Interfaces:**
- Produces (store):
  - Migration 4 creates these tables:
    - `passkeys(id PK, name, credential BLOB, created_at, last_used_at)`
    - `web_sessions(hash PK, created_at, seen_at, reauth_at, passkey_id)`
    - `enroll_tokens(hash PK, expires_at, used_at)`
    - `command_results(client_id, command_id, result BLOB, at, PRIMARY KEY(client_id, command_id))`
    - `push_subscriptions(endpoint PK, p256dh, auth, created_at, failures)`
  - It also adds a unique index on the Ticket's `request_id` when one is set.
  - Methods: CRUD for each table, `PruneCommandResults(before)` and `PruneWebSessions(now)`.
- Produces (commit hook):
  - `(*Store).OnCommit(func(Changes))`
  - `Changes{Tickets, Approvals, Triggers, Threads, Decisions []string; Policy bool}`: the ids written by one committed Tx. The hook runs after commit on the committing goroutine and must not block. The hub (Task 7) copies the changes onto a channel.
  - A rolled-back Tx reports nothing.
- Produces (core):
  - `TaskOptions.RequestID string`. With an id already stored, `SubmitOwnerTask` returns the existing Ticket and `ErrDuplicate`, and creates nothing.
  - `AddPersonInputWithID(ticketID, inputID, text string) error`: a repeated `inputID` is a no-op.

- [ ] **Step 1: Write failing tests.**
  - `TestMigration4FromV3`: a plan 2 database upgrades and its Tickets still read.
  - `TestCommitHookReportsWrittenIDs`
  - `TestCommitHookSilentOnRollback`
  - `TestSubmitWithSameRequestIDCreatesOneTicket`
  - `TestPersonInputSameIDOnce`
  - `TestCommandResultsPrune`
- [ ] **Step 2: Run** `go test ./internal/store ./internal/core` → FAIL.
- [ ] **Step 3: Implement.** Tx methods record ids into the Tx's change set, so the store stays domain-neutral apart from naming the collections.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(store,core): web tables, commit hook, idempotent submit and reply`.

### Task 6: Passkey auth, enrollment and sessions

**Files:**
- Create: `internal/web/auth.go`, `internal/web/authtest_test.go` (software authenticator), `internal/cli/web.go` (the `enroll`, `passkeys` and `revoke` parts)
- Test: `internal/web/auth_test.go`, `internal/cli/web_test.go`

**Interfaces:**
- Consumes: the store web tables (Task 5).
- Produces:
  - `NewAuth(st *store.Store, origin, rpID string, now func() time.Time) (*Auth, error)`. The RP name is `Athena` and the RP ID is the configured host.
  - HTTP handlers, all JSON, all checking Origin:
    - `POST /auth/register/begin {token}` and `POST /auth/register/finish`. The first passkey needs a valid enrollment token. Later passkeys need a valid enrollment token too; an authenticated session alone is not enough.
    - `POST /auth/login/begin` and `POST /auth/login/finish`: discoverable credentials. Finish sets the cookie.
    - `POST /auth/reauth/begin` and `POST /auth/reauth/finish`: they set `reauth_at`.
    - `POST /auth/logout`
    - `GET /auth/session` → `{authenticated, reauthValidUntil}`
  - `(*Auth).Session(r *http.Request) (Session, bool)`, where `Session{Hash string; ReauthAt time.Time}`
  - `(*Auth).RecentReauth(s Session) bool`: true within 5 min.
  - `IssueEnrollToken(st, ttl time.Duration) (token string, err error)`: 32 random bytes, base64url, stored hashed, single use, default TTL 15 min.
  - CLI:
    - `athena web enroll` prints `https://<host>[:port]/enroll#<token>`, plus a QR code drawn with Unicode half-blocks (the QR encoder is a small vendored, MIT-licensed implementation in `internal/cli/qr.go`, credited in its file header).
    - `athena web passkeys` lists the passkeys.
    - `athena web revoke <id>` deletes a passkey and every session it created.

- [ ] **Step 1: Write failing tests.** The software authenticator is ECDSA P-256, `none` attestation, with UV and UP flags set.
  - `TestEnrollAndLogin`: token, then register, then login, then cookie flags exactly as the Global Constraints say.
  - `TestEnrollTokenSingleUseAndExpiry`
  - `TestLoginWrongOriginRefused`: 403, no session row.
  - `TestRevokedPasskeyKillsSessions`
  - `TestSessionIdleAndAbsoluteExpiry`: 30 and 90 days, injected clock.
  - `TestReauthWindow`: valid at 4m59s, not at 5m01s.
  - `TestEnrollTokenNotLogged`: capture slog output during enroll; the token is absent.
- [ ] **Step 2: Run** `go test ./internal/web -run Auth ./internal/cli -run Web` → FAIL.
- [ ] **Step 3: Implement** with `go-webauthn`. Ceremony state is held server-side in memory, keyed by a random id in a short-lived (5 min) cookie `athena_ceremony`, never in the URL.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(web): passkey login, enrollment tokens and sessions`.

### Task 7: State views and the hub

**Files:**
- Create: `internal/hub/hub.go`, `internal/hub/views.go`
- Modify: `internal/node/node.go` (`Status() node.Status`, a plain Go struct with the NodeStatus fields; status changes are published through `Config.OnStatus func()`, and the hub maps `node.Status` to the proto)
- Test: `internal/hub/hub_test.go`, `internal/hub/views_test.go`

**Interfaces:**
- Consumes:
  - the commit hook (Task 5)
  - Engine reads: `Ticket`, `Tickets`, `PendingApprovals`, `Trigger`, `Thread`, `ActiveThreads`, `Decisions`
  - the server's health function and Policy view
  - `node.Status()`
- Produces:
  - `New(src Source, log *slog.Logger) *Hub`, where `Source` is an interface over the reads above, so tests can use a fake.
  - `Start(ctx)`
  - `Snapshot() *athenav1.Snapshot`, bounded:
    - Tickets: every open one plus the 100 most recently ended
    - Approvals: pending plus the last 50 answered
    - Threads: active plus the 100 most recent
    - Decisions: the last 200
    - Triggers: the last 200
  - `Subscribe(cursor uint64, epoch string) (sub *Sub, snapshotNeeded bool)`
  - `(*Sub).C() <-chan *athenav1.Delta`
  - `(*Sub).Close()`
  - `Epoch() string`: random per process start.
  - `Detail(ticketID string) (*athenav1.TicketDetail, error)`
- Behaviour:
  - The hub reads each change once and maps it to its view. `TicketSummary.needs_you` is true when the stage is `person`; `can_retry` comes from `core.CanRetry`.
  - Deltas are published with seq+1 into the ring and fanned out.
  - A subscriber whose channel is full (capacity 256) is closed with `resync`. It never blocks the hub.
  - Detail deltas go only to subscribers that watch that Ticket (`(*Sub).Watch(id)`, `Unwatch`).
  - The health probe runs every 15 s and publishes only on change.

- [ ] **Step 1: Write failing tests** (a fake Source):
  - `TestSnapshotBounds`: 150 ended Tickets give 100 in the snapshot.
  - `TestDeltaPerCommittedChange`: one change gives one delta, with seq increasing by 1.
  - `TestCursorInsideRingResumes`
  - `TestCursorOutsideRingNeedsSnapshot`
  - `TestEpochMismatchNeedsSnapshot`
  - `TestSlowSubscriberResyncedNotBlocking`: the hub keeps publishing to another subscriber.
  - `TestDetailOnlyToWatchers`
  - `TestRoutingRecordsInSnapshot`: a would-accept Decision appears with its reasons.
  - `TestApprovalInputRedacted`: a `ghs_…` string in the input is redacted.
- [ ] **Step 2: Run** `go test -race ./internal/hub` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(hub): bounded snapshot and delta feed from store commits`.

### Task 8: Dashboard connection and the shared command dispatcher

**Files:**
- Create: `internal/server/dispatch.go`, `internal/web/conn.go`
- Modify: `internal/server/control.go` (the JSON ops call `dispatch`), `internal/node/node.go` (`Interrupt(ticketID string) error`, using the session's `InterruptTurn`)
- Test: `internal/server/dispatch_test.go`, `internal/web/conn_test.go`

**Interfaces:**
- Consumes: link Session (Task 2), Auth (Task 6), Hub (Task 7), idempotent core commands (Task 5).
- Produces:
  - In `internal/web`: `type Commander interface { Do(ctx, o Origin, c *athenav1.Command) *athenav1.CommandResult }` and `Origin{ClientID string; Sensitive bool /* recent reauth */}`. `web` never imports `internal/server`; the server imports `web` for wiring.
  - In `internal/server`: `type Dispatcher`, which implements `web.Commander`.
  - The control socket's `Request` ops map onto the same actions, and keep their JSON responses unchanged; plan 1 and 2 CLI tests still pass.
  - Idempotency: `(client_id, command.id)` found in `command_results` returns the stored result without acting. Otherwise the result is stored after acting. SubmitTask passes `command.id` as `RequestID`; Reply uses it as the input id.
  - `SetPolicy`, `RevokePasskey`, `PushSubscribe`, `PushUnsubscribe` and `SetNotificationPrefs` without `Sensitive` → `reauth_required`.
  - `ServeConn(w, r)`:
    1. Checks Origin and the session cookie. No session → HTTP 401 before the upgrade.
    2. Upgrades with subprotocol `athena.v1`.
    3. Expects `Hello` within 10 s.
    4. Answers `Welcome`, then either a Snapshot followed by deltas, or the deltas after `cursor`.
    5. Serves Commands on `CONTROL` and answers each with `CommandResult` on `CONTROL`.
    6. A client `Resync` gets a fresh Snapshot.
  - A session revoked while connected is closed within 1 s (Auth notifies the open connections).

- [ ] **Step 1: Write failing tests** (a real `httptest` TLS server, the software authenticator and an `athenav1` client):
  - `TestConnRequiresSessionAndOrigin`
  - `TestHelloWelcomeSnapshotThenDeltas`: submit through the control socket; the WebSocket client receives a `TicketSummary` delta.
  - `TestReconnectWithCursorGetsMissedDeltasOnly`
  - `TestServerRestartForcesSnapshot`
  - `TestRetriedCommandActsOnce`: send `SubmitTask{id:"c1"}`, cut the connection before the result, reconnect and resend `c1`. One Ticket exists, and both results carry the same `ticket_id`.
  - `TestSensitiveCommandNeedsReauth`
  - `TestInterruptReachesSession`, using the fake runtime from plan 1's tests
  - `TestControlSocketUnchanged`: the existing CLI tests pass.
- [ ] **Step 2: Run** `go test -race ./internal/web ./internal/server` → FAIL.
- [ ] **Step 3: Implement.** Move the op logic out of `control.go` into `dispatch.go`; `control.go` keeps only the JSON line framing.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(web): dashboard connection over athena.v1 with exactly-once commands`.

### Task 9: Live session stream

**Files:**
- Create: `internal/node/stream.go`
- Create: `internal/render/render.go`, `internal/render/render_test.go` (moved with `git mv` from `internal/server/render.go` and `render_test.go`; the server's `logs` op imports it)
- Modify: `internal/runtime/claude/session.go` (`Options.OnLine func(offset int64, line []byte)`, called after each line is written to the transcript, with the line's starting byte offset), `internal/web/conn.go` (`StreamOpen`/`StreamClose`, credit)
- Test: `internal/node/stream_test.go`, `internal/render/render_test.go`, `internal/web/conn_test.go`

**Interfaces:**
- Produces:
  - `render.Items(line []byte, offset int64) []*athenav1.StreamItem`, in `internal/render`.
  - `(*Node).Subscribe(ticketID string, from int64) (backfill []Line, live <-chan Line, cancel func(), err error)`, with `Line{Offset int64; Data []byte}`. The node keeps raw lines; `internal/web/conn.go` renders them with `render.Items`, so `node` never imports `render` or `pb`.
    - `from = -1` gives the last 200 items.
    - A `from` older than the ring start is backfilled by reading the transcript file from that byte offset.
    - A `from` past the end gives an empty backfill.
  - Each live channel has capacity 512. When one fills, the node drops items for that subscriber only and records the first dropped offset. The connection then sends `StreamGap{from, to}` and continues from the latest item.
  - Rendering:
    - Text goes through `redact`.
    - `say` and `thinking` items are capped at 16 KiB each, with `…` marking a cut.
    - A tool call shows its tool name and the primary input (the same keys as `primaryKeys`).
    - A tool result is capped at 4 KiB.
    - `tool_use_id` pairs each result with its call.
  - Credit: the client grants items with `Credit{STREAM, n}`. With no credit left, the server keeps only a position. On the next grant it sends `StreamGap` and the latest tail of up to 200 items.

- [ ] **Step 1: Write failing tests.**
  - `TestOnLineOffsetsMatchFile`: the offsets equal the byte positions in the transcript.
  - `TestSubscribeBackfillFromFileThenLive`
  - `TestTailMinusOne`
  - `TestSlowSubscriberGetsGapOthersUnaffected`
  - `TestRenderRedactsAndCaps`
  - `TestStreamOverConnWithCredit`: credit 10, then 50 lines written, then 10 items, then a grant, then a `StreamGap` and the tail.
- [ ] **Step 2: Run** `go test -race ./internal/node ./internal/render ./internal/web` → FAIL.
- [ ] **Step 3: Implement.** Rings are per Ticket and survive across its Assignments. A ring is freed 10 min after its Ticket ends with no subscribers.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(stream): live session output by offset with gap-and-tail backpressure`.

### Task 10: Web Push

**Files:**
- Create: `internal/web/push.go`, `internal/web/notify.go`
- Test: `internal/web/push_test.go`, `internal/web/notify_test.go`

**Interfaces:**
- Produces:
  - `LoadOrCreateVAPID(dir string) (*VAPID, error)`: P-256, `<stateDir>/push/vapid.key` (0600). `(*VAPID).PublicKey() string` is base64url (uncompressed point), and `/push/key` serves it.
  - `Encrypt(sub Subscription, plaintext []byte, salt []byte, serverKey *ecdh.PrivateKey) ([]byte, error)`: RFC 8291 `aes128gcm`, record size 4096, a single record.
  - `VAPIDHeader(v *VAPID, endpoint string, now time.Time) string`: RFC 8292 `vapid t=<jwt>, k=<pub>`.
    - `aud` is the endpoint's origin.
    - `exp` is now + 12 h.
    - `sub` is `https://<host>`, never an email address.
  - `type Notifier`:
    1. It watches TicketSummary deltas from the hub.
    2. On each **entry** into a person stage, it enqueues a `push` effect with key `<ticket>:<stage>:<action>:<entry-time>`, so one entry sends at most one notification.
       - Payload: `{"title":"Athena","body":"<ticket title> <phrase>","url":"/t/<id>","tag":"<ticket id>"}`.
       - Header `Urgency: high` for approvals, `normal` otherwise. TTL 86400.
    3. Muted kinds (`approval`, `answer`, `review`, `delivery`, `blocked`, `failed`) are skipped.
    4. Nothing is sent while any connected client has reported `Visibility{visible:true}` within the last 30 s.
  - The `push` effect handler sends to every subscription:
    - 201 → done
    - 404 or 410 → delete the subscription
    - 429 or 5xx → retry with backoff from 5 s to 300 s
  - Startup reconciliation: every open Ticket already in a person stage, with no stored key, is enqueued once.

- [ ] **Step 1: Write failing tests.**
  - `TestEncryptRFC8291Vector`: RFC 8291 Appendix A gives exact bytes.
  - `TestVAPIDHeaderVerifies`: parse the JWT, verify ES256 with the public key, check `aud`, `exp` ≤ 24 h and `sub`.
  - `TestOneNotificationPerStageEntry`
  - `TestMutedKindSkipped`
  - `TestVisibleClientSuppresses`
  - `TestGoneSubscriptionDeleted`: an `httptest` push service answers 410.
  - `TestReconciliationOnStart`
  - `TestPushPayloadCopy`: the exact phrases from the Global Constraints.
- [ ] **Step 2: Run** `go test ./internal/web -run 'Push|Notif|Encrypt|VAPID'` → FAIL.
- [ ] **Step 3: Implement** with the standard library only.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(web): web push with RFC 8291/8292 and stage-entry notifications`.

### Task 11: HTTP server, wiring, setup CLI and docs

**Files:**
- Create: `internal/web/server.go`, `internal/web/assets/{index.html,enroll.html,app.js}` (embedded; plain HTML and JS, no framework), `docs/dev/dashboard.md`
- Modify: `internal/server/config.go` (the `web` part), `internal/server/server.go` (wiring), `internal/cli/web.go` (`setup`, `status`, `ca`), `README.md` (link)
- Test: `internal/web/server_test.go`, `internal/server/web_e2e_test.go`

**Interfaces:**
- Produces:
  - config.json `web`: `{"enabled": false, "listen": ":8443", "host": "", "ips": [], "tls": "ca", "acme": {...}}` (`pushContact` dropped, ruling R28). `host` is required when `enabled`. Validation errors name the field.
  - `athena web setup --host H [--ip IP]... [--listen ADDR] [--tls ca|acme|off] [--acme-email E --acme-provider P]` writes the `web` part and enables it.
  - `athena web status` prints the URL, the TLS mode, the certificate expiry, the CA fingerprint, the passkey count and the connected clients.
  - `athena web ca` prints the CA fingerprint and the paths of `ca.pem` and the profile.
  - Routes:
    - `/` and `/enroll`: placeholder pages that do passkey login and enrollment through `navigator.credentials`. After login, `/` shows "Signed in. The dashboard arrives in plan 4." and a live count of open Tickets read over `/ws`, which proves the whole path from a phone.
    - `/ws`, `/auth/*`, `/push/key`
    - `/ca.pem` and `/ca.mobileconfig`: CA mode only, public.
    - `/healthz`: unauthenticated, returns only `ok`.
  - Security headers on every response:
    - `Content-Security-Policy: default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'`
    - `Strict-Transport-Security: max-age=31536000` (TLS only)
    - `X-Content-Type-Options: nosniff`
    - `Referrer-Policy: no-referrer`
  - The server starts the web listener only when `web.enabled`. It stops it on shutdown, before the store closes.

- [ ] **Step 1: Write failing tests.**
  - `TestSecurityHeaders`
  - `TestHealthzLeaksNothing`
  - `TestCAEndpointsOnlyInCAMode`
  - `TestWebDisabledByDefault`
  - `TestWebE2E`, a full server in a temp state dir with `tls: ca` on `127.0.0.1` and the software authenticator:
    1. enroll and log in
    2. connect over `/ws` and receive a snapshot
    3. `SubmitTask` through the fake runtime; receive the Ticket's delta
    4. `StreamOpen` and receive items
    5. the fake session requests an Approval; the Approval delta and a push enqueued against an `httptest` push service
    6. `AnswerApproval` without reauth succeeds, because approving is not sensitive
    7. `SetPolicy` without reauth gives `reauth_required`; reauth, then it succeeds.
- [ ] **Step 2: Run** `go test ./internal/web ./internal/server -run 'Web|Security|Healthz|CAEnd'` → FAIL.
- [ ] **Step 3: Implement**, and write `docs/dev/dashboard.md`:
  - choosing the host, for example a UniFi local DNS record `athena.home.arpa` pointing at the server's VPN address
  - `athena web setup`
  - **CA mode:**
    - iPhone: open `/ca.mobileconfig` in Safari, install it under Settings → General → VPN & Device Management, then turn on full trust under Settings → General → About → Certificate Trust Settings
    - macOS: `security add-trusted-cert`
    - Linux: `update-ca-certificates`
  - **ACME mode:** the provider environment variables, and a manual verification step
  - `athena web enroll` and its QR code
  - adding the home-screen app, with a note that iOS push needs the trusted certificate and the app installed to the home screen
  - **Safety notes:**
    - the listener is reachable by anything that can reach the port, so bind it to the VPN interface address when possible
    - passkeys and step-up for sensitive changes
    - revoke a lost device with `athena web revoke`
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(web): https dashboard endpoint, setup CLI and docs`.

### Task 12: Budgets and the phone check

**Files:**
- Modify: `bench/bench_test.go`, `bench/rig_test.go`, `bench/baseline.json`
- Test: `bench/bench_test.go`

**Interfaces:**
- Consumes: everything above, through a loopback server with TLS `off`. Bench mode allows it on loopback.
- Produces these budgets in `TestBudgets`, with names exactly:
  - `link_delta_p95_ms` < 250
  - `stream_item_p95_ms` < 250
  - `command_result_p95_ms` < 250
  - `server_idle_rss_web_mb` < 100: one connected client, a 30 s settle, then the reading.

- [ ] **Step 1: Write the bench cases** (200 samples each):
  - `link_delta`: Engine commit to the client receiving the delta
  - `stream_item`: a fake-runtime line to the client receiving the item
  - `command_result`: `Reply` to its `CommandResult`
- [ ] **Step 2: Run** `make bench`. All four pass; record the baseline with `make bench-baseline`.
- [ ] **Step 3: Phone check, run by the owner:**
  1. `athena web setup` with the VPN host
  2. install the CA profile on the iPhone
  3. `athena web enroll` and scan the QR code
  4. sign in with Face ID; the placeholder shows the live open-Ticket count
  5. `athena task "..."`: the count changes within a second
  6. add to the home screen and allow notifications
  7. a fake-runtime Approval sends a push to the phone

  If the owner hasn't run it, record "not run" and the reason in the report, and ledger it. This is the only step that needs the owner.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `test(bench): link, stream and command latency budgets`.

---

## Deferred to later plans (decided here)

- **Plan 4:**
  - **TypeScript generation.** `protobuf-es` and `buf.gen.yaml`'s TS target come with the React app.
  - **The UI.** The placeholder pages are replaced. Plan 4's IndexedDB outbox resends unacknowledged commands by id; the server side of exactly-once is built here.
  - **Pause processing, Settings forms and router settings tools.** Plan 4 adds them on top of `SetPolicy` and the views here.
- **Plan 6:** nodes on this link, meaning mTLS with `(*CA).ClientCert`, enrollment join tokens, a SQLite outbox on nodes and the `BLOB` channel. Here the server and node share a process; `Node.Subscribe`/`Status`/`Interrupt` are the seam.
- **Downloads of Ticket files** for no-repo Tickets go over plain authenticated HTTPS GET in plan 4, not `BLOB`.
- **Plan 9:** the System and Performance pages' metric series, the audit log and alerts. Plan 3's `Health` covers the TLS, push, Sources, router and effects items only.
