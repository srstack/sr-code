package usheragent

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"

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
// AgentAPI) on the user's behalf and answers in natural language. v0.2.
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

// Handle drives the tool-call loop until the model returns finish_reason="stop"
// or maxIter is exhausted. Tool-result content is JSON for structured tools
// and falls back to a `{"error":...}` shape on failure.
func (a *LLMAgent) Handle(ctx context.Context, userMsg string) (string, error) {
	msgs := []ChatMessage{
		{Role: "system", Content: a.sysPrompt},
		{Role: "user", Content: userMsg},
	}

	for i := 0; i < a.maxIter; i++ {
		resp, err := a.client.ChatCompletion(ctx, ChatRequest{
			Model:    a.model,
			Messages: msgs,
			Tools:    a.tools,
		})
		if err != nil {
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", errors.New("empty choices in chat response")
		}
		choice := resp.Choices[0]
		// Always append the assistant turn so subsequent calls see it.
		msgs = append(msgs, choice.Message)

		switch choice.FinishReason {
		case "stop", "end_turn", "":
			return choice.Message.Content, nil
		case "tool_calls":
			if len(choice.Message.ToolCalls) == 0 {
				return "", errors.New("finish_reason=tool_calls but no tool_calls returned")
			}
			for _, call := range choice.Message.ToolCalls {
				out := a.executeTool(call.Function.Name, call.Function.Arguments)
				msgs = append(msgs, ChatMessage{
					Role:       "tool",
					ToolCallID: call.ID,
					Content:    out,
				})
			}
			continue
		case "length":
			return "", errors.New("response truncated by max_tokens")
		default:
			return "", fmt.Errorf("unexpected finish_reason: %q", choice.FinishReason)
		}
	}
	return "", fmt.Errorf("max iterations (%d) reached without final answer", a.maxIter)
}

// executeTool dispatches a tool call to AgentAPI. Output is always a JSON
// string (or `{"error":"..."}`) — that's what OpenAI-protocol `role:"tool"`
// messages expect for `content`.
func (a *LLMAgent) executeTool(name, argsJSON string) string {
	switch name {
	case "list_sessions":
		b, _ := json.Marshal(a.api.ListSessions())
		return string(b)

	case "send_to_session":
		var args struct {
			SessionID string `json:"session_id"`
			Text      string `json:"text"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if args.SessionID == "" || args.Text == "" {
			return errResult("session_id and text are required")
		}
		if err := a.api.SendToSession(args.SessionID, args.Text); err != nil {
			return errResult(err.Error())
		}
		return `{"status":"sent"}`

	case "list_pending_interactions":
		b, _ := json.Marshal(a.api.ListPendingInteractions())
		return string(b)

	case "respond_to_interaction":
		var args struct {
			ID       string `json:"id"`
			Behavior string `json:"behavior"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if args.ID == "" || (args.Behavior != "allow" && args.Behavior != "deny") {
			return errResult("id and behavior (allow|deny) are required")
		}
		if err := a.api.RespondInteraction(args.ID, hook.Response{
			Behavior: args.Behavior,
			Reason:   args.Reason,
			Scope:    "once",
		}); err != nil {
			return errResult(err.Error())
		}
		return `{"status":"ok"}`

	default:
		return errResult("unknown tool: " + name)
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
				Description: "Deliver a message to a specific Claude Code session. The session is resumed via `claude -p --resume`; this returns immediately and does not block on the session's response. Use list_sessions first if you don't already know the id.",
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

