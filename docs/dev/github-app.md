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

The Policy decides which GitHub accounts may start work and what happens to a message from them. Its first line is
the **accept mode**:

| Mode | Meaning |
| --- | --- |
| `off` | Athena ignores GitHub. |
| `observe` | **The default.** Athena reads, routes and records Decisions (`athena decisions` shows "would accept") but never starts work and never writes to GitHub. |
| `auto` | Athena accepts work from allowed accounts: it posts one acknowledgement, starts a Ticket and delivers a pull request. |

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
  means. `ci` has only the last cell.
- `rules`: named instructions for proactive work, each `{"name", "instructions", "enabled"}`.

Run in `observe` first. Mention the App in an issue of the sandbox repository, then read what Athena would have done:

    athena triggers          # what arrived
    athena threads           # per issue/PR: queue, eval, tickets
    athena decisions         # accept / stop / ignore, with reasons, "(would accept)" in observe

When the Decisions look right, **change `"mode"` to `"auto"` deliberately** and run `athena policy set` again. Do not
add logins you do not trust to `allowed`: in `auto` mode an allowed account that mentions the App makes Athena open
a worker session on your machine. Use `athena reevaluate <id>`, `athena dismiss <id>` and `athena unsubscribe <thread>`
to steer a single Decision or Thread.

You can also give Athena work yourself, in a configured repository, without a mention:

    athena task --repo yourlogin/sandbox "Fix the typo in README.md"

## Safety notes

- **The App is a separate identity from you.** Commits, pull requests and comments come from `<slug>[bot]`, with the
  bot's noreply commit email, not from your account. Its access is exactly the repositories you installed it on and the
  permissions above; revoke it from the installation page at any time. What it does is attributed to the bot, never to you.
- **The repository is public** (`danielpina1/athena`), and so are the issues, comments, branches and pull requests
  Athena writes on public repositories. Do not put secrets in task text, and install the App on private or sandbox
  repositories first. Anyone can mention the App, but only `allowed` accounts start work; for everyone else
  `admission` decides, and the default is to ignore them.
- **Observe is the default; `auto` is a choice.** Nothing starts work or writes to GitHub until you set `"mode": "auto"`.
- **Workers run as your OS user.** They have your files, your shell and your network. Athena confines them with rules
  (the Ticket's directory, blocked commands, no credential files, an Approval for anything else), but a script can do
  what a named path cannot, so the file-level isolation is **best effort**, not a security boundary. Harden it:
  - Turn on Claude Code's sandbox (`/sandbox` in Claude Code). Note that Athena starts workers with its own generated
    settings file and without your user settings, so check that the sandbox applies to workers before relying on it.
  - Optionally, run the server as a **dedicated, restricted OS user** that owns only `~/.athena` and its own `claude`
    login, and nothing else you care about (or a VM or container). This is the only complete answer.
- **A worker can obtain its own Assignment's token.** To push and open its pull request, a worker asks Athena for a
  token through a credential helper. It is short-lived (about an hour at most), limited to the single repository of the
  Ticket and to the worker permissions above, minted per Assignment, held in memory and never written to SQLite, logs,
  prompts or transcripts. A worker can therefore act on that repository as the bot until the token expires. It
  cannot reach your other repositories or your own credentials, and it cannot read the App's private key, which only the
  server reads.
- **The private key** stays `0600` in a directory only you can read. If it leaks, delete the key on the App's page and
  generate another; the installation tokens expire on their own.

## Troubleshooting

- `key file ... has mode 0644`: `chmod 600` the key.
- `the installation does not reach owner/name`: add the repository to the installation (**Install App** > **Configure**),
  or fix the `--repo` spelling.
- Nothing happens after a mention: `athena status` (is the source healthy?), `athena triggers`, `athena decisions`.
  In `observe` mode, "would accept" is the whole answer.
- A mention must name the App's slug as a whole word (`@athena-yourname`), outside quoted lines. The App's own comments
  never become Triggers.

## Live end-to-end test

The test creates an issue in a **sandbox repository** that mentions the App, waits for the acknowledgement and a pull
request from `athena/<ticket>` that adds `hello.txt`, merges that pull request through the API with the App's token,
waits for the Ticket to end `merged`, checks that no token or key reached `athena logs`, the transcripts or the server
log, and cleans up by closing the issue and deleting the branch. It uses the real `claude` (haiku), costs a few cents,
and merges a real pull request: point it only at a repository you do not mind changing.

It needs `claude` and `gh` (logged in as you, since the issue must come from you and not from the App) on `PATH`.
The server runs in a fresh temp state dir, configured from these variables; your own `~/.athena` is not used.

    ATHENA_LIVE_GITHUB=1 ATHENA_LIVE_REPO=yourlogin/sandbox \
    ATHENA_LIVE_APP_ID=123456 ATHENA_LIVE_APP_SLUG=athena-yourname \
    ATHENA_LIVE_APP_KEY=$HOME/.athena/github-app.pem ATHENA_LIVE_INSTALLATION=7654321 \
    go test -tags live -run LiveGitHub -v -timeout 25m ./internal/server

With `ATHENA_LIVE_GITHUB` or `ATHENA_LIVE_REPO` unset it skips with `set ATHENA_LIVE_GITHUB=1 and ATHENA_LIVE_REPO to
run`. The test saves its own Policy in the temp state dir (mode `auto`, your `gh` login allowed). Run it with the
default branch of the sandbox free of required checks or reviews, or the merge step will be refused.
