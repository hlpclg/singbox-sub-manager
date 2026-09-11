package monitor

import (
	"testing"
	"time"
)

func TestDecide_FailureCounting(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	state := State{Services: map[string]*ServiceState{
		"caddy": {FailureCount: 2},
	}}

	checks := map[string]string{
		"service.caddy":   "fail",
		"port.tcp443":     "pass",
		"port.tcp80":      "pass",
		"service.singbox": "pass",
		"port.udp443":     "pass",
	}

	newState, actions := Decide(state, checks, now, false)

	if newState.Services["caddy"].FailureCount != 3 {
		t.Errorf("expected count 3, got %d", newState.Services["caddy"].FailureCount)
	}
	if actions["caddy"] != "recover" {
		t.Errorf("expected recover action, got %s", actions["caddy"])
	}
}

func TestDecide_ComplexScenarios(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)

	// Test 1st, 2nd, 3rd failure
	state := State{}
	var actions map[string]string

	failChecks := map[string]string{
		"service.singbox": "fail",
		"port.udp443":     "pass",
		"service.caddy":   "pass",
		"port.tcp443":     "pass",
		"port.tcp80":      "pass",
	}

	passChecks := map[string]string{
		"service.singbox": "pass",
		"port.udp443":     "pass",
		"service.caddy":   "pass",
		"port.tcp443":     "pass",
		"port.tcp80":      "pass",
	}

	// 1st fail
	state, actions = Decide(state, failChecks, now, false)
	if state.Services["sing-box"].FailureCount != 1 || len(actions) > 0 {
		t.Errorf("1st fail: expected count 1, no actions. got %d, %v", state.Services["sing-box"].FailureCount, actions)
	}

	// 2nd fail
	state, actions = Decide(state, failChecks, now, false)
	if state.Services["sing-box"].FailureCount != 2 || len(actions) > 0 {
		t.Errorf("2nd fail: expected count 2, no actions. got %d, %v", state.Services["sing-box"].FailureCount, actions)
	}

	// 3rd fail
	state, actions = Decide(state, failChecks, now, false)
	if state.Services["sing-box"].FailureCount != 3 || actions["sing-box"] != "recover" {
		t.Errorf("3rd fail: expected count 3, recover action. got %d, %v", state.Services["sing-box"].FailureCount, actions)
	}

	// Cool down active
	state.Services["sing-box"].CooldownUntil = now.Add(30 * time.Minute)
	state, actions = Decide(state, failChecks, now, false)
	if actions["sing-box"] == "recover" {
		t.Errorf("should not recover during cooldown")
	}

	// Pass clears count if cooldown is over
	state.Services["sing-box"].CooldownUntil = now.Add(-time.Minute)
	state, actions = Decide(state, passChecks, now, false)
	if state.Services["sing-box"].FailureCount != 0 {
		t.Errorf("expected count 0 after pass")
	}

	// Paused doesn't increment count or recover
	state, actions = Decide(state, failChecks, now, true)
	if state.Services["sing-box"].FailureCount != 0 || len(actions) > 0 {
		t.Errorf("paused: expected count 0, no actions. got %d", state.Services["sing-box"].FailureCount)
	}
}

func TestServiceTriggers(t *testing.T) {
	// The restore transaction reuses these, so they must stay the same list
	// the monitor's own decisions are made from.
	if got := ServiceTriggers("sing-box"); len(got) != len(singboxTriggers) {
		t.Fatalf("sing-box triggers = %v, want %v", got, singboxTriggers)
	} else {
		for i := range singboxTriggers {
			if got[i] != singboxTriggers[i] {
				t.Errorf("sing-box triggers = %v, want %v", got, singboxTriggers)
			}
		}
	}
	if got := ServiceTriggers("caddy"); len(got) != len(caddyTriggers) {
		t.Fatalf("caddy triggers = %v, want %v", got, caddyTriggers)
	} else {
		for i := range caddyTriggers {
			if got[i] != caddyTriggers[i] {
				t.Errorf("caddy triggers = %v, want %v", got, caddyTriggers)
			}
		}
	}
	if got := ServiceTriggers("nginx"); got != nil {
		t.Errorf("unknown service triggers = %v, want nil", got)
	}

	// The caller must not be able to edit the monitor's own slices.
	mutated := ServiceTriggers("caddy")
	mutated[0] = "tampered"
	if caddyTriggers[0] == "tampered" {
		t.Error("ServiceTriggers handed out the package's own slice")
	}
}
