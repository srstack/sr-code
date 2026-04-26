# usher

A thin router that ushers messages to the right coding agent session.

usher gives you a web UI for managing multiple Claude Code sessions on your
machine. Send messages from any browser — including your phone over Tailscale —
without owning the Claude processes. Sessions are discovered by watching the
jsonl files Claude Code already writes to `~/.claude/projects/`, and messages
are delivered via headless `claude -p --resume`.

## Status

v0.1, in active development.

- [x] commit 1 — skeleton, jsonl discovery, read-only session list
- [x] commit 2 — `claude -p --resume` sender + SSE streaming (mode A)
- [x] commit 3 — hook server + permission relay
- [x] commit 4 — main chat + Usher Agent (rule-based)
- [x] **commit 5** — polish: transcript, cancel, run-state indicator

v0.1 is feature-complete.

## Design

- **No tmux, no PTY.** Sessions are discovered by watching jsonl files.
- **No SQLite.** Session state is a derived view of the filesystem; main chat
  history (later) is append-only jsonl under `$XDG_DATA_HOME/usher/`.
- **No process ownership.** Each "send" is a fresh `claude -p --resume`
  subprocess. Claude Code internally serializes concurrent resumes via
  filesystem-based queue events, so usher needs no locks of its own.
- **Stdlib first.** One direct third-party Go dep (`fsnotify`); no HTTP
  framework, no ORM, no logger lib, no testing lib. Frontend is plain HTML
  + a small `app.js` plus one vendored 2 KB markdown renderer
  (`snarkdown`, MIT, single file under `internal/web/static/vendor/`).
  No build step, no npm.

## Build & run

```
go build -o usher ./cmd/usher
./usher serve
```

Then open <http://127.0.0.1:7777>.

Flags:

```
./usher serve \
  --addr 127.0.0.1:7777 \
  --projects-dir ~/.claude/projects \
  --claude claude \
  --permission-mode bypassPermissions
```

`--permission-mode` is passed straight to `claude`. The default is now `default`,
which routes tool permission decisions through usher's hook UI (see below).
Pass `--permission-mode bypassPermissions` to run without prompting.

## Permission hook setup

Once you've installed the binary, register usher with Claude Code:

```
./usher setup
```

This adds a `PreToolUse` hook in `~/.claude/settings.json` that calls
`usher hook PreToolUse`. When any Claude Code session (managed by usher or
not) requests a tool, the hook posts to your running `usher serve` and
displays an "allow / deny" modal in the web UI. If `usher serve` isn't
running, the hook fails open (exits 0 with empty output) so your normal
Claude usage is not affected.

Uninstall:

```
./usher setup --remove
```

## Test

```
go test ./...
```

## What commit 1 includes

- `internal/jsonl` — parse Claude Code session jsonl, extract per-session metadata (id, cwd, title, timestamps).
- `internal/discovery` — initial scan + fsnotify watching of `~/.claude/projects/`; in-memory session map; sorted listing by last activity.
- `internal/web` — `GET /api/sessions`, `GET /healthz`, embedded static UI.
- `cmd/usher` — `usher serve` subcommand, signal-aware shutdown.

## What commit 2 adds

- `internal/sender` — runs `claude -p --resume <id> --output-format stream-json --include-partial-messages` from the session's original cwd, feeds prompt on stdin, parses each output line into a typed `StreamEvent`. SIGINT-on-cancel + 5 s `WaitDelay` for graceful + forced shutdown.
- `internal/broker` — small per-session pub/sub with bounded subscriber buffers; slow consumers are dropped rather than blocking the publisher.
- New web endpoints:
  - `GET /api/sessions/{id}` — single session metadata
  - `POST /api/sessions/{id}/send` — accepts `{"text": "…"}`, kicks off subprocess (returns 202 immediately, lifetime is detached from the HTTP request)
  - `GET /api/sessions/{id}/events` — SSE stream of all events for that session, with 15 s heartbeat
- UI gains a detail view: click a row → input box, live-streaming response area built from `text_delta` events, plus an event log. Hash-based routing (`#/`, `#/s/<id>`).

The frontend uses plain JS + `EventSource` rather than htmx — for SPA-style routing with a single live stream, plain JS turned out simpler than htmx + the SSE extension. We may revisit when the UI grows in commit 4.

## What commit 3 adds

- `internal/hook` — pending-interaction manager. `Submit` blocks until the
  user's UI decision arrives or context is cancelled.
- `usher hook <event-name>` — the binary's hook subcommand. Reads the hook
  payload from stdin, POSTs it to `$USHER_ADDR` (or `127.0.0.1:7777`),
  prints the server's reply on stdout. Fails open on connection error so
  Claude proceeds normally when usher is offline.
- `usher setup [--addr X] [--remove]` — installs/removes the `PreToolUse`
  hook entry in `~/.claude/settings.json`. Preserves user-defined hooks.
- New web endpoints:
  - `POST /hook/{event}` — receives Claude's hook payload; for `PreToolUse`,
    blocks on `hook.Manager.Submit` until the user decides
  - `GET /api/interactions` — list pending interactions
  - `POST /api/interactions/{id}/respond` — `{"behavior":"allow"|"deny"}`
- UI: a global modal polls `/api/interactions` every 2 s; when something is
  pending, an overlay shows the tool name, input, originating session, and
  allow/deny buttons.

The default `--permission-mode` flips back to `default` now that hooks are
the primary permission UI.

## What commit 4 adds

- `internal/router` — central coordinator. Holds discovery, sender, broker,
  hooks. Web layer and the agent both go through it; this also enforces the
  agent's surface area (it can only call methods named on `usheragent.AgentAPI`).
- `internal/mainchat` — append-only jsonl per chat at
  `$XDG_DATA_HOME/usher/mainchats/<id>.jsonl`. Single-process mutex
  serializes appends; missing files mean "empty chat".
- `internal/agent/usheragent` — the rule-based main-chat agent. Commands:

  ```
  /list                       list all Claude Code sessions
  /send <prefix> <text>       send <text> to the matching session
  /pending                    list pending permission requests
  /approve <id-prefix>        approve a pending request
  /deny <id-prefix>           deny a pending request
  /help                       show this help
  ```

  Session matches resolve by id prefix or title substring; ambiguous matches
  return the candidate list. Anything not starting with `/` produces a
  hint that natural-language routing is a v0.2 feature.

- New web endpoints:
  - `GET /api/mainchats` — list known chats
  - `GET /api/mainchats/{id}/messages?limit=N`
  - `POST /api/mainchats/{id}/send` — synchronous: appends user message, runs
    agent, appends agent reply, returns both
- UI: `main chat` link in the header opens the chat view at `#/chat`. The
  view is a scrollback + input box; messages persist between sessions
  because they're stored on disk.

`/send` from main chat dispatches the same fire-and-forget subprocess as the
session detail view: the agent confirms with `sent to <id>`, and you can
switch to that session's tab to watch the response stream in.

## What commit 5 adds

- **Session transcript**. `GET /api/sessions/{id}/transcript?limit=N` returns
  the user/assistant turns from the jsonl, with tool uses and tool results
  flattened to inline `[tool: Bash]` / `[result: …]` annotations so the
  conversation reads top-to-bottom. The detail view now shows this above the
  current-send response.
- **Cancellable sends**. `DELETE /api/sessions/{id}/send` SIGINTs the latest
  in-flight subprocess (5 s grace, then SIGKILL). The detail view swaps
  send for a cancel button while a subprocess is running.
- **Run-state indicator**. Router tracks active subprocesses and decorates
  `Session.Status` with `running` while one is in flight. The list view
  shows `● running` next to live sessions; the detail view's transcript
  refreshes automatically when the in-flight send exits.

## Markdown rendering

Assistant + user content (transcripts, current-send response, main-chat
messages) is rendered through `snarkdown` 2.0.0 (MIT, vendored as a single
2 KB file at `internal/web/static/vendor/snarkdown.js`). This handles the
LCD subset that maps cleanly onto IM platforms: bold, italic, inline code,
fenced code blocks, links, lists, headings, blockquotes.

The wrapper `renderMarkdown(md)` in `app.js` does three things snarkdown
itself won't:

1. HTML-escape the input first (snarkdown does **not** escape ordinary
   text, so a raw `<script>` would otherwise pass through). `>` is
   re-allowed afterward so blockquotes still parse — `<` stays escaped,
   so no real tag can form.
2. Strip `javascript:` / `data:` / `vbscript:` URL schemes from any anchor
   or image emitted by snarkdown.
3. Add `target="_blank" rel="noopener"` to all rendered links.

Future IM adapters (Telegram, Slack, …) take the same raw markdown source
from `Message.Content` and re-encode to their native flavor — markdown is
the lingua franca rather than HTML.

## v0.1 architecture summary

```
                            +------------------+
                            |     Web UI       |
                            +--------+---------+
                                     |
           list / detail / chat / interactions  / SSE
                                     |
                            +--------v---------+
                            |   web/ HTTP      |
                            +--------+---------+
                                     |
                            +--------v---------+
                            |   router/        |
                            |   coordinator    |
                            +-+------+-------+-+
                              |      |       |
              +---------------+      |       +-----------------+
              |                      |                         |
      +-------v-------+    +---------v--------+    +-----------v----------+
      | discovery/    |    | sender/          |    | hook/                |
      | scan + watch  |    | claude -p resume |    | pending interactions |
      | jsonl files   |    | streaming events |    +-----------+----------+
      +---------------+    +---------+--------+                |
                                     |                         |
                            +--------v--------+    +-----------v----------+
                            | broker/         |    | usher hook PreToolUse|
                            | per-session pub |    | (CLI subcommand)     |
                            +-----------------+    +----------------------+

       +-----------------+              +-------------------+
       | mainchat/       |  ←--read--   | agent/usheragent  |
       | jsonl per chat  |              | rule-based v0.1   |
       +-----------------+              +---------+---------+
                                                  |
                                          AgentAPI subset of
                                          router methods
```

Direct deps: `fsnotify` (filesystem watching) + `golang.org/x/sys` (transitive).
No SQL, no HTTP framework, no logger lib, no testing lib, no JS framework.
