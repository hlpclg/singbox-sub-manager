package monitor

import (
	"path/filepath"
	"testing"
)

func TestStateRepo_PauseResume(t *testing.T) {
	dir := t.TempDir()
	repo := NewStateRepo(filepath.Join(dir, "state.json"), filepath.Join(dir, "paused"))

	if repo.IsPaused() {
		t.Fatal("should not be paused initially")
	}

	if err := repo.Pause(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if !repo.IsPaused() {
		t.Fatal("should be paused")
	}

	// Create dummy state
	s := State{SchemaVersion: 1}
	s.Services = make(map[string]*ServiceState)
	s.Services["caddy"] = &ServiceState{FailureCount: 3}
	if err := repo.Save(s); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := repo.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if repo.IsPaused() {
		t.Fatal("should not be paused after resume")
	}

	loaded, err := repo.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Services["caddy"].FailureCount != 0 {
		t.Fatalf("expected failure count 0 after resume, got %d", loaded.Services["caddy"].FailureCount)
	}
}
