package health

import (
	"context"
	"fmt"
	"syscall"
)

// ---------------------------------------------------------------------------
// Disk space check (#15)
// ---------------------------------------------------------------------------

// diskSpaceThreshold is the minimum free space in bytes (500 MB).
const diskSpaceThreshold uint64 = 500 * 1000 * 1000

// defaultDiskPath is the subscription root used when cfg.SubscriptionRoot is empty.
const defaultDiskPath = "/var/www/proxy-sub"

// diskCheckImpl uses the injected or default statfs seam to check free space
// on the filesystem containing the subscription root directory.
//
// Linux statfs is a synchronous local syscall without a cancellable stdlib
// API. We test the injected seam and pre-cancelled context behavior rather
// than inventing a leaking timeout goroutine.
type diskCheckImpl struct{}

func diskCheck() Check { return diskCheckImpl{} }

func (diskCheckImpl) ID() string   { return "disk.space" }
func (diskCheckImpl) Name() string { return "disk space" }

func (c diskCheckImpl) Run(ctx context.Context, cfg Config) Result {
	if ctx.Err() != nil {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "cancelled"}
	}

	path := cfg.SubscriptionRoot
	if path == "" {
		path = defaultDiskPath
	}

	diskFree := cfg.DiskFree
	if diskFree == nil {
		diskFree = prodDiskFree
	}

	free, err := diskFree(path)
	if err != nil {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "statfs failed"}
	}

	if free < diskSpaceThreshold {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusWarn,
			Message: fmt.Sprintf("%s free", formatDiskSize(free))}
	}

	return Result{ID: c.ID(), Name: c.Name(), Status: StatusPass,
		Message: fmt.Sprintf("%s free", formatDiskSize(free))}
}

// prodDiskFree returns the available bytes on the filesystem containing path.
func prodDiskFree(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	// Bavail * Bsize gives available bytes for unprivileged users.
	return stat.Bavail * uint64(stat.Bsize), nil
}

// formatDiskSize produces human-readable disk sizes using decimal units
// (1 GB = 1,000,000,000 bytes) to match the threshold definition.
func formatDiskSize(bytes uint64) string {
	const (
		gb = 1000 * 1000 * 1000
		mb = 1000 * 1000
	)
	if bytes >= gb {
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(gb))
	}
	if bytes >= mb {
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(mb))
	}
	return fmt.Sprintf("%d B", bytes)
}
