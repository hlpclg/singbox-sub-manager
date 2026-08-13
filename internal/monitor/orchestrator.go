package monitor

import (
	"context"
	"fmt"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/health"
)

type Orchestrator struct {
	Repo             StateRepo
	Checker          *EligibilityChecker
	RunChecks        func(ctx context.Context, svcs ...string) []health.Result
	LoadRemoteChecks func(ctx context.Context) ([]health.Check, error)
	RunRemoteChecks  func(ctx context.Context, checks []health.Check) ([]health.Result, error)
	Restart          func(ctx context.Context, svc string) error
	Now              func() time.Time
	CheckTimeout     time.Duration
	RestartTimeout   time.Duration
	RecheckTimeout   time.Duration
}

type OrchestratorResult struct {
	ExitCode int
	Report   Report
}

type Report struct {
	Status        string            `json:"status"`
	Timestamp     string            `json:"timestamp"`
	DurationMS    int64             `json:"duration_ms"`
	Checks        []health.Result   `json:"checks"`
	Decisions     map[string]string `json:"decisions"`
	Actions       map[string]string `json:"actions,omitempty"`
	Rechecks      []health.Result   `json:"rechecks,omitempty"`
	RemoteSummary string            `json:"remote_summary,omitempty"`
}

func (o *Orchestrator) RunOnce(ctx context.Context) OrchestratorResult {
	start := o.Now()
	if o.RunChecks == nil {
		return OrchestratorResult{ExitCode: 3, Report: Report{
			Status: "internal_error", Timestamp: start.Format(time.RFC3339),
			Decisions: map[string]string{},
			Actions:   map[string]string{"monitor": "missing_run_checks"},
		}}
	}

	parentCtx := ctx
	timeout := o.CheckTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, timeout)
	defer cancel()

	report := Report{
		Timestamp: start.Format(time.RFC3339),
		Decisions: make(map[string]string),
		Actions:   make(map[string]string),
	}

	state, stateErr := o.Repo.Load()
	if stateErr == nil {
		stateErr = state.Validate()
	}

	paused, pauseErr := o.Repo.IsPaused()
	if pauseErr != nil {
		stateErr = fmt.Errorf("pause marker error: %v", pauseErr)
	}

	// Figure out if we should run remote checks
	runRemote := false
	if stateErr == nil {
		if start.Sub(state.Remote.LastCheckAt) >= 30*time.Minute || state.Remote.LastCheckAt.IsZero() {
			runRemote = true
		}
	}

	results := o.RunChecks(ctx)

	report.Checks = results

	checksMap := make(map[string]string)
	for _, r := range results {
		checksMap[r.ID] = string(r.Status)
	}
	for _, id := range []string{"service.singbox", "port.udp443", "service.caddy", "port.tcp80", "port.tcp443"} {
		if _, ok := checksMap[id]; !ok {
			report.Status = "internal_error"
			report.DurationMS = o.Now().Sub(start).Milliseconds()
			return OrchestratorResult{ExitCode: 3, Report: report}
		}
	}

	if stateErr != nil {
		report.Status = "internal_error"
		report.DurationMS = o.Now().Sub(start).Milliseconds()
		return OrchestratorResult{ExitCode: 3, Report: report}
	}

	newState, actions := Decide(state, checksMap, start, paused)
	report.Actions = actions
	for svc, triggers := range map[string][]string{"sing-box": singboxTriggers, "caddy": caddyTriggers} {
		decision := "healthy"
		for _, trigger := range triggers {
			if checksMap[trigger] != string(health.StatusPass) {
				decision = "degraded"
				break
			}
		}
		if action, ok := actions[svc]; ok {
			decision = action
		}
		report.Decisions[svc] = decision
	}

	recoveryFailed := false
	internalFailure := false

	for svc, action := range actions {
		if action != "recover" {
			continue
		}

		if ctx.Err() != nil {
			report.Actions[svc] = "cancelled"
			continue
		}

		if o.Checker == nil || o.Checker.RunConfigCheck == nil || o.Checker.CheckPortOwner == nil {
			report.Actions[svc] = "preflight_failed_missing_checker"
			continue
		}
		if o.Restart == nil {
			report.Actions[svc] = "internal_error_missing_restart"
			internalFailure = true
			continue
		}

		if err := o.Checker.CheckEligibility(ctx, svc); err != nil {
			report.Actions[svc] = fmt.Sprintf("preflight_failed: %v", err)
			continue
		}

		newState.Services[svc].RecoveryInProgress = true
		newState.Services[svc].LastRecoveryAt = start
		newState.Services[svc].CooldownUntil = start.Add(30 * time.Minute)

		if err := o.Repo.Save(newState); err != nil {
			report.Status = "internal_error"
			report.DurationMS = o.Now().Sub(start).Milliseconds()
			return OrchestratorResult{ExitCode: 3, Report: report}
		}

		// Restart bounded by timeout
		restartTimeout := o.RestartTimeout
		if restartTimeout <= 0 {
			restartTimeout = 30 * time.Second
		}
		restartCtx, restartCancel := context.WithTimeout(ctx, restartTimeout)
		restartErr := o.Restart(restartCtx, svc)
		restartCancel()

		if restartErr != nil {
			newState.Services[svc].LastRecoveryResult = "fail"
			if restartCtx.Err() == context.DeadlineExceeded {
				report.Actions[svc] = "restart_timeout"
			} else {
				report.Actions[svc] = "restart_failed"
			}
			recoveryFailed = true
		} else if ctx.Err() != nil {
			report.Actions[svc] = "cancelled"
			newState.Services[svc].LastRecoveryResult = "cancelled"
			recoveryFailed = true
		} else {
			var triggers []string
			if svc == "sing-box" {
				triggers = singboxTriggers
			} else {
				triggers = caddyTriggers
			}

			recheckTimeout := o.RecheckTimeout
			if recheckTimeout <= 0 {
				recheckTimeout = 30 * time.Second
			}
			recheckCtx, recheckCancel := context.WithTimeout(ctx, recheckTimeout)
			rechecks := o.RunChecks(recheckCtx, triggers...)
			recheckErr := recheckCtx.Err()
			recheckCancel()
			report.Rechecks = append(report.Rechecks, rechecks...)
			if ctx.Err() != nil || recheckErr != nil {
				newState.Services[svc].LastRecoveryResult = "cancelled"
				if recheckErr == context.DeadlineExceeded {
					report.Actions[svc] = "recheck_timeout"
				} else {
					report.Actions[svc] = "recheck_cancelled"
				}
				recoveryFailed = true
				newState.Services[svc].RecoveryInProgress = false
				continue
			}

			recheckFailed := false
			recheckMissing := false
			recheckMap := make(map[string]string)
			for _, r := range rechecks {
				recheckMap[r.ID] = string(r.Status)
			}
			for _, t := range triggers {
				if st, ok := recheckMap[t]; !ok || st != "pass" {
					recheckFailed = true
					if !ok {
						recheckMissing = true
					}
				}
			}

			if recheckFailed {
				newState.Services[svc].LastRecoveryResult = "fail"
				if recheckMissing {
					report.Actions[svc] = "recheck_missing"
				} else {
					report.Actions[svc] = "recheck_failed"
				}
				recoveryFailed = true
			} else {
				newState.Services[svc].LastRecoveryResult = "pass"
				newState.Services[svc].FailureCount = 0
				report.Actions[svc] = "recovered"
			}
		}

		newState.Services[svc].RecoveryInProgress = false
	}
	cancelled := ctx.Err() != nil

	if err := o.Repo.Save(newState); err != nil {
		report.Status = "internal_error"
		report.DurationMS = o.Now().Sub(start).Milliseconds()
		return OrchestratorResult{ExitCode: 3, Report: report}
	}
	if cancelled {
		report.Status = "internal_error"
		return OrchestratorResult{ExitCode: 3, Report: report}
	}

	remoteFailed := false
	remoteDegraded := false
	if !runRemote {
		report.RemoteSummary = "skipped: throttled"
	} else if o.LoadRemoteChecks == nil {
		report.RemoteSummary = "skipped: not configured"
	} else if o.RunRemoteChecks == nil {
		report.RemoteSummary = "skipped: remote runner unavailable"
		remoteFailed = true
	} else {
		// The local deadline covers local checks and recovery only. Remote probing
		// has its own bounded budget so a slow remote cannot consume local time.
		remoteCtx, remoteCancel := context.WithTimeout(parentCtx, 30*time.Second)
		remoteChecks, err := o.LoadRemoteChecks(remoteCtx)
		if err != nil {
			report.RemoteSummary = fmt.Sprintf("load failed: %v", err)
			remoteFailed = true
		} else {
			remoteResults, runErr := o.RunRemoteChecks(remoteCtx, remoteChecks)
			if runErr != nil {
				report.RemoteSummary = fmt.Sprintf("check failed: %v", runErr)
				remoteFailed = true
			} else {
				rPass, rWarn, rFail := 0, 0, 0
				for _, r := range remoteResults {
					switch r.Status {
					case health.StatusPass:
						rPass++
					case health.StatusWarn:
						rWarn++
					case health.StatusFail:
						rFail++
					}
				}
				remoteDegraded = rWarn > 0 || rFail > 0
				report.RemoteSummary = fmt.Sprintf("pass:%d warn:%d fail:%d", rPass, rWarn, rFail)
				report.Checks = append(report.Checks, remoteResults...)
				newState.Remote.LastCheckAt = start
				if err := o.Repo.Save(newState); err != nil {
					remoteFailed = true
					report.RemoteSummary = fmt.Sprintf("state save failed: %v", err)
				}
			}
		}
		remoteCancel()
	}

	report.DurationMS = o.Now().Sub(start).Milliseconds()

	if internalFailure {
		report.Status = "internal_error"
		return OrchestratorResult{ExitCode: 3, Report: report}
	}
	if recoveryFailed {
		report.Status = "recovery_failed"
		return OrchestratorResult{ExitCode: 1, Report: report}
	}
	if remoteFailed {
		report.Status = "internal_error"
		return OrchestratorResult{ExitCode: 3, Report: report}
	}

	// Re-evaluate hasFail/hasWarn after recovery
	finalHasFail := false
	finalHasWarn := false

	// Create a map to shadow initial checks with rechecks
	finalChecks := make(map[string]string)
	for k, v := range checksMap {
		finalChecks[k] = v
	}
	for _, r := range report.Rechecks {
		finalChecks[r.ID] = string(r.Status)
	}
	for _, st := range finalChecks {
		if st == "fail" {
			finalHasFail = true
		} else if st == "warn" {
			finalHasWarn = true
		}
	}

	unresolvedAction := false
	for _, action := range report.Actions {
		if action != "recovered" {
			unresolvedAction = true
			break
		}
	}
	if finalHasFail || finalHasWarn || paused || unresolvedAction || remoteDegraded {
		report.Status = "degraded"
		return OrchestratorResult{ExitCode: 2, Report: report}
	}

	report.Status = "healthy"
	return OrchestratorResult{ExitCode: 0, Report: report}
}
