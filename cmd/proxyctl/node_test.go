package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeNodes(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nodes.conf")
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

const sectioned = "[JP]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=p1\nOBFS_PASSWORD=o1\nSNI=www.bing.com\nENABLED=true\n\n[US]\nSERVER=5.6.7.8\nPORT=8443\nPASSWORD=p2\nOBFS_PASSWORD=o2\nSNI=www.apple.com\nENABLED=true\n"

func TestNodeList(t *testing.T) {
	p := writeNodes(t, sectioned)
	var out, errb bytes.Buffer
	if code := run([]string{"node", "list", "--nodes", p}, &out, &errb); code != 0 {
		t.Fatalf("list code=%d err=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "JP") || !strings.Contains(out.String(), "US") {
		t.Fatalf("list output: %s", out.String())
	}
}

func TestNodeDisableThenList(t *testing.T) {
	p := writeNodes(t, sectioned)
	var out, errb bytes.Buffer
	if code := run([]string{"node", "disable", "US", "--nodes", p}, &out, &errb); code != 0 {
		t.Fatalf("disable code=%d err=%s", code, errb.String())
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "[US]") || !strings.Contains(string(data), "ENABLED=false") {
		t.Fatalf("file after disable: %s", string(data))
	}
}

func TestNodeRemove(t *testing.T) {
	p := writeNodes(t, sectioned)
	var out, errb bytes.Buffer
	if code := run([]string{"node", "remove", "US", "--nodes", p}, &out, &errb); code != 0 {
		t.Fatalf("remove code=%d err=%s", code, errb.String())
	}
	data, _ := os.ReadFile(p)
	if strings.Contains(string(data), "[US]") {
		t.Fatalf("US not removed: %s", string(data))
	}
}

func TestNodeAddViaFlags(t *testing.T) {
	p := writeNodes(t, sectioned)
	var out, errb bytes.Buffer
	code := run([]string{"node", "add",
		"--name", "SG", "--server", "9.9.9.9", "--port", "443",
		"--password", "p3", "--obfs-password", "o3", "--sni", "www.bing.com",
		"--nodes", p}, &out, &errb)
	if code != 0 {
		t.Fatalf("add code=%d err=%s", code, errb.String())
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "[SG]") || !strings.Contains(string(data), "ENABLED=true") {
		t.Fatalf("file after add: %s", string(data))
	}
}

func TestNodeAddMissingFieldNonTTYErrors(t *testing.T) {
	p := writeNodes(t, sectioned)
	var out, errb bytes.Buffer
	// No --password; go test stdin is not a TTY, so it must error, not hang.
	code := run([]string{"node", "add",
		"--name", "SG", "--server", "9.9.9.9", "--port", "443",
		"--obfs-password", "o3", "--sni", "www.bing.com",
		"--nodes", p}, &out, &errb)
	if code == 0 {
		t.Fatal("expected error for missing field on non-TTY")
	}
	if !strings.Contains(errb.String(), "password") {
		t.Fatalf("expected missing-field message, got: %s", errb.String())
	}
}

func TestNodeEditPartialAndRename(t *testing.T) {
	p := writeNodes(t, sectioned)
	var out, errb bytes.Buffer
	// Change only the port of JP.
	if code := run([]string{"node", "edit", "JP", "--port", "8888", "--nodes", p}, &out, &errb); code != 0 {
		t.Fatalf("edit port code=%d err=%s", code, errb.String())
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "PORT=8888") {
		t.Fatalf("port not updated: %s", string(data))
	}
	// Rename JP -> JP2.
	out.Reset()
	errb.Reset()
	if code := run([]string{"node", "edit", "JP", "--name", "JP2", "--nodes", p}, &out, &errb); code != 0 {
		t.Fatalf("edit rename code=%d err=%s", code, errb.String())
	}
	data, _ = os.ReadFile(p)
	if !strings.Contains(string(data), "[JP2]") || strings.Contains(string(data), "[JP]") {
		t.Fatalf("rename failed: %s", string(data))
	}
}

func TestNodeEditRenameCollision(t *testing.T) {
	p := writeNodes(t, sectioned)
	var out, errb bytes.Buffer
	if code := run([]string{"node", "edit", "JP", "--name", "US", "--nodes", p}, &out, &errb); code == 0 {
		t.Fatal("expected rename collision error")
	}
}

func TestNodeMutationRefusesLegacy(t *testing.T) {
	p := writeNodes(t, "JP|1.2.3.4|443|p|o|www.bing.com\n")
	var out, errb bytes.Buffer
	code := run([]string{"node", "disable", "JP", "--nodes", p}, &out, &errb)
	if code == 0 {
		t.Fatal("expected non-zero exit on legacy mutation")
	}
	if !strings.Contains(errb.String(), "migrate") {
		t.Fatalf("expected migrate hint, got: %s", errb.String())
	}
}

func TestNodeDisableRejectsUnknownFlag(t *testing.T) {
	p := writeNodes(t, sectioned)
	var out, errb bytes.Buffer
	code := run([]string{"node", "disable", "US", "--bogus", "--nodes", p}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected exit code 2 for unknown flag, got %d (stderr=%s)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "unknown flag") || !strings.Contains(errb.String(), "--bogus") {
		t.Fatalf("expected stderr to mention unknown flag --bogus, got: %s", errb.String())
	}
}

func TestNodeRemoveMissing(t *testing.T) {
	p := writeNodes(t, sectioned)
	var out, errb bytes.Buffer
	if code := run([]string{"node", "remove", "NOPE", "--nodes", p}, &out, &errb); code == 0 {
		t.Fatal("expected error removing missing node")
	}
}

func TestNodeMigrateLegacyToSectioned(t *testing.T) {
	p := writeNodes(t, "JP|1.2.3.4|443|p1|o1|www.bing.com\nUS|5.6.7.8|443|p2|o2|www.apple.com\n")
	var out, errb bytes.Buffer
	if code := run([]string{"node", "migrate", "--nodes", p}, &out, &errb); code != 0 {
		t.Fatalf("migrate code=%d err=%s", code, errb.String())
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "[JP]") || !strings.Contains(string(data), "ENABLED=true") {
		t.Fatalf("migrated file: %s", string(data))
	}
	bak, err := os.ReadFile(p + ".bak")
	if err != nil || !strings.Contains(string(bak), "JP|1.2.3.4") {
		t.Fatalf("backup missing/wrong: %q err=%v", string(bak), err)
	}
	// Re-running on an already-sectioned file is a no-op success.
	out.Reset()
	errb.Reset()
	if code := run([]string{"node", "migrate", "--nodes", p}, &out, &errb); code != 0 {
		t.Fatalf("second migrate code=%d err=%s", code, errb.String())
	}
}

func TestNodeTest_Error(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// Test without node name
	code := cmdNode([]string{"test"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "error: node name required") {
		t.Errorf("expected name required error, got %s", stderr.String())
	}

	// Test missing file
	stderr.Reset()
	code = cmdNode([]string{"test", "missing", "--nodes", "/tmp/nonexistent-nodes.conf"}, &stdout, &stderr)
	if code != 3 {
		t.Errorf("expected 3, got %d", code)
	}
}
