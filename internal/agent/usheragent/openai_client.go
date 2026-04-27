package usheragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ChatClient is a minimal Chat Completions client. It speaks the OpenAI
// Chat Completions wire format, which is the de facto standard implemented
// by OpenAI itself, Ollama, DeepSeek, Together, Groq, OpenRouter, vLLM,
// LM Studio, and Anthropic's OpenAI-compatible endpoint.
//
// We intentionally do NOT use a vendor SDK: the protocol is a few hundred
// lines, hand-rolling stays provider-agnostic, and provider-specific
// extensions (Anthropic prompt caching, OpenAI structured outputs) are
// out of scope for usher's routing agent.
type ChatClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewChatClient builds a client for the given OpenAI-compatible endpoint.
// baseURL should include the API version segment (e.g. "https://api.openai.com/v1"
// or "http://localhost:11434/v1"); the trailing slash is optional.
// apiKey may be empty for backends that don't require auth (local Ollama).
func NewChatClient(baseURL, apiKey string) *ChatClient {
	return &ChatClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 120 * time.Second},
	}
}

// --- Wire types ---------------------------------------------------------

type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Tools    []ChatTool    `json:"tools,omitempty"`
}

type ChatMessage struct {
	Role       string     `json:"role"` // system | user | assistant | tool
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant role
	ToolCallID string     `json:"tool_call_id,omitempty"` // tool role
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function"
	Function ToolCallFunc `json:"function"`
}

type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded string per OpenAI spec
}

type ChatTool struct {
	Type     string       `json:"type"` // always "function"
	Function ChatFunction `json:"function"`
}

type ChatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type ChatResponse struct {
	ID      string       `json:"id"`
	Choices []ChatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`
}

type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type apiErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"` // some providers return string, others int
	} `json:"error"`
}

// APIError carries the HTTP status alongside the message so retry logic
// can distinguish 429 / 5xx from permanent failures without string-matching.
type APIError struct {
	StatusCode int
	Message    string
	// RetryAfter is the parsed `Retry-After` header in seconds, if present.
	// 0 means the header was absent or unparseable.
	RetryAfter int
}

func (e *APIError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message) }

// retry config — single retry on 429 or 5xx with a bounded back-off.
const (
	maxRetryAttempts = 1
	defaultBackoff   = 2 * time.Second
	maxBackoff       = 60 * time.Second
)

// ChatCompletion sends a non-streaming request and returns the decoded
// response. On a transient failure (HTTP 429 or 5xx), retries once,
// honoring the `Retry-After` header (capped at 60s) when present.
func (c *ChatClient) ChatCompletion(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	var resp ChatResponse
	var err error
	for attempt := 0; ; attempt++ {
		resp, err = c.doChatCompletion(ctx, req)
		if err == nil {
			return resp, nil
		}
		if attempt >= maxRetryAttempts || !shouldRetry(err) {
			return resp, err
		}
		delay := backoffFor(err)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return resp, ctx.Err()
		}
	}
}

func (c *ChatClient) doChatCompletion(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("chat completion request: %w", err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("read response body: %w", err)
	}

	if httpResp.StatusCode >= 400 {
		msg := ""
		var e apiErrorBody
		if err := json.Unmarshal(raw, &e); err == nil && e.Error.Message != "" {
			msg = e.Error.Message
		} else {
			msg = truncate(string(raw), 500)
		}
		return ChatResponse{}, &APIError{
			StatusCode: httpResp.StatusCode,
			Message:    msg,
			RetryAfter: parseRetryAfter(httpResp.Header.Get("Retry-After")),
		}
	}

	var resp ChatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ChatResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return resp, nil
}

// shouldRetry returns true for 429 and 5xx APIErrors. Network errors are
// not retried — most are local and unlikely to self-heal in 2s.
func shouldRetry(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.StatusCode == http.StatusTooManyRequests || (ae.StatusCode >= 500 && ae.StatusCode < 600)
}

// backoffFor returns the wait duration before retrying err. For 429 with
// a Retry-After header, that's the requested delay (capped at 60s); for
// other retryable errors it's a fixed default.
func backoffFor(err error) time.Duration {
	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		d := time.Duration(ae.RetryAfter) * time.Second
		if d > maxBackoff {
			d = maxBackoff
		}
		return d
	}
	return defaultBackoff
}

// parseRetryAfter parses the Retry-After header. The HTTP spec allows both
// "delta seconds" and an HTTP-date; we only handle the integer form, which
// is what every LLM provider uses in practice.
func parseRetryAfter(h string) int {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if n, err := strconv.Atoi(h); err == nil && n > 0 {
		return n
	}
	return 0
}
