package monitor

import (
	"context"
	"os/exec"
)

func RestartService(ctx context.Context, name string) error {
	cmd := exec.CommandContext(ctx, "systemctl", "restart", name)
	return cmd.Run()
}
