package monitor

import (
	"time"
)

var (
	singboxTriggers = []string{"service.singbox", "port.udp443"}
	caddyTriggers   = []string{"service.caddy", "port.tcp443", "port.tcp80"}
)

func Decide(state State, checks map[string]string, now time.Time, paused bool) (State, map[string]string) {
	actions := make(map[string]string)

	processService := func(svc string, triggers []string) {
		if state.Services == nil {
			state.Services = make(map[string]*ServiceState)
		}
		if state.Services[svc] == nil {
			state.Services[svc] = &ServiceState{}
		}
		svcState := state.Services[svc]

		hasFail := false
		allPass := true

		for _, t := range triggers {
			res, ok := checks[t]
			if !ok {
				allPass = false
				continue
			}
			if res == "fail" {
				hasFail = true
				allPass = false
				break
			} else if res != "pass" {
				allPass = false
			}
		}

		timeRewound := !svcState.LastCheckAt.IsZero() && now.Before(svcState.LastCheckAt)
		if !timeRewound {
			svcState.LastCheckAt = now
		}

		cooldownActive := now.Before(svcState.CooldownUntil)

		if hasFail {
			svcState.LastCheckResult = "fail"
			if !paused && !timeRewound {
				if svcState.FailureCount < 3 {
					svcState.FailureCount++
				}

				if svcState.FailureCount >= 3 && !cooldownActive {
					// A crash marker is a pending, already committed attempt. It must
					// be settled by observation before another restart is eligible.
					if !svcState.RecoveryInProgress {
						actions[svc] = "recover"
					} else {
						svcState.LastRecoveryResult = "incomplete"
						svcState.RecoveryInProgress = false
					}
				}
			}
		} else if allPass {
			svcState.LastCheckResult = "pass"
			if svcState.RecoveryInProgress {
				svcState.LastRecoveryResult = "incomplete"
				svcState.RecoveryInProgress = false
			}
			if !paused && !timeRewound {
				svcState.FailureCount = 0
			}
		} else {
			svcState.LastCheckResult = "warn_or_missing"
		}
	}

	processService("sing-box", singboxTriggers)
	processService("caddy", caddyTriggers)

	return state, actions
}
