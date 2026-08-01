package health

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// ExecRunner is the production CommandRunner.
type ExecRunner struct{}

// Run executes name with args, capturing stdout/stderr and the exit code.
// NotFound is set when the binary is not in PATH.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) CommandResult {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := CommandResult{Stdout: out.String(), Stderr: errb.String()}
	if err == nil {
		return res
	}
	if errors.Is(err, exec.ErrNotFound) {
		res.NotFound = true
		res.Err = err
		return res
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res
	}
	// Could not start (e.g. context cancelled). Report as error.
	res.ExitCode = -1
	res.Err = err
	return res
}

// runnerOf returns the configured runner or the production default.
func runnerOf(cfg Config) CommandRunner {
	if cfg.Runner != nil {
		return cfg.Runner
	}
	return ExecRunner{}
}
