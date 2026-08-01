package health

import (
	"context"
	"testing"
)

// fakeRunner returns a canned CommandResult and records the last call.
type fakeRunner struct {
	res      CommandResult
	lastName string
	lastArgs []string
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) CommandResult {
	f.lastName, f.lastArgs = name, args
	return f.res
}

func TestServiceCheck(t *testing.T) {
	cases := []struct {
		name string
		res  CommandResult
		want Status
	}{
		{"active", CommandResult{Stdout: "active\n"}, StatusPass},
		{"inactive", CommandResult{Stdout: "inactive\n", ExitCode: 3}, StatusFail},
		{"missing systemctl", CommandResult{NotFound: true}, StatusFail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fr := &fakeRunner{res: c.res}
			cfg := Config{Runner: fr, Timeouts: DefaultTimeouts()}
			got := singboxServiceCheck().Run(context.Background(), cfg)
			if got.Status != c.want {
				t.Fatalf("status = %q, want %q (msg %q)", got.Status, c.want, got.Message)
			}
			if fr.lastName != "systemctl" || fr.lastArgs[0] != "is-active" {
				t.Fatalf("unexpected command: %s %v", fr.lastName, fr.lastArgs)
			}
		})
	}
}
