package main

import (
	"context"
	"testing"

	"github.com/hlpclg/singbox-sub-manager/internal/health"
)

type monitorRunner struct {
	stdout   string
	unitPID  string
	lastName string
}

func (r monitorRunner) Run(_ context.Context, name string, _ ...string) health.CommandResult {
	if name == "systemctl" {
		return health.CommandResult{Stdout: r.unitPID}
	}
	return health.CommandResult{Stdout: r.stdout}
}

func TestCheckPortOwnerRequiresExactProcessName(t *testing.T) {
	cases := []struct {
		name string
		svc  string
		out  string
		want bool
	}{
		{"caddy helper rejected", "caddy", "LISTEN 0 128 0.0.0.0:443 0.0.0.0:* users:((\"caddy-helper\",pid=1))", false},
		{"caddy exact accepted", "caddy", "LISTEN 0 128 0.0.0.0:443 0.0.0.0:* users:((\"caddy\",pid=1))", true},
		{"not singbox rejected", "sing-box", "UNCONN 0 0 0.0.0.0:443 0.0.0.0:* users:((\"not-sing-box\",pid=1))", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPortOwner(context.Background(), health.Config{Runner: monitorRunner{stdout: tc.out, unitPID: "1\n"}}, tc.svc)
			if (err == nil) != tc.want {
				t.Fatalf("error=%v, want success=%v", err, tc.want)
			}
		})
	}
}

func TestCheckPortOwnerRequiresSystemdMainPID(t *testing.T) {
	out := `LISTEN 0 128 0.0.0.0:443 0.0.0.0:* users:(("caddy",pid=123,fd=7))`
	if err := checkPortOwner(context.Background(), health.Config{Runner: monitorRunner{stdout: out, unitPID: "456\n"}}, "caddy"); err == nil {
		t.Fatal("same-name process from another unit was accepted")
	}
	if err := checkPortOwner(context.Background(), health.Config{Runner: monitorRunner{stdout: out, unitPID: "123\n"}}, "caddy"); err != nil {
		t.Fatalf("target unit PID was rejected: %v", err)
	}
}
