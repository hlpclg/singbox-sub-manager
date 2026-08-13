package monitor

import (
	"context"
	"fmt"
)

type EligibilityChecker struct {
	RunConfigCheck func(ctx context.Context, svc string) error
	CheckPortOwner func(ctx context.Context, svc string) error
}

func (e *EligibilityChecker) CheckEligibility(ctx context.Context, service string) error {
	if e.RunConfigCheck != nil {
		if err := e.RunConfigCheck(ctx, service); err != nil {
			return fmt.Errorf("config check failed: %w", err)
		}
	}
	if e.CheckPortOwner != nil {
		if err := e.CheckPortOwner(ctx, service); err != nil {
			return fmt.Errorf("port check failed: %w", err)
		}
	}
	return nil
}
