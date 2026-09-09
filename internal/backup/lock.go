package backup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrLockBusy reports that the recovery lock stayed held for the whole bounded
// wait. It is distinguishable from internal errors so a CLI can tell the user
// to retry or pause the monitor instead of reporting a failure.
var ErrLockBusy = errors.New("backup: recovery lock is busy")

// Default bounded-wait parameters for the recovery lock.
const (
	DefaultLockRetryInterval = 100 * time.Millisecond
	DefaultLockMaxWait       = 30 * time.Second
)

// Locker is the subset of a file lock this package needs. Callers inject an
// implementation (production passes monitor.FileLock) so internal/backup does
// not depend on internal/monitor.
type Locker interface {
	TryLock() error
	Unlock() error
}

// LockLease is a held lock. Whoever owns the lease releases it; Restore never
// releases a lease it was handed.
type LockLease interface {
	Unlock() error
}

type lockLease struct {
	locker Locker

	mu       sync.Mutex
	released bool
	err      error
}

// Unlock releases the lock once. Later calls report the first result without
// touching the lock again, so a deferred release and an explicit one can
// safely coexist on the same lease.
func (l *lockLease) Unlock() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.released {
		l.err = l.locker.Unlock()
		l.released = true
	}
	return l.err
}

// AcquireWithRetry turns a non-blocking TryLock into a bounded wait. It always
// attempts at least once, gives up with ErrLockBusy after maxWait, and returns
// ctx.Err() immediately when the caller cancels while waiting.
func AcquireWithRetry(ctx context.Context, l Locker, interval, maxWait time.Duration) (LockLease, error) {
	if l == nil {
		return nil, fmt.Errorf("backup: no locker provided")
	}
	if interval <= 0 {
		interval = DefaultLockRetryInterval
	}
	if maxWait < 0 {
		maxWait = 0
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(maxWait)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lastErr := l.TryLock()
		if lastErr == nil {
			return &lockLease{locker: l}, nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w: %v", ErrLockBusy, lastErr)
		}
		// A fresh timer per attempt: reusing one would need a stop and a
		// drain, and a stale tick would burn a retry for nothing.
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
