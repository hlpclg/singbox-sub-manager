package health

import "testing"

func TestAllChecks_Count(t *testing.T) {
	checks := AllChecks()
	if len(checks) != 15 {
		t.Fatalf("AllChecks() returned %d checks, want 15", len(checks))
	}
}

func TestAllChecks_ExactOrder(t *testing.T) {
	expected := []string{
		"service.singbox", "service.caddy",
		"port.udp443", "port.tcp443", "port.tcp80",
		"config.singbox", "config.caddy",
		"subscription.token", "subscription.clash", "subscription.sr",
		"http.subscription", "tls.certificate", "tls.expiry",
		"dns.resolve", "disk.space",
	}
	checks := AllChecks()
	for i, c := range checks {
		if c.ID() != expected[i] {
			t.Errorf("AllChecks()[%d].ID() = %q, want %q", i, c.ID(), expected[i])
		}
	}
}

func TestAllChecks_NoDuplicates(t *testing.T) {
	seen := make(map[string]bool)
	for _, c := range AllChecks() {
		if seen[c.ID()] {
			t.Errorf("duplicate check ID: %q", c.ID())
		}
		seen[c.ID()] = true
	}
}

func TestAllChecks_FreshSlice(t *testing.T) {
	a := AllChecks()
	b := AllChecks()
	if &a[0] == &b[0] {
		t.Error("AllChecks() should return a fresh slice each call")
	}
}

func TestConcurrentIDs_ExactSet(t *testing.T) {
	ids := ConcurrentIDs()
	expected := map[string]bool{
		"dns.resolve":       true,
		"http.subscription": true,
		"tls.certificate":   true,
		"tls.expiry":        true,
	}
	if len(ids) != len(expected) {
		t.Fatalf("ConcurrentIDs() has %d entries, want %d", len(ids), len(expected))
	}
	for id := range expected {
		if !ids[id] {
			t.Errorf("missing concurrent ID: %q", id)
		}
	}
}

func TestConcurrentIDs_SubsetOfAllChecks(t *testing.T) {
	all := make(map[string]bool)
	for _, c := range AllChecks() {
		all[c.ID()] = true
	}
	for id := range ConcurrentIDs() {
		if !all[id] {
			t.Errorf("ConcurrentIDs contains %q which is not in AllChecks", id)
		}
	}
}
