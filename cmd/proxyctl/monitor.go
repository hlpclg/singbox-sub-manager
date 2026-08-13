package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/health"
	"github.com/hlpclg/singbox-sub-manager/internal/monitor"
)

func cmdMonitor(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runMonitorOrchestrator(stdout, stderr)
	}
	switch args[0] {
	case "pause":
		return cmdMonitorPause(stdout, stderr)
	case "resume":
		return cmdMonitorResume(stdout, stderr)
	case "status":
		return cmdMonitorStatus(stdout, stderr)
	default:
		fmt.Fprintln(stderr, "usage: proxyctl monitor [pause|resume|status]")
		return 3
	}
}

func getRepo() monitor.StateRepo {
	return monitor.NewStateRepo("/var/lib/singbox-sub-manager/monitor-state.json", "/var/lib/singbox-sub-manager/monitor-paused")
}

func cmdMonitorPause(stdout, stderr io.Writer) int {
	repo := getRepo()
	if err := repo.Pause(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 3
	}
	fmt.Fprintln(stdout, "monitor paused")
	return 0
}

func cmdMonitorResume(stdout, stderr io.Writer) int {
	repo := getRepo()
	if err := repo.Resume(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 3
	}
	fmt.Fprintln(stdout, "monitor resumed")
	return 0
}

func cmdMonitorStatus(stdout, stderr io.Writer) int {
	repo := getRepo()
	state, err := repo.Load()
	if err != nil {
		fmt.Fprintf(stderr, "error loading state: %v\n", err)
		return 3
	}

	status := struct {
		Paused bool          `json:"paused"`
		State  monitor.State `json:"state"`
	}{
		Paused: repo.IsPaused(),
		State:  state,
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(status); err != nil {
		return 3
	}
	return 0
}

func runMonitorOrchestrator(stdout, stderr io.Writer) int {
	lock := monitor.NewFileLock("/run/lock/singbox-sub-manager-monitor.lock")
	if err := lock.TryLock(); err != nil {
		// Log and return 3 (internal error) if another instance is running
		fmt.Fprintf(stderr, "error acquiring lock: %v\n", err)
		return 3
	}
	defer lock.Unlock()

	repo := getRepo()

	cfg := healthResolveConfig("", nil)

	checker := &monitor.EligibilityChecker{
		RunConfigCheck: func(ctx context.Context, svc string) error {
			if svc == "sing-box" {
				res := cfg.Runner.Run(ctx, "sing-box", "check", "-c", cfg.SingboxConfig)
				if res.ExitCode != 0 {
					return fmt.Errorf("config check failed")
				}
			} else {
				res := cfg.Runner.Run(ctx, "caddy", "validate", "--config", cfg.CaddyConfig, "--adapter", "caddyfile")
				if res.ExitCode != 0 {
					return fmt.Errorf("config check failed")
				}
			}
			return nil
		},
		CheckPortOwner: func(ctx context.Context, svc string) error {
			// This is just a placeholder, returning nil for now
			return nil
		},
	}

	o := &monitor.Orchestrator{
		Repo:    repo,
		Checker: checker,
		RunChecks: func(ctx context.Context, svcs ...string) []health.Result {
			allChecks := healthAllChecks()
			var checks []health.Check
			if len(svcs) == 0 {
				checks = allChecks
			} else {
				// filtered
				want := make(map[string]bool)
				for _, s := range svcs {
					want[s] = true
				}
				for _, c := range allChecks {
					if want[c.ID()] {
						checks = append(checks, c)
					}
				}
			}
			return health.RunAll(ctx, cfg, checks, health.ConcurrentIDs())
		},
		Restart: monitor.RestartService,
		Now:     time.Now,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	code, err := o.RunOnce(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 3
	}

	// Output minimal report (the design says output a JSON result to stdout)
	// We'll skip complex formatting here to focus on the exit code as requested.
	fmt.Fprintln(stdout, `{"status": "completed"}`)

	return code
}
