# agent-shift — setup & usage

`agent-shift` is a small daemon that connects **your Slack account** (not a bot) to **Claude Code** or
**Codex** running on your machine.

- You define **agents**: a trigger (workspace + channel/DM/group DM + sender + text filter), a system
  prompt, an engine (Claude Code or Codex) and a working directory.
- When a **top-level** message matches an agent, agent-shift starts a new CLI session in that directory
  and hands it the message.
- Every later **reply and reaction in that message's thread** is steered into the same session —
  even while it is working.
- The agent talks back through Slack tools (reply, send to anyone, read threads/channels, search,
  download/upload files, react, edit). Messages go out from your account with an
  `Assisted by Claude` / `Assisted by Codex` footer.

The CLIs run with their own logins, so your Claude / ChatGPT subscriptions are used. Slack is read by
**polling** with your own user token, from a small private Slack app that has no event subscription
and no Socket Mode. Slack never pushes anything to agent-shift, so there is nothing for it to fail to
deliver, or to switch off, while your computer is off.

---

## 1. Requirements

| | |
|---|---|
| OS | macOS or Windows |
| Go | 1.26+ (only to build) |
| Engines | [Claude Code](https://claude.com/claude-code) and/or [Codex CLI](https://developers.openai.com/codex/cli), installed and logged in (`claude auth login`, `codex login`) |
| Slack | A workspace where you may create and install an app for yourself. Some workspaces require an admin to approve it. |
| Node.js | 20+ (only if you change the UI in `web/`) |

## 2. Build & install

```sh
git clone https://github.com/daffirds/agent-shift && cd agent-shift

# macOS
go build -o agent-shift ./cmd/agent-shift && mv agent-shift ~/.local/bin/        # or /usr/local/bin

# Windows (PowerShell)
go build -o agent-shift.exe ./cmd/agent-shift
Move-Item agent-shift.exe $HOME\.local\bin\                               # any folder on your PATH
```

Cross-compiling works from either OS, e.g. `GOOS=darwin GOARCH=arm64 go build -o dist/agent-shift ./cmd/agent-shift`.

The configuration UI is a Vite app in `web/`. Its build is committed in `internal/webui/dist` and
embedded in the binary, so `go build` is all you need. After changing the UI, rebuild it with
`cd web && npm install && npm run build`. `npm run dev` serves it with hot reload and proxies the API to
a running `agent-shift ui`.

## 3. Set up

```sh
agent-shift ui
```

This opens the configuration UI in your browser, served from `127.0.0.1` for as long as the command
runs (`--no-browser` prints the link instead). The link carries a one-time key; other sites in your
browser can't use the UI. `agent-shift setup` is an alias.

**1 · Slack app.** On the **Connect** page, click **Create app in Slack**. Slack opens with the app
manifest filled in (user-token scopes only, listed in `internal/slack/scopes.go`). Pick your
workspace, click **Create**, then **Install to Workspace** and allow it.

**2 · Token.** In the app's **OAuth & Permissions** page, copy the **User OAuth Token** (`xoxp-…`)
and paste it on the **Connect** page. agent-shift asks Slack who the token belongs to, adds the
workspace, and stores the token in the macOS Keychain or Windows Credential Manager, never in the
config file. Missing scopes are listed; add them to the app and reinstall it.

**3 · Create an agent** on the **Agents** page:

| Field | Notes |
|---|---|
| Name | e.g. `ticket-triage` |
| Engine | Claude Code or Codex |
| Working directory | where the session runs, usually a repo |
| Conversations | channels, private channels, DMs, group DMs, or your own notes-to-self DM (searchable) |
| Who can trigger | anyone · only me · specific people / bots / **workflows** (picked from recent senders, or typed IDs) |
| Only messages containing… | optional keywords (case-insensitive, any matches) and/or a regex |
| System prompt | what the agent should do |
| Autonomy | **auto** (the engine's own safety reviewer approves actions), **full** (no approvals/sandbox — for unattended PRs), **read-only** |
| Who can steer from the thread | anyone · only me · me and the trigger senders |
| Git worktree per session | each session gets its own checkout, so several can work on one repo at once |
| Model · reasoning effort | picked from the models your Claude Code / Codex login offers, then one of that model's effort levels (higher = thinks longer, slower, uses more of your plan). **Default** follows the CLI's own setting |

**4 · Run.** **Start daemon** in the top bar starts it in the background; **Settings** can register
it to start at login. Every save is applied to a running daemon right away.

Post a matching message and watch the thread.

## 4. How a session behaves

- **Start** — only *top-level* messages trigger (never thread replies). The first enabled agent whose
  trigger matches wins. The agent receives the message (text, blocks, attachments, file list) plus
  who/where/link.
- **Steer** — new replies and reactions in the thread go to the same session. If it is busy they are
  injected into the running turn (Claude: streamed input · Codex: `turn/steer`).
- **Stop** — there is no stop button in Slack. Ask the agent in the thread ("stop", "hold off"); it
  reaches the running turn like any other reply, and the agent is told to stop when asked. A 🛑 reaction
  is just a reaction the agent reads. From your terminal, `agent-shift sessions interrupt <id>` cuts
  the current turn off.
- **Progress** — the agent is told that nobody sees its progress unless it shows it, and that
  `slack_react` on the trigger message is the quiet way (e.g. 👀 when it starts something long, ✅
  when done); it decides when that's worth it. `agent-shift sessions list` shows what is running.
- **Idle** — after `idle_timeout` (default 30m) the CLI process stops; the next thread message resumes
  the same conversation (`claude --resume` / Codex `thread/resume`).
- **Slots** — at most `max_sessions` (default 3) CLI processes run at once. When a new session needs a
  slot, the least recently active *idle* process is suspended first (it resumes on demand); only when
  every slot is busy does new work wait (shown as queued in `agent-shift sessions list`).
- **Restarts** — sessions survive daemon restarts; ones that were mid-task carry on automatically.
- **Polling** — one worker per workspace reads Slack within its rate limits:
  - threads with a session at work or recent activity ("hot") every ~4 s;
  - the agents' trigger conversations every ~5 s, slower when there are many (about 30 reads a minute
    in total);
  - every other tracked thread, however many there are, through one `search.messages` call every
    20 s: a new reply anywhere wakes its thread. Spare budget also re-reads quiet threads in turn,
    which covers anything search's index missed, and covers all threads if the token lacks
    `search:read`.

  So a reply in a live thread lands within seconds, and a reply to a weeks-old thread within about
  half a minute. On the computer this is a few small HTTPS requests per second at most.
- **Offline catch-up** — polling resumes where it stopped. After the daemon was off (computer shut
  down or asleep, network down) the first reads go back to the last watermark, up to `catch_up`
  (default 3 days): matching top-level messages start sessions, oldest first (the agent is told it is
  picking them up late and gets the thread's replies so far), and new replies in tracked threads are
  steered into their sessions. Nothing is handled twice. Reactions made while offline are not replayed.
- **Expiry** — after `thread_ttl` (default 7 days) without activity the thread stops steering.
- **Errors** — if the CLI can't start or a turn fails (e.g. usage limit), nothing is posted to Slack.
  `agent-shift sessions list` shows a session that couldn't start as failed, `agent-shift sessions show <id>`
  gives the last error, and the log has the details. The next reply in the thread retries.

agent-shift never writes to Slack on its own: every message, reaction, edit and upload comes from one of
the agent's tool calls. The agent's own messages, files and reactions are recognised and never fed
back to it or used as triggers, even though they come from your account.

### Tools available to the agent

All tools come from the `agentshift` MCP server that agent-shift hosts locally for each session (they are
pre-approved, so they never wait for a permission prompt):

| Tool | What it does |
|---|---|
| `slack_reply` | reply in the session's thread (optionally also to the channel) |
| `slack_send_message` | send to `#channel`, `@person`, email, user/channel ID, or a group DM — optionally into a thread |
| `slack_read_thread` | read a thread (default: the session's) |
| `slack_read_channel` | read recent messages of a channel / DM |
| `slack_search` | Slack search (`in:#chan from:@x after:…`) |
| `slack_get_user`, `slack_find_conversations` | look people and conversations up |
| `slack_download_file` | save an attachment locally so the agent can read it |
| `slack_upload_file` | share a local file (default: into the thread) |
| `slack_react` | add/remove a reaction (default: on the trigger message) |
| `slack_edit_message`, `slack_delete_message` | only for messages this session sent |

Messages are written in standard Markdown and rendered with Slack's markdown block.

## 5. Everyday commands

```text
agent-shift ui                       configuration UI in your browser (alias: setup)
agent-shift start | stop | restart   background daemon (no console window on Windows)
agent-shift run                      run in the foreground with live logs (Ctrl+C to stop)
agent-shift status                   daemon, Slack connections, agents, sessions
agent-shift logs -f                  follow the daemon log
agent-shift reload                   apply config.yaml edits to the running daemon

agent-shift agents list | show <name> | remove <name> | enable/disable <name>
agent-shift agents models [claude|codex]   models and effort levels your CLIs offer
agent-shift slack list | remove <workspace> | find <name>
agent-shift sessions list [-a] | show <id> | logs <id> | interrupt <id> | stop <id> | prune
agent-shift autostart enable | disable | status
agent-shift doctor                   check everything
```

- Session IDs can be shortened to any unique prefix.
- Commands that change the config reload a running daemon automatically. Agents are created and
  edited in `agent-shift ui` or in `config.yaml`.
- `agent-shift sessions logs <id>` shows what a session received and did (prompts, replies, tool calls);
  add `--raw -f` to follow the raw CLI protocol.
- `agent-shift slack find <text>` prints channel/DM/person IDs for hand-editing `config.yaml`.

## 6. Configuration file

Everything lives in `~/.agent-shift/config.yaml` (set `AGENT_SHIFT_HOME` to use another directory). You can
edit it by hand; run `agent-shift reload` afterwards.

```yaml
version: 1
daemon:
  max_sessions: 3        # CLI processes running at once (idle ones are suspended first)
  idle_timeout: 30m      # stop an idle CLI process (resumes on the next message)
  thread_ttl: 7d         # stop listening to a thread after this long without activity
  catch_up: 3d           # replay messages missed while offline, at most this far back (off = never)
  log_level: info        # debug | info | warn | error
engines:                 # optional
  claude:
    path: ""             # default: PATH and the usual install locations
    args: []             # extra flags for every Claude session
    env: {}              # extra env vars (ANTHROPIC_API_KEY is removed unless set here)
  codex:
    args: []
workspaces:              # written by `agent-shift ui`
  - team_id: T0123ABCD
    name: Acme
    user_id: U0123ABCD
    user_name: Ana
    app_id: A0123ABCD    # the Slack app the token belongs to (token itself: OS keychain)
agents:
  - name: ticket-triage
    engine: claude       # claude | codex
    model: ""            # optional, e.g. opus / sonnet (claude) or gpt-6.1-sol (codex); empty = CLI default
    effort: ""           # optional reasoning effort, e.g. low | medium | high | xhigh | max; empty = model default
                         # `agent-shift agents models` lists what your CLIs accept
    workdir: /Users/ana/code/payments
    worktree: true       # one git worktree per session
    autonomy: full       # auto | full | readonly
    system_prompt: |
      You triage support tickets for the payments service...
    trigger:
      workspace: T0123ABCD
      conversations:     # channel / private channel / DM / group DM IDs (plain IDs work too)
        - id: C0456EFGH
          name: tickets
      from:              # optional: user (U…), bot/workflow (B…) or app (A…) IDs, or "me"
        - id: B0789IJKL
          name: Ticket intake workflow
      contains: ["New ticket"]   # optional, any of these (case-insensitive)
      regex: ""                  # optional, must also match
    thread_from: []      # optional: who may steer from the thread (empty = anyone)
    footer: ""           # default "Assisted by Claude" / "Assisted by Codex"
    extra_args: []       # claude: extra flags · codex: extra key=value config overrides
```

## 7. Example: tickets from a Slack workflow → pull requests

1. Have a Slack **Workflow** post tickets into a channel, e.g. a form, or *"From a webhook"* so other
   systems can file tickets with
   `curl -X POST -H 'Content-Type: application/json' -d '{"ticket":"…"}' <workflow webhook URL>`,
   with a *"Send a message to #tickets"* step.
2. Submit one ticket so the workflow shows up as a sender.
3. In `agent-shift ui` → **Agents** → **New agent**:
   - conversation `#tickets`; **Who can trigger** → *Specific senders* → pick the workflow (listed as
     `workflow`, e.g. `B0789IJKL`),
   - workdir = your repo, **worktree** on, autonomy **full** (git push / `gh pr create` without prompts),
   - a system prompt such as:

     > Each message is a ticket from our intake workflow. Acknowledge it in the thread, reproduce the
     > problem, fix it on a new branch, run the tests, push, and open a draft PR with
     > `gh pr create --draft`. Reply in the thread with a short summary and the PR link. If the ticket
     > is unclear, ask in the thread and wait for an answer.

Replies in the ticket's thread ("also handle X", "use the staging config") steer the running session,
and "stop" in the thread stops it. `agent-shift sessions prune` later removes worktrees of ended sessions (branches are kept).

## 8. Files

```text
~/.agent-shift/
  config.yaml                     your configuration
  daemon.json, daemon.lock        running daemon info (local control address/token, pid)
  state.json                      until when Slack events were received (for offline catch-up)
  logs/daemon.log                 daemon log (rotated)
  sessions/<id>/session.json      session state (thread ↔ CLI conversation)
  sessions/<id>/transcript.jsonl  raw CLI protocol log (agent-shift sessions logs <id>)
  sessions/<id>/files/            attachments downloaded by the agent
  worktrees/<id>/                 git worktrees (when enabled)
```

## 9. Platform notes

- **Windows** — `agent-shift start` runs the daemon without a console window; each session's CLI runs in
  its own job object, so stopping a session (or the daemon dying) also stops everything it spawned.
  `agent-shift autostart enable` adds a per-user *Run* entry.
- **macOS** — `agent-shift autostart enable` installs a LaunchAgent (`~/Library/LaunchAgents`) that keeps
  the daemon running in your login session (needed for the Keychain). It records your current `PATH`
  so `claude` / `codex` are found; re-run it if you move them, or set `engines.<name>.path`.

## 10. Security notes

- The agent writes **as you**. Keep `autonomy`, `from` and `thread_from` as tight as the task allows;
  anyone who can post in the thread can steer the session unless `thread_from` is set.
- Slack text is handed to Claude verbatim (no slash commands, no `@file` expansion).
- `full` autonomy runs Claude with `bypassPermissions` and Codex with `danger-full-access` — use it for
  repos and machines where that is acceptable.
- The local MCP/control endpoints and the configuration UI listen on `127.0.0.1` only and require
  per-session / per-daemon / per-run random tokens. The UI also refuses other hosts and origins.
- Your Slack user token lives in the OS keychain. Anyone with it can act as you in Slack within its
  scopes; revoke it by uninstalling the app (or regenerating its tokens) in Slack.

## 11. Troubleshooting

- `agent-shift doctor` checks the CLIs and their logins, each Slack token and its
  scopes, the config and the daemon.
- `agent-shift logs -f` shows what the daemon does; `agent-shift sessions logs <id>` what a session did.
- **Nothing happens on a message** — is it top-level (not in a thread)? Does it match the trigger
  (`agent-shift agents show <name>`)? Are you a member of the channel? Is the daemon running?
- **"missing scopes"** — the token lacks scopes agent-shift needs (listed in
  `internal/slack/scopes.go`). Add them under the app's **OAuth & Permissions**, reinstall the app,
  and paste the new token with **Replace token**.
- **"search off"** in `agent-shift status` — the token can't use `search.messages` (no
  `search:read`). Quiet threads are then only re-read in turn, so replies to old threads take longer.
- **Removed reactions** are not passed on; added ones are, for threads being read.
- **Computer was off or asleep** — missed messages are replayed when the daemon is back (see
  *Offline catch-up*); `agent-shift status` shows when a workspace is still catching up. Anything older than `catch_up` is
  skipped, and the very first start replays nothing. Enable `agent-shift autostart` so the daemon is
  back as soon as you log in.
