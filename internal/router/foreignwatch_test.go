package router

import (
	"os"
	"testing"
)

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

const foreignTurn = `{"type":"user","message":{"role":"user","content":"<task-notification>workflow done</task-notification>"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"the research report"}]}}
`

// TestForeignTurnDetectedAndRelayedOnce: a turn the session ran on its own
// (background continuation) is detected at its end-of-turn marker, relayed
// exactly once with the assistant's text, and announced on the broker so an
// open detail view refetches.
func TestForeignTurnDetectedAndRelayedOnce(t *testing.T) {
	r := newQueueTestRouter(t)
	var got []string
	r.SetForeignTurnHandler(func(id, text string) { got = append(got, id+"|"+text) })

	// First sight baselines the pre-existing log; history is never relayed.
	r.scanForeignTurns()
	if len(got) != 0 {
		t.Fatalf("history relayed: %v", got)
	}

	path, ok := r.discovery.Path("abc12345")
	if !ok {
		t.Fatal("no path for session")
	}
	appendFile(t, path, foreignTurn)
	r.scanForeignTurns() // no marker yet: turn still open
	if len(got) != 0 {
		t.Fatalf("mid-turn content relayed: %v", got)
	}

	ch, unsub := r.broker.Subscribe("abc12345")
	defer unsub()
	appendFile(t, path, `{"type":"system","subtype":"turn_duration"}`+"\n")
	r.scanForeignTurns()
	if len(got) != 1 || got[0] != "abc12345|the research report" {
		t.Fatalf("relay = %v", got)
	}
	select {
	case ev := <-ch:
		if ev.Type != "subprocess.exit" {
			t.Errorf("broker event = %q, want subprocess.exit", ev.Type)
		}
	default:
		t.Error("no synthetic exit published for the detail view")
	}

	r.scanForeignTurns() // baseline advanced: no re-relay
	if len(got) != 1 {
		t.Errorf("double relay: %v", got)
	}
}

// TestForeignWatchIgnoresUsherTurns: turns usher itself ran (activeSend +
// baseline bump at turn end) never reach the foreign handler, while a real
// foreign turn afterwards still does.
func TestForeignWatchIgnoresUsherTurns(t *testing.T) {
	r := newQueueTestRouter(t)
	var got []string
	r.SetForeignTurnHandler(func(id, text string) { got = append(got, text) })
	r.scanForeignTurns() // baseline

	path, _ := r.discovery.Path("abc12345")
	tok := &sendToken{cancel: func() {}}
	r.sendMu.Lock()
	r.activeSend["abc12345"] = tok
	r.sendMu.Unlock()

	appendFile(t, path, `{"type":"user","message":{"role":"user","content":"usher prompt"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"usher-relayed answer"}]}}
{"type":"system","subtype":"turn_duration"}
`)
	r.scanForeignTurns() // in-flight usher turn: suppressed
	if len(got) != 0 {
		t.Fatalf("usher turn relayed as foreign: %v", got)
	}
	r.markSendIdle("abc12345", tok)
	r.releaseSend("abc12345", tok)
	r.scanForeignTurns() // baseline was bumped at turn end
	if len(got) != 0 {
		t.Fatalf("usher turn re-relayed after release: %v", got)
	}

	appendFile(t, path, foreignTurn+`{"type":"system","subtype":"turn_duration"}`+"\n")
	r.scanForeignTurns()
	if len(got) != 1 || got[0] != "the research report" {
		t.Errorf("foreign turn after usher turn: %v", got)
	}
}

func TestTurnCompleteMarker(t *testing.T) {
	cases := []struct {
		backend string
		line    string
		want    bool
	}{
		{"claude", `{"type":"system","subtype":"turn_duration","durationMs":12}`, true},
		{"claude", `{"type":"assistant","message":{}}`, false},
		{"codex", `{"timestamp":"t","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`, true},
		{"codex", `{"type":"event_msg","payload":{"type":"agent_message"}}`, false},
		{"claude", `not json`, false},
	}
	for _, c := range cases {
		if got := turnCompleteMarker(c.backend, []byte(c.line)); got != c.want {
			t.Errorf("turnCompleteMarker(%s, %s) = %v, want %v", c.backend, c.line, got, c.want)
		}
	}
}

// TestForeignTurnsBatchedInOneIntervalAllRelayed: two foreign turns
// completing between polls (chained background continuations) must BOTH be
// relayed, in order — the baseline jump may not swallow the earlier one.
func TestForeignTurnsBatchedInOneIntervalAllRelayed(t *testing.T) {
	r := newQueueTestRouter(t)
	var got []string
	r.SetForeignTurnHandler(func(_, text string) { got = append(got, text) })
	r.scanForeignTurns() // baseline

	path, _ := r.discovery.Path("abc12345")
	appendFile(t, path, `{"type":"user","message":{"role":"user","content":"first notification"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first result"}]}}
{"type":"system","subtype":"turn_duration"}
{"type":"user","message":{"role":"user","content":"second notification"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"second result"}]}}
{"type":"system","subtype":"turn_duration"}
`)
	r.scanForeignTurns()
	if len(got) != 2 || got[0] != "first result" || got[1] != "second result" {
		t.Fatalf("relayed %v, want both results in order", got)
	}
	r.scanForeignTurns()
	if len(got) != 2 {
		t.Errorf("re-relayed after baseline: %v", got)
	}
}
