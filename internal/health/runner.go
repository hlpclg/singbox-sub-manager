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
    // launch concurrent checks
    for i, c := range checks {
        if !concurrentIDs[c.ID()] {
            continue
        }
        wg.Add(1)
        go func(i int, c Check) {
            defer wg.Done()
            sem <- struct{}{}
            defer func() { <-sem }()
            results[i] = runOne(ctx, cfg, c)
        }(i, c)
    }

    // run serial checks inline
    for i, c := range checks {
        if concurrentIDs[c.ID()] {
            continue
        }
        results[i] = runOne(ctx, cfg, c)
    }

    wg.Wait()
    return results
}

// runOne runs a single check, enforcing the overall context deadline even if
// the check itself ignores ctx. The done channel is buffered so the inner
// goroutine never blocks and cannot leak: it always completes and sends once
// each check's own per-op timeout (derived from ctx) fires.
func runOne(ctx context.Context, cfg Config, c Check) Result {
    start := time.Now()
    done := make(chan Result, 1)
    go func() { done <- c.Run(ctx, cfg) }()
    select {
    case res := <-done:
        res.ID, res.Name = c.ID(), c.Name()
        res.DurationMS = time.Since(start).Milliseconds()
        return res
    case <-ctx.Done():
        return Result{
            ID:         c.ID(),
            Name:       c.Name(),
            Status:     StatusFail,
            Message:    "timeout",
            DurationMS: time.Since(start).Milliseconds(),
        }
    }
}
