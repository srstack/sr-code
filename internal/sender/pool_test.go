package sender

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// fakeTmux models just enough tmux server state for pool tests: an ordered
// list of window names within the single "usher" session, plus a log of the
// inject-path commands. Guarded by mu because the Sender drives the runner
// from a goroutine while tests inspect cmds.
type fakeTmux struct {
	mu        sync.Mutex
	windows   []string
	exists    bool
	cmds      [][]string
	failSpawn bool // when set, new-session/new-window return an error
}

func (f *fakeTmux) run(args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, args)
	switch args[0] {
	case "has-session":
		if f.exists {
			return "", nil
		}
		return "", errors.New("no session")
	case "new-session":
		if f.failSpawn {
			return "", errors.New("spawn failed")
		}
		f.exists = true
		f.addWindow(flagVal(args, "-n"))
		return "", nil
	case "new-window":
		if f.failSpawn {
			return "", errors.New("spawn failed")
		}
		f.addWindow(flagVal(args, "-n"))
		return "", nil
	case "list-windows":
		return strings.Join(f.windows, "\n"), nil
	case "kill-window":
		f.delWindow(idFromTarget(flagVal(args, "-t")))
		return "", nil
	case "kill-session":
		f.windows = nil
		f.exists = false
		return "", nil
	default: // set-window-option, set-buffer, paste-buffer, send-keys
		return "", nil
	}
}

func (f *fakeTmux) addWindow(name string) {
	if name != "" && !contains(f.windows, name) {
		f.windows = append(f.windows, name)
	}
}
func (f *fakeTmux) delWindow(name string) {
	for i, w := range f.windows {
		if w == name {
			f.windows = append(f.windows[:i], f.windows[i+1:]...)
			return
		}
	}
}

// countCmd returns how many recorded commands start with verb (thread-safe).
func (f *fakeTmux) countCmd(verb string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.cmds {
		if len(c) > 0 && c[0] == verb {
			n++
		}
	}
	return n
}

func flagVal(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
func idFromTarget(t string) string {
	if i := strings.IndexByte(t, ':'); i >= 0 {
		return t[i+1:]
	}
	return t
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestPool_EnsureSpawnsAndIsIdempotent(t *testing.T) {
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, nil, 8, quietLogger())

	if _, err := p.ensure("s1", "/tmp", true); err != nil {
		t.Fatal(err)
	}
	if !p.has("s1") {
		t.Fatal("s1 should be live after ensure")
	}
	// Second ensure must not create a second window.
	if _, err := p.ensure("s1", "/tmp", true); err != nil {
		t.Fatal(err)
	}
	if len(f.windows) != 1 {
		t.Fatalf("expected 1 window, got %v", f.windows)
	}
}

func TestPool_LRUEviction(t *testing.T) {
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, nil, 2, quietLogger())

	mustEnsure(t, p, "a")
	mustEnsure(t, p, "b")
	// Touch a so b is now the least-recently-used.
	mustEnsure(t, p, "a")
	// c exceeds cap (2) -> evict LRU, which is b.
	mustEnsure(t, p, "c")

	if p.has("b") {
		t.Fatal("b should have been evicted")
	}
	if !p.has("a") || !p.has("c") {
		t.Fatalf("a and c should be live, windows=%v", f.windows)
	}
	if len(f.windows) != 2 {
		t.Fatalf("cap is 2, got %v", f.windows)
	}
}

func TestPool_AdoptExistingWindows(t *testing.T) {
	f := &fakeTmux{exists: true, windows: []string{"old1", "old2"}}
	p := newPool(f, "claude", nil, nil, 8, quietLogger())
	if !p.has("old1") || !p.has("old2") {
		t.Fatal("adopt should pick up pre-existing windows")
	}
	if len(p.lru) != 2 {
		t.Fatalf("lru should be seeded with 2, got %v", p.lru)
	}
}

func TestPool_InjectUsesBracketedPaste(t *testing.T) {
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, nil, 8, quietLogger())
	mustEnsure(t, p, "s1")
	f.cmds = nil
	if err := p.inject("s1", "hello\nworld"); err != nil {
		t.Fatal(err)
	}
	var sawPaste, sawEnter bool
	for _, c := range f.cmds {
		if c[0] == "paste-buffer" && contains(c, "-p") {
			sawPaste = true
		}
		if c[0] == "send-keys" && contains(c, "Enter") {
			sawEnter = true
		}
	}
	if !sawPaste || !sawEnter {
		t.Fatalf("inject should bracketed-paste then Enter; cmds=%v", f.cmds)
	}
}

func TestPool_SpawnPropagatesEnv(t *testing.T) {
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, []string{"USHER_HOOK_SOCK=/tmp/x.sock"}, 8, quietLogger())
	mustEnsure(t, p, "s1")
	// The new-session command must carry -e USHER_HOOK_SOCK=... so the
	// spawned claude's permission hooks route back to this instance.
	var ok bool
	f.mu.Lock()
	for _, c := range f.cmds {
		if len(c) == 0 || c[0] != "new-session" {
			continue
		}
		for i, a := range c {
			if a == "-e" && i+1 < len(c) && c[i+1] == "USHER_HOOK_SOCK=/tmp/x.sock" {
				ok = true
			}
		}
	}
	f.mu.Unlock()
	if !ok {
		t.Fatalf("spawn should pass -e USHER_HOOK_SOCK; cmds=%v", f.cmds)
	}
}

func mustEnsure(t *testing.T, p *pool, id string) {
	t.Helper()
	if _, err := p.ensure(id, "/tmp", true); err != nil {
		t.Fatal(err)
	}
}
