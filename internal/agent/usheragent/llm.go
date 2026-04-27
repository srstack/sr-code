package usheragent

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
				"Current focus: session %s. When the user gives an instruction without naming a session, default to this one. Switch transparently if they refer to another, and briefly disclose the switch.",
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
		b, _ := json.Marshal(a.api.ListSessions())
		return string(b), ""

	case "send_to_session":
		var args struct {
			SessionID string `json:"session_id"`
			Text      string `json:"text"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
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
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
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
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
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
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
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
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
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

	default:
		return errResult("unknown tool: " + name), ""
	}
}

func errResult(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

func defaultTools() []ChatTool {
	return []ChatTool{
		{
			Type: "function",
			Function: ChatFunction{
				Name:        "list_sessions",
				Description: "List all known Claude Code sessions discovered on this machine. Returns id, cwd, title, status, and last_event_at for each. No arguments.",
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
				Name:        "send_to_session",
				Description: "Deliver a message to a specific Claude Code session and return immediately (does NOT wait for the assistant's response). Use this when the user just wants to queue work and will check the session detail tab themselves. Use list_sessions first if you don't already know the id.",
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
				Name:        "send_and_wait_for_response",
				Description: "Send a message to a session AND block until the assistant responds, returning the accumulated text. Use this when the user wants to SEE the result here in the chat. For long autonomous tasks (multi-minute coding runs), prefer send_to_session and tell the user to watch the session tab — this tool's default timeout is 300s (5min) and ceiling is 1800s (30min).",
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
				Name:        "read_session_transcript",
				Description: "Read the most recent N user/assistant turns from a session's transcript. Use this to summarize, quote, or answer questions about what's happening inside a specific session. Tool calls within the session are inlined as `tool: Name` markers. Returns an array of turn objects {role, content, ts}.",
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
				Name:        "create_session",
				Description: "Start a NEW Claude Code session in cwd with an initial message. Returns the new session id and the assistant's first response. Use when the user wants fresh context — scratch work, a new project — that doesn't fit any existing session. cwd MUST exist; the agent shouldn't invent a path. Default timeout 300s, max 900s. After creation, the new session's id becomes the focus and follow-up tools (send_to_session, read_session_transcript, send_and_wait_for_response) target it.",
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
		{
			Type: "function",
			Function: ChatFunction{
				Name:        "list_pending_interactions",
				Description: "List PreToolUse permission requests that are waiting for a user decision across all sessions. No arguments.",
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
				Name:        "respond_to_interaction",
				Description: "Approve or deny a single pending permission request by id.",
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
	}
}
