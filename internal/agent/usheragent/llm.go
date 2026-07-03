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

	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
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
	defaultWaitTimeout    = 300 * time.Second  // 5 min — covers most non-coding turns
	maxWaitTimeout        = 1800 * time.Second // 30 min — hard ceiling
	defaultReadTurns      = 20
	maxReadTurns          = 200
	defaultSearchHits     = 20
	maxSearchHits         = 100
	defaultSearchContext  = 120
	maxSearchContext      = 500
	defaultSearchSessions = 20
	maxSearchSessions     = 50
	defaultCreateTimeout  = 300 * time.Second // 5 min — initial-message turn
	maxCreateTimeout      = 900 * time.Second // 15 min — hard ceiling
)

// Handle drives the tool-call loop until the model returns finish_reason="stop"
// or maxIter is exhausted. history is mapped to OpenAI roles verbatim — the
// server owns its shape (summary anchoring, relay excerpts), Handle owns only
// the transport.
func (a *LLMAgent) Handle(ctx context.Context, history []HistoryMessage, currentFocus, userMsg string, relay RelaySink) (AgentResult, error) {
	// currentFocus is deliberately NOT injected as a message: a system
	// message after the static prompt would invalidate the provider's prefix
	// cache for the whole history every time focus changes. The focus id
	// lives in the <current_state> block at the message tail; the behavioral
	// rule ("default to the focus") lives in the static system prompt.
	_ = currentFocus
	msgs := []ChatMessage{{Role: "system", Content: a.sysPrompt}}
	for _, h := range history {
		role := "user"
		if h.Role == "agent" {
			role = "assistant"
		}
		msgs = append(msgs, ChatMessage{Role: role, Content: h.Content})
	}
	msgs = append(msgs, ChatMessage{Role: "user", Content: userMsg})

	focus := "" // session id touched this turn; carries across the loop's tool calls

	// Anti-poll guard: an immediately-repeated identical (name, args) tool
	// call is blocked — but only when the first attempt SUCCEEDED, so a
	// retry after an error result passes through. Weaker models "wait" for
	// async replies by re-reading the same transcript page or re-sending.
	// (Every error return below carries FocusSession so the server keeps
	// the turn's routing even when the loop fails.)
	lastCallSig := ""
	lastCallOK := false

	for i := 0; i < a.maxIter; i++ {
		resp, err := a.client.ChatCompletion(ctx, ChatRequest{
			Model:    a.model,
			Messages: msgs,
			Tools:    a.tools,
		})
		if err != nil {
			return AgentResult{FocusSession: focus}, err
		}
		if len(resp.Choices) == 0 {
			return AgentResult{FocusSession: focus}, errors.New("empty choices in chat response")
		}
		choice := resp.Choices[0]
		msgs = append(msgs, choice.Message)

		// Checked before the tool_calls dispatch: a max_tokens-truncated
		// response may carry PARTIAL tool calls — dispatching a cut-off
		// send_to_session would deliver a garbled half-message.
		if choice.FinishReason == "length" {
			return AgentResult{FocusSession: focus}, errors.New("response truncated by max_tokens")
		}

		// Some providers return tool calls with a missing/nonstandard
		// finish_reason — dispatch on the presence of tool_calls, so such a
		// turn isn't misread as an (empty) final answer.
		if len(choice.Message.ToolCalls) > 0 {
			for _, call := range choice.Message.ToolCalls {
				sig := call.Function.Name + "\x00" + call.Function.Arguments
				if sig == lastCallSig && lastCallOK {
					msgs = append(msgs, ChatMessage{
						Role:       "tool",
						ToolCallID: call.ID,
						Content:    errResult("repeated identical tool call blocked — do not poll for a session's reply (it is relayed automatically) or resend the same message; answer the user now with what you have"),
					})
					continue
				}
				out, focusUpdate := a.executeTool(ctx, call.Function.Name, call.Function.Arguments, relay)
				lastCallSig = sig
				lastCallOK = !strings.HasPrefix(out, `{"error":`)
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
		}

		switch choice.FinishReason {
		case "stop", "end_turn", "":
			return AgentResult{Reply: choice.Message.Content, FocusSession: focus}, nil
		case "tool_calls":
			// Glitch: finish_reason says tool_calls but none were present
			// (that case was dispatched above). Use the content if there is
			// any; otherwise surface the malformed turn instead of silently
			// returning an empty answer.
			if strings.TrimSpace(choice.Message.Content) != "" {
				return AgentResult{Reply: choice.Message.Content, FocusSession: focus}, nil
			}
			return AgentResult{FocusSession: focus}, errors.New("finish_reason=tool_calls but no tool_calls returned")
		default:
			return AgentResult{FocusSession: focus}, fmt.Errorf("unexpected finish_reason: %q", choice.FinishReason)
		}
	}

	// Tool budget exhausted with the model still calling tools. The user saw
	// none of that traffic, so a bare error wastes the whole turn — instead
	// force a text-only wrap-up: no tools offered, plus an explicit
	// instruction to answer from what it already learned.
	msgs = append(msgs, ChatMessage{
		Role:    "user",
		Content: "[system] Tool budget for this turn is exhausted. Stop calling tools and answer the user now, in plain text, from what you already learned. If you routed work to a session, just say so — its reply arrives automatically.",
	})
	resp, err := a.client.ChatCompletion(ctx, ChatRequest{Model: a.model, Messages: msgs})
	if err != nil {
		return AgentResult{FocusSession: focus}, fmt.Errorf("max iterations (%d) reached and wrap-up failed: %w", a.maxIter, err)
	}
	if len(resp.Choices) == 0 || strings.TrimSpace(resp.Choices[0].Message.Content) == "" {
		return AgentResult{FocusSession: focus}, fmt.Errorf("max iterations (%d) reached without final answer", a.maxIter)
	}
	return AgentResult{Reply: resp.Choices[0].Message.Content, FocusSession: focus}, nil
}

// summarizePrompt drives history compaction. The main chat is a router, not
// a knowledge store — session transcripts remain fully recoverable via tools
// — so the summary keeps only what future ROUTING needs and compresses hard.
const summarizePrompt = `You compress the older part of a session-routing chat into a standing summary. Keep ONLY, as terse bullet points:

- standing user instructions and preferences that remain in force ("always send X-type work to session Y", tone/format asks);
- which sessions were used for what (short id + one-line purpose) and roughly when last touched;
- unresolved threads: work still running, questions the user never answered, promised follow-ups.

DROP: completed exchanges, pleasantries, session reply bodies (a pointer like "session ab12cd34 answered the migration question" is enough — full text stays recoverable from the session transcript). Do not invent anything not in the input. Answer with the summary only, no preamble.`

// SummarizeHistory implements HistorySummarizer: one tools-free completion
// over the flattened history.
func (a *LLMAgent) SummarizeHistory(ctx context.Context, history []HistoryMessage) (string, error) {
	var b strings.Builder
	for _, h := range history {
		fmt.Fprintf(&b, "%s: %s\n\n", h.Role, h.Content)
	}
	resp, err := a.client.ChatCompletion(ctx, ChatRequest{
		Model: a.model,
		Messages: []ChatMessage{
			{Role: "system", Content: summarizePrompt},
			{Role: "user", Content: b.String()},
		},
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 || strings.TrimSpace(resp.Choices[0].Message.Content) == "" {
		return "", errors.New("empty summary")
	}
	return resp.Choices[0].Message.Content, nil
}

// executeTool dispatches a tool call to AgentAPI. Output is always a JSON
// string (or `{"error":"..."}`) — that's what OpenAI-protocol `role:"tool"`
// messages expect for `content`. The second return value is the session id
// this tool touched (empty for read-only / non-targeted tools); used by
// Handle to compute the turn's FocusSession. relay (may be nil) is where
// send/create replies land once the target session completes its turn.
func (a *LLMAgent) executeTool(ctx context.Context, name, argsJSON string, relay RelaySink) (string, string) {
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
		if relay == nil {
			// No relay channel (caller can't display async replies) —
			// plain fire-and-forget.
			if err := a.api.SendToSession(args.SessionID, args.Text); err != nil {
				return errResult(err.Error()), ""
			}
			return `{"status":"sent"}`, args.SessionID
		}
		if err := a.api.SendToSessionRelayed(args.SessionID, args.Text, relay); err != nil {
			return errResult(err.Error()), ""
		}
		return `{"status":"sent","note":"the session's reply will be shown to the user verbatim when it completes — do not wait for it or restate it"}`, args.SessionID

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
			Offset    *int   `json:"offset"`
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
		// Negative offset (or omitted) = most recent page; a caller pages older
		// turns with offset = returned offset - limit, using total to stop.
		offset := -1
		if args.Offset != nil {
			offset = *args.Offset
		}
		turns, start, total, err := a.api.ReadSessionTranscriptPage(args.SessionID, offset, limit)
		if err != nil {
			return errResult(err.Error()), ""
		}
		payload, _ := json.Marshal(map[string]any{
			"turns":    turns,
			"offset":   start,
			"total":    total,
			"has_more": start+len(turns) < total,
		})
		return string(payload), args.SessionID

	case "search_session_transcript":
		var args struct {
			SessionID    string `json:"session_id"`
			Query        string `json:"query"`
			MaxHits      int    `json:"max_hits"`
			ContextChars int    `json:"context_chars"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if args.SessionID == "" || strings.TrimSpace(args.Query) == "" {
			return errResult("session_id and query are required"), ""
		}
		maxHits := args.MaxHits
		if maxHits <= 0 {
			maxHits = defaultSearchHits
		}
		if maxHits > maxSearchHits {
			maxHits = maxSearchHits
		}
		ctxChars := args.ContextChars
		if ctxChars <= 0 {
			ctxChars = defaultSearchContext
		}
		if ctxChars > maxSearchContext {
			ctxChars = maxSearchContext
		}
		hits, truncated, err := a.api.SearchSessionTranscript(args.SessionID, args.Query, maxHits, ctxChars)
		if err != nil {
			return errResult(err.Error()), ""
		}
		payload, _ := json.Marshal(map[string]any{"hits": hits, "truncated": truncated})
		return string(payload), args.SessionID

	case "search_all_sessions":
		var args struct {
			Query        string `json:"query"`
			MaxSessions  int    `json:"max_sessions"`
			ContextChars int    `json:"context_chars"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if strings.TrimSpace(args.Query) == "" {
			return errResult("query is required"), ""
		}
		maxSessions := args.MaxSessions
		if maxSessions <= 0 {
			maxSessions = defaultSearchSessions
		}
		if maxSessions > maxSearchSessions {
			maxSessions = maxSearchSessions
		}
		ctxChars := args.ContextChars
		if ctxChars <= 0 {
			ctxChars = defaultSearchContext
		}
		if ctxChars > maxSearchContext {
			ctxChars = maxSearchContext
		}
		results, truncated, err := a.api.SearchAllSessions(args.Query, maxSessions, ctxChars)
		if err != nil {
			return errResult(err.Error()), ""
		}
		payload, _ := json.Marshal(map[string]any{"results": results, "truncated": truncated})
		// Cross-session search doesn't target one session, so it sets no focus.
		return string(payload), ""

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
		if relay == nil {
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
		}
		newID, err := a.api.CreateSessionRelayed(args.Cwd, args.InitialMessage, relay)
		if err != nil {
			return errResult(err.Error()), ""
		}
		payload, _ := json.Marshal(map[string]any{
			"session_id": newID,
			"status":     "created",
			"note":       "the session's first reply will be shown to the user verbatim when it completes — do not wait for it or restate it",
		})
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
				Description: `Deliver a message to a session. Returns immediately; when the session finishes its turn, its reply is automatically shown to the user IN THIS CHAT, verbatim — you never see it and must not wait for it or restate it. Updates focus to the target session.

USE FOR: the default way to route ANY instruction or question to a session — quick questions, long tasks, "ask X to ...", "run Z", follow-ups. Task duration doesn't matter; the reply arrives whenever it's ready.

AFTER CALLING: reply with at most one short routing sentence (or nothing beyond what the dashboard already shows). Never say you will "report back" — the relay is automatic.`,
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
		// send_and_wait_for_response is disabled for now: session replies reach
		// the user through the relay channel, so the agent has no display
		// reason to block its turn. The executeTool case + AgentAPI method are
		// kept — re-add a tool definition here to give the model an explicit
		// "wait because MY next step needs the reply content" chaining tool.
		{
			Type: "function",
			Function: ChatFunction{
				Name: "read_session_transcript",
				Description: `Read one page of user/assistant turns from a session's transcript. Tool uses inside the session are inlined as ` + "`tool: Name`" + ` annotations. Returns {turns: [{role, content, ts}, ...], offset, total, has_more} — offset is the absolute index of the first turn returned, total the whole transcript's turn count.

USE FOR: any question about what was DONE or SAID inside a session — "what did session X say?", "summarize Y", "what's the latest output from Z?", "any update?", deeper dives. ALSO the recovery path for excerpted replies: a chat message showing "[… N chars omitted …]" has its full text here — recent replies are in the default page; older ones, locate via search_session_transcript and jump with offset.

PAGING: with no offset you get the most recent ` + "`limit`" + ` turns. total tells you how many exist; if has_more is true there are older turns you haven't seen. To reach a specific spot — e.g. a turn_index from search_session_transcript, or the page before this one — pass offset (absolute, 0-based from the start). There is no depth limit; limit only bounds one page.

DO NOT USE FOR: looking up session metadata (cwd, title, status, count) — <current_state> already has that. For "switch to session X" prefer send_to_session so a visible action confirms the switch.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"session_id": map[string]any{"type": "string"},
						"limit":      map[string]any{"type": "integer", "description": "Optional. Page size (turns per call). Default 20, max 200."},
						"offset":     map[string]any{"type": "integer", "description": "Optional. Absolute 0-based index of the first turn to return. Omit for the most recent page. Use a turn_index from search_session_transcript to jump to a hit."},
					},
					"required":             []string{"session_id"},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "search_session_transcript",
				Description: `Find where a string appears in a session's transcript WITHOUT reading the whole thing. Scans every user/assistant turn (not just the recent window) and returns only the matching turns as [{role, ts, turn_index, occurrences, snippet}, ...] plus a "truncated" flag. Case-insensitive literal substring — not regex.

USE FOR: locating something specific in a long session — "did session X mention the migration?", "where did we discuss the timeout bug?", "find the commit hash Y talked about". Use this INSTEAD of read_session_transcript when the answer could be buried past the last 20 turns.

THEN: to see the full context around a hit, call read_session_transcript with offset=<turn_index> (jumps to that turn at any depth), or send_to_session to ask the session directly.

DO NOT USE FOR: reading the latest activity (use read_session_transcript) or matching tool/command/file names — search covers prose only, not tool annotations.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"session_id":    map[string]any{"type": "string"},
						"query":         map[string]any{"type": "string", "description": "Literal substring to find (case-insensitive)."},
						"max_hits":      map[string]any{"type": "integer", "description": "Optional. Max matching turns to return. Default 20, max 100."},
						"context_chars": map[string]any{"type": "integer", "description": "Optional. Characters of context on each side of the match. Default 120, max 500."},
					},
					"required":             []string{"session_id", "query"},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "search_all_sessions",
				Description: `Search EVERY session at once for a string — the way to answer "which session mentioned X?" without knowing the id. Returns one row per matching session: {results: [{session_id, title, cwd, hit_count, turn_index, snippet}, ...], truncated}, ranked by hit_count (most matches first). Case-insensitive literal substring over user/assistant prose.

USE FOR: locating the right session when you don't have its id — "which session was about the auth migration?", "who touched the deploy script?", "find the session discussing timeouts". Then route to the winner with send_to_session, or drill in with read_session_transcript (offset=turn_index) / search_session_transcript on that one id.

DO NOT USE FOR: searching inside a session you already identified (use search_session_transcript with its id) or matching tool/command/file names — search covers prose only.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query":         map[string]any{"type": "string", "description": "Literal substring to find (case-insensitive)."},
						"max_sessions":  map[string]any{"type": "integer", "description": "Optional. Max matching sessions to return. Default 20, max 50."},
						"context_chars": map[string]any{"type": "integer", "description": "Optional. Characters of context on each side of the match snippet. Default 120, max 500."},
					},
					"required":             []string{"query"},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "create_session",
				Description: `Start a brand-new Claude Code session in cwd and send it an initial message. Returns {session_id, status} immediately; the session's first reply is automatically shown to the user in this chat, verbatim — do not wait for it or restate it. New id becomes focus.

USE FOR: the user wants fresh context that doesn't fit any session in <current_state> — scratch experiments, a new project, isolated debugging.

DO NOT USE FOR: routing into an existing session that matches the work — use send_to_session on that one. cwd must already exist; do not invent paths. /tmp is a safe default for ephemeral / scratch work when the user gives no hint.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"cwd":             map[string]any{"type": "string", "description": "Working directory for the new session. Must exist."},
						"initial_message": map[string]any{"type": "string", "description": "First message sent to the new session."},
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
