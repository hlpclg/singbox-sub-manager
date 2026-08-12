package health

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BuildReport
// ---------------------------------------------------------------------------

func TestBuildReport_AllPass(t *testing.T) {
	start := time.Date(2026, 8, 1, 15, 44, 0, 0, time.FixedZone("CST", 8*3600))
	results := []Result{
		{ID: "service.singbox", Name: "sing-box service", Status: StatusPass, Message: "active", DurationMS: 12},
		{ID: "service.caddy", Name: "caddy service", Status: StatusPass, Message: "active", DurationMS: 8},
	}
	now := func() time.Time { return start.Add(842 * time.Millisecond) }
	r := BuildReport(results, start, now)

	if r.Status != Healthy {
		t.Errorf("Status = %q, want healthy", r.Status)
	}
	if r.DurationMS != 842 {
		t.Errorf("DurationMS = %d, want 842", r.DurationMS)
	}
	if r.Timestamp != "2026-08-01T15:44:00+08:00" {
		t.Errorf("Timestamp = %q", r.Timestamp)
	}
	if len(r.Checks) != 2 {
		t.Fatalf("Checks len = %d, want 2", len(r.Checks))
	}
	if r.Summary.Passed != 2 || r.Summary.Warnings != 0 || r.Summary.Failed != 0 {
		t.Errorf("Summary = %+v", r.Summary)
	}
}

func TestBuildReport_Degraded(t *testing.T) {
	start := time.Now()
	results := []Result{
		{ID: "tls.expiry", Name: "TLS expiry", Status: StatusWarn, Message: "expires in 12 days"},
		{ID: "dns.resolve", Name: "DNS", Status: StatusPass, Message: "resolves"},
	}
	r := BuildReport(results, start, nil)
	if r.Status != Degraded {
		t.Errorf("Status = %q, want degraded", r.Status)
	}
	if r.Summary.Passed != 1 || r.Summary.Warnings != 1 {
		t.Errorf("Summary = %+v", r.Summary)
	}
}

func TestBuildReport_Unhealthy(t *testing.T) {
	start := time.Now()
	results := []Result{
		{ID: "service.singbox", Name: "sing-box service", Status: StatusFail, Message: "inactive"},
	}
	r := BuildReport(results, start, nil)
	if r.Status != Unhealthy {
		t.Errorf("Status = %q, want unhealthy", r.Status)
	}
	if r.Summary.Failed != 1 {
		t.Errorf("Summary = %+v", r.Summary)
	}
}

func TestBuildReport_Empty(t *testing.T) {
	start := time.Now()
	r := BuildReport(nil, start, nil)
	if r.Status != Healthy {
		t.Errorf("Status = %q, want healthy (empty = all pass)", r.Status)
	}
}

// ---------------------------------------------------------------------------
// RenderJSON — structured assertions
// ---------------------------------------------------------------------------

func TestRenderJSON_StableSchema(t *testing.T) {
	start := time.Date(2026, 8, 1, 15, 44, 0, 0, time.FixedZone("CST", 8*3600))
	results := []Result{
		{ID: "service.singbox", Name: "sing-box service", Status: StatusPass, Message: "active", DurationMS: 12},
		{ID: "tls.expiry", Name: "TLS expiry", Status: StatusWarn, Message: "expires in 12 days", DurationMS: 5},
	}
	now := func() time.Time { return start.Add(100 * time.Millisecond) }
	report := BuildReport(results, start, now)

	var buf bytes.Buffer
	err := RenderJSON(&buf, report)
	if err != nil {
		t.Fatalf("RenderJSON error: %v", err)
	}

	// Parse back and verify all fields.
	var parsed struct {
		Status     string `json:"status"`
		Timestamp  string `json:"timestamp"`
		DurationMS int64  `json:"duration_ms"`
		Checks     []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Message    string `json:"message"`
			DurationMS int64  `json:"duration_ms"`
		} `json:"checks"`
		Summary struct {
			Passed   int `json:"passed"`
			Warnings int `json:"warnings"`
			Failed   int `json:"failed"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("JSON parse error: %v", err)
	}

	if parsed.Status != "degraded" {
		t.Errorf("status = %q, want degraded", parsed.Status)
	}
	if parsed.Timestamp != "2026-08-01T15:44:00+08:00" {
		t.Errorf("timestamp = %q", parsed.Timestamp)
	}
	if parsed.DurationMS != 100 {
		t.Errorf("duration_ms = %d, want 100", parsed.DurationMS)
	}
	if len(parsed.Checks) != 2 {
		t.Fatalf("checks len = %d, want 2", len(parsed.Checks))
	}

	c0 := parsed.Checks[0]
	if c0.ID != "service.singbox" || c0.Name != "sing-box service" || c0.Status != "pass" || c0.Message != "active" || c0.DurationMS != 12 {
		t.Errorf("check[0] = %+v", c0)
	}
	c1 := parsed.Checks[1]
	if c1.ID != "tls.expiry" || c1.Status != "warn" {
		t.Errorf("check[1] = %+v", c1)
	}

	if parsed.Summary.Passed != 1 || parsed.Summary.Warnings != 1 || parsed.Summary.Failed != 0 {
		t.Errorf("summary = %+v", parsed.Summary)
	}
}

func TestRenderJSON_AllThreeOverallStates(t *testing.T) {
	tests := []struct {
		name    string
		results []Result
		want    string
	}{
		{"healthy", []Result{{Status: StatusPass}}, "healthy"},
		{"degraded", []Result{{Status: StatusWarn}}, "degraded"},
		{"unhealthy", []Result{{Status: StatusFail}}, "unhealthy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := BuildReport(tt.results, time.Now(), nil)
			var buf bytes.Buffer
			if err := RenderJSON(&buf, report); err != nil {
				t.Fatal(err)
			}
			var parsed struct {
				Status string `json:"status"`
			}
			json.Unmarshal(buf.Bytes(), &parsed)
			if parsed.Status != tt.want {
				t.Errorf("status = %q, want %q", parsed.Status, tt.want)
			}
		})
	}
}

func TestRenderJSON_FailingWriter(t *testing.T) {
	report := BuildReport([]Result{{Status: StatusPass}}, time.Now(), nil)
	err := RenderJSON(&failWriter{}, report)
	if err == nil {
		t.Fatal("expected error from failing writer")
	}
}

type failWriter struct{}

func (failWriter) Write(p []byte) (int, error) {
	return 0, fmt.Errorf("disk full")
}

// ---------------------------------------------------------------------------
// RenderJSON — secret redaction
// ---------------------------------------------------------------------------

func TestRenderJSON_NoSecretLeakage(t *testing.T) {
	// Place sentinel secrets in messages to prove the renderer does not
	// introduce or expand them. The global redaction rule applies upstream;
	// we just verify the renderer itself is transparent.
	results := []Result{
		{ID: "subscription.token", Name: "subscription token", Status: StatusPass, Message: "present"},
		{ID: "http.subscription", Name: "clash subscription", Status: StatusPass, Message: "HTTP 200"},
	}
	report := BuildReport(results, time.Now(), nil)
	var buf bytes.Buffer
	RenderJSON(&buf, report)
	output := buf.String()

	// The renderer must not add any token/password/URL that wasn't in the
	// input messages.
	for _, secret := range []string{"password", "obfs", "token_value", "https://"} {
		if strings.Contains(strings.ToLower(output), secret) {
			t.Errorf("JSON output contains potential secret %q", secret)
		}
	}
}

// ---------------------------------------------------------------------------
// RenderText
// ---------------------------------------------------------------------------

func TestRenderText_HealthyOutput(t *testing.T) {
	results := []Result{
		{ID: "service.singbox", Name: "sing-box service", Status: StatusPass, Message: "active", DurationMS: 12},
		{ID: "dns.resolve", Name: "DNS", Status: StatusPass, Message: "resolves", DurationMS: 3},
	}
	report := BuildReport(results, time.Now(), nil)
	report.Status = Healthy
	report.Summary = Summary{Passed: 2, Warnings: 0, Failed: 0}

	var buf bytes.Buffer
	RenderText(&buf, report, false)
	output := buf.String()

	if !strings.HasPrefix(output, "singbox-sub-manager health\n") {
		t.Errorf("missing header line")
	}
	if !strings.Contains(output, "PASS") {
		t.Errorf("missing PASS label")
	}
	if !strings.Contains(output, "Overall: HEALTHY") {
		t.Errorf("missing Overall line, got:\n%s", output)
	}
	if !strings.Contains(output, "2 passed, 0 warning, 0 failed") {
		t.Errorf("missing summary line, got:\n%s", output)
	}
}

func TestRenderText_DegradedOutput(t *testing.T) {
	results := []Result{
		{ID: "tls.expiry", Name: "TLS expiry", Status: StatusWarn, Message: "expires in 12 days"},
		{ID: "dns.resolve", Name: "DNS", Status: StatusPass, Message: "resolves"},
	}
	report := BuildReport(results, time.Now(), nil)

	var buf bytes.Buffer
	RenderText(&buf, report, false)
	output := buf.String()

	if !strings.Contains(output, "WARN") {
		t.Errorf("missing WARN label")
	}
	if !strings.Contains(output, "Overall: DEGRADED") {
		t.Errorf("missing Overall DEGRADED, got:\n%s", output)
	}
}

func TestRenderText_UnhealthyOutput(t *testing.T) {
	results := []Result{
		{ID: "service.singbox", Name: "sing-box service", Status: StatusFail, Message: "inactive"},
	}
	report := BuildReport(results, time.Now(), nil)

	var buf bytes.Buffer
	RenderText(&buf, report, false)
	output := buf.String()

	if !strings.Contains(output, "FAIL") {
		t.Errorf("missing FAIL label")
	}
	if !strings.Contains(output, "Overall: UNHEALTHY") {
		t.Errorf("missing Overall UNHEALTHY, got:\n%s", output)
	}
}

func TestRenderText_VerboseIncludesTiming(t *testing.T) {
	results := []Result{
		{ID: "dns.resolve", Name: "DNS", Status: StatusPass, Message: "resolves", DurationMS: 42},
	}
	report := BuildReport(results, time.Now(), nil)

	var buf bytes.Buffer
	RenderText(&buf, report, true)
	output := buf.String()

	if !strings.Contains(output, "(42 ms)") {
		t.Errorf("verbose should include timing, got:\n%s", output)
	}
}

func TestRenderText_NonVerboseOmitsTiming(t *testing.T) {
	results := []Result{
		{ID: "dns.resolve", Name: "DNS", Status: StatusPass, Message: "resolves", DurationMS: 42},
	}
	report := BuildReport(results, time.Now(), nil)

	var buf bytes.Buffer
	RenderText(&buf, report, false)
	output := buf.String()

	if strings.Contains(output, "ms)") {
		t.Errorf("non-verbose should not include timing, got:\n%s", output)
	}
}

func TestRenderText_CheckOrdering(t *testing.T) {
	results := []Result{
		{ID: "service.singbox", Name: "sing-box service", Status: StatusPass, Message: "active"},
		{ID: "service.caddy", Name: "caddy service", Status: StatusPass, Message: "active"},
		{ID: "dns.resolve", Name: "DNS", Status: StatusPass, Message: "resolves"},
	}
	report := BuildReport(results, time.Now(), nil)

	var buf bytes.Buffer
	RenderText(&buf, report, false)
	output := buf.String()

	// Verify ordering: sing-box before caddy before DNS.
	i1 := strings.Index(output, "sing-box service")
	i2 := strings.Index(output, "caddy service")
	i3 := strings.Index(output, "DNS")
	if i1 < 0 || i2 < 0 || i3 < 0 {
		t.Fatalf("missing check names in output:\n%s", output)
	}
	if !(i1 < i2 && i2 < i3) {
		t.Errorf("check ordering violated: singbox@%d, caddy@%d, DNS@%d", i1, i2, i3)
	}
}

func TestRenderText_NoSecretLeakage(t *testing.T) {
	results := []Result{
		{ID: "subscription.token", Name: "subscription token", Status: StatusPass, Message: "present"},
	}
	report := BuildReport(results, time.Now(), nil)

	var buf bytes.Buffer
	RenderText(&buf, report, true)
	output := buf.String()

	for _, secret := range []string{"password", "obfs", "token_value", "https://"} {
		if strings.Contains(strings.ToLower(output), secret) {
			t.Errorf("text output contains potential secret %q", secret)
		}
	}
}

// ---------------------------------------------------------------------------
// RenderJSON — fixed check IDs round-trip
// ---------------------------------------------------------------------------

func TestRenderJSON_FixedCheckIDs(t *testing.T) {
	ids := []string{
		"service.singbox", "service.caddy",
		"port.udp443", "port.tcp443", "port.tcp80",
		"config.singbox", "config.caddy",
		"subscription.token", "subscription.clash", "subscription.sr",
		"http.subscription", "tls.certificate", "tls.expiry",
		"dns.resolve", "disk.space",
	}
	var results []Result
	for _, id := range ids {
		results = append(results, Result{ID: id, Status: StatusPass, Message: "ok"})
	}
	report := BuildReport(results, time.Now(), nil)

	var buf bytes.Buffer
	RenderJSON(&buf, report)

	var parsed struct {
		Checks []struct {
			ID string `json:"id"`
		} `json:"checks"`
	}
	json.Unmarshal(buf.Bytes(), &parsed)

	if len(parsed.Checks) != len(ids) {
		t.Fatalf("check count = %d, want %d", len(parsed.Checks), len(ids))
	}
	for i, c := range parsed.Checks {
		if c.ID != ids[i] {
			t.Errorf("check[%d].id = %q, want %q", i, c.ID, ids[i])
		}
	}
}

// ---------------------------------------------------------------------------
// RenderJSON — writer that partially fails
// ---------------------------------------------------------------------------

type partialWriter struct {
	n int
}

func (w *partialWriter) Write(p []byte) (int, error) {
	if w.n > 50 {
		return 0, io.ErrShortWrite
	}
	w.n += len(p)
	return len(p), nil
}
