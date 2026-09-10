package embed

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyStripsFramingHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		fmt.Fprint(w, "path="+r.URL.Path)
	}))
	defer upstream.Close()

	p := &Process{spec: Spec{Name: "test"}, childURL: upstream.URL}
	proxy := httptest.NewServer(p.Handler())
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/some/page?x=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "path=/some/page" {
		t.Errorf("body = %q, want %q", got, "path=/some/page")
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "" {
		t.Errorf("X-Frame-Options = %q, want stripped", got)
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != "" {
		t.Errorf("Content-Security-Policy = %q, want stripped (only directive was frame-ancestors)", got)
	}
}

func TestProxyKeepsCSPBeyondFrameAncestors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; frame-ancestors 'none'; script-src 'self' 'wasm-unsafe-eval'")
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()

	p := &Process{spec: Spec{Name: "test"}, childURL: upstream.URL}
	proxy := httptest.NewServer(p.Handler())
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	want := "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'"
	if got := resp.Header.Get("Content-Security-Policy"); got != want {
		t.Errorf("Content-Security-Policy = %q, want %q (frame-ancestors rewritten out)", got, want)
	}
}

func TestProxyNotReady(t *testing.T) {
	// Point at a closed port so every proxy attempt fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()

	p := &Process{spec: Spec{Name: "test"}, childURL: dead}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	p.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadGateway)
	}
	if got := rr.Body.String(); !strings.Contains(got, `embed "test" is not ready`) {
		t.Errorf("body = %q, want it to mention %q", got, `embed "test" is not ready`)
	}
}

func TestProxyWebSocketUpgrade(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			http.NotFound(w, r)
			return
		}
		key := r.Header.Get("Sec-WebSocket-Key")
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
		accept := wsAccept(key)
		fmt.Fprintf(bufrw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		bufrw.Flush()
		defer conn.Close()
		// Hold the tunnel briefly so the client can read the 101.
		buf := make([]byte, 1)
		conn.Read(buf) // blocks until client closes or sends a frame
	}))
	defer upstream.Close()

	p := &Process{spec: Spec{Name: "test"}, childURL: upstream.URL}
	proxy := httptest.NewServer(p.Handler())
	defer proxy.Close()

	addr := strings.TrimPrefix(proxy.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", addr)
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 101") {
		t.Errorf("status line = %q, want it to start with %q", line, "HTTP/1.1 101")
	}
}

// wsAccept computes Sec-WebSocket-Accept per RFC 6455.
func wsAccept(key string) string {
	h := sha1.New()
	io.WriteString(h, key+"258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
