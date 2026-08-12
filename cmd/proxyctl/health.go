package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/health"
)

var (
	healthResolveConfig = health.ResolveConfig
	healthAllChecks     = health.AllChecks
)

func cmdHealth(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(stderr)

	jsonFlag := fs.Bool("json", false, "output JSON report")
	verbose := fs.Bool("verbose", false, "show per-check timing")
	domain := fs.String("domain", "", "override domain for checks")

	if err := fs.Parse(args); err != nil {
		return 3
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "error: unexpected arguments after flags")
		return 3
	}

	cfg := healthResolveConfig(*domain, nil)

	checks := healthAllChecks()

	start := time.Now()
	results := health.RunAll(context.Background(), cfg, checks, health.ConcurrentIDs())
	report := health.BuildReport(results, start, nil)

	if *jsonFlag {
		if err := health.RenderJSON(stdout, report); err != nil {
			fmt.Fprintln(stderr, "error: failed to write JSON output")
			return 3
		}
	} else {
		health.RenderText(stdout, report, *verbose)
	}

	return health.ExitCode(report.Status)
}
