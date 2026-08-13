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

		svcState.LastCheckAt = now
		cooldownActive := now.Before(svcState.CooldownUntil)

		if hasFail {
			svcState.LastCheckResult = "fail"
			if !paused {
				// During cooldown, new fail keeps count at 3.
				// After cooldown, first new fail increments to 1 if it was 0, or just sets it.
				// Actually, if we were at 3 and cooldown expired, we should reset to 1 because we need a NEW failure round.
				// The spec: "冷却期内的新一轮 fail 保持计数为 3... 冷却结束后的第一轮新 fail 才允许重新进入恢复资格预检"
				// Wait, if cooldown is over, does it reset to 1?
				// "冷却结束后的第一轮新 fail 才允许重新进入恢复资格预检... 恢复后复检全部通过则清零... 否则计数固定为3。冷却结束后仍须先观察一轮新的fail"
				// Wait, if count is 3 and cooldown expires, a NEW fail makes it enter pre-flight.
				// So if count == 3 and cooldown is over, the NEXT fail should trigger recovery.
				// This implies count remains 3, and since cooldown is over, it triggers!
				if svcState.FailureCount < 3 {
					svcState.FailureCount++
				}

				if svcState.FailureCount >= 3 && !cooldownActive {
					// Cooldown is over. We can recover.
					// If RecoveryInProgress was left true from a crash but cooldown is over, we can ignore it.
					actions[svc] = "recover"
				}
			}
		} else if allPass {
			svcState.LastCheckResult = "pass"
			if !paused {
				svcState.FailureCount = 0
			}
		} else {
			svcState.LastCheckResult = "warn_or_missing"
			// Doesn't increment, doesn't clear.
		}
	}

	processService("sing-box", singboxTriggers)
	processService("caddy", caddyTriggers)

	return state, actions
}
