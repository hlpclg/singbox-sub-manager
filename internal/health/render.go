package health

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Report
// ---------------------------------------------------------------------------

// Report is the top-level health report produced by BuildReport.
type Report struct {
	Status     Overall  `json:"status"`
	Timestamp  string   `json:"timestamp"`
	DurationMS int64    `json:"duration_ms"`
	Checks     []Result `json:"checks"`
	Summary    Summary  `json:"summary"`
}

// BuildReport assembles a Report from check results and timing information.
func BuildReport(results []Result, start time.Time, now func() time.Time) Report {
	if now == nil {
		now = time.Now
	}
	end := now()
	return Report{
		Status:     Aggregate(results),
		Timestamp:  start.Format(time.RFC3339),
		DurationMS: end.Sub(start).Milliseconds(),
		Checks:     results,
		Summary:    Summarize(results),
	}
}

// ---------------------------------------------------------------------------
// Text renderer
// ---------------------------------------------------------------------------

// RenderText writes a human-readable health report to w.
// Verbose mode appends per-check timing.
func RenderText(w io.Writer, report Report, verbose bool) {
	fmt.Fprintln(w, "singbox-sub-manager health")
	for _, c := range report.Checks {
		label := strings.ToUpper(string(c.Status))
		if verbose {
			fmt.Fprintf(w, "%-4s  %-22s %s (%d ms)\n", label, c.Name, c.Message, c.DurationMS)
		} else {
			fmt.Fprintf(w, "%-4s  %-22s %s\n", label, c.Name, c.Message)
		}
	}
	fmt.Fprintf(w, "Overall: %s\n", strings.ToUpper(string(report.Status)))
	fmt.Fprintf(w, "%d passed, %d warning, %d failed\n",
		report.Summary.Passed, report.Summary.Warnings, report.Summary.Failed)
}

// ---------------------------------------------------------------------------
// JSON renderer
// ---------------------------------------------------------------------------

// RenderJSON writes the report as stable JSON to w.
// Writer errors are returned so the CLI can map them to exit code 3.
func RenderJSON(w io.Writer, report Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}
