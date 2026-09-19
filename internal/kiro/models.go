package kiro

import (
	"context"
	"encoding/json"
	"os/exec"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/backend"
)

// ModelCatalog resolves kiro's model list from
// `kiro-cli chat --list-models --format json`, cached for the process lifetime
// in practice (model rosters change rarely).
type ModelCatalog struct {
	Cmd string

	mu       sync.Mutex
	cached   []kiroModel
	cachedAt time.Time
}

type kiroModel struct {
	ID            string `json:"model_id"`
	Name          string `json:"model_name"`
	ContextWindow int64  `json:"context_window_tokens"`
}

const catalogTTL = 24 * time.Hour

func (c *ModelCatalog) list(ctx context.Context) []kiroModel {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && time.Since(c.cachedAt) < catalogTTL {
		return c.cached
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, c.Cmd, "chat", "--list-models", "--format", "json").Output()
	if err != nil {
		return c.cached
	}
	var doc struct {
		Models []kiroModel `json:"models"`
	}
	if json.Unmarshal(out, &doc) != nil || len(doc.Models) == 0 {
		return c.cached
	}
	c.cached = doc.Models
	c.cachedAt = time.Now()
	return c.cached
}

func (c *ModelCatalog) Models(ctx context.Context) ([]backend.Model, error) {
	var out []backend.Model
	for _, m := range c.list(ctx) {
		if m.ID != "" {
			out = append(out, backend.Model{ID: m.ID, DisplayName: m.Name})
		}
	}
	return out, nil
}

// ValidateModel is permissive: the catalog probe can fail offline, and a wrong
// id fails at spawn with a visible error in the session.
func (c *ModelCatalog) ValidateModel(ctx context.Context, model string) error {
	for _, m := range c.list(ctx) {
		if m.ID == model {
			return nil
		}
	}
	return nil
}

func (c *ModelCatalog) DefaultEffort(context.Context, string) (string, error) { return "", nil }

// ContextWindow returns the model's context limit from the catalog (0 unknown).
func (c *ModelCatalog) ContextWindow(ctx context.Context, model string) int64 {
	if model == "" {
		return 0
	}
	for _, m := range c.list(ctx) {
		if m.ID == model {
			return m.ContextWindow
		}
	}
	return 0
}
