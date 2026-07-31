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

func TestSubscriptionBuildEqualsMerge(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "nodes.conf")
	content := "[JP]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=p1\nOBFS_PASSWORD=o1\nSNI=www.bing.com\nENABLED=true\n"
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "out")
	var so, se bytes.Buffer
	if code := run([]string{"subscription", "build", "--nodes", p, "--output", out}, &so, &se); code != 0 {
		t.Fatalf("subscription build code=%d err=%s", code, se.String())
	}
	if _, err := os.Stat(filepath.Join(out, "clash.yaml")); err != nil {
		t.Fatalf("clash.yaml not generated: %v", err)
	}
}

func TestMergeExcludesDisabled(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "nodes.conf")
	content := "[JP]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=p1\nOBFS_PASSWORD=o1\nSNI=www.bing.com\nENABLED=true\n\n[US]\nSERVER=5.6.7.8\nPORT=443\nPASSWORD=p2\nOBFS_PASSWORD=o2\nSNI=www.apple.com\nENABLED=false\n"
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "out")
	var so, se bytes.Buffer
	if code := run([]string{"merge", "--nodes", p, "--output", out}, &so, &se); code != 0 {
		t.Fatalf("merge code=%d err=%s", code, se.String())
	}
	data, _ := os.ReadFile(filepath.Join(out, "sr.txt"))
	if strings.Contains(string(data), "US") {
		t.Fatalf("disabled node US leaked into subscription: %s", string(data))
	}
	if !strings.Contains(string(data), "JP") {
		t.Fatalf("enabled node JP missing: %s", string(data))
	}
}

func TestMergeAllDisabledErrors(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "nodes.conf")
	content := "[JP]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=p1\nOBFS_PASSWORD=o1\nSNI=www.bing.com\nENABLED=false\n"
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "out")
	var so, se bytes.Buffer
	if code := run([]string{"merge", "--nodes", p, "--output", out}, &so, &se); code == 0 {
		t.Fatal("expected error when all nodes disabled")
	}
}
