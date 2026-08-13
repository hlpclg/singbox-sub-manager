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
				"caddy": {},
			},
		},
	}

	o := &Orchestrator{
		Repo: repo,
		RunChecks: func(ctx context.Context, svcs ...string) []health.Result {
			if len(svcs) > 0 {
				return []health.Result{{ID: "service.singbox", Status: health.StatusPass}, {ID: "port.udp443", Status: health.StatusPass}}
			}
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

func TestDecide_TimeRewindKeepsWatermark(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	state := State{SchemaVersion: 1, Services: map[string]*ServiceState{
		"sing-box": {FailureCount: 2, LastCheckAt: now},
		"caddy":    {},
	}}
	checks := map[string]string{"service.singbox": "fail", "port.udp443": "fail", "service.caddy": "pass", "port.tcp80": "pass", "port.tcp443": "pass"}
	state, actions := Decide(state, checks, past, false)
	if len(actions) != 0 || state.Services["sing-box"].FailureCount != 2 || !state.Services["sing-box"].LastCheckAt.Equal(now) {
		t.Fatalf("rewound clock changed recovery state: count=%d last=%s actions=%v", state.Services["sing-box"].FailureCount, state.Services["sing-box"].LastCheckAt, actions)
	}
	_, actions = Decide(state, checks, past.Add(time.Minute), false)
	if len(actions) != 0 {
		t.Fatalf("rewound clock allowed recovery on next round: %v", actions)
	}
}

func TestDecide_CrashMarkerSettlesBeforeRecovery(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	state := State{SchemaVersion: 1, Services: map[string]*ServiceState{
		"sing-box": {FailureCount: 3, RecoveryInProgress: true, CooldownUntil: now.Add(-time.Minute)},
		"caddy":    {},
	}}
	checks := map[string]string{"service.singbox": "fail", "port.udp443": "fail", "service.caddy": "pass", "port.tcp80": "pass", "port.tcp443": "pass"}
	state, actions := Decide(state, checks, now, false)
	if len(actions) != 0 || state.Services["sing-box"].RecoveryInProgress || state.Services["sing-box"].LastRecoveryResult != "incomplete" {
		t.Fatalf("crash marker was not settled: state=%+v actions=%v", *state.Services["sing-box"], actions)
	}
	_, actions = Decide(state, checks, now.Add(time.Minute), false)
	if actions["sing-box"] != "recover" {
		t.Fatalf("next observation should be eligible for recovery: %v", actions)
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
				"caddy": {},
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

	// The first post-crash observation only settles the incomplete attempt.
	if len(res.Report.Actions) != 0 || repo.state.Services["sing-box"].RecoveryInProgress {
		t.Errorf("crash marker was not settled before another action: %+v", res)
	}

	res = o.RunOnce(context.Background())
	if !strings.HasPrefix(res.Report.Actions["sing-box"], "preflight_failed") {
		t.Errorf("second observation should fail preflight: %+v", res.Report.Actions)
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

func localPassResults() []health.Result {
	return []health.Result{
		{ID: "service.singbox", Status: health.StatusPass},
		{ID: "port.udp443", Status: health.StatusPass},
		{ID: "service.caddy", Status: health.StatusPass},
		{ID: "port.tcp80", Status: health.StatusPass},
		{ID: "port.tcp443", Status: health.StatusPass},
	}
}

func validDummyState() State {
	return State{SchemaVersion: 1, Services: map[string]*ServiceState{
		"sing-box": {},
		"caddy":    {},
	}}
}

func TestOrchestrator_MissingStableCheckIsNotHealthy(t *testing.T) {
	repo := &dummyRepo{state: validDummyState()}
	o := &Orchestrator{
		Repo: repo,
		RunChecks: func(context.Context, ...string) []health.Result {
			return []health.Result{{ID: "service.singbox", Status: health.StatusPass}}
		},
		Now: time.Now,
	}
	res := o.RunOnce(context.Background())
	if res.ExitCode == 0 || res.Report.Status == "healthy" {
		t.Fatalf("missing stable check reported healthy: %+v", res)
	}
}

func TestOrchestrator_RemoteLoadErrorIsInternalError(t *testing.T) {
	repo := &dummyRepo{state: validDummyState()}
	o := &Orchestrator{
		Repo:      repo,
		RunChecks: func(context.Context, ...string) []health.Result { return localPassResults() },
		LoadRemoteChecks: func(context.Context) ([]health.Check, error) {
			return nil, fmt.Errorf("nodes unreadable")
		},
		Now: time.Now,
	}
	res := o.RunOnce(context.Background())
	if res.ExitCode != 3 || res.Report.Status != "internal_error" {
		t.Fatalf("remote load error was not internal error: %+v", res)
	}
}

func TestOrchestrator_SuccessfulRecoveryReturnsHealthy(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	repo := &dummyRepo{state: State{SchemaVersion: 1, Services: map[string]*ServiceState{
		"sing-box": {FailureCount: 2}, "caddy": {},
	}}}
	o := &Orchestrator{
		Repo: repo,
		Checker: &EligibilityChecker{
			RunConfigCheck: func(context.Context, string) error { return nil },
			CheckPortOwner: func(context.Context, string) error { return nil },
		},
		RunChecks: func(_ context.Context, ids ...string) []health.Result {
			if len(ids) > 0 {
				return []health.Result{{ID: "service.singbox", Status: health.StatusPass}, {ID: "port.udp443", Status: health.StatusPass}}
			}
			return []health.Result{
				{ID: "service.singbox", Status: health.StatusFail}, {ID: "port.udp443", Status: health.StatusFail},
				{ID: "service.caddy", Status: health.StatusPass}, {ID: "port.tcp80", Status: health.StatusPass}, {ID: "port.tcp443", Status: health.StatusPass},
			}
		},
		Restart: func(ctx context.Context, svc string) error { return nil },
		Now:     func() time.Time { return now },
	}
	res := o.RunOnce(context.Background())
	if res.ExitCode != 0 || res.Report.Status != "healthy" {
		t.Fatalf("successful recovery was not healthy: %+v", res)
	}
}

func TestOrchestrator_CancelledContextSkipsRecheck(t *testing.T) {
	now := time.Now()
	repo := &dummyRepo{state: State{SchemaVersion: 1, Services: map[string]*ServiceState{
		"sing-box": {FailureCount: 2}, "caddy": {},
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	var recheck bool
	o := &Orchestrator{
		Repo: repo,
		Checker: &EligibilityChecker{
			RunConfigCheck: func(context.Context, string) error { return nil },
			CheckPortOwner: func(context.Context, string) error { return nil },
		},
		RunChecks: func(_ context.Context, ids ...string) []health.Result {
			if len(ids) > 0 {
				recheck = true
			}
			return []health.Result{
				{ID: "service.singbox", Status: health.StatusFail}, {ID: "port.udp443", Status: health.StatusFail},
				{ID: "service.caddy", Status: health.StatusPass}, {ID: "port.tcp80", Status: health.StatusPass}, {ID: "port.tcp443", Status: health.StatusPass},
			}
		},
		Restart: func(context.Context, string) error { cancel(); return nil },
		Now:     func() time.Time { return now },
	}
	res := o.RunOnce(ctx)
	if recheck || res.Report.Actions["sing-box"] == "recovered" {
		t.Fatalf("cancelled recovery still rechecked/reported success: %+v", res)
	}
}

func TestOrchestrator_RemoteThrottleAndPersistedTimestamp(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	repo := &dummyRepo{state: validDummyState()}
	remoteLoads := 0
	remoteRuns := 0
	o := &Orchestrator{
		Repo:      repo,
		RunChecks: func(context.Context, ...string) []health.Result { return localPassResults() },
		LoadRemoteChecks: func(context.Context) ([]health.Check, error) {
			remoteLoads++
			return nil, nil
		},
		RunRemoteChecks: func(context.Context, []health.Check) ([]health.Result, error) {
			remoteRuns++
			return nil, nil
		},
		Now: func() time.Time { return now },
	}
	if got := o.RunOnce(context.Background()); got.ExitCode != 0 {
		t.Fatalf("first monitor run failed: %+v", got)
	}
	if got := o.RunOnce(context.Background()); got.ExitCode != 0 {
		t.Fatalf("second monitor run failed: %+v", got)
	}
	if remoteLoads != 1 || remoteRuns != 1 {
		t.Fatalf("remote checks were not throttled: loads=%d runs=%d", remoteLoads, remoteRuns)
	}
}

func TestOrchestrator_CorruptStateStillRunsReadOnlyChecks(t *testing.T) {
	checksRun := false
	repo := &dummyRepo{err: fmt.Errorf("corrupt state")}
	o := &Orchestrator{
		Repo: repo,
		RunChecks: func(context.Context, ...string) []health.Result {
			checksRun = true
			return localPassResults()
		},
		Now: time.Now,
	}
	res := o.RunOnce(context.Background())
	if res.ExitCode != 3 || !checksRun {
		t.Fatalf("corrupt state did not produce read-only exit 3: %+v checks=%v", res, checksRun)
	}
}
