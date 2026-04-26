// Package usheragent is the main-chat agent that routes user messages to
// Claude Code sessions and resolves permission requests.
//
// AgentAPI is intentionally a strict subset of router.Router's surface: the
// agent can read sessions and pending interactions and can send to a session
// or respond to an interaction, but it cannot subscribe to event streams,
// receive raw hook payloads, or talk to broker / discovery / hook managers
// directly. This boundary is what prevents future LLM agents (v0.2+) from
// looping on themselves or escalating their own privileges.
package usheragent

import (
	"context"

	"usher/internal/core"
	"usher/internal/hook"
)

type AgentAPI interface {
	ListSessions() []core.Session
	SendToSession(id, text string) error
	ListPendingInteractions() []hook.Pending
	RespondInteraction(id string, resp hook.Response) error
}

// Agent processes a user message in the main chat and returns a reply.
// v0.1 has only the rule-based implementation; v0.2 will swap in an LLM
// implementation behind the same interface.
type Agent interface {
	Handle(ctx context.Context, userMsg string) (reply string, err error)
}
