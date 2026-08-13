package monitor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type ServiceState struct {
	FailureCount       int       `json:"failure_count"`
	LastCheckAt        time.Time `json:"last_check_at"`
	LastCheckResult    string    `json:"last_check_result"`
	LastRecoveryAt     time.Time `json:"last_recovery_at"`
	LastRecoveryResult string    `json:"last_recovery_result"`
	CooldownUntil      time.Time `json:"cooldown_until"`
	RecoveryInProgress bool      `json:"recovery_in_progress"`
}

type RemoteState struct {
	LastCheckAt time.Time `json:"last_check_at"`
}

type State struct {
	SchemaVersion int                      `json:"schema_version"`
	Services      map[string]*ServiceState `json:"services"`
	Remote        RemoteState              `json:"remote"`
}

type StateRepo interface {
	Load() (State, error)
	Save(State) error
	Pause() error
	Resume() error
	IsPaused() (bool, error)
}

type fileStateRepo struct {
	jsonPath  string
	pausePath string
}

func NewStateRepo(jsonPath, pausePath string) StateRepo {
	return &fileStateRepo{jsonPath: jsonPath, pausePath: pausePath}
}

func (r *fileStateRepo) IsPaused() (bool, error) {
	_, err := os.Stat(r.pausePath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("failed to check pause marker: %w", err)
}

func (r *fileStateRepo) Pause() error {
	f, err := os.Create(r.pausePath)
	if err != nil {
		return err
	}
	f.Close()
	return syncDir(filepath.Dir(r.pausePath))
}

func (r *fileStateRepo) Resume() error {
	state, err := r.Load()
	if err != nil {
		return fmt.Errorf("load state failed: %w", err)
	}
	for _, s := range state.Services {
		s.FailureCount = 0
	}
	if err := r.Save(state); err != nil {
		return fmt.Errorf("save state failed: %w", err)
	}
	if err := os.Remove(r.pausePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove pause marker failed: %w", err)
	}
	return syncDir(filepath.Dir(r.pausePath))
}

func (r *fileStateRepo) Load() (State, error) {
	var s State
	s.Services = make(map[string]*ServiceState)
	data, err := os.ReadFile(r.jsonPath)
	if err != nil {
		if os.IsNotExist(err) {
			s.SchemaVersion = 1
			return s, nil
		}
		return s, fmt.Errorf("read state file: %w", err)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("unmarshal state: %w", err)
	}
	if s.SchemaVersion != 1 {
		return s, fmt.Errorf("unsupported schema version: %d", s.SchemaVersion)
	}
	if s.Services == nil {
		s.Services = make(map[string]*ServiceState)
	}
	return s, nil
}

func (r *fileStateRepo) Save(s State) error {
	s.SchemaVersion = 1
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.jsonPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.jsonPath); err != nil {
		return err
	}
	return syncDir(filepath.Dir(r.jsonPath))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
