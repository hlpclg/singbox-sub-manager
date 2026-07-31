package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	out := strings.TrimSpace(stdout.String())
	if out != Version {
		t.Errorf("expected version %q, got %q", Version, out)
	}
}

func TestRunInvalidUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected exit code 2 for empty args, got %d", code)
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"unknown_cmd"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected exit code 2 for unknown command, got %d", code)
	}
}

func TestRunMergeAndValidate(t *testing.T) {
	tmpDir := t.TempDir()
	nodesFile := filepath.Join(tmpDir, "nodes.conf")
	nodesContent := "# managed-by: installer\nNode1|1.2.3.4|443|pass123|obfs123|example.com\n"
	if err := os.WriteFile(nodesFile, []byte(nodesContent), 0644); err != nil {
		t.Fatalf("failed to write nodes.conf: %v", err)
	}

	outDir := filepath.Join(tmpDir, "out")

	var stdout, stderr bytes.Buffer
	code := run([]string{"merge", "--nodes", nodesFile, "--output", outDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("merge failed with code %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "generated 1 nodes") {
		t.Errorf("unexpected merge output: %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"validate", "--nodes", nodesFile}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("validate failed with code %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "validation passed") {
		t.Errorf("unexpected validate output: %s", stdout.String())
	}
}

func TestRunValidateFailure(t *testing.T) {
	tmpDir := t.TempDir()
	nodesFile := filepath.Join(tmpDir, "nodes.conf")
	nodesContent := "# managed-by: installer\nNode1|1.2.3.4|443|CHANGE_ME|obfs123|example.com\n"
	if err := os.WriteFile(nodesFile, []byte(nodesContent), 0644); err != nil {
		t.Fatalf("failed to write nodes.conf: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"validate", "--nodes", nodesFile}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1 for CHANGE_ME validation, got %d", code)
	}
	if !strings.Contains(stderr.String(), "validation failed") {
		t.Errorf("expected validation failed message in stderr, got %s", stderr.String())
	}
}

func TestRunInvalidFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"merge", "--invalid-flag"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected exit code 2 for invalid flag in merge, got %d", code)
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"validate", "--invalid-flag"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected exit code 2 for invalid flag in validate, got %d", code)
	}
}
