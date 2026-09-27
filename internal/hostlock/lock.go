// Package hostlock provides the shared L1->L2->L3 lock ordering from spec
// §11 (docs/superpowers/specs/2026-09-18-v0.10-reality-node-install-design.md).
// Every new call site added in v0.10 acquires whatever subset of {L1, L2, L3}
// it needs through Acquire, which always takes them in L1, L2, L3 order and
// releases them in the reverse order, regardless of the order Levels were
// passed in. This centralizes the one invariant every caller must share:
// never hold a later lock while requesting an earlier one.
//
// The actual flock primitive is monitor.FileLock (unchanged, still used
// directly by monitor's own TryLock/3-exit-code path); the bounded-retry
// wait is backup.AcquireWithRetry (unchanged, still used directly wherever
// only a single lock is needed). This package adds only the ordering and
// multi-level lease on top of both.
package hostlock

import (
	"context"
	"fmt"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/backup"
	"github.com/hlpclg/singbox-sub-manager/internal/monitor"
)

// Level identifies one of the three fixed lock scopes (spec §11.1).
type Level int

const (
	L1 Level = iota
	L2
	L3
)

func (l Level) String() string {
	switch l {
	case L1:
		return "L1"
	case L2:
		return "L2"
	case L3:
		return "L3"
	default:
		return fmt.Sprintf("Level(%d)", int(l))
	}
}

// orderedLevels is the fixed acquisition order (spec §11.1: "加锁顺序固定为
// L1 → L2 → L3"). Release always walks it backwards.
var orderedLevels = [...]Level{L1, L2, L3}

// Paths are the fixed production lock file paths (spec §11.1), overridable
// so tests never touch /run/lock.
type Paths struct {
	L1 string
	L2 string
	L3 string
}

// ProdPaths returns the fixed production lock paths from spec §11.1.
func ProdPaths() Paths {
	return Paths{
		L1: "/run/lock/singbox-sub-manager.lock",
		L2: "/run/lock/singbox-sub-manager-monitor.lock",
		L3: "/run/lock/proxyctl-reality.lock",
	}
}

// DefaultMaxWait is the bounded wait for a level per spec §11.1: L1 and L2
// wait up to 30 seconds, L3 up to 10 seconds.
func DefaultMaxWait(level Level) time.Duration {
	if level == L3 {
		return 10 * time.Second
	}
	return 30 * time.Second
}

func pathFor(paths Paths, level Level) (string, error) {
	switch level {
	case L1:
		return paths.L1, nil
	case L2:
		return paths.L2, nil
	case L3:
		return paths.L3, nil
	default:
		return "", fmt.Errorf("hostlock: unknown level %v", level)
	}
}

// newLocker constructs the real flock-backed Locker for a path. Tests inject
// an alternate constructor via AcquireOptions.NewLocker instead of touching
// the filesystem.
func newLocker(path string) backup.Locker {
	return monitor.NewFileLock(path)
}

// AcquireOptions customizes Acquire for tests: NewLocker replaces the real
// flock primitive, and MaxWait overrides the fixed per-level timeout so
// tests do not need to wait 10-30 real seconds to prove a timeout path.
type AcquireOptions struct {
	NewLocker func(path string) backup.Locker
	MaxWait   func(level Level) time.Duration
}

// Lease holds zero or more of L1/L2/L3. Levels are always acquired in L1,
// L2, L3 order and released in reverse (spec §11.1), independent of the
// order Levels was given in.
type Lease struct {
	held []heldLevel
}

type heldLevel struct {
	level Level
	lease backup.LockLease
}

// Levels reports which levels this lease currently holds, in acquisition
// (L1, L2, L3) order. Used by tests and by callers that need to know, e.g.,
// whether L3 was actually taken.
func (l *Lease) Levels() []Level {
	out := make([]Level, 0, len(l.held))
	for _, h := range l.held {
		out = append(out, h.level)
	}
	return out
}

// Acquire takes the requested levels (any subset of L1/L2/L3, in any input
// order) in the fixed L1->L2->L3 order. If any level times out or ctx is
// cancelled, every level already acquired is released, in reverse order,
// before returning the error: a partial lease is never handed back.
func Acquire(ctx context.Context, paths Paths, opts AcquireOptions, levels ...Level) (*Lease, error) {
	want := map[Level]bool{}
	for _, l := range levels {
		want[l] = true
	}

	newLockerFn := opts.NewLocker
	if newLockerFn == nil {
		newLockerFn = newLocker
	}
	maxWaitFn := opts.MaxWait
	if maxWaitFn == nil {
		maxWaitFn = DefaultMaxWait
	}

	lease := &Lease{}
	for _, level := range orderedLevels {
		if !want[level] {
			continue
		}
		path, err := pathFor(paths, level)
		if err != nil {
			lease.Release()
			return nil, err
		}
		locker := newLockerFn(path)
		held, err := backup.AcquireWithRetry(ctx, locker, backup.DefaultLockRetryInterval, maxWaitFn(level))
		if err != nil {
			lease.Release()
			return nil, fmt.Errorf("acquire %s: %w", level, err)
		}
		lease.held = append(lease.held, heldLevel{level: level, lease: held})
	}
	return lease, nil
}

// Release unlocks every held level in reverse acquisition order (L3, L2,
// L1), matching spec §11.1's "释放顺序相反". Safe to call multiple times or
// on a zero-value Lease; only the first call's error (if any) is returned.
func (l *Lease) Release() error {
	if l == nil {
		return nil
	}
	var firstErr error
	for i := len(l.held) - 1; i >= 0; i-- {
		if err := l.held[i].lease.Unlock(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	l.held = nil
	return firstErr
}
