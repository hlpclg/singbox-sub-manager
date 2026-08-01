package health

import "testing"

func r(status Status) Result { return Result{Status: status} }

func TestAggregateAndExitCode(t *testing.T) {
    cases := []struct {
        name    string
        results []Result
        want    Overall
        exit    int
    }{
        {"all pass", []Result{r(StatusPass), r(StatusPass)}, Healthy, 0},
        {"one warn", []Result{r(StatusPass), r(StatusWarn)}, Degraded, 2},
        {"one fail", []Result{r(StatusPass), r(StatusFail)}, Unhealthy, 1},
        {"fail beats warn", []Result{r(StatusWarn), r(StatusFail)}, Unhealthy, 1},
        {"empty", nil, Healthy, 0},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            got := Aggregate(c.results)
            if got != c.want {
                t.Fatalf("Aggregate = %q, want %q", got, c.want)
            }
            if ec := ExitCode(got); ec != c.exit {
                t.Fatalf("ExitCode = %d, want %d", ec, c.exit)
            }
        })
    }
}

func TestSummarize(t *testing.T) {
    got := Summarize([]Result{r(StatusPass), r(StatusPass), r(StatusWarn), r(StatusFail)})
    want := Summary{Passed: 2, Warnings: 1, Failed: 1}
    if got != want {
        t.Fatalf("Summarize = %+v, want %+v", got, want)
    }
}
