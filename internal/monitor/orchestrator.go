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

func (o *Orchestrator) RunOnce(ctx context.Context) (int, error) {
	state, err := o.Repo.Load()
	if err != nil {
		return 3, fmt.Errorf("load state: %w", err)
	}

	paused := o.Repo.IsPaused()

	// Run all checks
	results := o.RunChecks(ctx)
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

	now := o.Now()

	newState, actions := Decide(state, checksMap, now, paused)

	recoveryFailed := false

	for svc, action := range actions {
		if action != "recover" {
			continue
		}

		// Check eligibility
		if o.Checker != nil {
			if err := o.Checker.CheckEligibility(ctx, svc); err != nil {
				// Eligibility failed, skip recovery but record it as degraded
				hasWarn = true // treated as degraded
				continue
			}
		}

		// Pre-commit
		newState.Services[svc].RecoveryInProgress = true
		newState.Services[svc].LastRecoveryAt = now
		newState.Services[svc].CooldownUntil = now.Add(30 * time.Minute)

		if err := o.Repo.Save(newState); err != nil {
			return 3, fmt.Errorf("pre-commit save state: %w", err)
		}

		// Restart
		restartErr := o.Restart(ctx, svc)

		if restartErr != nil {
			newState.Services[svc].LastRecoveryResult = "fail"
			recoveryFailed = true
		} else {
			// Re-check
			var triggers []string
			if svc == "sing-box" {
				triggers = singboxTriggers
			} else {
				triggers = caddyTriggers
			}

			rechecks := o.RunChecks(ctx, triggers...)
			recheckFailed := false
			for _, r := range rechecks {
				if r.Status == health.StatusFail {
					recheckFailed = true
					break
				}
			}

			if recheckFailed {
				newState.Services[svc].LastRecoveryResult = "fail"
				recoveryFailed = true
			} else {
				newState.Services[svc].LastRecoveryResult = "pass"
				newState.Services[svc].FailureCount = 0
			}
		}

		// Clear in progress
		newState.Services[svc].RecoveryInProgress = false
	}

	// Final save
	if err := o.Repo.Save(newState); err != nil {
		return 3, fmt.Errorf("final save state: %w", err)
	}

	if recoveryFailed {
		return 1, nil
	}
	if hasFail || hasWarn || paused || len(actions) > 0 {
		return 2, nil
	}

	return 0, nil
}
