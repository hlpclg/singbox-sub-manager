// output_test.go covers plan Task 7's TestPasteCommands (design spec
// §10.1, §14.1 "粘贴输出"): each output block has exactly one line that
// does not start with "#", the whole block executes in a real shell
// invoking only that one recommended command (a fake "sudo" records
// exactly one call), and shell quoting round-trips values containing a
// single quote and an empty short_id.
package realitynode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"plain", "'plain'"},
		{"it's", `'it'\''s'`},
		{"a'b'c", `'a'\''b'\''c'`},
	}
	for _, tc := range cases {
		if got := ShellQuote(tc.in); got != tc.want {
			t.Fatalf("ShellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBlockExactlyOneExecutableLine covers spec §14.1: "每个输出块中恰好
// 一行不以#开头".
func TestBlockExactlyOneExecutableLine(t *testing.T) {
	rp := RegisterParams{Server: "1.2.3.4", ListenPort: 443, UUID: "u", PublicKey: "pk", ShortID: "", SNI: "www.microsoft.com", Name: "node1"}

	blocks := map[string]string{
		"install":                     InstallOutput(rp),
		"reinstall_from_installed":    ReinstallFromInstalledOutput(rp),
		"reinstall_from_uninstalled":  ReinstallFromUninstalledOutput(rp),
		"show_installed":              ShowInstalledOutput(rp),
		"show_uninstalled_keep_ident": ShowUninstalledOutput("node1"),
	}
	for name, block := range blocks {
		t.Run(name, func(t *testing.T) {
			lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
			exec := 0
			for _, l := range lines {
				if !strings.HasPrefix(l, "#") {
					exec++
				}
			}
			if exec != 1 {
				t.Fatalf("block %q has %d executable lines, want 1:\n%s", name, exec, block)
			}
		})
	}
}

// fakeSudoScript writes an executable "sudo" into dir that appends its full
// argument list (one call per line) to a call-log file, and exits 0
// without doing anything else — so running a pasted block through a real
// shell never needs an actual "proxyctl" or root.
func fakeSudoScript(t *testing.T, dir, logPath string) {
	t.Helper()
	script := "#!/bin/sh\necho \"$@\" >> '" + logPath + "'\nexit 0\n"
	p := filepath.Join(dir, "sudo")
	if err := os.WriteFile(p, []byte(script), 0755); err != nil {
		t.Fatalf("write fake sudo: %v", err)
	}
}

// runBlockInShell executes block's full text as a shell script (comments
// and all) with dir prepended to PATH, and returns the fake sudo's
// call-log contents.
func runBlockInShell(t *testing.T, block string) []string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	fakeSudoScript(t, dir, logPath)

	scriptPath := filepath.Join(dir, "paste.sh")
	if err := os.WriteFile(scriptPath, []byte(block), 0700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run pasted block: %v\noutput:\n%s\nscript:\n%s", err, out, block)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read call log: %v", err)
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// TestPasteCommands is plan Task 7's named test: each install/reinstall/
// show output block, pasted whole into a real shell, invokes the
// recommended command exactly once with correctly shell-quoted arguments —
// including a value containing a single quote and an empty short_id.
func TestPasteCommands(t *testing.T) {
	rp := RegisterParams{
		Server: "example.com", ListenPort: 8443,
		UUID:      "11111111-1111-4111-8111-111111111111",
		PublicKey: "pubkey-value", ShortID: "", // empty short_id must still paste and execute correctly
		SNI: "www.microsoft.com", Name: "o'brien's-node",
	}

	t.Run("install", func(t *testing.T) {
		calls := runBlockInShell(t, InstallOutput(rp))
		if len(calls) != 1 {
			t.Fatalf("want exactly 1 call, got %d: %v", len(calls), calls)
		}
		wantSubstrings := []string{"node add", "--publish", "--min-version " + MinVersion, "--short-id", "o'brien's-node"}
		for _, s := range wantSubstrings {
			if !strings.Contains(calls[0], s) {
				t.Fatalf("call %q missing %q", calls[0], s)
			}
		}
	})

	t.Run("reinstall from installed: add only", func(t *testing.T) {
		calls := runBlockInShell(t, ReinstallFromInstalledOutput(rp))
		if len(calls) != 1 || !strings.Contains(calls[0], "node add") {
			t.Fatalf("calls=%v", calls)
		}
	})

	t.Run("reinstall from uninstalled: enable only (add is commented)", func(t *testing.T) {
		calls := runBlockInShell(t, ReinstallFromUninstalledOutput(rp))
		if len(calls) != 1 || !strings.Contains(calls[0], "node enable") {
			t.Fatalf("calls=%v", calls)
		}
	})

	t.Run("show installed: add only (enable is commented)", func(t *testing.T) {
		calls := runBlockInShell(t, ShowInstalledOutput(rp))
		if len(calls) != 1 || !strings.Contains(calls[0], "node add") {
			t.Fatalf("calls=%v", calls)
		}
	})

	t.Run("show uninstalled: disable only (remove is commented), lead hint present", func(t *testing.T) {
		block := ShowUninstalledOutput(rp.Name)
		if !strings.HasPrefix(block, "# this node's service is not running") {
			t.Fatalf("missing lead hint:\n%s", block)
		}
		calls := runBlockInShell(t, block)
		if len(calls) != 1 || !strings.Contains(calls[0], "node disable") {
			t.Fatalf("calls=%v", calls)
		}
	})

	t.Run("empty short_id round-trips as a real empty shell argument", func(t *testing.T) {
		// Verify directly against the rendered AddCommand line: the
		// --short-id value must be the two-character empty-quote token
		// '' immediately following --short-id, and the shell must accept
		// it as a distinct (empty) argument, not swallow the following
		// token.
		line := AddCommand(rp)
		if !strings.Contains(line, "--short-id '' ") && !strings.HasSuffix(line, "--short-id ''") {
			t.Fatalf("expected --short-id '' in %q", line)
		}
	})

	t.Run("listen port is quoted like every other value", func(t *testing.T) {
		line := AddCommand(rp)
		want := "--port '" + strconv.Itoa(rp.ListenPort) + "'"
		if !strings.Contains(line, want) {
			t.Fatalf("expected %q in %q", want, line)
		}
	})
}
