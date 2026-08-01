package health

import (
	"context"
	"sync"
	"time"
)

const maxConcurrency = 4

// RunAll executes checks. Checks whose ID is in concurrentIDs run in parallel
// (bounded to maxConcurrency); all others run serially in order. Results are
// returned in the same order as checks. The whole run is bounded by
// cfg.Timeouts.Overall; a check still running when that fires is reported as a
// timeout failure.
//
// Every Check.Run must honour ctx: when ctx.Done() fires, Run must return
// promptly. RunAll does not spawn wrapper goroutines to force-cancel a
// misbehaving check.
func RunAll(ctx context.Context, cfg Config, checks []Check, concurrentIDs map[string]bool) []Result {
	overall := cfg.Timeouts.Overall
	if overall <= 0 {
		overall = DefaultTimeouts().Overall
	}
	ctx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()

	results := make([]Result, len(checks))

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrency)

	// Launch concurrent checks.
	for i, c := range checks {
		if !concurrentIDs[c.ID()] {
			continue
		}
		wg.Add(1)
		go func(i int, c Check) {
			defer wg.Done()
			start := time.Now()
			// Acquire a concurrency slot, respecting the overall deadline.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = timeoutResult(c, start)
				return
			}
			res := c.Run(ctx, cfg)
			if ctx.Err() != nil {
				results[i] = timeoutResult(c, start)
			} else {
				res.ID, res.Name = c.ID(), c.Name()
				res.DurationMS = time.Since(start).Milliseconds()
				results[i] = res
			}
		}(i, c)
	}

	// Serial checks run inline, in order.
	for i, c := range checks {
		if concurrentIDs[c.ID()] {
			continue
		}
		start := time.Now()
		if ctx.Err() != nil {
			results[i] = timeoutResult(c, start)
			continue
		}
		res := c.Run(ctx, cfg)
		if ctx.Err() != nil {
			results[i] = timeoutResult(c, start)
		} else {
			res.ID, res.Name = c.ID(), c.Name()
			res.DurationMS = time.Since(start).Milliseconds()
			results[i] = res
		}
	}

	wg.Wait()
	return results
}

// timeoutResult builds a uniform timeout failure for check c.
func timeoutResult(c Check, start time.Time) Result {
	return Result{
		ID:         c.ID(),
		Name:       c.Name(),
		Status:     StatusFail,
		Message:    "timeout",
		DurationMS: time.Since(start).Milliseconds(),
	}
}
