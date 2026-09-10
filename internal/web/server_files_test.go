package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/discovery"
	"github.com/nexustar/usher/internal/hook"
	"github.com/nexustar/usher/internal/router"
	"github.com/nexustar/usher/internal/sessionmeta"
)

// stubRuntime satisfies backend.Runtime so router.GetSession can probe
// liveness without a live agent process.
type stubRuntime struct{}

func (stubRuntime) Start(context.Context, backend.StartRequest) (string, <-chan backend.Event, error) {
	return "", nil, nil
}
func (stubRuntime) Send(context.Context, string, string, string) (<-chan backend.Event, error) {
	return nil, nil
}
func (stubRuntime) Has(string) bool        { return false }
func (stubRuntime) LiveSessions() []string { return nil }
func (stubRuntime) Interrupt(string) error { return nil }
func (stubRuntime) Kill(string) error      { return nil }
func (stubRuntime) Shutdown()              {}

type filesResponse struct {
	Path    string `json:"path"`
	Entries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Size *int64 `json:"size"`
	} `json:"entries"`
	Truncated bool `json:"truncated"`
}

// newFilesTestServer builds a Server with one session whose cwd holds files,
// dirs, symlinks (in-fence, dangling, and escaping), and an over-cap dir.
func newFilesTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	root := t.TempDir()
	cwd := filepath.Join(root, "proj")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(cwd, "cmd"), 0o755))
	must(os.WriteFile(filepath.Join(cwd, "go.mod"), []byte("module example.com/proj\n"), 0o600))
	must(os.WriteFile(filepath.Join(cwd, "cmd", "main.go"), []byte("package main\n"), 0o600))
	must(os.Symlink("cmd", filepath.Join(cwd, "linkdir")))
	must(os.Symlink("go.mod", filepath.Join(cwd, "linkfile")))
	must(os.Symlink("nope", filepath.Join(cwd, "dangling")))
	must(os.Symlink(root, filepath.Join(cwd, "escape")))
	must(os.MkdirAll(filepath.Join(cwd, "big"), 0o755))
	for i := 0; i < maxFileEntries+1; i++ {
		must(os.WriteFile(filepath.Join(cwd, "big", fmt.Sprintf("f%04d", i)), nil, 0o600))
	}

	sessRoot := filepath.Join(root, "sessions")
	must(os.MkdirAll(filepath.Join(sessRoot, "slug"), 0o755))
	line := fmt.Sprintf(`{"type":"user","sessionId":"s1","cwd":%q,"timestamp":"2026-09-01T10:00:00.000Z","message":{"role":"user","content":"seed"},"uuid":"u1"}`+"\n", cwd)
	must(os.WriteFile(filepath.Join(sessRoot, "slug", "s1.jsonl"), []byte(line), 0o600))

	d, err := discovery.NewMulti(slog.Default(), discovery.NewClaudeSource(sessRoot))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r := router.New(d, map[string]backend.Backend{"claude": {Runtime: stubRuntime{}}}, "claude",
		broker.New(), hook.New(filepath.Join(root, "auto.json")),
		sessionmeta.New(filepath.Join(root, "meta.json"), 0), nil)
	return NewServer("", "", "", nil, r, nil, nil, nil, "", "", "", slog.Default()), "s1", cwd
}

func getFiles(t *testing.T, s *Server, id, dir string) (*httptest.ResponseRecorder, filesResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+id+"/files?dir="+url.QueryEscape(dir), nil)
	req.SetPathValue("id", id)
	s.handleSessionFiles(rec, req)
	var body filesResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("invalid JSON body %q: %v", rec.Body.String(), err)
		}
	}
	return rec, body
}

func entryByName(body filesResponse, name string) *struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size *int64 `json:"size"`
} {
	for i := range body.Entries {
		if body.Entries[i].Name == name {
			return &body.Entries[i]
		}
	}
	return nil
}

func TestSessionFilesListing(t *testing.T) {
	s, id, _ := newFilesTestServer(t)

	rec, body := getFiles(t, s, id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if body.Path != "" {
		t.Errorf("path = %q, want root %q", body.Path, "")
	}
	if body.Truncated {
		t.Error("root listing should not be truncated")
	}

	goMod := entryByName(body, "go.mod")
	if goMod == nil || goMod.Type != "file" {
		t.Fatalf("go.mod entry = %+v, want file", goMod)
	}
	if goMod.Size == nil || *goMod.Size != int64(len("module example.com/proj\n")) {
		t.Errorf("go.mod size = %v, want %d", goMod.Size, len("module example.com/proj\n"))
	}
	if e := entryByName(body, "cmd"); e == nil || e.Type != "directory" || e.Size != nil {
		t.Errorf("cmd entry = %+v, want directory without size", e)
	}
	// Symlinks report the target's type after EvalSymlinks.
	if e := entryByName(body, "linkdir"); e == nil || e.Type != "directory" {
		t.Errorf("linkdir entry = %+v, want directory", e)
	}
	if e := entryByName(body, "linkfile"); e == nil || e.Type != "file" || e.Size == nil {
		t.Errorf("linkfile entry = %+v, want file with size", e)
	}
	// Unresolvable symlink → other.
	if e := entryByName(body, "dangling"); e == nil || e.Type != "other" {
		t.Errorf("dangling entry = %+v, want other", e)
	}
	// A symlink escaping the cwd never leaks the target's type.
	if e := entryByName(body, "escape"); e == nil || e.Type != "other" {
		t.Errorf("escape entry = %+v, want other", e)
	}

	// Entries carry only name/type/size — no file contents anywhere.
	var raw struct {
		Entries []map[string]any `json:"entries"`
	}
	rec2, _ := getFiles(t, s, id, "")
	if err := json.Unmarshal(rec2.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, e := range raw.Entries {
		for k := range e {
			if k != "name" && k != "type" && k != "size" {
				t.Errorf("entry %v carries unexpected key %q", e, k)
			}
		}
	}
}

func TestSessionFilesSubdir(t *testing.T) {
	s, id, _ := newFilesTestServer(t)
	rec, body := getFiles(t, s, id, "cmd")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if body.Path != "cmd" {
		t.Errorf("path = %q, want %q", body.Path, "cmd")
	}
	if len(body.Entries) != 1 || body.Entries[0].Name != "main.go" || body.Entries[0].Type != "file" {
		t.Errorf("entries = %+v, want just main.go", body.Entries)
	}
}

func TestSessionFilesFencing(t *testing.T) {
	s, id, _ := newFilesTestServer(t)
	for _, dir := range []string{"..", "../", "../../etc", "/etc", "escape", "go.mod"} {
		rec, _ := getFiles(t, s, id, dir)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("dir=%q: status = %d, want 400 (body %q)", dir, rec.Code, rec.Body.String())
		}
	}
	rec, _ := getFiles(t, s, "no-such-session", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown session: status = %d, want 404", rec.Code)
	}
}

func TestSessionFilesTruncation(t *testing.T) {
	s, id, _ := newFilesTestServer(t)
	rec, body := getFiles(t, s, id, "big")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if !body.Truncated {
		t.Error("over-cap dir should set truncated")
	}
	if len(body.Entries) != maxFileEntries {
		t.Errorf("entries = %d, want cap %d", len(body.Entries), maxFileEntries)
	}
}

type filePreview struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	Content   string `json:"content"`
}

func getFile(t *testing.T, s *Server, id, path string) (*httptest.ResponseRecorder, filePreview) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+id+"/file?path="+url.QueryEscape(path), nil)
	req.SetPathValue("id", id)
	s.handleSessionFile(rec, req)
	var body filePreview
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("invalid JSON body %q: %v", rec.Body.String(), err)
		}
	}
	return rec, body
}

func TestSessionFilePreview(t *testing.T) {
	s, id, _ := newFilesTestServer(t)
	rec, body := getFile(t, s, id, "go.mod")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if body.Path != "go.mod" {
		t.Errorf("path = %q, want %q", body.Path, "go.mod")
	}
	if body.Content != "module example.com/proj\n" {
		t.Errorf("content = %q", body.Content)
	}
	if body.Size != int64(len("module example.com/proj\n")) {
		t.Errorf("size = %d, want %d", body.Size, len("module example.com/proj\n"))
	}
	if body.Truncated {
		t.Error("small file should not be truncated")
	}

	rec, body = getFile(t, s, id, "cmd/main.go")
	if rec.Code != http.StatusOK || body.Content != "package main\n" {
		t.Errorf("nested: status = %d, content = %q", rec.Code, body.Content)
	}
}

func TestSessionFileLineCap(t *testing.T) {
	s, id, cwd := newFilesTestServer(t)
	var sb strings.Builder
	for i := 0; i < maxFilePreviewLines+100; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	full := sb.String()
	if err := os.WriteFile(filepath.Join(cwd, "big.txt"), []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, body := getFile(t, s, id, "big.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if !body.Truncated {
		t.Error("over-cap file should set truncated")
	}
	if n := strings.Count(body.Content, "\n"); n != maxFilePreviewLines {
		t.Errorf("content lines = %d, want cap %d", n, maxFilePreviewLines)
	}
	if body.Size != int64(len(full)) {
		t.Errorf("size = %d, want full file size %d", body.Size, len(full))
	}
}

func TestSessionFileBinary(t *testing.T) {
	s, id, cwd := newFilesTestServer(t)
	if err := os.WriteFile(filepath.Join(cwd, "bin.dat"), []byte{'a', 'b', 0, 'c'}, 0o600); err != nil {
		t.Fatal(err)
	}
	rec, _ := getFile(t, s, id, "bin.dat")
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestSessionFileFencing(t *testing.T) {
	s, id, _ := newFilesTestServer(t)
	// Outside the workspace (lexical or via escaping symlink).
	for _, p := range []string{"..", "../../etc/passwd", "/etc/passwd", "escape"} {
		rec, _ := getFile(t, s, id, p)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("path=%q: status = %d, want 400 (body %q)", p, rec.Code, rec.Body.String())
		}
	}
	// A directory is not previewable.
	rec, _ := getFile(t, s, id, "cmd")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("dir: status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	// Missing file and unknown session.
	rec, _ = getFile(t, s, id, "nope.txt")
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing file: status = %d, want 404", rec.Code)
	}
	rec, _ = getFile(t, s, "no-such-session", "go.mod")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown session: status = %d, want 404", rec.Code)
	}
}
