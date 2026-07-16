package web

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTerminalControlsAreAllowListed(t *testing.T) {
	want := map[string]string{
		"up": "Up", "down": "Down", "left": "Left", "right": "Right",
		"enter": "Enter", "escape": "Escape", "tab": "Tab",
		"ctrl-c": "C-c", "ctrl-z": "C-z", "ctrl-d": "C-d", "ctrl-x": "C-x",
		"ctrl-o": "C-o", "ctrl-w": "C-w", "ctrl-k": "C-k", "ctrl-u": "C-u",
	}
	if len(terminalControls) != len(want) {
		t.Fatalf("terminalControls has %d entries, want %d", len(terminalControls), len(want))
	}
	for name, key := range want {
		if terminalControls[name] != key {
			t.Errorf("terminalControls[%q] = %q, want %q", name, terminalControls[name], key)
		}
	}
}

func TestTerminalInputValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty text", `{"request_id":"r1"}`},
		{"missing request id", `{"text":"pwd"}`},
		{"nul", `{"text":"a\u0000b","request_id":"r1"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/terminal/input", bytes.NewBufferString(tt.body))
			(&Server{}).handleTerminalInput(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestFocusSwitchBanner(t *testing.T) {
	full := "0af0c1d2-3e4f-5678-9abc-def012345678"

	// Same focus → no banner; the persistent UI focus link already shows it.
	if got := focusSwitchBanner(full, full, "auth"); got != "" {
		t.Errorf("same focus should give no banner, got %q", got)
	}
	// Turn that touched no session → no banner.
	if got := focusSwitchBanner(full, "", "auth"); got != "" {
		t.Errorf("untouched turn should give no banner, got %q", got)
	}

	// Switch between sessions → "Switching to" + title + link.
	got := focusSwitchBanner("11111111-aaaa", full, "auth-service")
	want := "↪ Switching to [auth-service](#/s/" + full + ")\n\n"
	if got != want {
		t.Errorf("switch banner:\n got %q\nwant %q", got, want)
	}

	// First focus (none → X) → "Routing to".
	if got := focusSwitchBanner("", full, "auth-service"); got != "↪ Routing to [auth-service](#/s/"+full+")\n\n" {
		t.Errorf("first-focus banner = %q", got)
	}

	// Untitled session → short id as link text.
	if got := focusSwitchBanner("", full, ""); got != "↪ Routing to [0af0c1d2](#/s/"+full+")\n\n" {
		t.Errorf("untitled banner = %q", got)
	}
}

func TestGzipMiddleware(t *testing.T) {
	json200 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "26")
		_, _ = w.Write([]byte(`{"hello":"compressed-yes"}`))
	})
	sse := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, ok := w.(http.Flusher); !ok {
			t.Error("wrapper must still satisfy http.Flusher")
		}
		_, _ = w.Write([]byte("event: x\ndata: {}\n\n"))
	})
	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	})

	t.Run("compresses json 200", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/x", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		gzipMiddleware(json200).ServeHTTP(rec, req)
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", got)
		}
		if rec.Header().Get("Content-Length") != "" {
			t.Error("identity Content-Length must be dropped on a gzipped body")
		}
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("body is not gzip: %v", err)
		}
		body, _ := io.ReadAll(zr)
		if string(body) != `{"hello":"compressed-yes"}` {
			t.Errorf("decompressed body = %q", body)
		}
	})

	t.Run("skips without accept-encoding", func(t *testing.T) {
		rec := httptest.NewRecorder()
		gzipMiddleware(json200).ServeHTTP(rec, httptest.NewRequest("GET", "/api/x", nil))
		if rec.Header().Get("Content-Encoding") != "" {
			t.Error("must not compress when the client didn't ask")
		}
		if rec.Body.String() != `{"hello":"compressed-yes"}` {
			t.Errorf("body = %q", rec.Body.String())
		}
	})

	t.Run("leaves SSE untouched", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/s/events", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		gzipMiddleware(sse).ServeHTTP(rec, req)
		if rec.Header().Get("Content-Encoding") != "" {
			t.Error("event-stream must never be compressed")
		}
		if rec.Body.String() != "event: x\ndata: {}\n\n" {
			t.Errorf("body = %q", rec.Body.String())
		}
	})

	t.Run("leaves non-200 untouched", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/x", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		gzipMiddleware(notFound).ServeHTTP(rec, req)
		if rec.Header().Get("Content-Encoding") != "" {
			t.Error("non-200 must not be compressed")
		}
		if rec.Body.String() != `{"error":"nope"}` {
			t.Errorf("body = %q", rec.Body.String())
		}
	})
}

func TestPermissionRequestDecision(t *testing.T) {
	// allow → behavior allow, no message
	allow := permissionRequestDecision("allow", "ignored reason", nil)
	hso, _ := allow["hookSpecificOutput"].(map[string]any)
	if hso == nil || hso["hookEventName"] != "PermissionRequest" {
		t.Fatalf("allow: bad hookSpecificOutput: %v", allow)
	}
	dec, _ := hso["decision"].(map[string]any)
	if dec == nil || dec["behavior"] != "allow" {
		t.Fatalf("allow: behavior = %v", dec)
	}
	if _, hasMsg := dec["message"]; hasMsg {
		t.Errorf("allow must not carry a message: %v", dec)
	}

	// deny with reason → behavior deny + message
	deny := permissionRequestDecision("deny", "blocked by usher", nil)
	dec = deny["hookSpecificOutput"].(map[string]any)["decision"].(map[string]any)
	if dec["behavior"] != "deny" || dec["message"] != "blocked by usher" {
		t.Fatalf("deny: decision = %v", dec)
	}

	// deny without reason → behavior deny, no message key
	bare := permissionRequestDecision("deny", "", nil)
	dec = bare["hookSpecificOutput"].(map[string]any)["decision"].(map[string]any)
	if _, hasMsg := dec["message"]; hasMsg {
		t.Errorf("deny without reason must omit message: %v", dec)
	}
}

func TestPermissionRequestDecisionUpdatedPermissions(t *testing.T) {
	first := json.RawMessage(`{"type":"addRules","behavior":"allow","rules":[{"toolName":"Bash","ruleContent":"npm test"}],"destination":"session"}`)
	second := json.RawMessage(`{"type":"addDirectories","behavior":"allow","directories":["/work/shared"],"destination":"session"}`)
	deny := json.RawMessage(`{"type":"addRules","behavior":"deny","rules":[{"toolName":"Read"}]}`)
	suggestions := allowSuggestions([]json.RawMessage{first, deny, second})
	if len(suggestions) != 2 || string(suggestions[0]) != string(first) || string(suggestions[1]) != string(second) {
		t.Fatalf("filtered suggestions = %s", suggestions)
	}
	out := permissionRequestDecision("allow", "", suggestions)
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"updatedPermissions":[{"type":"addRules"`)) {
		t.Fatalf("response missing updatedPermissions: %s", data)
	}
	if !hasAllowSuggestion([]json.RawMessage{first}) {
		t.Fatal("allow suggestion was not recognized")
	}
	if hasAllowSuggestion([]json.RawMessage{deny}) {
		t.Fatal("deny suggestion enabled allow always")
	}
}

func TestCodexModels(t *testing.T) {
	// codex disabled → nil
	if got := (&Server{codexModelsPath: ""}).codexModels(); got != nil {
		t.Errorf("disabled → nil, got %v", got)
	}
	// codex enabled but cache missing → fallback to the current named models.
	got := (&Server{codexModelsPath: "/no/such/models_cache.json"}).codexModels()
	if len(got) != 2 || got[0].Value != "gpt-5.5" {
		t.Fatalf("missing-cache fallback = %v, want gpt-5.5 then mini", got)
	}
	// a real catalog → list-visible only, sorted by priority
	p := filepath.Join(t.TempDir(), "models_cache.json")
	os.WriteFile(p, []byte(`{"models":[
		{"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list","priority":2,"default_reasoning_level":"low"},
		{"slug":"auto-review","display_name":"x","visibility":"hide","priority":1,"default_reasoning_level":"high"},
		{"slug":"gpt-5.4-mini","display_name":"GPT-5.4 Mini","visibility":"list","priority":1}
	]}`), 0o644)
	got = (&Server{codexModelsPath: p}).codexModels()
	if len(got) != 2 || got[0].Value != "gpt-5.4-mini" || got[1].Value != "gpt-5.5" {
		t.Fatalf("catalog parse/sort = %v (want mini then 5.5, hide excluded)", got)
	}
	s := &Server{codexModelsPath: p}
	if effort := s.codexDefaultEffort("gpt-5.5"); effort != "low" {
		t.Errorf("default effort = %q, want low", effort)
	}
	if effort := s.codexDefaultEffort("auto-review"); effort != "high" {
		t.Errorf("hidden model default effort = %q, want high", effort)
	}
}

func TestHumanizeAge(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		last time.Time
		want string
	}{
		{time.Time{}, "unknown"},                 // zero → unknown
		{now.Add(-10 * time.Second), "just now"}, // <1m
		{now.Add(-5 * time.Minute), "5m ago"},    // minutes
		{now.Add(-3 * time.Hour), "3h ago"},      // hours
		{now.Add(-50 * time.Hour), "2d ago"},     // days (floored)
		{now.Add(10 * time.Second), "just now"},  // future/skew
	}
	for _, c := range cases {
		if got := humanizeAge(now, c.last); got != c.want {
			t.Errorf("humanizeAge(now, %v) = %q, want %q", c.last, got, c.want)
		}
	}
}
