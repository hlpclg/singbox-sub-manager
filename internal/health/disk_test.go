package health

import (
	"context"
	"fmt"
	"testing"
)

func TestDiskCheck_AboveThreshold(t *testing.T) {
	chk := diskCheck()
	if chk.ID() != "disk.space" {
		t.Fatalf("ID = %q, want disk.space", chk.ID())
	}
	if chk.Name() != "disk space" {
		t.Fatalf("Name = %q, want 'disk space'", chk.Name())
	}

	cfg := Config{
		SubscriptionRoot: "/var/www/proxy-sub",
		DiskFree: func(path string) (uint64, error) {
			if path != "/var/www/proxy-sub" {
				t.Errorf("path = %q, want /var/www/proxy-sub", path)
			}
			return 12_400_000_000, nil // 12.4 GB
		},
	}
	r := chk.Run(context.Background(), cfg)
	if r.Status != StatusPass {
		t.Fatalf("status = %q, want pass", r.Status)
	}
	if r.Message != "12.4 GB free" {
		t.Fatalf("message = %q, want '12.4 GB free'", r.Message)
	}
}

func TestDiskCheck_BelowThreshold(t *testing.T) {
	chk := diskCheck()
	cfg := Config{
		SubscriptionRoot: "/var/www/proxy-sub",
		DiskFree: func(_ string) (uint64, error) {
			return 200_000_000, nil // 200 MB, below 500 MB threshold
		},
	}
	r := chk.Run(context.Background(), cfg)
	if r.Status != StatusWarn {
		t.Fatalf("status = %q, want warn", r.Status)
	}
	if r.Message != "200.0 MB free" {
		t.Fatalf("message = %q, want '200.0 MB free'", r.Message)
	}
}

func TestDiskCheck_ExactThreshold(t *testing.T) {
	chk := diskCheck()
	cfg := Config{
		SubscriptionRoot: "/var/www/proxy-sub",
		DiskFree: func(_ string) (uint64, error) {
			return 500_000_000, nil // exactly 500 MB
		},
	}
	r := chk.Run(context.Background(), cfg)
	if r.Status != StatusPass {
		t.Fatalf("status = %q, want pass (exact boundary is >= threshold)", r.Status)
	}
}

func TestDiskCheck_JustBelowThreshold(t *testing.T) {
	chk := diskCheck()
	cfg := Config{
		SubscriptionRoot: "/var/www/proxy-sub",
		DiskFree: func(_ string) (uint64, error) {
			return 499_999_999, nil // 1 byte below threshold
		},
	}
	r := chk.Run(context.Background(), cfg)
	if r.Status != StatusWarn {
		t.Fatalf("status = %q, want warn (1 byte below threshold)", r.Status)
	}
}

func TestDiskCheck_StatfsError(t *testing.T) {
	chk := diskCheck()
	cfg := Config{
		SubscriptionRoot: "/var/www/proxy-sub",
		DiskFree: func(_ string) (uint64, error) {
			return 0, fmt.Errorf("no such file or directory")
		},
	}
	r := chk.Run(context.Background(), cfg)
	if r.Status != StatusFail {
		t.Fatalf("status = %q, want fail", r.Status)
	}
	if r.Message != "statfs failed" {
		t.Fatalf("message = %q, want 'statfs failed'", r.Message)
	}
}

func TestDiskCheck_DefaultPath(t *testing.T) {
	chk := diskCheck()
	var capturedPath string
	cfg := Config{
		SubscriptionRoot: "", // empty → default
		DiskFree: func(path string) (uint64, error) {
			capturedPath = path
			return 1_000_000_000, nil
		},
	}
	r := chk.Run(context.Background(), cfg)
	if r.Status != StatusPass {
		t.Fatalf("status = %q, want pass", r.Status)
	}
	if capturedPath != "/var/www/proxy-sub" {
		t.Fatalf("path = %q, want /var/www/proxy-sub (default)", capturedPath)
	}
}

func TestDiskCheck_InjectedPath(t *testing.T) {
	chk := diskCheck()
	var capturedPath string
	cfg := Config{
		SubscriptionRoot: "/custom/path",
		DiskFree: func(path string) (uint64, error) {
			capturedPath = path
			return 1_000_000_000, nil
		},
	}
	chk.Run(context.Background(), cfg)
	if capturedPath != "/custom/path" {
		t.Fatalf("path = %q, want /custom/path", capturedPath)
	}
}

func TestDiskCheck_PreCancelledContext(t *testing.T) {
	chk := diskCheck()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := Config{
		SubscriptionRoot: "/var/www/proxy-sub",
		DiskFree: func(_ string) (uint64, error) {
			t.Fatal("DiskFree should not be called on pre-cancelled context")
			return 0, nil
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

func TestDiskCheck_ZeroFreeSpace(t *testing.T) {
	chk := diskCheck()
	cfg := Config{
		SubscriptionRoot: "/var/www/proxy-sub",
		DiskFree: func(_ string) (uint64, error) {
			return 0, nil
		},
	}
	r := chk.Run(context.Background(), cfg)
	if r.Status != StatusWarn {
		t.Fatalf("status = %q, want warn", r.Status)
	}
	if r.Message != "0 B free" {
		t.Fatalf("message = %q, want '0 B free'", r.Message)
	}
}

func TestFormatDiskSize(t *testing.T) {
	tests := []struct {
		bytes uint64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{999_999, "999999 B"},
		{1_000_000, "1.0 MB"},
		{200_000_000, "200.0 MB"},
		{499_999_999, "500.0 MB"},
		{500_000_000, "500.0 MB"},
		{1_000_000_000, "1.0 GB"},
		{12_400_000_000, "12.4 GB"},
		{100_000_000_000, "100.0 GB"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.bytes), func(t *testing.T) {
			got := formatDiskSize(tt.bytes)
			if got != tt.want {
				t.Errorf("formatDiskSize(%d) = %q, want %q", tt.bytes, got, tt.want)
			}
		})
	}
}
