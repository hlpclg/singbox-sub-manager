package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hlpclg/singbox-sub-manager/internal/health"
)

// fakeCheck implements health.Check for CLI tests to isolate from production.
type fakeCheck struct {
	id     string
	name   string
	status health.Status
}

func (c fakeCheck) ID() string   { return c.id }
func (c fakeCheck) Name() string { return c.name }
func (c fakeCheck) Run(ctx context.Context, cfg health.Config) health.Result {
	return health.Result{
		ID:      c.id,
		Name:    c.name,
		Status:  c.status,
		Message: "fake result",
	}
}

func injectFakeRegistry(status health.Status) func() {
	origResolve := healthResolveConfig
	origChecks := healthAllChecks

	healthResolveConfig = func(domain string, _ func(string) ([]byte, error)) health.Config {
		return health.Config{Domain: domain}
	}
	healthAllChecks = func() []health.Check {
		var checks []health.Check
		// Provide exactly 15 checks so length assertions still pass.
		for _, id := range []string{
			"service.singbox", "service.caddy", "port.udp443", "port.tcp443",
			"port.tcp80", "config.singbox", "config.caddy", "subscription.token",
			"subscription.clash", "subscription.sr", "http.subscription",
			"tls.certificate", "tls.expiry", "dns.resolve", "disk.space",
		} {
			checks = append(checks, fakeCheck{id: id, name: "fake", status: status})
		}
		return checks
	}

	return func() {
		healthResolveConfig = origResolve
		healthAllChecks = origChecks
	}
}

func TestHealthRouting(t *testing.T) {
	restore := injectFakeRegistry(health.StatusPass)
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"health"}, &stdout, &stderr)
	if code == 2 {
		t.Errorf("health should not return usage exit code 2, got stderr: %s", stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "singbox-sub-manager health") {
		t.Errorf("missing header in text output:\n%s", output)
	}
	if !strings.Contains(output, "Overall: HEALTHY") {
		t.Errorf("missing Overall line in output:\n%s", output)
	}
}

func TestHealthJSON(t *testing.T) {
	restore := injectFakeRegistry(health.StatusPass)
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"health", "--json"}, &stdout, &stderr)
	if code == 2 {
		t.Errorf("health --json should not return usage exit code 2")
	}

	var parsed struct {
		Status  string `json:"status"`
		Summary struct {
			Passed   int `json:"passed"`
			Warnings int `json:"warnings"`
			Failed   int `json:"failed"`
		} `json:"summary"`
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nraw: %s", err, stdout.String())
	}
	if parsed.Status == "" {
		t.Error("JSON status field is empty")
	}
	if len(parsed.Checks) != 15 {
		t.Errorf("expected 15 checks in JSON, got %d", len(parsed.Checks))
	}
}

func TestHealthVerbose(t *testing.T) {
	restore := injectFakeRegistry(health.StatusPass)
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"health", "--verbose"}, &stdout, &stderr)
	if code == 2 {
		t.Errorf("health --verbose should not return usage exit code 2")
	}
	if !strings.Contains(stdout.String(), "ms)") {
		t.Errorf("verbose output should contain timing, got:\n%s", stdout.String())
	}
}

func TestHealthDomainFlag(t *testing.T) {
	restore := injectFakeRegistry(health.StatusPass)
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"health", "--domain", "test.example.com"}, &stdout, &stderr)
	if code == 2 {
		t.Errorf("health --domain should not return usage exit code 2")
	}
}

func TestHealthUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"health", "--unknown-flag"}, &stdout, &stderr)
	if code != 3 {
		t.Errorf("expected exit code 3 for unknown flag, got %d", code)
	}
}

func TestHealthExtraArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"health", "extra-arg"}, &stdout, &stderr)
	if code != 3 {
		t.Errorf("expected exit code 3 for extra arguments, got %d", code)
	}
}

func TestHealthUsageIncluded(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run([]string{}, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "health") {
		t.Errorf("usage output should mention health command:\n%s", stderr.String())
	}
}

func TestHealthNoSecretInOutput(t *testing.T) {
	restore := injectFakeRegistry(health.StatusPass)
	defer restore()

	var stdout, stderr bytes.Buffer
	run([]string{"health", "--json"}, &stdout, &stderr)
	combined := stdout.String() + stderr.String()
	for _, secret := range []string{"password", "obfs_password"} {
		if strings.Contains(strings.ToLower(combined), secret) {
			t.Errorf("output contains potential secret %q", secret)
		}
	}
}

func TestHealth_RemoteFlagError(t *testing.T) {
	oldResolve := healthResolveConfig
	oldChecks := healthAllChecks
	defer func() {
		healthResolveConfig = oldResolve
		healthAllChecks = oldChecks
	}()

	healthResolveConfig = func(domain string, readFile func(string) ([]byte, error)) health.Config {
		return health.Config{
			Timeouts: health.DefaultTimeouts(),
		}
	}
	healthAllChecks = func() []health.Check { return nil }

	var stdout, stderr bytes.Buffer
	exitCode := cmdHealth([]string{"--remote", "--nodes", "/tmp/bad-nodes.conf"}, &stdout, &stderr)
	if exitCode != 3 {
		t.Errorf("expected exit code 3, got %d", exitCode)
	}
	if !strings.Contains(stderr.String(), "error: failed to load remote nodes") {
		t.Errorf("expected error about remote nodes, got %s", stderr.String())
	}
}
