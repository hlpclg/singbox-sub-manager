package health

// Result is one check's report.
type Result struct {
    ID         string `json:"id"`
    Name       string `json:"name"`
    Status     Status `json:"status"`
    Message    string `json:"message"`
    DurationMS int64  `json:"duration_ms"`
}

// Overall is the aggregate verdict.
type Overall string

const (
    Healthy   Overall = "healthy"
    Degraded  Overall = "degraded"
    Unhealthy Overall = "unhealthy"
)

// Aggregate reduces per-check statuses: any fail dominates, else any warn, else healthy.
func Aggregate(results []Result) Overall {
    hasFail, hasWarn := false, false
    for _, r := range results {
        switch r.Status {
        case StatusFail:
            hasFail = true
        case StatusWarn:
            hasWarn = true
        }
    }
    switch {
    case hasFail:
        return Unhealthy
    case hasWarn:
        return Degraded
    default:
        return Healthy
    }
}

// ExitCode maps an Overall to the process exit code. Exit 3 (arg/internal error) is decided by the CLI, not here.
func ExitCode(o Overall) int {
    switch o {
    case Unhealthy:
        return 1
    case Degraded:
        return 2
    default:
        return 0
    }
}

// Summary counts checks by status.
type Summary struct {
    Passed   int `json:"passed"`
    Warnings int `json:"warnings"`
    Failed   int `json:"failed"`
}

// Summarize tallies results into a Summary.
func Summarize(results []Result) Summary {
    var s Summary
    for _, r := range results {
        switch r.Status {
        case StatusPass:
            s.Passed++
        case StatusWarn:
            s.Warnings++
        case StatusFail:
            s.Failed++
        }
    }
    return s
}
