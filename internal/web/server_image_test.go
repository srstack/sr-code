package web

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveWithinDir is the security boundary for the /image endpoint: a path
// is served only if it resolves — after symlink evaluation — strictly inside
// the session cwd.
func TestResolveWithinDir(t *testing.T) {
	dir := t.TempDir()
	// a real file inside dir
	inside := filepath.Join(dir, "chart.png")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// a subdir file
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	subFile := filepath.Join(sub, "a.png")
	if err := os.WriteFile(subFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// a file outside dir
	outDir := t.TempDir()
	outside := filepath.Join(outDir, "secret.png")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// a symlink inside dir pointing outside
	link := filepath.Join(dir, "escape.png")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		rel    string
		wantOK bool
	}{
		{"relative inside", "chart.png", true},
		{"absolute inside", inside, true},
		{"subdir", "sub/a.png", true},
		{"dot-dot escape", "../" + filepath.Base(outDir) + "/secret.png", false},
		{"absolute outside", outside, false},
		{"symlink escaping dir", "escape.png", false},
		{"nonexistent", "missing.png", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := resolveWithinDir(dir, c.rel)
			if ok != c.wantOK {
				t.Fatalf("resolveWithinDir(%q) ok=%v want=%v (got=%q)", c.rel, ok, c.wantOK, got)
			}
		})
	}
}

// TestResolveWithinDir_EmptyDir guards the no-cwd case (a session whose cwd is
// unknown must never serve anything).
func TestResolveWithinDir_EmptyDir(t *testing.T) {
	if _, ok := resolveWithinDir("", "anything.png"); ok {
		t.Fatal("empty dir must not resolve")
	}
}
