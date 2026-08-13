package monitor

import (
	"context"
	"fmt"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/health"
)

type Orchestrator struct {
	Repo         StateRepo
	Checker      *EligibilityChecker
	RunChecks    func(ctx context.Context, svcs ...string) []health.Result
	Restart      func(ctx context.Context, svc string) error
	Now          func() time.Time
	CheckTimeout time.Duration
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
		// If we can't determine pause state reliably, we treat it as an error to prevent accidental recovery.
		stateErr = fmt.Errorf("pause marker error: %v", pauseErr)
	}

	// Always run checks even if state is corrupt
	results := o.RunChecks(ctx)
	report.Checks = results

	checksMap := make(map[string]string)
	hasFail := false
	hasWarn := false
	for _, r := range results {
		checksMap[r.ID] = string(r.Status)
		if r.Status == health.StatusFail {
			hasFail = true
		} else if r.Status == health.StatusWarn {
			hasWarn = true
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
			// Cancelled, do not attempt recovery
			report.Actions[svc] = "cancelled"
			continue
		}

		// Pre-flight
		if o.Checker != nil {
			if err := o.Checker.CheckEligibility(ctx, svc); err != nil {
				report.Actions[svc] = "preflight_failed"
				hasWarn = true
				continue
			}
		}

		newState.Services[svc].RecoveryInProgress = true
		newState.Services[svc].LastRecoveryAt = start
		newState.Services[svc].CooldownUntil = start.Add(30 * time.Minute)

		if err := o.Repo.Save(newState); err != nil {
			report.Status = "internal_error"
			report.DurationMS = o.Now().Sub(start).Milliseconds()
			return OrchestratorResult{ExitCode: 3, Report: report}
		}

		// Restart with its own timeout bounds if needed, but bounded by ctx
		restartErr := o.Restart(ctx, svc)

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

	if hasFail || hasWarn || paused || len(actions) > 0 {
		report.Status = "degraded"
		return OrchestratorResult{ExitCode: 2, Report: report}
	}

	report.Status = "healthy"
	return OrchestratorResult{ExitCode: 0, Report: report}
}
