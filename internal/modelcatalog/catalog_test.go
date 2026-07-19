package modelcatalog

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexModels(t *testing.T) {
	ctx := context.Background()
	got, err := (Codex{Path: "/no/such/models_cache.json"}).Models(ctx)
	if err != nil || len(got) != 2 || got[0].ID != "gpt-5.5" {
		t.Fatalf("missing-cache fallback = %v, %v; want gpt-5.5 then mini", got, err)
	}

	p := filepath.Join(t.TempDir(), "models_cache.json")
	if err := os.WriteFile(p, []byte(`{"models":[
		{"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list","priority":2,"default_reasoning_level":"low"},
		{"slug":"auto-review","display_name":"x","visibility":"hide","priority":1,"default_reasoning_level":"high"},
		{"slug":"gpt-5.4-mini","display_name":"GPT-5.4 Mini","visibility":"list","priority":1}
	]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog := Codex{Path: p}
	got, err = catalog.Models(ctx)
	if err != nil || len(got) != 2 || got[0].ID != "gpt-5.4-mini" || got[1].ID != "gpt-5.5" {
		t.Fatalf("catalog parse/sort = %v, %v; want mini then 5.5", got, err)
	}
	if effort, _ := catalog.DefaultEffort(ctx, "gpt-5.5"); effort != "low" {
		t.Errorf("default effort = %q, want low", effort)
	}
	if effort, _ := catalog.DefaultEffort(ctx, "auto-review"); effort != "high" {
		t.Errorf("hidden model default effort = %q, want high", effort)
	}
	if err := catalog.ValidateModel(ctx, "auto-review"); err != nil {
		t.Errorf("hidden catalog model rejected: %v", err)
	}
	if err := catalog.ValidateModel(ctx, "not-in-catalog"); err == nil {
		t.Error("unknown Codex model accepted")
	}
}

func TestClaudeValidateModel(t *testing.T) {
	catalog := Claude{}
	for _, model := range []string{"sonnet", "claude-sonnet-4-5-20250929"} {
		if err := catalog.ValidateModel(context.Background(), model); err != nil {
			t.Errorf("valid Claude model %q rejected: %v", model, err)
		}
	}
	if err := catalog.ValidateModel(context.Background(), "arbitrary"); err == nil {
		t.Error("unknown Claude model accepted")
	}
}
