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
	"net/url"
	"strconv"
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

	p := newTestProcess(upstream.URL)
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

func TestProxyRewritesHostAndOrigin(t *testing.T) {
	var gotHost, gotOrigin string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotOrigin = r.Header.Get("Origin")
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()

	p := newTestProcess(upstream.URL)
	proxy := httptest.NewServer(p.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest("GET", proxy.URL+"/api/settings/describe", nil)
	req.Host = "192.168.3.2:7781"
	req.Header.Set("Origin", "http://192.168.3.2:7781")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	if gotHost != upstreamURL.Host {
		t.Errorf("upstream saw Host = %q, want %q", gotHost, upstreamURL.Host)
	}
	if want := "http://" + upstreamURL.Host; gotOrigin != want {
		t.Errorf("upstream saw Origin = %q, want %q", gotOrigin, want)
	}
}

func TestProxyPathHandlerRebasesAndStrips(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		switch {
		case strings.HasSuffix(r.URL.Path, ".css"):
			w.Header().Set("Content-Type", "text/css")
			fmt.Fprint(w, `body{background:url(/assets/bg.png)}`)
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<html><head><title>t</title><link href="/favicon.ico" rel="icon"></head><body><script src="/assets/app.js"></script></body></html>`)
		}
	}))
	defer upstream.Close()

	p := newTestProcess(upstream.URL)
	proxy := httptest.NewServer(p.PathHandler("/embed/test"))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/embed/test/deep/page")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if gotPath != "/deep/page" {
		t.Errorf("upstream path = %q, want /deep/page (prefix stripped)", gotPath)
	}
	for _, want := range []string{
		`<base href="/embed/test/">`,
		`src="/embed/test/assets/app.js"`,
		`href="/embed/test/favicon.ico"`,
		`src="/embed/test/__usher_bootstrap.js"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("HTML body missing %q\n---\n%s", want, body)
		}
	}

	css, err := http.Get(proxy.URL + "/embed/test/style.css")
	if err != nil {
		t.Fatal(err)
	}
	cbody, _ := io.ReadAll(css.Body)
	css.Body.Close()
	if !strings.Contains(string(cbody), "url(/embed/test/assets/bg.png)") {
		t.Errorf("CSS not rebased: %s", cbody)
	}
}

func TestProxyPathHandlerRedirectsBarePrefix(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()
	p := newTestProcess(upstream.URL)
	proxy := httptest.NewServer(p.PathHandler("/embed/test"))
	defer proxy.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(proxy.URL + "/embed/test")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != "/embed/test/" {
		t.Errorf("bare prefix: %d %q, want 307 /embed/test/", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestProxyKeepsCSPBeyondFrameAncestors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; frame-ancestors 'none'; script-src 'self' 'wasm-unsafe-eval'")
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()

	p := newTestProcess(upstream.URL)
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

	p := newTestProcess(dead)
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

	p := newTestProcess(upstream.URL)
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

func TestProxyPathHandlerRebasesJSAssets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprint(w, `const x = import("/plugins/??a/client.js,b/client.js");const y="/assets/app.js";const z=fetch("/api/session/list");const base="/plugins";`)
	}))
	defer upstream.Close()
	p := newTestProcess(upstream.URL)
	proxy := httptest.NewServer(p.PathHandler("/embed/test"))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/embed/test/app.js")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `import("/embed/test/plugins/??a/client.js,b/client.js")`) {
		t.Errorf("dynamic import not rebased: %s", body)
	}
	if !strings.Contains(string(body), `"/embed/test/assets/app.js"`) {
		t.Errorf("asset literal not rebased: %s", body)
	}
	if !strings.Contains(string(body), `fetch("/api/session/list")`) {
		t.Errorf("/api must be left to the runtime bootstrap: %s", body)
	}
	if !strings.Contains(string(body), `const base="/embed/test/plugins";`) {
		t.Errorf("trailing-slash-less base not rebased: %s", body)
	}
}

func TestProxyPathHandlerRebasesInlineScript(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head></head><body><script>var b="/plugins";</script></body></html>`)
	}))
	defer upstream.Close()
	p := newTestProcess(upstream.URL)
	proxy := httptest.NewServer(p.PathHandler("/embed/test"))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/embed/test/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `var b="/embed/test/plugins";`) {
		t.Errorf("inline loader base not rebased: %s", body)
	}
}

func TestProxyPathHandlerPassesThroughLargeBodies(t *testing.T) {
	payload := strings.Repeat("A", 500)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		fmt.Fprint(w, payload)
	}))
	defer upstream.Close()

	old := maxRebaseBytes
	maxRebaseBytes = 128
	defer func() { maxRebaseBytes = old }()

	p := newTestProcess(upstream.URL)
	proxy := httptest.NewServer(p.PathHandler("/embed/test"))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/embed/test/big.js")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != payload {
		t.Errorf("large body corrupted: got %d bytes, want %d", len(body), len(payload))
	}
}

// newTestProcess builds a Process pointing at a test upstream.
func newTestProcess(target string) *Process {
	p := &Process{spec: Spec{Name: "test"}}
	p.childURL.Store(target)
	p.query.Store("")
	return p
}

func TestProxyInjectsBasicAuth(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()

	p := newTestProcess(upstream.URL)
	p.spec.BasicAuth = "opencode:s3cret"
	proxy := httptest.NewServer(p.Handler())
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/api/session")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	user, pass, ok := parseBasic(gotAuth)
	if !ok || user != "opencode" || pass != "s3cret" {
		t.Fatalf("upstream Authorization = %q, want basic opencode:s3cret", gotAuth)
	}
}

// parseBasic decodes a "Basic <b64>" header value.
func parseBasic(h string) (user, pass string, ok bool) {
	const prefix = "Basic "
	if !strings.HasPrefix(h, prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, prefix))
	if err != nil {
		return "", "", false
	}
	u, p, found := strings.Cut(string(raw), ":")
	return u, p, found
}
