// Package transcript adapts built-in persisted formats to the backend-neutral
// transcript contract.
package transcript

import (
	"context"
	"crypto/rand"
	"fmt"
	"path/filepath"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/codexrollout"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/jsonl"
)

type Claude struct{}

func (Claude) ReadTurns(path string, limit int) ([]core.Turn, int, error) {
	return jsonl.ReadTurns(path, limit)
}
func (Claude) NewAssembler() backend.Assembler { return jsonl.NewAssembler() }
func (Claude) IsTurnComplete(raw []byte) bool  { return jsonl.IsTurnComplete(raw) }
func (Claude) IsTurnAborted([]byte) bool       { return false }

type Codex struct{}

func (Codex) ReadTurns(path string, limit int) ([]core.Turn, int, error) {
	return codexrollout.ReadTurns(path, limit)
}
func (Codex) NewAssembler() backend.Assembler { return codexrollout.NewAssembler() }
func (Codex) IsTurnComplete(raw []byte) bool  { return codexrollout.IsTurnComplete(raw) }
func (Codex) IsTurnAborted(raw []byte) bool   { return codexrollout.IsTurnAborted(raw) }

type ClaudeForker struct{}

func (ClaudeForker) Fork(_ context.Context, _ string, path, afterID string) (string, string, error) {
	id := newSessionID()
	dst := filepath.Join(filepath.Dir(path), id+".jsonl")
	return id, dst, jsonl.ForkCopy(path, dst, afterID, id)
}

type CodexForker struct{}

func (CodexForker) Fork(_ context.Context, sourceID, path, afterID string) (string, string, error) {
	id := newSessionID()
	dst := filepath.Join(filepath.Dir(path), codexrollout.RolloutFilename(id, time.Now()))
	return id, dst, codexrollout.ForkCopy(path, dst, afterID, id, sourceID)
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
