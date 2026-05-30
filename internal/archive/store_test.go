package archive

import (
	"path/filepath"
	"testing"
	"time"
)

const sevenDays = 7 * 24 * time.Hour

func TestIsArchived_DefaultsToActivityWindow(t *testing.T) {
	s := New("", sevenDays)
	now := time.Now()

	if s.IsArchived("a", now.Add(-1*time.Hour), now) {
		t.Errorf("fresh session should be visible")
	}
	if !s.IsArchived("b", now.Add(-8*24*time.Hour), now) {
		t.Errorf("8-day-old session should auto-archive")
	}
	if s.IsArchived("c", time.Time{}, now) {
		t.Errorf("zero last_event should not auto-archive (treat as unknown)")
	}
}

func TestIsArchived_ManualOverridesActivity(t *testing.T) {
	s := New("", sevenDays)
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour)

	// Manual archive beats fresh activity.
	s.Archive("fresh")
	if !s.IsArchived("fresh", now, now) {
		t.Errorf("manual archive should override fresh activity")
	}

	// Unarchive on a stale session must override the auto rule —
	// otherwise it would auto-archive on the next check.
	s.Unarchive("stale", stale, now)
	if s.IsArchived("stale", stale, now) {
		t.Errorf("unarchive on stale should leave it visible")
	}
}

func TestUnarchive_FreshSessionDeletesEntry(t *testing.T) {
	s := New("", sevenDays)
	now := time.Now()
	fresh := now.Add(-1 * time.Hour)
	stale := now.Add(-30 * 24 * time.Hour)

	s.Archive("a")
	s.Unarchive("a", fresh, now)

	// Visible now (no manual entry, fresh).
	if s.IsArchived("a", fresh, now) {
		t.Errorf("a should be visible after unarchive on fresh")
	}
	// Later, when it goes stale, auto-archive resumes — the unarchive
	// did NOT leave a permanent DecisionShown override behind.
	if !s.IsArchived("a", stale, now) {
		t.Errorf("fresh-unarchive must not leave an override; stale should auto-archive again")
	}
}

func TestAutoArchiveDisabled(t *testing.T) {
	s := New("", 0)
	now := time.Now()
	stale := now.Add(-365 * 24 * time.Hour)

	// No auto-archive: even ancient sessions stay visible by default.
	if s.IsArchived("untouched", stale, now) {
		t.Errorf("autoAfter=0 must never auto-archive")
	}

	// Manual archive still works.
	s.Archive("manual")
	if !s.IsArchived("manual", now, now) {
		t.Errorf("manual archive should still take effect with autoAfter=0")
	}

	// Unarchive: nothing is "stale" when auto is off, so unarchive just
	// deletes the entry — no need to write DecisionShown.
	s.Unarchive("manual", stale, now)
	if s.IsArchived("manual", stale, now) {
		t.Errorf("unarchive should clear the manual archive even with autoAfter=0")
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archived.json")
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour)

	s1 := New(path, sevenDays)
	s1.Archive("a")
	s1.Unarchive("b", stale, now) // stale → DecisionShown override
	s1.Archive("c")

	s2 := New(path, sevenDays)
	if !s2.IsArchived("a", now, now) {
		t.Errorf("a should be archived after rehydrate")
	}
	if s2.IsArchived("b", stale, now) {
		t.Errorf("b should be visible after rehydrate (DecisionShown survives)")
	}
	if !s2.IsArchived("c", now, now) {
		t.Errorf("c should be archived after rehydrate")
	}
}

func TestArchive_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archived.json")
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour)

	s := New(path, sevenDays)
	s.Archive("a")
	s.Archive("a") // second call must not crash or duplicate
	s.Unarchive("a", stale, now)
	s.Unarchive("a", stale, now)

	// Stale unarchive writes DecisionShown; survives rehydrate.
	s2 := New(path, sevenDays)
	if s2.IsArchived("a", stale, now) {
		t.Errorf("after Archive → Unarchive on stale, session should stay visible")
	}
}
