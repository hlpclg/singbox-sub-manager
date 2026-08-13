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

func InitialState() State {
	return State{SchemaVersion: 1, Services: map[string]*ServiceState{
		"sing-box": {},
		"caddy":    {},
	}}
}

func (s *State) Validate() error {
	if s.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schema version: %d", s.SchemaVersion)
	}
	if s.Services == nil {
		return fmt.Errorf("services missing")
	}
	for _, svc := range []string{"sing-box", "caddy"} {
		if s.Services[svc] == nil {
			return fmt.Errorf("service state missing: %s", svc)
		}
	}
	for svc, st := range s.Services {
		if st == nil {
			return fmt.Errorf("service state is null: %s", svc)
		}
		if st.FailureCount < 0 || st.FailureCount > 3 {
			return fmt.Errorf("invalid failure count for %s: %d", svc, st.FailureCount)
		}
	}
	return nil
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
	f, err := os.OpenFile(r.pausePath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			info, statErr := os.Stat(r.pausePath)
			if statErr != nil {
				return statErr
			}
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
				return fmt.Errorf("pause marker has invalid permissions")
			}
			return nil // already paused
		}
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
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
	data, err := os.ReadFile(r.jsonPath)
	if err != nil {
		if os.IsNotExist(err) {
			s = InitialState()
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
	if err := s.Validate(); err != nil {
		return s, err
	}
	return s, nil
}

func (r *fileStateRepo) Save(s State) error {
	if err := s.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(r.jsonPath)
	f, err := os.CreateTemp(dir, "state-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	defer os.Remove(tmpName)

	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}

	n, err := f.Write(data)
	if err != nil {
		f.Close()
		return err
	}
	if n != len(data) {
		f.Close()
		return fmt.Errorf("short write: %d < %d", n, len(data))
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, r.jsonPath); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
