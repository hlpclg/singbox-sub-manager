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
		for _, t := range triggers {
			if checks[t] == "fail" {
				hasFail = true
				break
			}
		}
		
		svcState.LastCheckAt = now
		if hasFail {
			svcState.LastCheckResult = "fail"
			if !paused {
				if svcState.FailureCount < 3 {
					svcState.FailureCount++
				}
				if svcState.FailureCount >= 3 {
					if now.After(svcState.CooldownUntil) || now.Equal(svcState.CooldownUntil) {
						if !svcState.RecoveryInProgress {
							actions[svc] = "recover"
						}
					}
				}
			}
		} else {
			svcState.LastCheckResult = "pass"
			if !paused && (now.After(svcState.CooldownUntil) || now.Equal(svcState.CooldownUntil)) {
				svcState.FailureCount = 0
			}
		}
	}
	
	processService("sing-box", singboxTriggers)
	processService("caddy", caddyTriggers)
	
	return state, actions
}
