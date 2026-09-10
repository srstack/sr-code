package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/auth"
	"github.com/nexustar/usher/internal/embed"
)

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// waitForServer polls url until any HTTP response comes back (connection
// refused means Run hasn't reached Serve yet).
func waitForServer(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s never came up", url)
}

func TestEmbedListenersAndAPI(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		_, _ = w.Write([]byte("embed upstream body"))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	authStore, err := auth.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := authStore.SetPassword("test-password"); err != nil {
		t.Fatal(err)
	}
	cookieVal, err := authStore.IssueCookie()
	if err != nil {
		t.Fatal(err)
	}
	cookie := authStore.NewSessionCookie(cookieVal)

	mainPort := freeTCPPort(t)
	embedPort := freeTCPPort(t)
	s := NewServer("127.0.0.1:"+strconv.Itoa(mainPort), filepath.Join(dir, "hook.sock"),
		"", authStore, nil, nil, nil, nil, "", "", "", slog.Default())
	s.Embeds = []EmbedMount{{Process: embed.NewForTest(upstream), ListenPort: embedPort}}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-errCh; err != nil {
			t.Fatalf("Run: %v", err)
		}
	}()

	mainBase := "http://127.0.0.1:" + strconv.Itoa(mainPort)
	embedBase := "http://127.0.0.1:" + strconv.Itoa(embedPort)
	waitForServer(t, mainBase+"/healthz")
	waitForServer(t, embedBase+"/")

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	t.Run("embed listener requires auth", func(t *testing.T) {
		resp, err := noRedirect.Get(embedBase + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303", resp.StatusCode)
		}
		// The embed listener's root is the child, so the redirect must point
		// at the MAIN UI's login form (absolute URL, main port).
		want := mainBase + "/login"
		if loc := resp.Header.Get("Location"); loc != want {
			t.Errorf("Location = %q, want %q", loc, want)
		}
	})

	t.Run("embed listener has no auth-exempt paths", func(t *testing.T) {
		// The main listener's exemptions must not leak onto the embed port:
		// every path there proxies to the child, so an exempt path would
		// serve child bytes to unauthenticated clients.
		for _, p := range []string{"/login", "/logout", "/healthz", "/sw.js", "/manifest.webmanifest", "/icons/icon-192.png"} {
			resp, err := noRedirect.Get(embedBase + p)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusSeeOther {
				t.Errorf("GET %s: status = %d, want 303", p, resp.StatusCode)
			}
			if loc := resp.Header.Get("Location"); loc != mainBase+"/login" {
				t.Errorf("GET %s: Location = %q, want %q", p, loc, mainBase+"/login")
			}
			if strings.Contains(string(body), "embed upstream body") {
				t.Errorf("GET %s: unauthenticated request reached the child", p)
			}
		}
	})

	t.Run("api embeds requires auth", func(t *testing.T) {
		resp, err := noRedirect.Get(mainBase + "/api/embeds")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("api embeds lists the mount", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, mainBase+"/api/embeds", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		// Cmd/Args of the spec must never be exposed; an empty query is omitted.
		for _, key := range []string{`"cmd"`, `"args"`, `"query"`} {
			if strings.Contains(string(body), key) {
				t.Errorf("body %s must not contain %s", body, key)
			}
		}
		var list []struct {
			Name  string `json:"name"`
			Title string `json:"title"`
			Port  int    `json:"port"`
			Ready bool   `json:"ready"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			t.Fatalf("body %s: %v", body, err)
		}
		if len(list) != 1 {
			t.Fatalf("got %d embeds, want 1", len(list))
		}
		got := list[0]
		if got.Name != "test" || got.Title != "Test" {
			t.Errorf("name/title = %q/%q, want test/Test", got.Name, got.Title)
		}
		if got.Port != embedPort {
			t.Errorf("port = %d, want %d", got.Port, embedPort)
		}
		if !got.Ready {
			t.Error("ready = false, want true")
		}
	})

	t.Run("embed listener proxies with framing headers stripped", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, embedBase+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(cookie)
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "embed upstream body" {
			t.Errorf("body = %q", body)
		}
		if got := resp.Header.Get("X-Frame-Options"); got != "" {
			t.Errorf("X-Frame-Options = %q, want stripped", got)
		}
		if got := resp.Header.Get("Content-Security-Policy"); got != "" {
			t.Errorf("Content-Security-Policy = %q, want stripped", got)
		}
	})
}

// TestEmbedWebSocketUpgradeThroughGzip is the regression test for embed
// listeners 502ing every real-browser WebSocket upgrade: browsers always
// send Accept-Encoding: gzip on the WS handshake, and gzipMiddleware must
// not wrap (and thereby de-Hijack) the upgrade's ResponseWriter — Go 1.25's
// ReverseProxy hijacks via http.NewResponseController.
func TestEmbedWebSocketUpgradeThroughGzip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream ResponseWriter is not a Hijacker")
			return
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			t.Errorf("upstream hijack: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(bufrw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: x\r\n\r\n")
		bufrw.Flush()
		// Hold the tunnel briefly so the client can read the 101.
		buf := make([]byte, 1)
		conn.Read(buf) // blocks until client closes or sends a frame
	}))
	defer upstream.Close()

	// The embed listener's real middleware stack, minus auth (not under test).
	srv := httptest.NewServer(gzipMiddleware(embed.NewForTest(upstream).Handler()))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Raw client modeled on a real browser handshake — including the
	// Accept-Encoding: gzip that triggered the 502.
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nAccept-Encoding: gzip\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", addr)
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 101") {
		t.Errorf("status line = %q, want it to start with %q", line, "HTTP/1.1 101")
	}
}
