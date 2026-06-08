package usheragent

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"usher/internal/core"
	"usher/internal/hook"
)

//go:embed prompts/system_prompt.md
var defaultLLMSystemPrompt string

// LLMConfig configures NewLLM. Client and Model are required.
type LLMConfig struct {
	Client       *ChatClient
	Model        string
	SystemPrompt string // optional override; defaults to embedded prompts/system_prompt.md
	MaxIters     int    // default 10; bounds runaway tool-call loops
	Strict       bool   // append small-model enforcement block to the system prompt
}

// LLMAgent is a provider-agnostic main-chat agent built on the OpenAI
// Chat Completions wire format. It calls a small set of tools (mirroring
// AgentAPI) on the user's behalf and answers in natural language.
type LLMAgent struct {
	api       AgentAPI
	client    *ChatClient
	model     string
	sysPrompt string
	tools     []ChatTool
	maxIter   int
}

func NewLLM(api AgentAPI, cfg LLMConfig) (*LLMAgent, error) {
	if cfg.Client == nil {
		return nil, errors.New("usheragent: nil ChatClient")
	}
	if cfg.Model == "" {
		return nil, errors.New("usheragent: missing Model")
	}
	sys := cfg.SystemPrompt
	if sys == "" {
		sys = defaultLLMSystemPrompt
	}
	if cfg.Strict {
		sys = sys + strictModeAddendum
	}
	iters := cfg.MaxIters
	if iters <= 0 {
		iters = 10
	}
	return &LLMAgent{
		api:       api,
		client:    cfg.Client,
		model:     cfg.Model,
		sysPrompt: sys,
		tools:     defaultTools(),
		maxIter:   iters,
	}, nil
}

const (
	defaultWaitTimeout   = 300 * time.Second  // 5 min — covers most non-coding turns
	maxWaitTimeout       = 1800 * time.Second // 30 min — hard ceiling
	defaultReadTurns     = 20
	maxReadTurns         = 200
	defaultCreateTimeout = 300 * time.Second // 5 min — initial-message turn
	maxCreateTimeout     = 900 * time.Second // 15 min — hard ceiling
)

// Handle drives the tool-call loop until the model returns finish_reason="stop"
// or maxIter is exhausted. history (≤ 40 messages = 20 turns) is mapped to
// OpenAI roles. currentFocus is injected as a separate system message —
// kept distinct from the static prompt so prompt-cache hit rate isn't
// disturbed if/when caching is added later.
func (a *LLMAgent) Handle(ctx context.Context, history []HistoryMessage, currentFocus, userMsg string) (AgentResult, error) {
	msgs := []ChatMessage{{Role: "system", Content: a.sysPrompt}}
	if currentFocus != "" {
		msgs = append(msgs, ChatMessage{
			Role: "system",
			Content: fmt.Sprintf(
				"Current focus: session %s. When the user gives an instruction without naming a session, default to this one. Don't announce switches or add focus links yourself — the dashboard does that automatically.",
				currentFocus,
			),
		})
	}
	for _, h := range history {
		role := "user"
		if h.Role == "agent" {
			role = "assistant"
		}
		msgs = append(msgs, ChatMessage{Role: role, Content: h.Content})
	}
	msgs = append(msgs, ChatMessage{Role: "user", Content: userMsg})

	focus := "" // session id touched this turn; carries across the loop's tool calls

	for i := 0; i < a.maxIter; i++ {
		resp, err := a.client.ChatCompletion(ctx, ChatRequest{
			Model:    a.model,
			Messages: msgs,
			Tools:    a.tools,
		})
		if err != nil {
			return AgentResult{}, err
		}
		if len(resp.Choices) == 0 {
			return AgentResult{}, errors.New("empty choices in chat response")
		}
		choice := resp.Choices[0]
		msgs = append(msgs, choice.Message)

		switch choice.FinishReason {
		case "stop", "end_turn", "":
			return AgentResult{Reply: choice.Message.Content, FocusSession: focus}, nil
		case "tool_calls":
			if len(choice.Message.ToolCalls) == 0 {
				return AgentResult{}, errors.New("finish_reason=tool_calls but no tool_calls returned")
			}
			for _, call := range choice.Message.ToolCalls {
				out, focusUpdate := a.executeTool(ctx, call.Function.Name, call.Function.Arguments)
				if focusUpdate != "" {
					focus = focusUpdate
				}
				msgs = append(msgs, ChatMessage{
					Role:       "tool",
					ToolCallID: call.ID,
					Content:    out,
				})
			}
			continue
		case "length":
			return AgentResult{}, errors.New("response truncated by max_tokens")
		default:
			return AgentResult{}, fmt.Errorf("unexpected finish_reason: %q", choice.FinishReason)
		}
	}
	return AgentResult{}, fmt.Errorf("max iterations (%d) reached without final answer", a.maxIter)
}

// executeTool dispatches a tool call to AgentAPI. Output is always a JSON
// string (or `{"error":"..."}`) — that's what OpenAI-protocol `role:"tool"`
// messages expect for `content`. The second return value is the session id
// this tool touched (empty for read-only / non-targeted tools); used by
// Handle to compute the turn's FocusSession.
func (a *LLMAgent) executeTool(ctx context.Context, name, argsJSON string) (string, string) {
	switch name {
	case "list_sessions":
		b, _ := json.Marshal(a.enrichedSessions())
		return string(b), ""

	case "send_to_session":
		var args struct {
			SessionID string `json:"session_id"`
			Text      string `json:"text"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if args.SessionID == "" || args.Text == "" {
			return errResult("session_id and text are required"), ""
		}
		if err := a.api.SendToSession(args.SessionID, args.Text); err != nil {
			return errResult(err.Error()), ""
		}
		return `{"status":"sent"}`, args.SessionID

	case "send_and_wait_for_response":
		var args struct {
			SessionID      string `json:"session_id"`
			Text           string `json:"text"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if args.SessionID == "" || args.Text == "" {
			return errResult("session_id and text are required"), ""
		}
		timeout := defaultWaitTimeout
		if args.TimeoutSeconds > 0 {
			t := time.Duration(args.TimeoutSeconds) * time.Second
			if t > maxWaitTimeout {
				t = maxWaitTimeout
			}
			timeout = t
		}
		text, err := a.api.SendToSessionAndWait(ctx, args.SessionID, args.Text, timeout)
		if err != nil {
			payload, _ := json.Marshal(map[string]any{"response": text, "error": err.Error()})
			return string(payload), args.SessionID
		}
		payload, _ := json.Marshal(map[string]any{"response": text})
		return string(payload), args.SessionID

	case "read_session_transcript":
		var args struct {
			SessionID string `json:"session_id"`
			Limit     int    `json:"limit"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if args.SessionID == "" {
			return errResult("session_id is required"), ""
		}
		limit := args.Limit
		if limit <= 0 {
			limit = defaultReadTurns
		}
		if limit > maxReadTurns {
			limit = maxReadTurns
		}
		turns, err := a.api.ReadSessionTranscript(args.SessionID, limit)
		if err != nil {
			return errResult(err.Error()), ""
		}
		b, _ := json.Marshal(turns)
		return string(b), args.SessionID

	case "create_session":
		var args struct {
			Cwd            string `json:"cwd"`
			InitialMessage string `json:"initial_message"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if args.Cwd == "" || args.InitialMessage == "" {
			return errResult("cwd and initial_message are required"), ""
		}
		timeout := defaultCreateTimeout
		if args.TimeoutSeconds > 0 {
			t := time.Duration(args.TimeoutSeconds) * time.Second
			if t > maxCreateTimeout {
				t = maxCreateTimeout
			}
			timeout = t
		}
		newID, text, err := a.api.CreateSession(ctx, args.Cwd, args.InitialMessage, timeout)
		if err != nil {
			payload, _ := json.Marshal(map[string]any{"session_id": newID, "response": text, "error": err.Error()})
			return string(payload), newID
		}
		payload, _ := json.Marshal(map[string]any{"session_id": newID, "response": text})
		return string(payload), newID

	case "list_pending_interactions":
		b, _ := json.Marshal(a.api.ListPendingInteractions())
		return string(b), ""

	case "respond_to_interaction":
		var args struct {
			ID       string `json:"id"`
			Behavior string `json:"behavior"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if args.ID == "" || (args.Behavior != "allow" && args.Behavior != "deny") {
			return errResult("id and behavior (allow|deny) are required"), ""
		}
		if err := a.api.RespondInteraction(args.ID, hook.Response{
			Behavior: args.Behavior,
			Reason:   args.Reason,
			Scope:    "once",
		}); err != nil {
			return errResult(err.Error()), ""
		}
		// Don't update focus from respond_to_interaction; keep it tied to
		// send-class operations so "now show me" reliably means the session
		// the agent last sent to.
		return `{"status":"ok"}`, ""

	case "set_auto_approve":
		var args struct {
			SessionID string `json:"session_id"`
			Enabled   bool   `json:"enabled"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if args.SessionID == "" {
			return errResult("session_id is required"), ""
		}
		a.api.SetAutoApprove(args.SessionID, args.Enabled)
		// Housekeeping, not a send — don't change focus (same as respond).
		payload, _ := json.Marshal(map[string]any{"status": "ok", "session_id": args.SessionID, "auto_approve": args.Enabled})
		return string(payload), ""

	case "set_archived":
		var args struct {
			SessionID string `json:"session_id"`
			Archived  bool   `json:"archived"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if args.SessionID == "" {
			return errResult("session_id is required"), ""
		}
		if args.Archived {
			a.api.Archive(args.SessionID)
		} else {
			a.api.Unarchive(args.SessionID)
		}
		payload, _ := json.Marshal(map[string]any{"status": "ok", "session_id": args.SessionID, "archived": args.Archived})
		return string(payload), ""

	default:
		return errResult("unknown tool: " + name), ""
	}
}

// sessionView adds the two web-sidebar flags (archived, auto_approve) to a
// session so the agent can see and report them.
type sessionView struct {
	core.Session
	Archived    bool `json:"archived"`
	AutoApprove bool `json:"auto_approve"`
}

func (a *LLMAgent) enrichedSessions() []sessionView {
	sessions := a.api.ListSessions()
	out := make([]sessionView, len(sessions))
	for i, s := range sessions {
		out[i] = sessionView{
			Session:     s,
			Archived:    a.api.IsArchived(s.ID),
			AutoApprove: a.api.IsAutoApprove(s.ID),
		}
	}
	return out
}

func errResult(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

// repairJSONArgs makes a best-effort fix to malformed tool-call argument
// JSON emitted by small models. Patterned after Hermes-Agent's 5-pass
// repair pipeline. Returns the original string if it parses cleanly, the
// repaired string if any pass succeeds, or "{}" if everything fails (a
// recoverable empty object beats a hard error mid-loop).
//
// We only touch arguments that already FAILED to parse — never modify
// already-valid JSON. The tool dispatch path calls Unmarshal on the
// returned string and reports its own errors as usual.
func repairJSONArgs(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "{}"
	}
	var probe any
	if err := json.Unmarshal([]byte(raw), &probe); err == nil {
		return raw
	}
	s := raw
	// 1. Strip trailing commas before } or ]
	s = jsonTrailingComma.ReplaceAllString(s, "$1")
	// 2. Quote unquoted keys ({key: 1} → {"key": 1}). Conservative — only
	//    matches positions clearly inside an object (after `{` or `,`).
	s = jsonUnquotedKey.ReplaceAllString(s, `$1"$2":`)
	// 3. Python literal sentinels → JSON
	s = jsonPyNone.ReplaceAllString(s, "null")
	s = jsonPyTrue.ReplaceAllString(s, "true")
	s = jsonPyFalse.ReplaceAllString(s, "false")
	// 4. Single-quoted string values → double-quoted (very narrow: covers
	//    {key: 'value'} mistake, not nested complex strings).
	s = jsonSingleQuoteVal.ReplaceAllStringFunc(s, func(m string) string {
		// Replace surrounding single quotes with double; escape any
		// embedded double quotes.
		inner := m[1 : len(m)-1]
		inner = strings.ReplaceAll(inner, `"`, `\"`)
		return `"` + inner + `"`
	})
	if err := json.Unmarshal([]byte(s), &probe); err == nil {
		return s
	}
	return "{}"
}

var (
	jsonTrailingComma  = regexp.MustCompile(`,(\s*[}\]])`)
	jsonUnquotedKey    = regexp.MustCompile(`([{,]\s*)([A-Za-z_][A-Za-z0-9_]*)\s*:`)
	jsonPyNone         = regexp.MustCompile(`\bNone\b`)
	jsonPyTrue         = regexp.MustCompile(`\bTrue\b`)
	jsonPyFalse        = regexp.MustCompile(`\bFalse\b`)
	jsonSingleQuoteVal = regexp.MustCompile(`'[^'\n]*'`)
)

func defaultTools() []ChatTool {
	return []ChatTool{
		{
			Type: "function",
			Function: ChatFunction{
				Name: "list_sessions",
				Description: `Refresh the full list of Claude Code sessions discovered on this machine. Returns id, cwd, title, status, started_at, last_event_at, archived, auto_approve for each.

USE FOR: questions you can't answer from <current_state> — exact timestamps, status that may have changed in the last few seconds.

DO NOT USE FOR: simple metadata trivia like "how many sessions", "which session is in /tmp", "what's the focused cwd" — that's already in the <current_state> block at the end of the user message.`,
				Parameters: map[string]any{
					"type":                 "object",
					"properties":           map[string]any{},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "send_to_session",
				Description: `Deliver a message to a session and return immediately. The session keeps working in the background; you do NOT see its response here. Updates focus to the target session.

USE FOR: explicit "kick off X", "let it run", "I'll check the tab myself", or known long-running work (full test suites, deploys) that exceeds the 30-min wait ceiling.

DO NOT USE WHEN: the user wants to see the answer in this chat. Default to send_and_wait_for_response in that case.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"session_id": map[string]any{
							"type":        "string",
							"description": "Full session ID, exactly as returned by list_sessions.",
						},
						"text": map[string]any{
							"type":        "string",
							"description": "The message text to send.",
						},
					},
					"required":             []string{"session_id", "text"},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "send_and_wait_for_response",
				Description: `Send a message to a session AND block until the assistant's reply (default 300s, max 1800s). Returns {response, error?}. Updates focus to the target session.

USE FOR: the default way to relay an instruction that has a visible answer — "ask X to ...", "have X explain Y", "run Z and tell me what it says", any "and tell me" follow-up.

DO NOT USE FOR: tasks obviously > 30 min (deploys, full builds). Use send_to_session for those instead.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"session_id":      map[string]any{"type": "string"},
						"text":            map[string]any{"type": "string"},
						"timeout_seconds": map[string]any{"type": "integer", "description": "Optional. Default 300, max 1800."},
					},
					"required":             []string{"session_id", "text"},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "read_session_transcript",
				Description: `Read recent user/assistant turns from a session's transcript. Tool uses inside the session are inlined as ` + "`tool: Name`" + ` annotations. Returns [{role, content, ts}, ...].

USE FOR: any question about what was DONE or SAID inside a session — "what did session X say?", "summarize Y", "what's the latest output from Z?", "any update?", deeper dives.

DO NOT USE FOR: looking up session metadata (cwd, title, status, count) — <current_state> already has that. For "switch to session X" prefer send_and_wait_for_response so a visible action confirms the switch.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"session_id": map[string]any{"type": "string"},
						"limit":      map[string]any{"type": "integer", "description": "Optional. Default 20, max 200."},
					},
					"required":             []string{"session_id"},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "create_session",
				Description: `Start a brand-new Claude Code session in cwd, send it an initial message, wait for first reply (default 300s, max 900s). Returns {session_id, response, error?}. New id becomes focus.

USE FOR: the user wants fresh context that doesn't fit any session in <current_state> — scratch experiments, a new project, isolated debugging.

DO NOT USE FOR: routing into an existing session that matches the work — use send_and_wait_for_response on that one. cwd must already exist; do not invent paths. /tmp is a safe default for ephemeral / scratch work when the user gives no hint.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"cwd":             map[string]any{"type": "string", "description": "Working directory for the new session. Must exist."},
						"initial_message": map[string]any{"type": "string", "description": "First message sent to the new session."},
						"timeout_seconds": map[string]any{"type": "integer", "description": "Optional. Default 300, max 900."},
					},
					"required":             []string{"cwd", "initial_message"},
					"additionalProperties": false,
				},
			},
		},
		// Permission tools disabled for now: requests are resolved by the global
		// web modal, so the agent never gets a turn to act on them. executeTool
		// cases + AgentAPI methods are kept — uncomment to re-enable.
		/*
				{
					Type: "function",
					Function: ChatFunction{
						Name: "list_pending_interactions",
						Description: `List PreToolUse permission requests across all sessions waiting for a user decision. Returns [{id, session_id, tool_name, tool_input, cwd, created_at}, ...].

			USE FOR: "any pending approvals?", "what's waiting", "show me the queue".

			DO NOT USE FOR: deciding to approve or deny — that's respond_to_interaction. The count alone is in <current_state>.pending_permission_requests.`,
						Parameters: map[string]any{
							"type":                 "object",
							"properties":           map[string]any{},
							"additionalProperties": false,
						},
					},
				},
				{
					Type: "function",
					Function: ChatFunction{
						Name: "respond_to_interaction",
						Description: `Approve or deny a single pending PreToolUse permission request by id.

			USE FOR: explicit user authorization — "approve the bash one", "deny X", "let it run", "block the rm".

			DO NOT blanket-approve anything dangerous-looking (mass deletes, sending sensitive data, broad access) without confirming with the user first.`,
						Parameters: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":       map[string]any{"type": "string"},
								"behavior": map[string]any{"type": "string", "enum": []string{"allow", "deny"}},
								"reason":   map[string]any{"type": "string"},
							},
							"required":             []string{"id", "behavior"},
							"additionalProperties": false,
						},
					},
				},
		*/
		{
			Type: "function",
			Function: ChatFunction{
				Name: "set_auto_approve",
				Description: `Turn a session's permission auto-approval on or off. When on, usher silently allows that session's PreToolUse prompts instead of queuing them for a decision.

USE FOR: "stop asking me about session X", "auto-approve the deploy session", "let X run unattended", and turning it back off ("ask me again for X").

DO NOT blanket-enable on a session doing dangerous work (mass deletes, prod changes) without confirming with the user — it suppresses every future prompt for that session.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"session_id": map[string]any{"type": "string", "description": "Full session ID, exactly as returned by list_sessions."},
						"enabled":    map[string]any{"type": "boolean", "description": "true to auto-approve this session's prompts, false to resume asking."},
					},
					"required":             []string{"session_id", "enabled"},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "set_archived",
				Description: `Archive or unarchive a session. Archived sessions are hidden from the default session list (they still exist and can be unarchived). Use to tidy finished / stale sessions.

USE FOR: "archive the old spike", "hide the finished sessions", "bring back session X", "unarchive Y".`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"session_id": map[string]any{"type": "string", "description": "Full session ID, exactly as returned by list_sessions."},
						"archived":   map[string]any{"type": "boolean", "description": "true to archive (hide), false to unarchive (restore)."},
					},
					"required":             []string{"session_id", "archived"},
					"additionalProperties": false,
				},
			},
		},
	}
}
