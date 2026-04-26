package usheragent

import (
	"context"
	"fmt"
	"strings"

	"usher/internal/core"
	"usher/internal/hook"
)

// RuleAgent is the v0.1 main-chat agent: a small dispatcher over slash
// commands. v0.2 will replace it with an LLM agent behind the same Agent
// interface.
type RuleAgent struct {
	api AgentAPI
}

func NewRule(api AgentAPI) *RuleAgent { return &RuleAgent{api: api} }

const helpText = `commands:
  /list                       list all Claude Code sessions
  /send <prefix> <text>       send <text> to the matching session
  /pending                    list pending permission requests
  /approve <id-prefix>        approve a pending request
  /deny <id-prefix>           deny a pending request
  /help                       show this help`

func (a *RuleAgent) Handle(_ context.Context, userMsg string) (string, error) {
	msg := strings.TrimSpace(userMsg)
	if msg == "" {
		return "", nil
	}

	cmd, rest := splitOnce(msg)
	switch cmd {
	case "/help":
		return helpText, nil
	case "/list":
		return a.list(), nil
	case "/send":
		return a.send(rest), nil
	case "/pending":
		return a.pending(), nil
	case "/approve":
		return a.respond(rest, "allow"), nil
	case "/deny":
		return a.respond(rest, "deny"), nil
	}
	if strings.HasPrefix(cmd, "/") {
		return "unknown command: " + cmd + ". Try /help.", nil
	}
	return "natural-language routing isn't implemented in v0.1; try /help for available commands.", nil
}

func (a *RuleAgent) list() string {
	sessions := a.api.ListSessions()
	if len(sessions) == 0 {
		return "no sessions"
	}
	var lines []string
	for _, s := range sessions {
		lines = append(lines, fmt.Sprintf("%s  %s  %s",
			shortID(s.ID), padRight(truncate(titleOr(s), 40), 40), s.Cwd))
	}
	return strings.Join(lines, "\n")
}

func (a *RuleAgent) send(args string) string {
	prefix, text := splitOnce(args)
	text = strings.TrimSpace(text)
	if prefix == "" || text == "" {
		return "usage: /send <session-prefix> <text>"
	}
	matches := matchSessions(a.api.ListSessions(), prefix)
	if len(matches) == 0 {
		return "no sessions match: " + prefix
	}
	if len(matches) > 1 {
		var lines []string
		for _, m := range matches {
			lines = append(lines, fmt.Sprintf("  %s  %s", shortID(m.ID), titleOr(m)))
		}
		return "ambiguous; matches:\n" + strings.Join(lines, "\n")
	}
	if err := a.api.SendToSession(matches[0].ID, text); err != nil {
		return "send failed: " + err.Error()
	}
	return fmt.Sprintf("sent to %s (%s)", shortID(matches[0].ID), titleOr(matches[0]))
}

func (a *RuleAgent) pending() string {
	list := a.api.ListPendingInteractions()
	if len(list) == 0 {
		return "no pending interactions"
	}
	var lines []string
	for _, p := range list {
		lines = append(lines, fmt.Sprintf("%s  %s  in session %s",
			shortID(p.ID), p.ToolName, shortID(p.SessionID)))
	}
	return strings.Join(lines, "\n")
}

func (a *RuleAgent) respond(prefix, behavior string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		verb := behavior
		return fmt.Sprintf("usage: /%s <interaction-id-prefix>", verb)
	}
	list := a.api.ListPendingInteractions()
	var matches []hook.Pending
	for _, p := range list {
		if strings.HasPrefix(p.ID, prefix) {
			matches = append(matches, p)
		}
	}
	if len(matches) == 0 {
		return "no pending interaction matches: " + prefix
	}
	if len(matches) > 1 {
		return "ambiguous; multiple matches"
	}
	err := a.api.RespondInteraction(matches[0].ID, hook.Response{Behavior: behavior, Reason: "via main chat"})
	if err != nil {
		return "failed: " + err.Error()
	}
	verb := "allowed"
	if behavior == "deny" {
		verb = "denied"
	}
	return fmt.Sprintf("%s %s", verb, shortID(matches[0].ID))
}

// --- helpers -------------------------------------------------------------

func splitOnce(s string) (string, string) {
	parts := strings.SplitN(s, " ", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], strings.TrimSpace(parts[1])
}

func matchSessions(all []core.Session, q string) []core.Session {
	qLower := strings.ToLower(q)
	var out []core.Session
	for _, s := range all {
		if strings.HasPrefix(s.ID, q) || strings.Contains(strings.ToLower(s.Title), qLower) {
			out = append(out, s)
		}
	}
	return out
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

func titleOr(s core.Session) string {
	if s.Title == "" {
		return "(untitled)"
	}
	return s.Title
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func padRight(s string, n int) string {
	r := []rune(s)
	if len(r) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(r))
}
