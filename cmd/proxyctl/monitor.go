package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/health"
	"github.com/hlpclg/singbox-sub-manager/internal/health/remote"
	"github.com/hlpclg/singbox-sub-manager/internal/monitor"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
)

func cmdMonitor(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runMonitorOrchestrator(stdout, stderr)
	}
	if len(args) > 1 {
		fmt.Fprintln(stderr, "usage: proxyctl monitor [pause|resume|status]")
		return 3
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
	lock := monitor.NewFileLock("/run/lock/singbox-sub-manager-monitor.lock")
	if err := lock.TryLock(); err != nil {
		fmt.Fprintf(stderr, "error acquiring lock: %v\n", err)
		return 3
	}
	defer lock.Unlock()

	repo := getRepo()
	if err := repo.Pause(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 3
	}
	fmt.Fprintln(stdout, "monitor paused")
	return 0
}

func cmdMonitorResume(stdout, stderr io.Writer) int {
	lock := monitor.NewFileLock("/run/lock/singbox-sub-manager-monitor.lock")
	if err := lock.TryLock(); err != nil {
		fmt.Fprintf(stderr, "error acquiring lock: %v\n", err)
		return 3
	}
	defer lock.Unlock()

	repo := getRepo()
	if err := repo.Resume(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 3
	}
	fmt.Fprintln(stdout, "monitor resumed")
	return 0
}

func cmdMonitorStatus(stdout, stderr io.Writer) int {
	// Status reads without lock as per spec
	repo := getRepo()
	state, err := repo.Load()
	if err != nil {
		fmt.Fprintf(stderr, "error loading state: %v\n", err)
		return 3
	}

	paused, _ := repo.IsPaused()

	status := struct {
		Paused bool          `json:"paused"`
		State  monitor.State `json:"state"`
	}{
		Paused: paused,
		State:  state,
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(status); err != nil {
		return 3
	}
	return 0
}

func checkPortOwner(ctx context.Context, cfg health.Config, svc string) error {
	var portArgs []string
	if svc == "sing-box" {
		portArgs = []string{"-lnup"}
	} else {
		portArgs = []string{"-lnpt"}
	}

	res := cfg.Runner.Run(ctx, "ss", portArgs...)
	if res.ExitCode != 0 {
		return fmt.Errorf("ss command failed")
	}

	out := res.Stdout
	if svc == "sing-box" {
		// check UDP 443
		if !strings.Contains(out, ":443 ") {
			return nil // no one owns it, safe to restart
		}
		if !strings.Contains(out, "sing-box") {
			return fmt.Errorf("port udp 443 not owned by sing-box")
		}
	} else {
		// check TCP 80, 443
		if !strings.Contains(out, ":80 ") && !strings.Contains(out, ":443 ") {
			return nil // no one owns it
		}
		if (strings.Contains(out, ":80 ") || strings.Contains(out, ":443 ")) && !strings.Contains(out, "caddy") {
			return fmt.Errorf("port tcp 80/443 not owned by caddy")
		}
	}
	return nil
}

func runMonitorOrchestrator(stdout, stderr io.Writer) int {
	lock := monitor.NewFileLock("/run/lock/singbox-sub-manager-monitor.lock")
	if err := lock.TryLock(); err != nil {
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
			return checkPortOwner(ctx, cfg, svc)
		},
	}

	now := time.Now()

	o := &monitor.Orchestrator{
		Repo:    repo,
		Checker: checker,
		RunChecks: func(ctx context.Context, svcs ...string) []health.Result {
			allChecks := healthAllChecks()
			var checks []health.Check
			if len(svcs) == 0 {
				checks = allChecks

				// Add remote checks if past 30m
				state, err := repo.Load()
				if err == nil {
					// Also, run remote checks if it's the first time
					if state.Remote.LastCheckAt.IsZero() || now.Sub(state.Remote.LastCheckAt) >= 30*time.Minute {
						// Include remote checks
						if ns, _, err := nodes.Load(defaultNodesPath); err == nil {
							for _, n := range nodes.Enabled(ns) {
								checks = append(checks, remote.NewNodeCheck(n))
							}
						}
						// Update last check time
						state.Remote.LastCheckAt = now
						_ = repo.Save(state)
					}
				}
			} else {
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
			concurrent := health.ConcurrentIDs()
			for _, c := range checks {
				if strings.HasPrefix(c.ID(), "remote.") {
					concurrent[c.ID()] = true
				}
			}
			return health.RunAll(ctx, cfg, checks, concurrent)
		},
		Restart:      monitor.RestartService,
		Now:          time.Now,
		CheckTimeout: 60 * time.Second,
	}

	ctx := context.Background()

	result := o.RunOnce(ctx)

	// Build JSON
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result.Report); err != nil {
		return 3
	}

	return result.ExitCode
}
