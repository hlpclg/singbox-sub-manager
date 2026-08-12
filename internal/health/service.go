package health

import (
	"context"
	"strings"
)

// serviceCheck reports a systemd unit as active. It judges on the "active"
// output rather than the exit code or localized text.
type serviceCheck struct {
	id, name, unit string
}

func (c serviceCheck) ID() string   { return c.id }
func (c serviceCheck) Name() string { return c.name }

func (c serviceCheck) Run(ctx context.Context, cfg Config) Result {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeouts.Command)
	defer cancel()
	res := runnerOf(cfg).Run(ctx, "systemctl", "is-active", c.unit)
	if res.NotFound {
		return Result{Status: StatusFail, Message: "systemctl not found"}
	}
	state := strings.TrimSpace(res.Stdout)
	if state == "active" {
		return Result{Status: StatusPass, Message: "active"}
	}
	if state == "" {
		state = "inactive"
	}
	return Result{Status: StatusFail, Message: state}
}

// Constructors used by the CLI (Task 10).
func singboxServiceCheck() Check {
	return serviceCheck{"service.singbox", "sing-box service", "sing-box"}
}
func caddyServiceCheck() Check { return serviceCheck{"service.caddy", "caddy service", "caddy"} }
