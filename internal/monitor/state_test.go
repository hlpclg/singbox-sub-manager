package monitor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateRepo_PauseResume(t *testing.T) {
	dir := t.TempDir()
	repo := NewStateRepo(filepath.Join(dir, "state.json"), filepath.Join(dir, "paused"))

	paused, err := repo.IsPaused()
	if err != nil || paused {
		t.Fatalf("should not be paused initially: %v", err)
	}

	if err := repo.Pause(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	paused, _ = repo.IsPaused()
	if !paused {
		t.Fatal("should be paused")
	}

	s := State{SchemaVersion: 1}
	s.Services = make(map[string]*ServiceState)
	s.Services["sing-box"] = &ServiceState{}
	s.Services["caddy"] = &ServiceState{FailureCount: 3}
	if err := repo.Save(s); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := repo.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	paused, _ = repo.IsPaused()
	if paused {
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

func TestStateRepo_RejectsCorruptStateWithoutOverwriting(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	pausePath := filepath.Join(dir, "paused")
	if err := os.WriteFile(statePath, []byte(`{"schema_version":1,"services":{"caddy":null}}`), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(statePath)
	repo := NewStateRepo(statePath, pausePath)
	if _, err := repo.Load(); err == nil {
		t.Fatal("expected invalid service state to be rejected")
	}
	if err := repo.Resume(); err == nil {
		t.Fatal("resume must fail on corrupt state")
	}
	after, _ := os.ReadFile(statePath)
	if string(after) != string(before) {
		t.Fatal("corrupt state was overwritten")
	}
}

func TestStateRepo_RejectsInvalidSchemaAndCount(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	pausePath := filepath.Join(dir, "paused")
	cases := []string{
		`{"schema_version":2,"services":{}}`,
		`{"schema_version":1,"services":{"caddy":{"failure_count":4}}}`,
	}
	for _, data := range cases {
		if err := os.WriteFile(statePath, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewStateRepo(statePath, pausePath).Load(); err == nil {
			t.Fatalf("expected invalid state to fail: %s", data)
		}
	}
}
