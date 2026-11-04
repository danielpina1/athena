# Dashboard setup

`athena server` can serve the dashboard over HTTPS for your phone and laptop: you sign in with a passkey, and the page
follows your Tickets live over one WebSocket. Plan 3 ships the endpoint, the login and push; the page itself is a
placeholder that signs in and shows a live count of open Tickets, which is enough to prove the whole path from a
phone. The full dashboard arrives in plan 4.

The endpoint is meant for a private network: your home LAN or, better, the VPN your devices use to reach the server.
It is off until you run `athena web setup`.

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
browser never sends to the server or writes to its history, and it is spent the moment the page starts enrolling: a
failed attempt needs a new link.

Then open `https://athena.home.arpa:8443/` and sign in with the passkey. The page says **Signed in. The dashboard
arrives in plan 4.** and counts your open Tickets live: submit a task with `athena task` and watch the count change.

`athena web passkeys` lists the enrolled passkeys.

## 5. Add it to the home screen

In Safari, **Share** > **Add to Home Screen** opens the dashboard as its own app. iOS delivers web push only to a site
added to the home screen, and only over a certificate it trusts: in mode `ca`, install and fully trust the CA first
(step 3a). Push subscription lives in plan 4's dashboard; the server side (stage-entry notifications for Tickets
waiting on you and for Approvals) runs already.

## Safety notes

- **Bind to the VPN address.** The listener is reachable by anything that can reach its port, and anyone who reaches it
  can try to sign in. `--listen 10.8.0.1:8443` keeps it off the LAN and the internet; `:8443` listens everywhere,
  and setup warns about it. Never forward the port on your router.
- **Passkeys only.** There are no passwords: a session starts with a passkey assertion with user verification (Face
  ID, Touch ID, a PIN), lasts 30 days unused and 90 days at most, and lives in an `HttpOnly`, `Secure`,
  `SameSite=Strict` cookie. Every login request and the WebSocket must come from the configured origin.
- **Step-up for sensitive changes.** Changing the Policy, revoking a passkey and changing push settings need a passkey
  assertion within the last 5 minutes (signing in counts); otherwise the dashboard answers `reauth_required` and asks
  for the passkey again. Approving a tool call is not sensitive.
- **A lost device:** `athena web passkeys`, then `athena web revoke <id>` (a prefix of the id is enough). With the
  server running, the device's open connections close at once; its sessions end either way.
- **Headers.** Every response carries a strict Content-Security-Policy (only the server's own scripts and styles, no
  framing), `Strict-Transport-Security` (over TLS), `X-Content-Type-Options: nosniff` and `Referrer-Policy:
  no-referrer`.
- **Secrets on disk:** the CA key (`pki/ca.key`), the server key (`tls/server.key`), the ACME account key and the
  push key (`push/vapid.key`) are 0600 in the 0700 state directory. Back the state directory up as a whole, and treat
  the backup as secret.

## For developers: the client contract

Plan 4's dashboard (and any other client) talks to `/ws` with subprotocol `athena.v1`: one protobuf `Frame`
(`proto/athena/v1`) per binary WebSocket message, at most 1 MiB, `permessage-deflate` without context takeover.
Unknown fields and a `Hello.protocol` other than 1 are refused with error code `invalid`. The placeholder in
`internal/web/assets/app.js` is a minimal working client.

- **Link sessions are per connection.** Each WebSocket is a fresh link session: durable frames are numbered from 1
  in each direction and acked cumulatively (`Frame.ack`; a `Ping` with `at_unix_ms` 0 is a standalone ack). The
  server ignores `Hello.last_seq` and replays nothing at the link level. Answer each `Ping` with a `Pong` carrying the
  same time; the server drops a peer silent for 45 s, and stops sending durable frames to one that never acks.
- **State is recovered through the hub cursor.** Send `Hello{protocol: 1, client_id, epoch, cursor}` with the epoch
  of the last `Welcome` and the highest `Delta.seq` (or `Snapshot.cursor`) applied. If the epoch matches and the
  cursor is still in the server's ring, `Welcome.snapshot_follows` is false and the missed Deltas follow; otherwise a
  `Snapshot` comes first and replaces the client's state. A `Resync` frame asks for a fresh Snapshot at any time.
  `client_id` is 1 to 64 of `[A-Za-z0-9._-]`, stable per device.
- **Commands are idempotent by id.** Keep every `Command` until its `CommandResult` arrives, and after a reconnect
  resend each one that has none, with the same `id`: the server answers a repeat from the stored result (kept 24 h)
  without acting twice. A result with code `reauth_required` is not stored: after the passkey step-up, resend the same
  command. Result codes are `unauthenticated`, `reauth_required`, `invalid`, `not_found`, `conflict`, `unavailable` and
  `internal`.
- **Credit is per connection.** Each connection starts with an implicit STREAM credit of 256 items; send
  `Credit{channel: STREAM, items}` (on CONTROL) as the client consumes items, after every reconnect too, since credit
  does not carry over. Without credit a stream keeps only its position and resumes with a `StreamGap` and the latest
  tail.
- **ticket_detail Deltas have seq 0.** They go only to connections that sent `WatchTicket`, live outside the ring and
  must not move the cursor or count in gap detection; re-send `WatchTicket` after a reconnect (the server sends the
  current detail at once).
- **Auth is HTTP.** `POST /auth/register/begin|finish`, `/auth/login/begin|finish`, `/auth/reauth/begin|finish`,
  `/auth/logout` and `GET /auth/session` take and return the WebAuthn JSON (base64url fields); every POST and the
  upgrade must carry `Origin` equal to the configured origin, or get 403. The connection closes with 4002 when its
  session ends (revoked, logged out, expired); 1001 means the server is shutting down: reconnect.
- **Push.** `GET /push/key` is the VAPID public key for `pushManager.subscribe`; `PushSubscribe`,
  `PushUnsubscribe` and `SetNotificationPrefs` are commands (sensitive). Report `Visibility{visible}` while the
  dashboard is on screen: no push is sent while a client reported visible in the last 30 s.
