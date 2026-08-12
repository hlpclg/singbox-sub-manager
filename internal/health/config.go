package health

import "context"

// configCheck validates a config file by invoking a validator binary. A
// missing binary is a WARN (tool not installed); a non-zero exit is a FAIL
// (config invalid).
type configCheck struct {
	id, name, bin string
	args          func(cfg Config) []string
}

func (c configCheck) ID() string   { return c.id }
func (c configCheck) Name() string { return c.name }

func (c configCheck) Run(ctx context.Context, cfg Config) Result {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeouts.Command)
	defer cancel()
	res := runnerOf(cfg).Run(ctx, c.bin, c.args(cfg)...)
	if res.NotFound {
		return Result{Status: StatusWarn, Message: c.bin + " not installed"}
	}
	if res.ExitCode == 0 && res.Err == nil {
		return Result{Status: StatusPass, Message: "valid"}
	}
	return Result{Status: StatusFail, Message: "invalid config"}
}

func singboxConfigCheck() Check {
	return configCheck{
		id:   "config.singbox",
		name: "sing-box config",
		bin:  "sing-box",
		args: func(cfg Config) []string { return []string{"check", "-c", cfg.SingboxConfig} },
	}
}

func caddyConfigCheck() Check {
	return configCheck{
		id:   "config.caddy",
		name: "caddy config",
		bin:  "caddy",
		args: func(cfg Config) []string {
			return []string{"validate", "--config", cfg.CaddyConfig, "--adapter", "caddyfile"}
		},
	}
}
