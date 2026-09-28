// Package realitynode: timeouts.go defines the single seam every spec
// §13.2 phase budget this package owns goes through, instead of calling
// context.WithTimeout directly. Review finding F1: without this seam,
// nothing proved that (a) each phase requests exactly its spec duration,
// and (b) a timeout is enforced by that phase's own budget rather than by
// whatever deadline the caller's ctx happens to already carry — a parent
// given no deadline at all would otherwise never exercise the phase's own
// enforcement path.
package realitynode

import (
	"context"
	"time"
)

// TimeoutFunc derives a context bounded by d from parent. Production is
// defaultTimeoutFunc (a thin wrapper over context.WithTimeout). A test
// injects a fake that records d (proving it equals the spec constant),
// and/or substitutes a short-lived context regardless of d (proving that,
// even when parent carries no deadline of its own, this seam's budget is
// what fires) — see download_test.go, selfcheck_test.go and
// systemd_test.go for both styles.
type TimeoutFunc func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc)

func defaultTimeoutFunc(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}

// timeoutOrDefault returns f, or defaultTimeoutFunc when f is nil — every
// call site uses this instead of checking for nil itself.
func timeoutOrDefault(f TimeoutFunc) TimeoutFunc {
	if f != nil {
		return f
	}
	return defaultTimeoutFunc
}
