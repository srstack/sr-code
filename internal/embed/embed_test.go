package embed

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestMain re-execs the test binary as a helper child: with
// EMBED_TEST_CHILD=1 it runs a tiny loopback HTTP server instead of tests.
func TestMain(m *testing.M) {
	if os.Getenv("EMBED_TEST_CHILD") == "1" {
		// Optional crash-once mode: the first invocation (marker absent)
		// records the marker and exits; the restart serves. Used by the
		// restart test.
		if marker := os.Getenv("EMBED_TEST_CRASH_MARKER"); marker != "" {
			if _, err := os.Stat(marker); err != nil {
				_ = os.WriteFile(marker, []byte("1"), 0o644)
				os.Exit(7)
			}
		}
		port := ""
		for i, a := range os.Args {
			if a == "--port" && i+1 < len(os.Args) {
				port = os.Args[i+1]
			}
		}
		if os.Getenv("EMBED_TEST_URL_STYLE") == "fragment" {
			fmt.Println("Local:    http://127.0.0.1:" + port + "/#token=sekret")
		} else {
			fmt.Println("listening on http://127.0.0.1:" + port + "/?token=sekret") // stdout capture target
		}
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
		http.ListenAndServe("127.0.0.1:"+port, nil)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestProcessLifecycle(t *testing.T) {
	self, _ := os.Executable()
	p, err := Start(context.Background(), Spec{
		Name: "test", Title: "Test", Cmd: self,
		Args:       []string{"--port", "{port}"},
		Env:        []string{"EMBED_TEST_CHILD=1"},
		HealthPath: "/", URLPattern: `(http://\S+)`,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !p.Ready() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !p.Ready() {
		t.Fatal("child never became ready")
	}
	if p.StartQuery() != "?token=sekret" {
		t.Fatalf("StartQuery = %q", p.StartQuery())
	}
	resp, err := http.Get(p.ChildURL() + "/")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("child not serving: %v", err)
	}
	resp.Body.Close()
}

// TestProcessRestartsAfterEarlyExit: a child that exits immediately is
// restarted on a fresh port and becomes ready without any external action.
func TestProcessRestartsAfterEarlyExit(t *testing.T) {
	self, _ := os.Executable()
	marker := t.TempDir() + "/crashed"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := Start(ctx, Spec{
		Name: "test", Title: "Test", Cmd: self,
		Args:       []string{"--port", "{port}"},
		Env:        []string{"EMBED_TEST_CHILD=1", "EMBED_TEST_CRASH_MARKER=" + marker},
		HealthPath: "/",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	firstURL := p.ChildURL()
	deadline := time.Now().Add(15 * time.Second)
	for !p.Ready() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if !p.Ready() {
		t.Fatal("child did not become ready after an early exit")
	}
	if p.ChildURL() == firstURL {
		t.Errorf("restart reused port %q; want a fresh port", firstURL)
	}
	resp, err := http.Get(p.ChildURL() + "/")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("restarted child not serving: %v", err)
	}
	resp.Body.Close()
}

// TestProcessCapturesFragmentToken: children that put the boot token in the
// URL fragment (kimi 2.0 prints http://host:port/#token=…) must have it
// captured, not just query strings.
func TestProcessCapturesFragmentToken(t *testing.T) {
	self, _ := os.Executable()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := Start(ctx, Spec{
		Name: "test", Title: "Test", Cmd: self,
		Args:       []string{"--port", "{port}"},
		Env:        []string{"EMBED_TEST_CHILD=1", "EMBED_TEST_URL_STYLE=fragment"},
		HealthPath: "/", URLPattern: `(http://\S+)`,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for p.StartQuery() == "" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := p.StartQuery(); got != "#token=sekret" {
		t.Fatalf("StartQuery = %q, want %q", got, "#token=sekret")
	}
}
