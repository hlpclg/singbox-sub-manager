package monitor

import (
	"encoding/json"
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
	IsPaused() bool
}

type fileStateRepo struct {
	jsonPath  string
	pausePath string
}

func NewStateRepo(jsonPath, pausePath string) StateRepo {
	return &fileStateRepo{jsonPath: jsonPath, pausePath: pausePath}
}

func (r *fileStateRepo) IsPaused() bool {
	_, err := os.Stat(r.pausePath)
	return err == nil
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
	if err == nil {
		for _, s := range state.Services {
			s.FailureCount = 0
		}
		if err := r.Save(state); err != nil {
			return err
		}
	}
	if err := os.Remove(r.pausePath); err != nil && !os.IsNotExist(err) {
		return err
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
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, err
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
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_RDWR, 0600)
	if err == nil {
		f.Sync()
		f.Close()
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
