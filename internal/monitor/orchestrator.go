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
	Restart          func(ctx context.Context, svc string) error
	Now              func() time.Time
	CheckTimeout     time.Duration
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
	Actions       map[string]string `json:"actions,omitempty"`
	Rechecks      []health.Result   `json:"rechecks,omitempty"`
	RemoteSummary string            `json:"remote_summary,omitempty"`
}

func (o *Orchestrator) RunOnce(ctx context.Context) OrchestratorResult {
	start := o.Now()

	if o.CheckTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.CheckTimeout)
		defer cancel()
	}

	report := Report{
		Timestamp: start.Format(time.RFC3339),
		Actions:   make(map[string]string),
	}

	state, stateErr := o.Repo.Load()

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

	// Add remote checks
	if runRemote && o.LoadRemoteChecks != nil {
		if remoteChecks, err := o.LoadRemoteChecks(ctx); err == nil {
			var concurrentIDs = health.ConcurrentIDs()
			for _, c := range remoteChecks {
				concurrentIDs[c.ID()] = true
			}
			cfg := health.Config{Timeouts: health.DefaultTimeouts()} // used only for defaults inside RunAll, but actually not ideal. We assume RunAll uses context.
			remoteResults := health.RunAll(ctx, cfg, remoteChecks, concurrentIDs)

			results = append(results, remoteResults...)

			// Remote summary
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
			report.RemoteSummary = fmt.Sprintf("pass:%d warn:%d fail:%d", rPass, rWarn, rFail)

			state.Remote.LastCheckAt = start
		} else {
			report.RemoteSummary = fmt.Sprintf("load failed: %v", err)
		}
	}

	report.Checks = results

	checksMap := make(map[string]string)
	for _, r := range results {
		checksMap[r.ID] = string(r.Status)
		if r.Status == health.StatusFail {
		} else if r.Status == health.StatusWarn {
		}
	}

	if stateErr != nil {
		report.Status = "internal_error"
		report.DurationMS = o.Now().Sub(start).Milliseconds()
		return OrchestratorResult{ExitCode: 3, Report: report}
	}

	newState, actions := Decide(state, checksMap, start, paused)
	report.Actions = actions

	recoveryFailed := false

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
		restartCtx, restartCancel := context.WithTimeout(ctx, 30*time.Second)
		restartErr := o.Restart(restartCtx, svc)
		restartCancel()

		if restartErr != nil {
			newState.Services[svc].LastRecoveryResult = "fail"
			report.Actions[svc] = "restart_failed"
			recoveryFailed = true
		} else {
			var triggers []string
			if svc == "sing-box" {
				triggers = singboxTriggers
			} else {
				triggers = caddyTriggers
			}

			rechecks := o.RunChecks(ctx, triggers...)
			report.Rechecks = append(report.Rechecks, rechecks...)

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

	if err := o.Repo.Save(newState); err != nil {
		report.Status = "internal_error"
		report.DurationMS = o.Now().Sub(start).Milliseconds()
		return OrchestratorResult{ExitCode: 3, Report: report}
	}

	report.DurationMS = o.Now().Sub(start).Milliseconds()

	if recoveryFailed {
		report.Status = "recovery_failed"
		return OrchestratorResult{ExitCode: 1, Report: report}
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

	if finalHasFail || finalHasWarn || paused || len(actions) > 0 {
		report.Status = "degraded"
		return OrchestratorResult{ExitCode: 2, Report: report}
	}

	report.Status = "healthy"
	return OrchestratorResult{ExitCode: 0, Report: report}
}
