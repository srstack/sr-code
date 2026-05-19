# usher

A thin router that ushers messages to the right coding agent session.

usher gives you a web UI for managing multiple Claude Code sessions on your
machine. Send messages from any browser — including your phone over Tailscale —
without owning the Claude processes. Sessions are discovered by watching the
jsonl files Claude Code already writes to `~/.claude/projects/`, and messages
are delivered via headless `claude -p --resume`.

## Status

- [x] **v0.1** — feature complete: jsonl discovery, send/cancel/transcript,
  permission hook bridge, rule-based main chat, run-state indicator.
- [x] **v0.2** — provider-agnostic LLM main chat: OpenAI Chat Completions
  client (any backend — OpenAI, Anthropic OAI-compat, Ollama, DeepSeek,
  Gemini, Groq, OpenRouter, vLLM, LM Studio), tool loop with 6 tools
  (list/send/wait/read-transcript/list-pending/respond), 20-turn
  conversation memory, `FocusSession` tracking that lets the user
  pretend it's a single session.
- [x] **auth** — argon2id password + HMAC-signed stateless cookie for
  the web UI, Unix-socket hook channel, bind-gate that refuses
  non-loopback `--addr` without a password (see *Authentication* below).
- [ ] streaming LLM output through main chat SSE (5-30s "thinking…"
  becomes visible progress)
- [ ] mid-flight LLM cancel from main chat

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

Requires Go 1.22+. There is no other toolchain — no npm, no make extensions,
nothing to bootstrap.

```
make build       # → ./usher (CGO off, stripped, ~7 MB)
make run         # build + ./usher serve (loopback on :7777)
make test        # go test ./...
make check       # vet + test
make install     # → $GOBIN/usher
make dist        # cross-compile linux/darwin × amd64/arm64 into dist/
make help        # list targets
```

Then open <http://127.0.0.1:7777>.

The full flag set:

```
./usher serve \
  --addr 127.0.0.1:7777 \
  --projects-dir ~/.claude/projects \
  --data-dir ~/.local/share/usher \
  --claude claude \
  --permission-mode default \
  --agent-mode rule \
  --llm-base-url https://api.openai.com/v1 \
  --llm-model "" \
  --llm-api-key-env OPENAI_API_KEY
```

`--permission-mode` is passed straight to `claude`. Default `default`
routes tool permission decisions through usher's hook UI (see below);
pass `bypassPermissions` to skip prompting entirely.

`--agent-mode rule` is the slash-command agent (see *Main chat* below).
`--agent-mode llm` switches to the LLM backend; see *LLM agent* below
for working configurations of common providers.

## Permission hook setup

Once you've installed the binary, register usher with Claude Code:

```
./usher setup
```

This adds a `PreToolUse` hook in `~/.claude/settings.json` that calls
`usher hook PreToolUse`. When any Claude Code session (managed by usher or
not) requests a tool, the hook talks to your running `usher serve` over
a **Unix domain socket** at `<data-dir>/hook.sock` (mode 0600) and
displays an "allow / deny" modal in the web UI. If `usher serve` isn't
running, the hook fails open (exits 0 with empty output) so your normal
Claude usage is not affected.

Pass `--sock /path/to/hook.sock` to `usher setup` if you run `usher serve`
with a non-default `--data-dir`. Uninstall:

```
./usher setup --remove
```

## Authentication

The web UI is unauthenticated by default and binds to `127.0.0.1:7777`.
That is safe for local-only use. To expose usher on a non-loopback
interface (e.g. so you can reach it from your phone over Tailscale), set
a password first:

```
./usher set-password               # prompts twice on the terminal
# or, for scripting:
echo -n 'hunter2' | ./usher set-password --password-stdin
```

`usher serve` will then **refuse to bind a non-loopback `--addr` until a
password exists** (e.g. `--addr 0.0.0.0:7777` or `--addr <tailnet-ip>:7777`).
Once set, every request goes through a login page, with state carried by
an HttpOnly + SameSite=Lax cookie.

Implementation:

- **argon2id** hash of the password is stored in `<data-dir>/auth.json`
  (mode 0600). No plaintext is ever read from a flag, env var, or config
  file — only stdin, on a TTY or via `--password-stdin`.
- A separate 32-byte HMAC secret is generated on first start at
  `<data-dir>/secret` (mode 0600) and reused on subsequent restarts so
  cookies survive a restart.
- Each cookie value is `base64url(HMAC(secret, password_hash))` — there
  is **no server-side session table**. Rotating the password (via
  `set-password`) changes the hash, which invalidates every cookie ever
  issued. That is the only way to forcibly sign out other devices.
- `/login` rate-limits per client IP with exponential backoff (1s → 2s →
  4s … capped at 60s) after 5 consecutive failed attempts; a successful
  login resets the counter.

The hook socket is **always** Unix-domain regardless of whether a
password is set: that channel never traverses the public web port and
is protected by filesystem permissions instead.

### Threat model

What auth in v0.1 **does** defend against:

- Other devices on your LAN or tailnet (the original motivation).
- A compromised tailnet peer trying to talk to your usher.
- Accidental `--addr 0.0.0.0` exposure (the bind gate refuses to start).
- A neighboring container that shares your host's network namespace but
  not its filesystem (e.g. `--network host`) — it can't reach the hook
  Unix socket and can't read `auth.json` / `secret` to forge a cookie.

What it **does not** defend against:

- Code running as your OS user on the host. Such code can read
  `auth.json` + `secret` and forge a cookie, read your jsonl session
  history directly, or just run `claude -p --resume <id>` itself —
  bypassing usher entirely. The OS user account is the trust boundary;
  applying further isolation is out of scope (use a dedicated UID,
  container, or sandbox if that matters to you).

This is the same posture as code-server, Jupyter, and most other
single-user local web tools.


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
- `internal/agent/usheragent` — main-chat agent. Two backends behind one
  `Agent` interface: rule-based (default) and LLM. Commands for the rule
  agent:

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

## LLM agent (v0.2)

A natural-language agent backend lives behind the same `Agent` interface.
Switch backends with `--agent-mode llm`:

```
./usher serve \
  --agent-mode llm \
  --llm-base-url https://api.openai.com/v1 \
  --llm-model gpt-4o-mini \
  --llm-api-key-env OPENAI_API_KEY
```

It speaks the **OpenAI Chat Completions** wire format (`/v1/chat/completions`)
— the de facto standard implemented by OpenAI, Anthropic's OpenAI-compatible
endpoint, Ollama, DeepSeek, Together, Groq, OpenRouter, vLLM, LM Studio,
and most local-model servers. Examples:

```
# Local Ollama (no API key needed)
./usher serve --agent-mode llm \
  --llm-base-url http://localhost:11434/v1 \
  --llm-model qwen2.5:14b \
  --llm-api-key-env ""

# DeepSeek
./usher serve --agent-mode llm \
  --llm-base-url https://api.deepseek.com/v1 \
  --llm-model deepseek-chat \
  --llm-api-key-env DEEPSEEK_API_KEY

# Anthropic via its OpenAI-compatible mode
./usher serve --agent-mode llm \
  --llm-base-url https://api.anthropic.com/v1 \
  --llm-model claude-haiku-4-5 \
  --llm-api-key-env ANTHROPIC_API_KEY
```

The agent has six tools mirroring the agent's `AgentAPI` (a strict subset of
`router.Router`):

| Tool | Effect | Updates focus? |
|---|---|---|
| `list_sessions` | enumerate sessions on disk | no |
| `read_session_transcript` | quote / summarize a session's recent turns | yes |
| `send_to_session` | fire-and-forget delivery | yes |
| `send_and_wait_for_response` | deliver and block for assistant text (default 5 min, max 30 min) | yes |
| `list_pending_interactions` | enumerate pending PreToolUse permissions | no |
| `respond_to_interaction` | allow/deny a pending permission | no |

Tool definitions live inline in `internal/agent/usheragent/llm.go`; the
system prompt is embedded from
`internal/agent/usheragent/prompts/system_prompt.md`.

Each turn the agent is fed up to **20 turns of prior history** plus a
`Current focus: session <id>` system message synthesized from the most
recent tool call that targeted a session. Result: the user can pretend
there's a single session ("refactor auth", "now run the tests") and usher
implicitly routes to the focused one — or asks for clarification when
genuinely ambiguous.

We deliberately do **not** vendor an LLM SDK. The Chat Completions HTTP
client is ~120 lines of pure stdlib (`internal/agent/usheragent/openai_client.go`),
which keeps the dependency tree at one direct third-party package
(`fsnotify`) and lets users bring any provider they like. Tradeoff:
provider-specific features (Anthropic prompt caching, OpenAI structured
outputs, adaptive thinking) are out of scope. For usher's routing agent
this is the right balance — Haiku-tier or local models handle the workload.

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

## Architecture summary

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

Direct deps: `fsnotify` (filesystem watching), `golang.org/x/crypto`
(argon2id), `golang.org/x/term` (echoless password prompt). No SQL, no
HTTP framework, no logger lib, no testing lib, no JS framework. The
LLM HTTP client is hand-rolled in ~120 lines.
