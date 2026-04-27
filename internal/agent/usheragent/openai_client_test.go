package usheragent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func atomicAdd(p *int32, n int32) int32 { return atomic.AddInt32(p, n) }

func TestChatClient_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key-123" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var req ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "test-model" {
			t.Errorf("model = %q", req.Model)
		}
		if len(req.Messages) != 1 || req.Messages[0].Content != "hi" {
			t.Errorf("messages = %+v", req.Messages)
		}
		_, _ = w.Write([]byte(`{"id":"r1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer srv.Close()

	c := NewChatClient(srv.URL+"/v1", "key-123")
	resp, err := c.ChatCompletion(context.Background(), ChatRequest{
		Model:    "test-model",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content != "hello" {
		t.Errorf("content = %q", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
}

func TestChatClient_NoAuthHeaderWhenKeyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("expected no Authorization header, got %q", got)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := NewChatClient(srv.URL+"/v1", "") // empty -> Ollama / local
	if _, err := c.ChatCompletion(context.Background(), ChatRequest{
		Model:    "any",
		Messages: []ChatMessage{{Role: "user", Content: "x"}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestChatClient_4xxErrorParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad model","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	c := NewChatClient(srv.URL+"/v1", "k")
	_, err := c.ChatCompletion(context.Background(), ChatRequest{
		Model:    "x",
		Messages: []ChatMessage{{Role: "user", Content: "x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "bad model") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("expected status code in err: %v", err)
	}
}

func TestChatClient_NonJSONErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream timeout"))
	}))
	defer srv.Close()

	c := NewChatClient(srv.URL+"/v1", "k")
	_, err := c.ChatCompletion(context.Background(), ChatRequest{Model: "x", Messages: []ChatMessage{{Role: "user", Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "upstream") {
		t.Errorf("err = %v", err)
	}
}

func TestChatClient_RetriesOn429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomicAdd(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"after retry"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := NewChatClient(srv.URL+"/v1", "k")
	resp, err := c.ChatCompletion(context.Background(), ChatRequest{
		Model:    "x",
		Messages: []ChatMessage{{Role: "user", Content: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content != "after retry" {
		t.Errorf("Content = %q", resp.Choices[0].Message.Content)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls (1 retry), got %d", calls)
	}
}

func TestChatClient_RetriesOn5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomicAdd(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	c := NewChatClient(srv.URL+"/v1", "k")
	resp, err := c.ChatCompletion(context.Background(), ChatRequest{Model: "x", Messages: []ChatMessage{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content != "recovered" {
		t.Errorf("Content = %q", resp.Choices[0].Message.Content)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls, got %d", calls)
	}
}

func TestChatClient_NoRetryOn4xxOther(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomicAdd(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
	}))
	defer srv.Close()
	c := NewChatClient(srv.URL+"/v1", "k")
	if _, err := c.ChatCompletion(context.Background(), ChatRequest{Model: "x", Messages: []ChatMessage{{Role: "user", Content: "x"}}}); err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on 400), got %d", calls)
	}
}

func TestChatClient_GivesUpAfterOneRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomicAdd(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := NewChatClient(srv.URL+"/v1", "k")
	if _, err := c.ChatCompletion(context.Background(), ChatRequest{Model: "x", Messages: []ChatMessage{{Role: "user", Content: "x"}}}); err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls != 2 {
		t.Errorf("expected 2 calls (1 + 1 retry), got %d", calls)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := map[string]int{"": 0, "5": 5, "0": 0, "-3": 0, "abc": 0, "  10  ": 10}
	for in, want := range cases {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestChatClient_BaseURLTrailingSlashTolerated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q (no double-slash expected)", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := NewChatClient(srv.URL+"/v1/", "k")
	if _, err := c.ChatCompletion(context.Background(), ChatRequest{Model: "x", Messages: []ChatMessage{{Role: "user", Content: "x"}}}); err != nil {
		t.Fatal(err)
	}
}
