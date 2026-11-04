# GitHub App setup

Athena works on GitHub as a **GitHub App**: it polls the repositories you choose, a router session (haiku by default)
decides what each mention or comment means, and workers deliver pull requests as the App's bot, with a short-lived
token for one repository per Assignment. This page takes you from nothing to a working installation. It needs a GitHub
account, a few minutes in the browser, and the `athena` binary (`make build`).

Athena never receives webhooks: the server is not reachable from GitHub, so it polls every repository (30 s by
default) with conditional requests.

## 1. Create the App

GitHub: **Settings** (your personal account, or **Your organizations** > the organization > **Settings**) >
**Developer settings** > **GitHub Apps** > **New GitHub App**. Fill in:

| Field | Value |
| --- | --- |
| GitHub App name | A name unique across GitHub, at most 34 characters, for example `athena-yourname`. Its URL name (the *slug*, shown in the App's page URL `github.com/apps/<slug>`) is what people mention: `@athena-yourname`. The bot account is `<slug>[bot]`. |
| Homepage URL | Any URL; the repository URL (`https://github.com/danielpina1/athena`) will do. |
| Webhook > **Active** | **Unchecked.** Athena polls; leave the webhook URL and secret empty. |
| Where can this GitHub App be installed? | **Only on this account.** |

### Repository permissions

Under **Repository permissions**, set exactly these, and leave every other permission (repository, organization and
account) at *No access*:

| Permission | Access |
| --- | --- |
| Actions | Read-only |
| Checks | Read-only |
| Commit statuses | Read-only |
| Contents | Read and write |
| Issues | Read and write |
| Metadata | Read-only (mandatory; GitHub sets it) |
| Pull requests | Read and write |

Do not subscribe to any event. Click **Create GitHub App**.

What each is for: *Contents* lets a worker push its `athena/<ticket>` branch; *Pull requests* and *Issues* let Athena
open the pull request and post the acknowledgement, answer, result and attention comments; *Checks*, *Actions* and
*Commit statuses* let it read CI results; *Metadata* is how GitHub lets an App see the repository at all. A worker's
own token carries only contents, pull requests and issues write, checks, actions and metadata read, on one
repository.

### Write down the ids and download the key

On the App's page:

- **App ID** (a number near the top). This is `--app-id`.
- The **slug** is the last part of the App's public URL. This is `--slug`.
- Scroll to **Private keys** (in the left sidebar: **Credentials** > **Key pairs**) and click **Generate a private key**
  / **New key**. The browser downloads a `.pem` file.

Move the key where only you can read it:

    mkdir -p ~/.athena && chmod 700 ~/.athena
    mv ~/Downloads/<your-app>.*.private-key.pem ~/.athena/github-app.pem
    chmod 600 ~/.athena/github-app.pem

Athena refuses a key file that group or others can read. The key is read only by the server; its *path* is what goes
into the configuration.

## 2. Install the App

On the App's page, **Install App** > your account > **Only select repositories**, and pick the repositories Athena may
work in. Start with a sandbox repository you do not mind being edited. Finish with **Install**.

The URL you land on ends in the **installation id**: `https://github.com/settings/installations/<installation id>`
(for an organization: `github.com/organizations/<org>/settings/installations/<id>`). This is `--installation`.

## 3. Configure Athena

    athena github setup --app-id 123456 --slug athena-yourname --key ~/.athena/github-app.pem \
        --installation 7654321 --repo yourlogin/sandbox

Repeat `--repo owner/name` for more repositories (each must be one the installation reaches). This writes
`~/.athena/config.json`, mode 0600, with the key's path, never the key. Add `--state-dir DIR` to every command if you
use another state directory. Add `--interval SECONDS` to poll more or less often than every 30 s.

By default Athena takes in only the messages that mention the App, follow-ups on its own issues and pull requests,
and CI on the pull requests it follows. To let it see every message of a repository (every issue, pull request,
comment and check suite, mentioned or not), add `--receive-all owner/name` (repeatable; each must also be a
`--repo`). It is the `"receiveAll"` list in the `github` part of `config.json`, empty by default. Only those
repositories reach the Policy's unaddressed admission cells and its `ci` row (section 4); a message of any other
repository that does not mention the App is recorded as `not_received`.

Check it. This talks to GitHub: it mints an installation token, reads the bot and the installation's account, and
lists the repositories the installation reaches. Every configured repository must be among them.

    athena github check
    bot:          athena-yourname[bot]
    account:      yourlogin
    repositories:
      yourlogin/sandbox

Then start (or restart) the server, which reads the configuration when it starts:

    athena server

`athena status` shows the GitHub source and the router, with the remaining rate limit. If the check fails when the
server starts, the server keeps running without the GitHub source and retries in the background (30 s, doubling to
10 min); `athena status` says why.

## 4. The Policy: who may start work, and what Athena does with it

The Policy decides which GitHub accounts may start work and what happens to a message from them.

**Explicit requests start work in every mode.** An account on `allowed` that mentions the App, an allowed account's
follow-up on an issue or pull request Athena works on, and a mention that an `unlistedAddressed: "explicit"` cell
admits are accepted whatever the mode, `observe` and `off` included: Athena posts its acknowledgement, starts a worker
session on your machine and opens a pull request.

The **accept mode** (the Policy's first line) governs everything else: the *inferred* (proactive) candidates, such as
a message that does not mention the App, from an account the admission table lets in, or a follow-up from an account
not on the list.

| Mode | Inferred candidates |
| --- | --- |
| `off` | Not admitted: Athena records them as ignored. |
| `observe` | **The default.** Routed and recorded as Decisions (`athena decisions` shows "would accept"); never started. |
| `auto` | Accepted when every rule the router matched is enabled: one acknowledgement, a Ticket and a pull request. |

Until you save a Policy, the mode is `observe` and **Allowed** holds the installation's account (you, for a personal
installation). Look at it, edit it, save it:

    athena policy show > policy.json
    $EDITOR policy.json
    athena policy set policy.json

`policy set` replaces the whole document, validates it, and tells the open Tickets the change affects. An unknown
field is refused. The shape, with your login in `allowed`:

```json
{
  "mode": "observe",
  "rules": [],
  "sources": {
    "github": {
      "allowed": ["your-github-login"],
      "bots": [],
      "admission": {
        "issues":   {"listedUnaddressed": "inferred", "unlistedAddressed": "ignore", "unlistedUnaddressed": "ignore"},
        "pulls":    {"listedUnaddressed": "inferred", "unlistedAddressed": "ignore", "unlistedUnaddressed": "ignore"},
        "comments": {"listedUnaddressed": "inferred", "unlistedAddressed": "ignore", "unlistedUnaddressed": "ignore"},
        "ci":       {"unlistedUnaddressed": "ignore"}
      }
    }
  }
}
```

- `allowed`: the GitHub logins that may start work (the *list*). Put your own login here.
- `bots`: bot logins whose messages may become candidates for routing.
- `admission`: one row per receiving scope (`issues`, `pulls`, `comments`, `ci`), with a choice for each case: a
  listed account that does not mention the App (`listedUnaddressed`: `inferred` or `ignore`), an account not on the list
  that mentions it (`unlistedAddressed`: `explicit`, `inferred` or `ignore`), and an account not on the list that does
  not (`unlistedUnaddressed`: `inferred` or `ignore`). The values above are the defaults and are what an empty cell
  means. `ci` has only the last cell. The unaddressed cells and the `ci` row apply only to repositories on
  `receiveAll` (section 3); elsewhere such a message is `not_received`.
- `rules`: named instructions for proactive work, each `{"name", "instructions", "enabled"}`.

To watch Athena before it acts, run in `observe` with nobody on the list, so that no message is an explicit request.
Save a Policy with `"allowed": []` and `"unlistedAddressed": "inferred"` in the `issues` and `comments` rows, mention
the App in an issue of the sandbox repository, then read what Athena would have done:

    athena triggers          # what arrived
    athena threads           # per issue/PR: queue, eval, tickets
    athena decisions         # accept / stop / ignore, with reasons, "(would accept)" in observe

A mention from an account on `allowed` is not a test: it is a real request in every mode, and Athena starts the work
and writes to GitHub.

When the Decisions look right, put your login back in `allowed`, set the cells back, and choose the mode: stay in
`observe` to have Athena act only on explicit requests, or **change `"mode"` to `"auto"` deliberately** to let
proactive work through too. Run `athena policy set` again. Do not add logins you do not trust to `allowed`: in every
mode an allowed account that mentions the App makes Athena open a worker session on your machine. Use `athena
reevaluate <id>`, `athena dismiss <id>` and `athena unsubscribe <thread>` to steer a single Decision or Thread.

You can also give Athena work yourself, in a configured repository, without a mention:

    athena task --repo yourlogin/sandbox "Fix the typo in README.md"

## 5. The worker sandbox

Athena turns on Claude Code's sandbox for every worker session on macOS and Linux. Workers start with
`--settings <generated file> --setting-sources ""`, so your own `/sandbox` or `settings.json` sandbox setting never
reaches them; the generated `.athena/settings.json` carries the sandbox block instead. A worker's Bash commands, and
every process they start (git, gh, scripts, builds), may then:

- **write** only the Ticket's directory, the repository's git directory (so commits work in the worktree), the
  sandbox's own temporary directory (`$TMPDIR`) and the paths you add. The git directory's `config` and `hooks` and
  Athena's own `.athena/settings.json`, `.athena/mcp.json` and `.athena/.gitignore` stay read-only, even inside those.
- **read** everything your user can, except the GitHub App's private key.
- **connect** only to GitHub (`github.com`, `*.github.com`, `*.githubusercontent.com`), the hosts you add, and the
  node socket. `athena git-credential` (git's credential helper) and `athena gh` reach Athena through that socket, so
  pushing and opening the pull request keep working. Any other host is refused, never asked about.

A worker cannot ask to run a command outside the sandbox (Claude Code's `dangerouslyDisableSandbox` is ignored), and a
sandboxed command still goes through Athena's rules and Approvals as before. The sandbox covers commands only: Claude
Code's own Read, Edit, Write and WebFetch tools, the hooks and Athena's MCP server run outside it, and Athena's rules
are what confine those.

Configure it in the `sandbox` part of `~/.athena/config.json` (every field is optional; restart the server after a
change). `athena github setup` keeps this part when it rewrites the file.

```json
{
  "sandbox": {
    "enabled": true,
    "required": false,
    "allowedDomains": ["proxy.golang.org", "sum.golang.org", "registry.npmjs.org"],
    "allowWrite": ["/Users/you/Library/Caches/go-build", "/Users/you/go/pkg/mod"],
    "weakerNetworkIsolation": false,
    "caFile": ""
  }
}
```

| Field | Meaning |
| --- | --- |
| `enabled` | Unset: on for macOS and Linux. `false` turns it off; the server log then warns at every start. |
| `required` | `true` makes a worker fail to start when the sandbox cannot start (on Linux: `bubblewrap` or `socat` missing). By default Claude Code warns and runs the commands **unsandboxed**; set it once the sandbox works on your machine. |
| `allowedDomains` | Hosts reachable besides GitHub: package registries and module proxies your projects' builds and tests download from. `*.example.com` matches the subdomains. |
| `allowWrite` | Absolute paths writable besides the Ticket's directory, such as build and module caches in your home directory (`go build` writes `~/Library/Caches/go-build` on macOS, `~/.cache/go-build` on Linux). |
| `weakerNetworkIsolation` | macOS: lets commands reach the `trustd` service. `gh` (and other Go programs) need it to verify GitHub's certificate behind a TLS-inspecting proxy whose CA is in the keychain. It opens a path out of the sandbox through that service. |
| `caFile` | Absolute path to a PEM bundle that workers' git, curl and OpenSSL verify TLS with (`SSL_CERT_FILE`, `GIT_SSL_CAINFO`, `CURL_CA_BUNDLE`). Inside the sandbox git and curl cannot use the keychain, so behind a TLS-inspecting proxy they need the proxy's CA in a file. |

**Behind a TLS-inspecting proxy** (a corporate secure web gateway that re-signs HTTPS with its own CA), a sandboxed
worker's `git push` fails with `SSL certificate problem: unable to get local issuer certificate` and `gh` cannot
verify GitHub either. Export the system roots plus the proxy's CA into one bundle your user can read, and set both
fields:

    { cat /etc/ssl/cert.pem; security find-certificate -a -p -c "<the CA's name>" /Library/Keychains/System.keychain; } \
        > ~/.athena/ca.pem

    "sandbox": {"caFile": "/Users/you/.athena/ca.pem", "weakerNetworkIsolation": true}

**On Linux** the sandbox needs `bubblewrap` and `socat` installed. Linux cannot allow one Unix socket by path, so for
workers to reach the node socket Athena lets their commands connect to **any** Unix socket (the Docker socket or an
SSH agent, if one is reachable); macOS allows the node socket alone.

## Safety notes

- **The App is a separate identity from you.** Commits, pull requests and comments come from `<slug>[bot]`, with the
  bot's noreply commit email, not from your account. Its access is exactly the repositories you installed it on and the
  permissions above; revoke it from the installation page at any time. What it does is attributed to the bot, never to
  you.
- **The repository is public** (`danielpina1/athena`), and so are the issues, comments, branches and pull requests
  Athena writes on public repositories. Do not put secrets in task text, and install the App on private or sandbox
  repositories first. Anyone can mention the App, but only `allowed` accounts start work; for everyone else
  `admission` decides, and the default is to ignore them.
- **Observe is the default; `auto` is a choice.** Observe holds back proactive work only. An explicit request (an
  allowed account that mentions the App, or a cell set to `explicit`) starts work and writes to GitHub in every mode;
  keep `allowed` to accounts you trust.
- **Workers run as your OS user.** They have your files, your shell and your network, confined by two layers, and
  neither is a security boundary on its own:
  - **Athena's rules** (the Ticket's directory, blocked commands, no credential files, an Approval for anything else)
    govern every tool call, including Claude Code's own Read, Edit and Write tools. They judge named paths and parsed
    commands, and a script can do what a named path cannot, so they are **best effort**.
  - **Claude Code's sandbox** (section 5, on by default on macOS and Linux) confines what a worker's commands actually
    do: writes to the worktree and its git directory, reads of everything but the App key, connections to GitHub and
    your listed hosts. It does not cover Claude Code's own tools, which run outside it, and unless `required` is set a
    machine where it cannot start runs the commands unsandboxed with only a warning. The server log says
    `claude code sandbox is on for workers` at every start.
  - For a real boundary, run the whole server as a **dedicated, restricted OS user** that owns only `~/.athena` and its
    own `claude` login and nothing else you care about, or in a **VM or container**.
- **A worker can obtain its own Assignment's token.** To push and open its pull request, a worker asks Athena for a
  token through a credential helper. It is short-lived (about an hour at most), limited to the single repository of the
  Ticket and to the worker permissions above, minted per Assignment, held in memory and never written to SQLite, logs,
  prompts or transcripts. When the worker's session ends, Athena drops the token and revokes it with GitHub (best
  effort: if the revocation fails, the token still expires on its own). A worker can therefore act on that repository
  as the bot while its session runs, and a copy it kept until the revocation or, at worst, the expiry.
- **The private key is not out of a worker's reach.** The sandbox denies workers' commands the key file, and Athena's
  rules deny Claude Code's Read tool the credential paths (the key, your `~/.ssh`, `~/.claude` and the like). But a
  worker runs as your OS user, as the server does, and the rules are not a boundary: with the sandbox off, or through
  a tool the sandbox does not cover, a determined worker can read any 0600 file that user can read, the App's private
  key included. The restricted-user option keeps *your other* files, keys and logins away from workers, but not the
  App key itself, which the server (and so that user) must read. Limit the damage: install the App only on repositories
  you can afford to have written to, and if you suspect a leak delete the key on the App's page and generate another;
  installation tokens expire on their own. Keep the key `0600` in a directory only that user can read.

## Troubleshooting

- `key file ... has mode 0644`: `chmod 600` the key.
- `the installation does not reach owner/name`: add the repository to the installation (**Install App** >
  **Configure**), or fix the `--repo` spelling.
- Nothing happens after a mention: `athena status` (is the source healthy?), `athena triggers`, `athena decisions`.
  In `observe` mode, "would accept" on an inferred candidate is the whole answer.
- A worker's `git push` fails with `SSL certificate problem`: a TLS-inspecting proxy; set `caFile` and
  `weakerNetworkIsolation` (section 5).
- A worker's build or test fails with `Operation not permitted` or `CONNECT tunnel failed, response 403`: the sandbox
  refused a write or a host; add the cache path to `allowWrite` or the host to `allowedDomains` (section 5).
- A mention must name the App's slug as a whole word (`@athena-yourname`), outside quoted lines. The App's own comments
  never become Triggers.

## Live end-to-end test

The test creates an issue in a **sandbox repository** that mentions the App, waits for the acknowledgement and a pull
request from `athena/<ticket>` that adds a file `hello-<unix time>.txt` (a new name each run, so a second run still
produces a diff), merges that pull request through the API with the App's token, waits for the Ticket to end
`merged`, checks that no token or key reached `athena logs`, the transcripts or the server log, and cleans up by
closing the issue and deleting the branch. It uses the real `claude` (haiku), costs a few cents, and merges a real
pull request: point it only at a repository you do not mind changing.

It needs `claude` and `gh` (logged in as you, since the issue must come from you and not from the App) on `PATH`.
The worker runs sandboxed, as by default; behind a TLS-inspecting proxy add `ATHENA_LIVE_SANDBOX_CA_FILE=<bundle>`
(section 5), which the test puts in `caFile` with `weakerNetworkIsolation`.
The server runs in a fresh temp state dir, configured from these variables; your own `~/.athena` is not used.

    ATHENA_LIVE_GITHUB=1 ATHENA_LIVE_REPO=yourlogin/sandbox \
    ATHENA_LIVE_APP_ID=123456 ATHENA_LIVE_APP_SLUG=athena-yourname \
    ATHENA_LIVE_APP_KEY=$HOME/.athena/github-app.pem ATHENA_LIVE_INSTALLATION=7654321 \
    go test -tags live -run LiveGitHub -v -timeout 30m ./internal/server

With `ATHENA_LIVE_GITHUB` or `ATHENA_LIVE_REPO` unset it skips with `set ATHENA_LIVE_GITHUB=1 and ATHENA_LIVE_REPO to
run`. When `ATHENA_LIVE_GITHUB=1` is set, a missing `claude`, `gh` or App variable fails the test instead of skipping
it. The whole test (server start, acknowledgement, pull request, merge and the Ticket's end) shares one 25-minute
deadline, inside the documented `-timeout 30m`. The test saves its own Policy in the temp state dir (mode `auto`, your
`gh` login allowed). Run it with the default branch of the sandbox free of required checks or reviews, or the merge
step will be refused.
