package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/health"
)

func TestOrchestrator_TimeRewind(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	past := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)

	repo := &dummyRepo{
		state: State{
			SchemaVersion: 1,
			Services: map[string]*ServiceState{
				"sing-box": {
					FailureCount: 2,
					LastCheckAt:  now,
				},
			},
		},
	}

	o := &Orchestrator{
		Repo: repo,
		RunChecks: func(ctx context.Context, svcs ...string) []health.Result {
			return []health.Result{
				{ID: "service.singbox", Status: health.StatusFail},
				{ID: "port.udp443", Status: health.StatusFail},
				{ID: "service.caddy", Status: health.StatusPass},
				{ID: "port.tcp80", Status: health.StatusPass},
				{ID: "port.tcp443", Status: health.StatusPass},
			}
		},
		Now: func() time.Time { return past },
	}

	res := o.RunOnce(context.Background())
	if res.ExitCode != 2 { // Degraded because of failure, but NO recovery
		t.Errorf("expected exit code 2, got %d", res.ExitCode)
	}

	if repo.state.Services["sing-box"].FailureCount != 2 {
		t.Errorf("expected FailureCount to remain 2, got %d", repo.state.Services["sing-box"].FailureCount)
	}
}

func TestOrchestrator_CrashRecoveryAndPreflightFail(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)

	repo := &dummyRepo{
		state: State{
			SchemaVersion: 1,
			Services: map[string]*ServiceState{
				"sing-box": {
					FailureCount:       3,
					RecoveryInProgress: true,
					CooldownUntil:      now.Add(-time.Minute), // Cooldown expired
				},
			},
		},
	}

	o := &Orchestrator{
		Repo: repo,
		Checker: &EligibilityChecker{
			RunConfigCheck: func(ctx context.Context, svc string) error { return fmt.Errorf("config error") },
			CheckPortOwner: func(ctx context.Context, svc string) error { return nil },
		},
		RunChecks: func(ctx context.Context, svcs ...string) []health.Result {
			return []health.Result{
				{ID: "service.singbox", Status: health.StatusFail},
				{ID: "port.udp443", Status: health.StatusFail},
				{ID: "service.caddy", Status: health.StatusPass},
				{ID: "port.tcp80", Status: health.StatusPass},
				{ID: "port.tcp443", Status: health.StatusPass},
			}
		},
		Now: func() time.Time { return now },
	}

	res := o.RunOnce(context.Background())
	if res.ExitCode != 2 {
		t.Errorf("expected exit code 2, got %d", res.ExitCode)
	}

	// Should attempt recovery but fail preflight
	action := res.Report.Actions["sing-box"]
	if !strings.HasPrefix(action, "preflight_failed") {
		t.Errorf("expected preflight_failed action, got %s", action)
	}
}

func TestOrchestrator_DualServiceRecovery_And_RestartFail(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)

	repo := &dummyRepo{
		state: State{
			SchemaVersion: 1,
			Services: map[string]*ServiceState{
				"sing-box": {FailureCount: 2},
				"caddy":    {FailureCount: 2},
			},
		},
	}

	restarts := make(map[string]bool)

	o := &Orchestrator{
		Repo: repo,
		Checker: &EligibilityChecker{
			RunConfigCheck: func(ctx context.Context, svc string) error { return nil },
			CheckPortOwner: func(ctx context.Context, svc string) error { return nil },
		},
		RunChecks: func(ctx context.Context, svcs ...string) []health.Result {
			return []health.Result{
				{ID: "service.singbox", Status: health.StatusFail},
				{ID: "port.udp443", Status: health.StatusFail},
				{ID: "service.caddy", Status: health.StatusFail},
				{ID: "port.tcp80", Status: health.StatusFail},
				{ID: "port.tcp443", Status: health.StatusFail},
			}
		},
		Restart: func(ctx context.Context, svc string) error {
			restarts[svc] = true
			if svc == "caddy" {
				return fmt.Errorf("systemctl error")
			}
			return nil
		},
		Now: func() time.Time { return now },
	}

	res := o.RunOnce(context.Background())
	if res.ExitCode != 1 { // 1 because caddy recovery failed
		t.Errorf("expected exit code 1, got %d", res.ExitCode)
	}

	if !restarts["sing-box"] || !restarts["caddy"] {
		t.Errorf("expected both to restart, got %v", restarts)
	}

	if repo.state.Services["sing-box"].FailureCount != 0 { // recheck failed because mock RunChecks still returns fail
		// wait, recheck uses same RunChecks mock which returns FAIL!
		// So sing-box recheck also fails!
		if res.Report.Actions["sing-box"] != "recheck_failed" {
			t.Errorf("expected sing-box recheck_failed, got %s", res.Report.Actions["sing-box"])
		}
	}
}

func TestOrchestrator_PrecommitFail(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)

	repo := &dummyRepo{
		state: State{
			SchemaVersion: 1,
			Services: map[string]*ServiceState{
				"sing-box": {FailureCount: 2},
			},
		},
		err: fmt.Errorf("io error"),
	}
	// Clear the load error, but make save fail
	repo.err = nil

	o := &Orchestrator{
		Repo: repo,
		Checker: &EligibilityChecker{
			RunConfigCheck: func(ctx context.Context, svc string) error { return nil },
			CheckPortOwner: func(ctx context.Context, svc string) error { return nil },
		},
		RunChecks: func(ctx context.Context, svcs ...string) []health.Result {
			// Trigger save error
			repo.err = fmt.Errorf("io error")
			return []health.Result{
				{ID: "service.singbox", Status: health.StatusFail},
				{ID: "port.udp443", Status: health.StatusFail},
				{ID: "service.caddy", Status: health.StatusPass},
				{ID: "port.tcp80", Status: health.StatusPass},
				{ID: "port.tcp443", Status: health.StatusPass},
			}
		},
		Restart: func(ctx context.Context, svc string) error {
			t.Fatal("should not reach restart")
			return nil
		},
		Now: func() time.Time { return now },
	}

	res := o.RunOnce(context.Background())
	if res.ExitCode != 3 {
		t.Errorf("expected exit code 3, got %d", res.ExitCode)
	}
}
