// Package usheragent is the main-chat agent that routes user messages to
// Claude Code sessions and resolves permission requests.
//
// AgentAPI is intentionally a strict subset of router.Router's surface: the
// agent can read sessions, peek at transcripts, send to a session (with or
// without waiting for a response), and respond to a pending interaction —
// but it cannot subscribe to event streams, receive raw hook payloads, or
// talk to broker / discovery / hook managers directly. This boundary is
// what prevents future LLM agents from looping on themselves or escalating
// their own privileges.
package usheragent

import (
	"context"
	"time"

	"usher/internal/core"
	"usher/internal/hook"
)

type AgentAPI interface {
	ListSessions() []core.Session
	SendToSession(id, text string) error
	ListPendingInteractions() []hook.Pending
	RespondInteraction(id string, resp hook.Response) error

	// ReadSessionTranscript returns the most recent N user/assistant turns
	// from the session's jsonl. limit ≤ 0 means "no cap"; callers should
	// pass a sane default (the LLM agent uses 20 with a 200 ceiling).
	ReadSessionTranscript(id string, limit int) ([]core.TranscriptTurn, error)

	// SendToSessionAndWait spawns the same fire-and-forget claude subprocess
	// as SendToSession but blocks until the assistant turn completes (or
	// timeout/ctx cancel), returning the accumulated assistant text.
	SendToSessionAndWait(ctx context.Context, id, text string, timeout time.Duration) (string, error)

	// CreateSession starts a brand-new session in cwd with the given initial
	// message and waits for the first assistant response. Returns the new
	// session id and the assistant text.
	CreateSession(ctx context.Context, cwd, initialMsg string, timeout time.Duration) (string, string, error)
}

// HistoryMessage is one prior turn handed to Agent.Handle. The Agent is
// responsible for converting these into its own backend's message shape
// (e.g. the LLM agent maps Role="agent" to OpenAI's "assistant").
type HistoryMessage struct {
	Role    string // "user" | "agent"
	Content string
}

// AgentResult is what Agent.Handle returns. FocusSession is the session id
// the agent ended up working with this turn — empty when no session-targeted
// tool was called. The server merges this with the previous focus and stores
// it on the persisted agent message.
type AgentResult struct {
	Reply        string
	FocusSession string
}

// Agent processes a user message in the main chat and returns a reply.
// history is the recent persisted user/agent turns (newest last; the current
// user message is NOT included). currentFocus is the most recent
// FocusSession from prior agent messages, or "" if none.
type Agent interface {
	Handle(ctx context.Context, history []HistoryMessage, currentFocus, userMsg string) (AgentResult, error)
}

// strictModeAddendum is appended to the system prompt when LLMConfig.Strict
// is set. Designed to harden small / mid-tier models (Haiku, Flash, mini,
// Qwen-7B-class) against the metadata-hallucination failure mode where the
// model answers from memory of earlier turns instead of using the current
// state injected by the server. Patterned after Hermes-Agent's
// TOOL_USE_ENFORCEMENT_MODELS prompt fragments.
const strictModeAddendum = `

## Strict mode (small-model enforcement)

Every user message ends with a <current_state> block listing all sessions
(id prefix, cwd, status, title) and the current focus with cwd + title.
This block is the ground truth.

- Trivia about session count, cwd, title, status, or focus → answer
  from <current_state>. Do NOT call list_sessions for these; the answer
  is already in the message.
- Questions about session CONTENT (what was said, what was done, how
  something was implemented) → call read_session_transcript or
  send_and_wait_for_response. Never paraphrase from memory of an earlier
  reply you wrote.
- "Switch to session X" / "use the X one" → you MUST call a tool that
  targets X (read_session_transcript, send_to_session,
  send_and_wait_for_response). Do NOT claim a switch without a tool call;
  the focus only updates when a tool actually fires.
- If you find yourself writing "as I mentioned earlier", "based on my
  summary", or "you have N sessions" without a verifying tool call,
  stop and either consult <current_state> or call a tool first.`
