package monitor

import (
	"context"
	"testing"
)

func TestEligibility_ConfigFail(t *testing.T) {
	checker := &EligibilityChecker{
		RunConfigCheck: func(ctx context.Context, svc string) error {
			return context.DeadlineExceeded
		},
	}
	err := checker.CheckEligibility(context.Background(), "caddy")
	if err == nil {
		t.Fatal("expected error")
	}
}
