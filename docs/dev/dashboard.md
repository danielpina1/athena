# Dashboard setup

`athena server` serves the dashboard over HTTPS for your phone and laptop: you sign in with a passkey, and the app
follows your Tickets live over one WebSocket. It is a PWA (the React app in `web/`, embedded in the binary), so on
an iPhone it installs from Safari's **Add to Home Screen** and can send push notifications.

The endpoint is meant for a private network: your home LAN or, better, the VPN your devices use to reach the server.
It is off until you run `athena web setup`.

## What the dashboard shows

| View | What it is for |
| --- | --- |
| **Needs you** | Everything waiting on you, one tap each: answer a question inline, approve or deny an Approval (with a note), review finished work, retry or stop a stalled Ticket. Approvals come first. |
| **Tickets** | The Tickets, filtered by stage, repository and model, with a search. A Ticket opens to its live session (streamed output, tool calls paired with their results, a reply box), its timeline, and its Changes (the diff) or Files. On a wide screen up to three sessions can be pinned side by side. |
| **New task** | A task in free text, an optional repository (a known one, a local git repository, or `owner/name`) and a model. |
| **Graph** | The Tickets as nodes in lanes by repository, orange where you are needed, with a list fallback and a time scrubber. |
| **Routing** | The Triggers, Threads and Decisions behind the Tickets. |
| **Nodes** | The nodes, their slots and load. |
| **Settings** | The Policy (with conflict handling when two screens edit it), Prompts, the Devices (passkeys, add a device, sign out), Notifications and the execution environment. |

Keyboard (desktop): `⌘K` command palette, `j`/`k` move, `⏎` open, `a` approve, `d` deny, `r` reply, `/` search,
`g n`, `g t`, `g g` go to Needs you, Tickets, Graph, `?` the list. The app works offline from its last state and sends
what you did once the link is back, exactly once.

## 1. Choose the host

The dashboard needs a DNS name, not a bare address: passkeys are bound to a name, and the certificate names it too.
Pick one your devices resolve to the server's VPN address. With a UniFi gateway, for example, add a local DNS record
in the UniFi Network application (a **Host (A)** entry; recent releases have it under **Settings** > **Routing** >
**DNS**): domain `athena.home.arpa`, IP address the server's VPN address (for example `10.8.0.1`). Make sure the VPN
hands its clients the gateway as their DNS server.

`home.arpa` is reserved for home networks, so it never clashes with a public name. Check from a device on the VPN:

    dscacheutil -q host -a name athena.home.arpa   # macOS
    getent hosts athena.home.arpa                   # Linux

## 2. Run `athena web setup`

    athena web setup --host athena.home.arpa --ip 10.8.0.1 --listen 10.8.0.1:8443

| Flag | Meaning |
| --- | --- |
| `--host` | The DNS name from step 1. Required. |
| `--ip` | An address the certificate also names (mode `ca`); repeat for more. Optional. |
| `--listen` | Where the server listens (default `:8443`, every interface). Prefer the VPN interface's address. |
| `--tls` | `ca` (default: Athena's own certificate authority), `acme` (a public certificate) or `off` (plain HTTP, only on a loopback address with host `localhost`, for development). |
| `--acme-email`, `--acme-provider`, `--acme-agree-tos` | Mode `acme`, see below. |

Setup writes the `web` part of `~/.athena/config.json` and enables it; running it again changes only the flags you
give. In mode `ca` it also creates the CA and prints its SHA-256 fingerprint. Restart `athena server` afterwards; the
log shows `dashboard serving` with the URL.

The resulting configuration:

    "web": {"enabled": true, "listen": "10.8.0.1:8443", "host": "athena.home.arpa", "ips": ["10.8.0.1"], "tls": "ca",
            "acme": {"agreeTos": false}}

`athena web status` shows the URL, the TLS mode, the certificate's expiry, the CA fingerprint, the passkeys and, while
the server runs, the connected clients. `GET /healthz` answers `ok` without a session, for a monitor.

## 3a. Mode `ca`: trust Athena's CA on each device

Athena signs the dashboard's certificate (valid 397 days, re-issued 30 days before it expires) with its own CA. The
CA is name-constrained to your host and the `--ip` addresses, so even a stolen CA key cannot vouch for any other site.
Each device must trust it once. `athena web ca` prints the fingerprint and the paths of `ca.pem` and the Apple
profile; the server also serves both at `/ca.pem` and `/ca.mobileconfig`.

Compare the fingerprint your device shows with `athena web ca`'s before you trust the CA.

**iPhone and iPad.** Open `https://athena.home.arpa:8443/ca.mobileconfig` in **Safari** (Safari warns that the site
is not trusted yet; that is expected, continue). Then:

1. **Settings** > **General** > **VPN & Device Management** > **Athena CA (athena.home.arpa)** > **Install**.
2. **Settings** > **General** > **About** > **Certificate Trust Settings** > turn on full trust for **Athena CA**.
   Without this step the profile is installed but Safari still refuses the certificate.

**macOS.**

    sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ~/.athena/pki/ca.pem

(Copy `ca.pem` to the Mac first if the server is another machine. Firefox keeps its own store: enable
`security.enterprise_roots.enabled` in `about:config`, or import `ca.pem` under **Settings** > **Certificates**.)

**Linux** (Debian, Ubuntu):

    sudo cp ca.pem /usr/local/share/ca-certificates/athena-ca.crt
    sudo update-ca-certificates

Chrome and Chromium on Linux read their own NSS store: `certutil -d sql:$HOME/.pki/nssdb -A -t C,, -n athena -i ca.pem`.

If you later change `--host` or `--ip`, setup refuses to reuse the CA made for the old ones and says how to start a
new one (move `~/.athena/pki` away); you then install the new profile on every device.

## 3b. Mode `acme`: a public certificate through DNS-01

When the host is a subdomain of a public domain you control (for example `athena.example.com`, still pointing at the
VPN address), Let's Encrypt can issue a certificate that every device already trusts. Athena proves the domain with a
DNS TXT record, so the server needs no inbound access from the internet.

    athena web setup --host athena.example.com --listen 10.8.0.1:8443 --tls acme \
        --acme-provider cloudflare --acme-email you@example.com --acme-agree-tos

`--acme-agree-tos` is your agreement to the certificate authority's terms
([Let's Encrypt's](https://letsencrypt.org/repository/)); without it no account is registered and
`athena web status` says so. The provider reads its credentials from the server's environment (lego's variables;
each also has a `_FILE` variant that names a file holding the value):

| Provider | Variables |
| --- | --- |
| `cloudflare` | `CLOUDFLARE_DNS_API_TOKEN` (a token with Zone:DNS:Edit on the zone), or `CLOUDFLARE_EMAIL` + `CLOUDFLARE_API_KEY` |
| `exec` | `EXEC_PATH`: a program called as `present <fqdn> <value>` and `cleanup <fqdn> <value>`; `EXEC_MODE=RAW` passes domain, token and key instead |
| `httpreq` | `HTTPREQ_ENDPOINT` (your service's URL), optionally `HTTPREQ_USERNAME`, `HTTPREQ_PASSWORD` and `HTTPREQ_MODE=RAW` |

Optional in `config.json`: `web.acme.resolvers` (recursive resolvers to check the TXT record with, when the local one
answers for a private view of the zone) and `web.acme.directory` (another ACME CA, or Let's Encrypt's staging
directory `https://acme-staging-v02.api.letsencrypt.org/directory` while you try it out).

The server starts without a certificate and gets one in the background; it renews 30 days before expiry and keeps
serving the old one through failures. **Verify it once by hand** after the first start:

    athena web status                          # TLS: acme: certificate valid until ...
    curl -sv https://athena.example.com:8443/healthz 2>&1 | grep -E 'issuer|subject|^ok'

The issuer should be Let's Encrypt (or your CA) and the answer `ok`. A failure is in the server log (entries with
`"component":"web"`) and in `athena web status`.

## 4. Enroll a device: `athena web enroll`

On the server:

    athena web enroll

It prints a single-use link, good for 15 minutes (`--ttl` up to 24h), and the same link as a QR code. Scan it with the
phone's camera (or open the link on the laptop) and create the passkey; the device's passkey manager keeps it
(iCloud Keychain, Google Password Manager, a security key). The token travels in the link's fragment, which the
browser never sends to the server; the page replaces the address in place (dropping the fragment) before it makes any
request, and the token is spent the moment the page starts enrolling: a failed attempt needs a new link.

Then open the dashboard's address (for example `https://athena.home.arpa:8443/`) and sign in with the passkey: the
app opens on **Needs you**, and a Ticket you start with `athena task` shows up in **Tickets** at once. A link that was
already used or has expired says so and shows `athena web enroll` to run for a new one.

`athena web passkeys` lists the enrolled passkeys.

## 5. Add it to the home screen

In Safari, **Share** > **Add to Home Screen** opens the dashboard as its own app. iOS delivers web push only to a site
added to the home screen, and only over a certificate it trusts: in mode `ca`, install and fully trust the CA first
(step 3a). Then open the app from the home screen and choose **On** under **Settings** > **Notifications**:
stage-entry notifications for Tickets waiting on you and for Approvals arrive as push.

## 6. Local repositories for New task

New task lists your local git repositories under **Local repositories**, besides the ones Tickets already used. The
server looks for them in the folders of `localRepoRoots` in `~/.athena/config.json` (restart the server after a
change):

```json
{
  "localRepoRoots": ["~/code", "/srv/git"]
}
```

| Field | Meaning |
| --- | --- |
| `localRepoRoots` | Absolute paths, or paths starting with `~/`. Each folder, its children and its grandchildren are searched for a git repository (a `.git` directory or file), not below a repository found, skipping hidden folders, `node_modules`, `vendor`, symbolic links and Athena's state directory. A root that is itself a symbolic link is resolved once, and the repositories are listed by their resolved paths. Left out, it is the existing ones of `~/Developer`, `~/code`, `~/src`, `~/repos`, `~/Projects` and `~/Desktop`; `[]` turns the listing off. At most 200 repositories, in at most 5 seconds a scan; a scan cut short by the time keeps the last complete list (the server log says so once). The server searches in the background at start and every 3 minutes. |

The dashboard shows these paths only to the signed-in owner. Typing any absolute path in New task still works.

## Safety notes

- **Bind to the VPN address.** The listener is reachable by anything that can reach its port, and anyone who reaches it
  can try to sign in. `--listen 10.8.0.1:8443` keeps it off the LAN and the internet; `:8443` listens everywhere,
  and setup warns about it. Never forward the port on your router.
- **Passkeys only.** There are no passwords: a session starts with a passkey assertion with user verification (Face
  ID, Touch ID, a PIN), lasts 30 days unused and 90 days at most, and lives in an `HttpOnly`, `Secure`,
  `SameSite=Strict` cookie. Every login request and the WebSocket must come from the configured origin.
- **Step-up for sensitive changes.** Changing the Policy, changing or resetting a prompt part, revoking a passkey and changing push settings need a passkey
  assertion within the last 5 minutes (signing in counts); otherwise the dashboard answers `reauth_required` and asks
  for the passkey again. Approving a tool call is not sensitive.
- **A lost device:** `athena web passkeys`, then `athena web revoke <id>` (a prefix of the id is enough). With the
  server running, the device's open connections close at once; its sessions end either way, and the push
  subscriptions those sessions made go with them, so the device stops receiving notifications. Logging out, and a
  session expiring, remove that session's push subscriptions the same way; sign in and subscribe again to get them
  back.
- **Headers.** Every response carries a strict Content-Security-Policy (only the server's own scripts and styles, no
  framing), `Strict-Transport-Security` (over TLS), `X-Content-Type-Options: nosniff` and `Referrer-Policy:
  no-referrer`.
- **Secrets on disk:** the CA key (`pki/ca.key`), the server key (`tls/server.key`), the ACME account key and the
  push key (`push/vapid.key`) are 0600 in the 0700 state directory. Back the state directory up as a whole, and treat
  the backup as secret.

## Phone check

Run it once after a release that changes the dashboard. The owner's setup is the primary path: the server is reached
at a public name, `app.athena-ops.com`, whose DNS points at the server's VPN address, with a Let's Encrypt
certificate obtained by Cloudflare DNS-01 (mode `acme`, section 3b). The phone reaches it over the UniFi VPN, so no CA
profile is needed on it.

    athena web setup --host app.athena-ops.com --listen <vpn-ip>:8443 --tls acme \
        --acme-provider cloudflare --acme-email <you@example.com> --acme-agree-tos

The server reads the Cloudflare token from the file `CLOUDFLARE_DNS_API_TOKEN_FILE` names (a file with mode 0600
that holds the token; never paste the token into a command line, a shell history, a config file or the repository).

Checklist, on the iPhone with the VPN on:

- [ ] 1. `make build`, then run the setup above (once), then restart the server. `athena web status` shows a valid
  Let's Encrypt certificate, and `https://app.athena-ops.com:8443/healthz` answers `ok` in Safari.
- [ ] 2. On the Mac run `athena web enroll`, scan the QR code with the phone's camera and create the passkey with
  Face ID.
- [ ] 3. In Safari, **Share** > **Add to Home Screen**, then open Athena from the home screen icon and sign in.
- [ ] 4. **Settings** > **Notifications**, choose **On** (enable them), and allow notifications when iOS asks.
- [ ] 5. Start a task from the phone, watch it live, reply to it, then approve an Approval from the lock-screen push.
- [ ] 6. On the Mac open the same dashboard: an action on one screen shows on the other in under a second.
- [ ] 7. Turn on Airplane mode in the middle of an approve, then turn it off: the approval happens exactly once.

**Alternative: Athena's own CA.** Without a public name, use `athena web setup --host <vpn-name> --ip <vpn-ip>
--listen <vpn-ip>:8443` (mode `ca`), restart the server, and before step 2 install the CA profile on the phone and turn
on full trust for it (section 3a). The other steps are the same.

## For developers: the client contract

The dashboard (and any other client) talks to `/ws` with subprotocol `athena.v1`: one protobuf `Frame`
(`proto/athena/v1`) per binary WebSocket message, at most 1 MiB, `permessage-deflate` without context takeover.
Unknown fields and a `Hello.protocol` other than 1 are refused with error code `invalid`. The dashboard in `web/`
is the reference client.

- **Link sessions are per connection.** Each WebSocket is a fresh link session: durable frames are numbered from 1
  in each direction and acked cumulatively (`Frame.ack`; a `Ping` with `at_unix_ms` 0 is a standalone ack). The
  server ignores `Hello.last_seq` and replays nothing at the link level. Answer each `Ping` with a `Pong` carrying the
  same time; the server drops a peer silent for 45 s, and stops sending durable frames to one that never acks.
- **State is recovered through the hub cursor.** Send `Hello{protocol: 1, client_id, epoch, cursor}` with the epoch
  of the last `Welcome` and the highest `Delta.seq` (or `Snapshot.cursor`) applied. If the epoch matches and the
  cursor is still in the server's ring, `Welcome.snapshot_follows` is false and the missed Deltas follow; otherwise a
  `Snapshot` comes first and replaces the client's state. A `Resync` frame asks for a fresh Snapshot at any time.
  `client_id` is 1 to 64 of `[A-Za-z0-9._-]`, stable per device.
- **Commands are idempotent by id.** A command id is 1 to 128 bytes (longer or empty is refused `invalid`). Keep every
  `Command` until its `CommandResult` arrives, and after a reconnect resend each one that has none, with the same
  `id`: the server answers a repeat from the stored result (kept 24 h) without acting twice. A result is stored once
  the command acted, whether it succeeded or was refused for its content (`invalid`, `not_found`, `conflict`). Four
  codes mean nothing was done and are never stored: `reauth_required`, `unauthenticated`, `unavailable` and
  `internal`. Resend the same command, same `id`, after fixing the cause (the passkey step-up, signing in again, less
  load, a server fix). Reads (`ListPasskeys`) are not stored either. Result codes are `unauthenticated`,
  `reauth_required`, `invalid`, `not_found`, `conflict`, `unavailable` and `internal`.
- **Credit is per connection.** Each connection starts with an implicit STREAM credit of 256 items; send
  `Credit{channel: STREAM, items}` (on CONTROL) as the client consumes items, after every reconnect too, since credit
  does not carry over. Without credit a stream keeps only its position and resumes with a `StreamGap` and the latest
  tail.
- **ticket_detail Deltas have seq 0.** They go only to connections that sent `WatchTicket`, live outside the ring and
  must not move the cursor or count in gap detection; re-send `WatchTicket` after a reconnect (the server sends the
  current detail at once).
- **Auth is HTTP.** `POST /auth/register/begin|finish`, `/auth/login/begin|finish`, `/auth/reauth/begin|finish`,
  `/auth/logout` and `GET /auth/session` take and return the WebAuthn JSON (base64url fields); every POST and the
  upgrade must carry `Origin` equal to the configured origin, or get 403.
- **Ticket diffs and files are HTTP.** Three `GET` routes need a session (else 401 `{"code":"unauthenticated"}`) and
  answer with `Cache-Control: no-store`. A Ticket that is unknown, not placed, or of the other kind, and a file that
  is not there or may not be read, give 404 `{"code":"not_found"}`; any other failure gives 500 `{"code":"internal"}`.
  - `/api/tickets/{id}/diff` (a Ticket in a git repository): `{"base","branch","files":[{"path","oldPath"?,"status",
    "added","deleted","binary"}],"patch","truncated"}`. `branch` is `athena/<lowercased id>`; `base` is the
    merge-base with `origin/HEAD`, else with the repository's `HEAD`. `status` is `A`, `M`, `D`, `R`, `C` or `T`.
    `patch` is cut at 2 MiB. `truncated` is set when the patch was cut, and also when the file list was (git's
    file-list output passed 4 MiB: the files listed carry real counts, the rest are missing).
  - `/api/tickets/{id}/files` (a Ticket without a repository): `{"files":[{"path","size","modTime"}],"truncated"}`,
    at most 1000 regular files in at most 2000 directories (and 10 s of walking), breadth-first, sorted by path;
    `truncated` says a cap was hit. `.athena/` (any letter case), symlinks and hard-linked files are left out.
  - `/api/tickets/{id}/files/{path...}`: the file as an attachment (`Content-Type: application/octet-stream`,
    `X-Content-Type-Options: nosniff`). Paths outside the Ticket's directory, under `.athena/` (any letter case), through a symlink, or to a
    hard-linked file are 404.
- **Close codes.** The server closes a connection with one of these:

  | Code | Meaning | Client action |
  | --- | --- | --- |
  | 1001 | The server is shutting down. | Reconnect with the cursor. |
  | 1011 | The server failed: it could not build or send a Snapshot, or could not send a frame; an `internal` Error usually comes first. | Reconnect with the cursor; report it if it repeats. |
  | 1013 | The client is too far behind (its queue or unacked frames hit the bound). | Reconnect with the cursor: it gets the missed Deltas, or a Snapshot. |
  | 4000 | The peer was silent for 45 s (no frame, no `Pong`). | Reconnect. |
  | 4001 | Protocol violation: a bad or late `Hello`, an undecodable frame (an `invalid` Error is sent first, best effort) or a sequence gap. | Fix the client; do not retry blindly. |
  | 4002 | The login behind the connection ended (revoked, logged out, expired). | Sign in again, then reconnect and resend commands without a result. |
- **Prompts.** Prompt overrides: `GetPrompt` (a read, not stored) returns a `PromptDetail`; `SetPrompt` and `ResetPrompt` (sensitive) carry the `PromptsView.revision` as `base_revision`, which is one counter for all parts (stale: `conflict`; empty or over 64 KiB text: `invalid`; unknown part: `not_found`). A new session starts with the effective prompt, and a session resumed while its Ticket is open keeps the prompt it started with, and a reopened ended Ticket gets the current effective prompt (kept in the state directory, outside the sandbox's write set; with the sandbox off, or an owner `allowWrite` that covers the state directory, a worker could rewrite its kept prompt).
- **Push.** `GET /push/key` is the VAPID public key for `pushManager.subscribe`; `PushSubscribe`,
  `PushUnsubscribe` and `SetNotificationPrefs` are commands (sensitive). Report `Visibility{visible}` while the
  dashboard is on screen: no push is sent while a client reported visible in the last 30 s.

## Working on the UI

The dashboard is a Vite + React app in `web/`; `make web` builds it into `internal/web/ui/dist/`, which the Go binary
embeds (the directory is git-ignored; without a build the server shows "Dashboard not built: run make web").

1. `make web` installs the packages (`npm ci`, only when `web/node_modules` is missing) and builds.
2. For fast iteration, run a server with TLS off on loopback (`athena web setup --tls off --host localhost --listen
   127.0.0.1:8443`, then `athena server`), and in another terminal `npm --prefix web run dev`. Vite serves the app
   with hot reload and proxies `/ws`, `/auth`, `/push` and `/api` to `ATHENA_DEV_SERVER` (default
   `http://localhost:8443`), rewriting `Origin` to that server's. Passkey ceremonies are bound to the server's origin, so sign in once at
   `http://localhost:8443` (the built app), then open the Vite dev server at `http://localhost:5173`: the session
   cookie is not port-scoped, so the proxy reuses it.
3. `make web-check` runs typecheck, lint, unit tests, the build and the bundle budget; `make check` includes it.
   `make e2e` runs the browser tests (Playwright against a real server built into `bin/e2e/`, never `bin/athena`, with a
   fake runtime), including `full-flow.spec.ts`, the whole path from sign-in to sign-out on a phone and a desktop viewport.

Browser enrollment is the dashboard's `/enroll` page: it reads the token from the link's fragment, removes the
fragment from the address, asks for a device name, creates the passkey (`/auth/register/begin|finish`) and signs in. A
spent or expired link shows "This link was already used or has expired" and has no retry; run `athena web enroll` again.

The TypeScript types in `web/src/gen/` are generated from `proto/` by `make proto` together with the Go code; commit
both. Mockups of the design are in `docs/design/mockups/`.
