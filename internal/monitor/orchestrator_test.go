package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/health"
)

type dummyRepo struct {
	state  State
	paused bool
	err    error
}

func (r *dummyRepo) Load() (State, error)    { return r.state, r.err }
func (r *dummyRepo) Save(s State) error      { r.state = s; return r.err }
func (r *dummyRepo) Pause() error            { r.paused = true; return r.err }
func (r *dummyRepo) Resume() error           { r.paused = false; return r.err }
func (r *dummyRepo) IsPaused() (bool, error) { return r.paused, nil }

func TestOrchestrator_RunOnce_Healthy(t *testing.T) {
	o := &Orchestrator{
		Repo: &dummyRepo{state: InitialState()},
		RunChecks: func(ctx context.Context, svcs ...string) []health.Result {
			return []health.Result{
				{ID: "service.singbox", Status: health.StatusPass},
				{ID: "port.udp443", Status: health.StatusPass},
				{ID: "service.caddy", Status: health.StatusPass},
				{ID: "port.tcp80", Status: health.StatusPass},
				{ID: "port.tcp443", Status: health.StatusPass},
			}
		},
		Now: time.Now,
	}

	res := o.RunOnce(context.Background())
	if res.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", res.ExitCode)
	}
}
