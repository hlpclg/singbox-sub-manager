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

func TestNodeRemoveMissing(t *testing.T) {
	p := writeNodes(t, sectioned)
	var out, errb bytes.Buffer
	if code := run([]string{"node", "remove", "NOPE", "--nodes", p}, &out, &errb); code == 0 {
		t.Fatal("expected error removing missing node")
	}
}
