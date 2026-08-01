package health

import (
	"context"
	"testing"
	"time"
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

// blockingRunner waits for cancellation so timeout behavior can be tested
// without invoking a real command.
type blockingRunner struct {
	cancelled chan struct{}
}

func (f *blockingRunner) Run(ctx context.Context, name string, args ...string) CommandResult {
	<-ctx.Done()
	close(f.cancelled)
	return CommandResult{Err: ctx.Err()}
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

func TestServiceCheckCommandTimeout(t *testing.T) {
	runner := &blockingRunner{cancelled: make(chan struct{})}
	cfg := Config{
		Runner:   runner,
		Timeouts: Timeouts{Command: 20 * time.Millisecond},
	}

	start := time.Now()
	got := singboxServiceCheck().Run(context.Background(), cfg)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("check returned after %s, want prompt return after command timeout", elapsed)
	}
	if got.Status != StatusFail {
		t.Fatalf("status = %q, want %q", got.Status, StatusFail)
	}
	select {
	case <-runner.cancelled:
	default:
		t.Fatal("runner did not observe context cancellation")
	}
}
