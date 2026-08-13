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

func (r *dummyRepo) Load() (State, error) { return r.state, r.err }
func (r *dummyRepo) Save(s State) error   { r.state = s; return r.err }
func (r *dummyRepo) Pause() error         { r.paused = true; return r.err }
func (r *dummyRepo) Resume() error        { r.paused = false; return r.err }
func (r *dummyRepo) IsPaused() bool       { return r.paused }

func TestOrchestrator_RunOnce_Healthy(t *testing.T) {
	o := &Orchestrator{
		Repo: &dummyRepo{},
		RunChecks: func(ctx context.Context, svcs ...string) []health.Result {
			return []health.Result{
				{ID: "service.singbox", Status: health.StatusPass},
				{ID: "service.caddy", Status: health.StatusPass},
			}
		},
		Now: time.Now,
	}

	code, err := o.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
}
