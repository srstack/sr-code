package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestLegacyPermissionRequestContinuesWithoutPending(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hook/PermissionRequest", nil)
	req.SetPathValue("event", "PermissionRequest")
	(&Server{}).handleHook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got, want := rec.Body.String(), "{\"continue\":true}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
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
