package health

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCheck is a controllable Check for runner tests.
type fakeCheck struct {
	id     string
	status Status
	delay  time.Duration
	active *int32 // optional: tracks concurrent in-flight count
	peak   *int32
}

func (f fakeCheck) ID() string   { return f.id }
func (f fakeCheck) Name() string { return f.id }
func (f fakeCheck) Run(ctx context.Context, cfg Config) Result {
	if f.active != nil {
		n := atomic.AddInt32(f.active, 1)
		for {
			p := atomic.LoadInt32(f.peak)
			if n <= p || atomic.CompareAndSwapInt32(f.peak, p, n) {
				break
			}
		}
		defer atomic.AddInt32(f.active, -1)
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return Result{Status: StatusFail, Message: "ctx"}
		}
	}
	return Result{Status: f.status}
}

func TestRunAllPreservesOrder(t *testing.T) {
	checks := []Check{
		fakeCheck{id: "a", status: StatusPass},
		fakeCheck{id: "b", status: StatusWarn},
		fakeCheck{id: "c", status: StatusFail},
	}
	cfg := Config{Timeouts: DefaultTimeouts()}
	got := RunAll(context.Background(), cfg, checks, map[string]bool{"b": true, "c": true})
	if got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
		t.Fatalf("order not preserved: %+v", got)
	}
	if got[0].Status != StatusPass || got[1].Status != StatusWarn || got[2].Status != StatusFail {
		t.Fatalf("statuses wrong: %+v", got)
	}
}

func TestRunAllConcurrencyCap(t *testing.T) {
	var active, peak int32
	var checks []Check
	ids := map[string]bool{}
	for i := 0; i < 10; i++ {
		id := string(rune('a' + i))
		checks = append(checks, fakeCheck{id: id, status: StatusPass, delay: 20 * time.Millisecond, active: &active, peak: &peak})
		ids[id] = true
	}
	cfg := Config{Timeouts: DefaultTimeouts()}
	RunAll(context.Background(), cfg, checks, ids)
	if peak > maxConcurrency {
		t.Fatalf("peak concurrency %d exceeded cap %d", peak, maxConcurrency)
	}
}

func TestRunAllOverallTimeout(t *testing.T) {
	checks := []Check{fakeCheck{id: "slow", status: StatusPass, delay: time.Second}}
	cfg := Config{Timeouts: DefaultTimeouts()}
	cfg.Timeouts.Overall = 50 * time.Millisecond
	got := RunAll(context.Background(), cfg, checks, nil)
	if got[0].Status != StatusFail || got[0].Message != "timeout" {
		t.Fatalf("expected timeout fail, got %+v", got[0])
	}
}

func TestRunAllTimeoutConcurrencyAndCleanExit(t *testing.T) {
	var active, peak int32
	var checks []Check
	ids := map[string]bool{}
	for i := 0; i < 8; i++ {
		id := string(rune('a' + i))
		checks = append(checks, fakeCheck{id: id, status: StatusPass, delay: 10 * time.Second, active: &active, peak: &peak})
		ids[id] = true
	}

	cfg := Config{Timeouts: DefaultTimeouts()}
	cfg.Timeouts.Overall = 100 * time.Millisecond

	start := time.Now()
	got := RunAll(context.Background(), cfg, checks, ids)
	elapsed := time.Since(start)

	if peak != maxConcurrency {
		t.Fatalf("peak concurrency = %d, want %d", peak, maxConcurrency)
	}
	if active != 0 {
		t.Fatalf("active goroutines = %d, want 0 (leaked)", active)
	}
	for _, res := range got {
		if res.Status != StatusFail || res.Message != "timeout" {
			t.Fatalf("expected timeout fail for %s, got status=%q message=%q", res.ID, res.Status, res.Message)
		}
	}
	if elapsed < 50*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("RunAll elapsed %v, expected around 100ms", elapsed)
	}
}
