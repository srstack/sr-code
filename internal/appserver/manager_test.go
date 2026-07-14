package appserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fakeAppServer(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "workers.log")
	script := filepath.Join(dir, "fake-codex")
	body := `#!/bin/sh
printf 'start %s\n' "$$" >> "$FAKE_LOG"
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"userAgent":"fake/1"}}'
IFS= read -r line
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"thread/start"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"new-thread"}}}' ;;
    *'"method":"thread/resume"'*)
      printf 'resume\n' >> "$FAKE_LOG"
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id" ;;
    *'"method":"turn/start"'*)
      thread=$(printf '%s' "$line" | sed -n 's/.*"threadId":"\([^"]*\)".*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"%s","turn":{"status":"completed"}}}\n' "$thread" ;;
    *'"method":"turn/interrupt"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id" ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, logPath
}

func TestManagerLRUEvictsIdleWorkerAndColdResumes(t *testing.T) {
	script, logPath := fakeAppServer(t)
	m := NewManager(script, nil, nil, nil, []string{"FAKE_LOG=" + logPath}, 1, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := m.StartThread(ctx, "/tmp", "")
	if err != nil {
		t.Fatal(err)
	}
	turn, _, err := m.StartTurn(ctx, id, "one", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	<-turn
	turn, _, err = m.StartTurn(ctx, "resumed-thread", "two", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	<-turn
	m.Shutdown()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "start ") != 2 || strings.Count(string(b), "resume") != 1 {
		t.Fatalf("worker lifecycle log = %q", b)
	}
	if m.Has(id) {
		t.Fatalf("LRU did not evict %s", id)
	}
}

func TestManagerConcurrentResumeStartsOneWorker(t *testing.T) {
	script, logPath := fakeAppServer(t)
	m := NewManager(script, nil, nil, nil, []string{"FAKE_LOG=" + logPath}, 2, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.getOrResume(ctx, "same-thread", "/tmp")
			errs <- err
		}()
	}
	wg.Wait()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	m.Shutdown()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "start ") != 1 || strings.Count(string(b), "resume") != 1 {
		t.Fatalf("concurrent resume log = %q", b)
	}
}

func TestManagerWorkerFailureIsIsolated(t *testing.T) {
	script, logPath := fakeAppServer(t)
	m := NewManager(script, nil, nil, nil, []string{"FAKE_LOG=" + logPath}, 2, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := m.getOrResume(ctx, "first", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.getOrResume(ctx, "second", "/tmp"); err != nil {
		t.Fatal(err)
	}
	first.client.mu.Lock()
	cmd := first.client.cmd
	first.client.mu.Unlock()
	first.client.stopProcess(cmd, context.Canceled)
	if m.Has("first") {
		t.Error("failed worker still reported live")
	}
	if !m.Has("second") {
		t.Error("unrelated worker was lost")
	}
	m.Shutdown()
}

func TestManagerMaxLiveRejectsWhenAllWorkersBusy(t *testing.T) {
	m := NewManager("unused", nil, nil, nil, nil, 1, nil)
	m.workers["busy"] = &worker{client: m.newClient(), busy: true, lastUsed: time.Now()}
	if _, err := m.reserve(); err == nil || !strings.Contains(err.Error(), "all busy") {
		t.Fatalf("reserve error = %v, want all busy", err)
	}
}
