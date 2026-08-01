package health

import (
	"context"
	"testing"
)

func TestConfigCheck(t *testing.T) {
	cases := []struct {
		name string
		res  CommandResult
		want Status
	}{
		{"valid", CommandResult{ExitCode: 0}, StatusPass},
		{"invalid", CommandResult{ExitCode: 1}, StatusFail},
		{"tool missing", CommandResult{NotFound: true}, StatusWarn},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fr := &fakeRunner{res: c.res}
			cfg := Config{Runner: fr, CaddyConfig: "/etc/caddy/Caddyfile", Timeouts: DefaultTimeouts()}
			got := caddyConfigCheck().Run(context.Background(), cfg)
			if got.Status != c.want {
				t.Fatalf("status = %q, want %q", got.Status, c.want)
			}
		})
	}
	// Verify the caddy validator is invoked with the required adapter flag.
	fr := &fakeRunner{res: CommandResult{ExitCode: 0}}
	cfg := Config{Runner: fr, CaddyConfig: "/etc/caddy/Caddyfile", Timeouts: DefaultTimeouts()}
	caddyConfigCheck().Run(context.Background(), cfg)
	joined := fr.lastArgs
	found := false
	for i := 0; i+1 < len(joined); i++ {
		if joined[i] == "--adapter" && joined[i+1] == "caddyfile" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("caddy validate missing --adapter caddyfile: %v", joined)
	}
}
