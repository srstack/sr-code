package usheragent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
