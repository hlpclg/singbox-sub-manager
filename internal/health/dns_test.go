package health

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestDNSCheck_Success(t *testing.T) {
	chk := dnsCheck()
	if chk.ID() != "dns.resolve" {
		t.Fatalf("ID = %q, want dns.resolve", chk.ID())
	}
	if chk.Name() != "DNS" {
		t.Fatalf("Name = %q, want DNS", chk.Name())
	}

	cfg := Config{
		Domain:   "sub.example.com",
		Timeouts: DefaultTimeouts(),
		LookupHost: func(ctx context.Context, host string) ([]string, error) {
			if host != "sub.example.com" {
				t.Errorf("lookup host = %q, want sub.example.com", host)
			}
			return []string{"1.2.3.4"}, nil
		},
	}
	r := chk.Run(context.Background(), cfg)
	if r.Status != StatusPass {
		t.Fatalf("status = %q, want pass", r.Status)
	}
	if r.Message != "resolves" {
		t.Fatalf("message = %q, want resolves", r.Message)
	}
}

func TestDNSCheck_MultipleAddresses(t *testing.T) {
	chk := dnsCheck()
	cfg := Config{
		Domain:   "multi.example.com",
		Timeouts: DefaultTimeouts(),
		LookupHost: func(_ context.Context, _ string) ([]string, error) {
			return []string{"1.2.3.4", "5.6.7.8"}, nil
		},
	}
	r := chk.Run(context.Background(), cfg)
	if r.Status != StatusPass {
		t.Fatalf("status = %q, want pass", r.Status)
	}
}

func TestDNSCheck_EmptyResult(t *testing.T) {
	chk := dnsCheck()
	cfg := Config{
		Domain:   "empty.example.com",
		Timeouts: DefaultTimeouts(),
		LookupHost: func(_ context.Context, _ string) ([]string, error) {
			return []string{}, nil
		},
	}
	r := chk.Run(context.Background(), cfg)
	if r.Status != StatusFail {
		t.Fatalf("status = %q, want fail", r.Status)
	}
	if r.Message != "no addresses" {
		t.Fatalf("message = %q, want 'no addresses'", r.Message)
	}
}

func TestDNSCheck_LookupError(t *testing.T) {
	chk := dnsCheck()
	cfg := Config{
		Domain:   "fail.example.com",
		Timeouts: DefaultTimeouts(),
		LookupHost: func(_ context.Context, _ string) ([]string, error) {
			return nil, &net.DNSError{
				Err:  "no such host",
				Name: "fail.example.com",
			}
		},
	}
	r := chk.Run(context.Background(), cfg)
	if r.Status != StatusFail {
		t.Fatalf("status = %q, want fail", r.Status)
	}
	if r.Message != "lookup failed: fail.example.com" {
		t.Fatalf("message = %q, want 'lookup failed: fail.example.com'", r.Message)
	}
}

func TestDNSCheck_NoDomain(t *testing.T) {
	chk := dnsCheck()
	cfg := Config{
		Domain:   "",
		Timeouts: DefaultTimeouts(),
		LookupHost: func(_ context.Context, _ string) ([]string, error) {
			t.Fatal("LookupHost should not be called when domain is empty")
			return nil, nil
		},
	}
	r := chk.Run(context.Background(), cfg)
	if r.Status != StatusFail {
		t.Fatalf("status = %q, want fail", r.Status)
	}
	if r.Message != "no domain configured" {
		t.Fatalf("message = %q, want 'no domain configured'", r.Message)
	}
}

func TestDNSCheck_Timeout(t *testing.T) {
	chk := dnsCheck()
	cfg := Config{
		Domain:   "slow.example.com",
		Timeouts: Timeouts{DNS: 50 * time.Millisecond},
		LookupHost: func(ctx context.Context, _ string) ([]string, error) {
			// Simulate a timeout by returning a DNS timeout error when
			// the context deadline has passed.
			<-ctx.Done()
			return nil, &net.DNSError{
				Err:       "i/o timeout",
				Name:      "slow.example.com",
				IsTimeout: true,
			}
		},
	}
	r := chk.Run(context.Background(), cfg)
	if r.Status != StatusFail {
		t.Fatalf("status = %q, want fail", r.Status)
	}
	if r.Message != "timeout" {
		t.Fatalf("message = %q, want timeout", r.Message)
	}
}

func TestDNSCheck_Cancellation(t *testing.T) {
	chk := dnsCheck()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	cfg := Config{
		Domain:   "cancel.example.com",
		Timeouts: DefaultTimeouts(),
		LookupHost: func(_ context.Context, _ string) ([]string, error) {
			t.Fatal("LookupHost should not be called on pre-cancelled context")
			return nil, nil
		},
	}
	r := chk.Run(ctx, cfg)
	if r.Status != StatusFail {
		t.Fatalf("status = %q, want fail", r.Status)
	}
	if r.Message != "cancelled" {
		t.Fatalf("message = %q, want cancelled", r.Message)
	}
}

func TestDNSCheck_FakeObservesCancellation(t *testing.T) {
	// Prove the fake LookupHost observes the context cancellation signal.
	chk := dnsCheck()
	observed := make(chan struct{})

	cfg := Config{
		Domain:   "observe.example.com",
		Timeouts: Timeouts{DNS: 5 * time.Second},
		LookupHost: func(ctx context.Context, _ string) ([]string, error) {
			<-ctx.Done()
			close(observed)
			return nil, ctx.Err()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		done <- chk.Run(ctx, cfg)
	}()

	// Let the goroutine start and block on ctx.Done().
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-observed:
		// Good — the fake observed the cancellation.
	case <-time.After(2 * time.Second):
		t.Fatal("fake did not observe cancellation within 2s")
	}

	select {
	case r := <-done:
		if r.Status != StatusFail {
			t.Fatalf("status = %q, want fail", r.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after cancellation")
	}
}

func TestDNSCheck_DefaultTimeout(t *testing.T) {
	// When Timeouts.DNS is zero, the default 3s timeout should apply.
	chk := dnsCheck()
	var capturedDeadline time.Time
	cfg := Config{
		Domain:   "default-timeout.example.com",
		Timeouts: Timeouts{}, // DNS = 0
		LookupHost: func(ctx context.Context, _ string) ([]string, error) {
			dl, ok := ctx.Deadline()
			if !ok {
				t.Error("expected a deadline on the context")
			}
			capturedDeadline = dl
			return []string{"1.2.3.4"}, nil
		},
	}

	before := time.Now()
	r := chk.Run(context.Background(), cfg)
	if r.Status != StatusPass {
		t.Fatalf("status = %q, want pass", r.Status)
	}

	// The deadline should be approximately 3s from now (the default).
	margin := capturedDeadline.Sub(before)
	if margin < 2*time.Second || margin > 4*time.Second {
		t.Fatalf("deadline margin = %v, want ~3s", margin)
	}
}

func TestDNSErrMessage(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"timeout DNSError", &net.DNSError{Err: "timeout", Name: "x.com", IsTimeout: true}, "timeout"},
		{"non-timeout DNSError", &net.DNSError{Err: "no such host", Name: "bad.com"}, "lookup failed: bad.com"},
		{"context deadline", context.DeadlineExceeded, "timeout"},
		{"context cancelled", context.Canceled, "cancelled"},
		{"generic error", fmt.Errorf("something"), "lookup failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dnsErrMessage(tt.err)
			if got != tt.want {
				t.Errorf("dnsErrMessage(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
