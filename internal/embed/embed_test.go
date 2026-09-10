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
		port := ""
		for i, a := range os.Args {
			if a == "--port" && i+1 < len(os.Args) {
				port = os.Args[i+1]
			}
		}
		fmt.Println("listening on http://127.0.0.1:" + port + "/?token=sekret") // stdout capture target
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
