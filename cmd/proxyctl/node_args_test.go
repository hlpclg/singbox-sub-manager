package main

// TestStrictNodeArgs (plan Task 7, design spec §4.5 & §14.1 "严格模式解
// 析"): exercises StrictScan/DetermineStrictMode/RejectLeadingDash/
// IsHelpOnly directly against synthetic FlagSpec sets covering every flag
// named in the spec's matrix (--publish, --purge, --recover, --abandon,
// --enabled), independent of which concrete CLI command eventually wires
// them in (service install/reinstall/show, Task 7's own commands, use only
// a subset; --publish/--abandon/--enabled belong to Task 8/9's commands but
// the scanning engine itself must already handle them correctly and
// reusably).
import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func specs(kv ...FlagSpec) []FlagSpec { return kv }

func TestStrictScanUnknownFlag(t *testing.T) {
	_, err := StrictScan([]string{"--bogus", "x"}, specs(FlagSpec{Name: "name", Kind: FlagString}), 0)
	if err == nil {
		t.Fatalf("want error for unknown flag")
	}
}

func TestStrictScanDuplicateFlag(t *testing.T) {
	fs := specs(FlagSpec{Name: "name", Kind: FlagString})
	_, err := StrictScan([]string{"--name", "a", "--name", "b"}, fs, 0)
	if err == nil {
		t.Fatalf("want error for duplicate flag")
	}
}

func TestStrictScanMissingValue(t *testing.T) {
	fs := specs(FlagSpec{Name: "name", Kind: FlagString})
	_, err := StrictScan([]string{"--name"}, fs, 0)
	if err == nil {
		t.Fatalf("want error for missing value")
	}
}

func TestStrictScanBoolForms(t *testing.T) {
	fs := specs(FlagSpec{Name: "enabled", Kind: FlagBool})

	t.Run("bare flag means true", func(t *testing.T) {
		got, err := StrictScan([]string{"--enabled"}, fs, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.Bool("enabled") {
			t.Fatalf("want enabled=true")
		}
	})
	t.Run("=true and =false accepted", func(t *testing.T) {
		got, err := StrictScan([]string{"--enabled=false"}, fs, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Bool("enabled") {
			t.Fatalf("want enabled=false")
		}
	})
	t.Run("=garbage rejected", func(t *testing.T) {
		if _, err := StrictScan([]string{"--enabled=maybe"}, fs, 0); err == nil {
			t.Fatalf("want error for --enabled=maybe")
		}
	})
	t.Run("space-separated value is NOT consumed: becomes positional (spec example)", func(t *testing.T) {
		got, err := StrictScan([]string{"--enabled", "false"}, fs, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.Bool("enabled") {
			t.Fatalf("want enabled=true (bare form)")
		}
		if len(got.Positional) != 1 || got.Positional[0] != "false" {
			t.Fatalf("want positional=[false], got %v", got.Positional)
		}
	})
}

func TestStrictScanPublishExactBoolOnly(t *testing.T) {
	fs := specs(FlagSpec{Name: "publish", Kind: FlagExactBool, DoubleDashOnly: true})

	t.Run("bare --publish accepted", func(t *testing.T) {
		got, err := StrictScan([]string{"--publish"}, fs, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.Bool("publish") {
			t.Fatalf("want publish=true")
		}
	})
	t.Run("--publish=true rejected: exact bool takes no value form", func(t *testing.T) {
		if _, err := StrictScan([]string{"--publish=true"}, fs, 0); err == nil {
			t.Fatalf("want error for --publish=true")
		}
	})
	t.Run("single-dash -publish rejected as unknown (double-dash only)", func(t *testing.T) {
		if _, err := StrictScan([]string{"-publish"}, fs, 0); err == nil {
			t.Fatalf("want error for -publish")
		}
	})
}

func TestStrictScanOtherBoolFlagNames(t *testing.T) {
	// spec §4.5's matrix explicitly names --purge, --recover, --abandon
	// alongside --publish/--enabled as bool flags following the regular
	// (non-exact) --x / --x=true|false rule; only --publish is exact-only.
	fs := specs(
		FlagSpec{Name: "purge", Kind: FlagBool},
		FlagSpec{Name: "recover", Kind: FlagBool},
		FlagSpec{Name: "abandon", Kind: FlagBool},
	)
	for _, name := range []string{"purge", "recover", "abandon"} {
		got, err := StrictScan([]string{"--" + name + "=true"}, fs, 0)
		if err != nil {
			t.Fatalf("--%s=true: unexpected error: %v", name, err)
		}
		if !got.Bool(name) {
			t.Fatalf("--%s=true: want true", name)
		}
	}
}

func TestStrictScanPositionalCount(t *testing.T) {
	fs := specs(FlagSpec{Name: "publish", Kind: FlagExactBool, DoubleDashOnly: true})

	t.Run("zero positional required, one given: error", func(t *testing.T) {
		if _, err := StrictScan([]string{"extra"}, fs, 0); err == nil {
			t.Fatalf("want error")
		}
	})
	t.Run("one positional required, zero given: error", func(t *testing.T) {
		if _, err := StrictScan(nil, fs, 1); err == nil {
			t.Fatalf("want error")
		}
	})
	t.Run("one positional required, one given: ok, flag before or after", func(t *testing.T) {
		for _, args := range [][]string{
			{"--publish", "myname"},
			{"myname", "--publish"},
		} {
			got, err := StrictScan(args, fs, 1)
			if err != nil {
				t.Fatalf("args=%v: unexpected error: %v", args, err)
			}
			if len(got.Positional) != 1 || got.Positional[0] != "myname" {
				t.Fatalf("args=%v: positional=%v", args, got.Positional)
			}
			if !got.Bool("publish") {
				t.Fatalf("args=%v: want publish=true", args)
			}
		}
	})
}

func TestStrictScanValueFlagEqualsForm(t *testing.T) {
	fs := specs(FlagSpec{Name: "server", Kind: FlagString})
	got, err := StrictScan([]string{"--server=1.2.3.4"}, fs, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Values["server"] != "1.2.3.4" {
		t.Fatalf("server=%q", got.Values["server"])
	}
}

func TestRejectLeadingDash(t *testing.T) {
	if err := RejectLeadingDash("-evil"); err == nil {
		t.Fatalf("want error for leading dash")
	}
	if err := RejectLeadingDash("ok-name"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDetermineStrictMode(t *testing.T) {
	valueFlags := map[string]bool{"name": true, "server": true, "nodes": true}

	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"no flags at all", []string{"myname"}, false},
		{"plain --publish is strict", []string{"--publish", "myname"}, true},
		{"single-dash -publish still triggers mode judgment", []string{"-publish", "myname"}, true},
		{"--name --publish: --publish is --name's VALUE, not strict", []string{"--name", "--publish"}, false},
		{"--name=--publish (equals form) is also just a value", []string{"--name=--publish"}, false},
		{"--publish after an unrelated value flag", []string{"--server", "1.2.3.4", "--publish"}, true},
		{"--nodes consumes its own value, not mistaken for publish position", []string{"--nodes", "path", "myname"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetermineStrictMode(tc.args, valueFlags)
			if got != tc.want {
				t.Fatalf("DetermineStrictMode(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestIsHelpOnly(t *testing.T) {
	if !IsHelpOnly([]string{"--help"}) {
		t.Fatalf("--help alone: want true")
	}
	if !IsHelpOnly([]string{"-h"}) {
		t.Fatalf("-h alone: want true")
	}
	if IsHelpOnly([]string{"--help", "extra"}) {
		t.Fatalf("--help with another arg: want false (falls through to unknown-flag handling)")
	}
	if IsHelpOnly(nil) {
		t.Fatalf("no args: want false")
	}
	// When --help is combined with other args, StrictScan (which never
	// registers "help"/"h" as a known flag) must reject it as unknown,
	// per spec "与其他参数同时出现时,按未知flag处理,返回2".
	fs := specs(FlagSpec{Name: "name", Kind: FlagString})
	if _, err := StrictScan([]string{"--help", "--name", "x"}, fs, 0); err == nil {
		t.Fatalf("want error: --help combined with other args must be rejected as unknown flag")
	}
}

// TestStrictNodeArgs is the plan's named strict-parsing matrix (spec §4.5,
// §14.1). Sections: the reusable scan engine and its post-scan rules, the
// three service commands as wired today, and the untouched legacy CRUD
// entries. The finer-grained TestStrictScan* tests above stay as they are.
func TestStrictNodeArgs(t *testing.T) {
	// One spec set shaped like the strict commands Task 8/9 will add, so the
	// whole §4.5 rule list is exercised against the engine they will reuse.
	full := []FlagSpec{
		{Name: "publish", Kind: FlagExactBool, DoubleDashOnly: true},
		{Name: "name", Kind: FlagString},
		{Name: "server", Kind: FlagString},
		{Name: "nodes", Kind: FlagString},
		{Name: "min-version", Kind: FlagString},
		{Name: "enabled", Kind: FlagBool},
		{Name: "purge", Kind: FlagBool},
		{Name: "recover", Kind: FlagBool},
		{Name: "abandon", Kind: FlagBool},
	}
	// parse applies the scan plus the two placement/conflict rules.
	parse := func(args []string, positional int, abandonAllowed bool) (StrictArgs, error) {
		a, err := StrictScan(args, full, positional)
		if err != nil {
			return a, err
		}
		if err := CheckPublishNodesConflict(a); err != nil {
			return a, err
		}
		return a, CheckAbandonPlacement(a, abandonAllowed)
	}

	t.Run("engine", func(t *testing.T) {
		cases := []struct {
			name           string
			args           []string
			positional     int
			abandonAllowed bool
			wantErr        bool
		}{
			{"duplicate flag", []string{"--name", "a", "--name", "b"}, 0, false, true},
			{"duplicate bool", []string{"--purge", "--purge"}, 0, false, true},
			{"duplicate mixed spellings", []string{"--name=a", "-name", "b"}, 0, false, true},
			{"unknown flag", []string{"--bogus"}, 0, false, true},
			{"unknown single dash", []string{"-bogus=1"}, 0, false, true},
			{"missing value at end", []string{"--name"}, 0, false, true},
			{"bool --x accepted", []string{"--purge"}, 0, false, false},
			{"bool --x=true accepted", []string{"--recover=true"}, 0, false, false},
			{"bool --x=false accepted", []string{"--enabled=false"}, 0, false, false},
			{"bool --x=yes rejected", []string{"--purge=yes"}, 0, false, true},
			{"bool --x=1 rejected", []string{"--purge=1"}, 0, false, true},
			{"--enabled false leaves a positional", []string{"--enabled", "false"}, 0, false, true},
			{"--enabled false with one positional allowed", []string{"--enabled", "false"}, 1, false, false},
			{"zero positionals wanted, one given", []string{"x"}, 0, false, true},
			{"one positional wanted, none given", nil, 1, false, true},
			{"one positional wanted, two given", []string{"a", "b"}, 1, false, true},
			{"flags before or after the name", []string{"--publish", "n", "--min-version", "v0.10.0"}, 1, false, false},
			{"name before flags", []string{"n", "--publish"}, 1, false, false},
			{"--publish accepted", []string{"--publish"}, 0, false, false},
			{"--publish=x rejected", []string{"--publish=x"}, 0, false, true},
			{"--publish=true rejected", []string{"--publish=true"}, 0, false, true},
			{"-publish rejected", []string{"-publish"}, 0, false, true},
			{"--publish with --nodes", []string{"--publish", "--nodes", "/tmp/x"}, 0, false, true},
			{"--nodes with --publish (either order)", []string{"--nodes=/tmp/x", "--publish"}, 0, false, true},
			{"--nodes alone", []string{"--nodes", "/tmp/x"}, 0, false, false},
			{"--abandon outside recover", []string{"--abandon"}, 0, false, true},
			{"--abandon=true outside recover", []string{"--abandon=true"}, 0, false, true},
			{"--abandon inside recover", []string{"--recover", "--abandon"}, 0, true, false},
			{"--help is not a known flag", []string{"--help"}, 0, false, true},
			{"-h is not a known flag", []string{"-h"}, 0, false, true},
			{"--help mixed with others", []string{"--help", "--name", "a"}, 0, false, true},
		}
		for _, tc := range cases {
			t.Run(strings.ReplaceAll(tc.name, " ", "_"), func(t *testing.T) {
				_, err := parse(tc.args, tc.positional, tc.abandonAllowed)
				if (err != nil) != tc.wantErr {
					t.Fatalf("parse(%v) error = %v, wantErr %v", tc.args, err, tc.wantErr)
				}
				if err != nil {
					var se *StrictScanError
					if !errors.As(err, &se) {
						t.Fatalf("error must be a *StrictScanError (maps to exit 2), got %T", err)
					}
				}
			})
		}

		t.Run("--name --publish is a value, not a mode", func(t *testing.T) {
			valueFlags := map[string]bool{"name": true, "server": true, "nodes": true, "min-version": true}
			if DetermineStrictMode([]string{"--name", "--publish"}, valueFlags) {
				t.Fatalf("--publish consumed as --name's value must not select strict mode")
			}
			a, err := StrictScan([]string{"--name", "--publish"}, full, 0)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if a.Values["name"] != "--publish" || a.Set["publish"] {
				t.Fatalf("--publish must be --name's value: %+v", a)
			}
			if RejectLeadingDash(a.Values["name"]) == nil {
				t.Fatalf("as a node name it starts with '-' and must still be rejected")
			}
			for _, args := range [][]string{{"--publish"}, {"-publish"}, {"--server", "x", "--publish"}, {"n", "--publish"}} {
				if !DetermineStrictMode(args, valueFlags) {
					t.Errorf("DetermineStrictMode(%v) = false, want strict", args)
				}
			}
		})

		t.Run("dash-leading names are rejected", func(t *testing.T) {
			for _, n := range []string{"-a", "--a", "-", "--", "-publish"} {
				if RejectLeadingDash(n) == nil {
					t.Errorf("RejectLeadingDash(%q) = nil", n)
				}
			}
			if RejectLeadingDash("a-b") != nil {
				t.Errorf("a-b must be accepted")
			}
		})

		t.Run("help alone", func(t *testing.T) {
			if !IsHelpOnly([]string{"--help"}) || !IsHelpOnly([]string{"-h"}) {
				t.Fatalf("sole --help/-h must be recognised")
			}
			if IsHelpOnly([]string{"-h", "x"}) || IsHelpOnly([]string{"x", "--help"}) {
				t.Fatalf("--help/-h mixed with other arguments must not be treated as help")
			}
		})
	})

	t.Run("service commands", func(t *testing.T) {
		// --help / -h alone: usage on stdout, exit 0; mixed: exit 2.
		for _, sub := range [][]string{{"node", "service"}, {"node", "service", "install"}, {"node", "service", "reinstall"}, {"node", "service", "show"}} {
			for _, h := range []string{"--help", "-h"} {
				log := installFakeRuntime(t, fakeRuntimeOpts{})
				var out, errb bytes.Buffer
				if code := run(append(append([]string(nil), sub...), h), &out, &errb); code != 0 || out.Len() == 0 {
					t.Errorf("%v %s: exit=%d out=%q err=%q, want 0 with usage", sub, h, code, out.String(), errb.String())
				}
				if len(log.list()) != 0 {
					t.Errorf("%v %s touched the runtime: %v", sub, h, log.list())
				}
			}
		}
		mixed := [][]string{
			{"node", "service", "install", "--help", "--name", "a", "--server", "1.2.3.4"},
			{"node", "service", "install", "--name", "a", "--server", "1.2.3.4", "-h"},
			{"node", "service", "reinstall", "-h", "--singbox-version", "1"},
			{"node", "service", "show", "--help", "extra"},
			{"node", "service", "--help", "install"},
		}
		// wrong flags for these commands (rules that Task 8/9 wire into
		// recover/publish/CRUD: here they are simply unknown flags).
		misplaced := [][]string{
			{"node", "service", "install", "--name", "a", "--server", "1.2.3.4", "--abandon"},
			{"node", "service", "install", "--name", "a", "--server", "1.2.3.4", "--publish"},
			{"node", "service", "install", "--name", "a", "--server", "1.2.3.4", "--publish", "--nodes", "/x"},
			{"node", "service", "reinstall", "--abandon"},
			{"node", "service", "reinstall", "--publish=x"},
			{"node", "service", "reinstall", "-publish"},
			{"node", "service", "show", "--abandon"},
			{"node", "service", "show", "--publish"},
			{"node", "service", "show", "--nodes", "/x"},
		}
		// --name --publish: --publish is --name's value, and a name may not
		// start with '-', so it is rejected as a usage error, not run.
		values := [][]string{
			{"node", "service", "install", "--name", "--publish", "--server", "1.2.3.4"},
			{"node", "service", "install", "--name", "-x", "--server", "1.2.3.4"},
			{"node", "service", "install", "--name=-x", "--server", "1.2.3.4"},
		}
		// repeated / unknown / missing value / bool spellings / positionals
		shapes := [][]string{
			{"node", "service", "install", "--name", "a", "--name", "b", "--server", "1.2.3.4"},
			{"node", "service", "install", "--name", "a", "--server", "1.2.3.4", "--port", "1", "--port=2"},
			{"node", "service", "install", "--name", "a", "--server"},
			{"node", "service", "install", "--name", "a", "--server", "1.2.3.4", "--bogus=1"},
			{"node", "service", "install", "--name", "a", "--server", "1.2.3.4", "positional"},
			{"node", "service", "reinstall", "--singbox-version", "1", "--singbox-version", "2", "--singbox-sha256", "x"},
			{"node", "service", "reinstall", "--singbox-sha256"},
			{"node", "service", "show", "--purge"},
			{"node", "service", "show", "false"},
		}
		for _, group := range [][][]string{mixed, misplaced, values, shapes} {
			for _, args := range group {
				log := installFakeRuntime(t, fakeRuntimeOpts{})
				var out, errb bytes.Buffer
				if code := run(args, &out, &errb); code != 2 {
					t.Errorf("%v: exit = %d, want 2 (stderr %q)", args, code, errb.String())
				}
				if len(log.list()) != 0 || out.Len() != 0 {
					t.Errorf("%v: a usage error must touch nothing; runtime=%v stdout=%q", args, log.list(), out.String())
				}
			}
		}
	})

	t.Run("legacy CRUD is untouched", func(t *testing.T) {
		// Duplicate flags: the last one wins, exactly as before v0.10.
		p := writeNodes(t, sectioned)
		var out, errb bytes.Buffer
		code := run([]string{"node", "add", "--name", "A", "--name", "B", "--server", "9.9.9.9", "--port", "1", "--port", "2",
			"--password", "p", "--obfs-password", "o", "--sni", "s", "--nodes", p}, &out, &errb)
		if code != 0 || out.String() != "added node \"B\"\n" || errb.Len() != 0 {
			t.Fatalf("legacy duplicate flags: code=%d out=%q err=%q", code, out.String(), errb.String())
		}
		data, _ := os.ReadFile(p)
		if !strings.Contains(string(data), "[B]") || strings.Contains(string(data), "[A]") || !strings.Contains(string(data), "PORT=2") {
			t.Fatalf("last duplicate value must win:\n%s", data)
		}

		// No role line on stdout or stderr for the unchanged commands.
		for _, args := range [][]string{{"node", "list", "--nodes", p}, {"node", "disable", "JP", "--nodes", p}} {
			out.Reset()
			errb.Reset()
			if code := run(args, &out, &errb); code != 0 {
				t.Fatalf("%v: code=%d err=%s", args, code, errb.String())
			}
			if strings.Contains(strings.ToLower(out.String()+errb.String()), "role") {
				t.Errorf("%v printed a role line: out=%q err=%q", args, out.String(), errb.String())
			}
		}

		// The v0.10 strict flags are not wired into the legacy entries yet
		// (--publish/--nodes-conflict is Task 9's, recover/--abandon Task 8's):
		// today they are rejected as unknown flags with the legacy exit code 2
		// and leave the file byte-for-byte unchanged.
		before, _ := os.ReadFile(p)
		for _, args := range [][]string{
			{"node", "add", "--publish", "--nodes", p, "--name", "X", "--server", "1.2.3.4", "--port", "1", "--password", "p", "--obfs-password", "o", "--sni", "s"},
			{"node", "enable", "JP", "--publish", "--nodes", p},
			{"node", "remove", "JP", "--publish=x", "--nodes", p},
			{"node", "enable", "JP", "--abandon", "--nodes", p},
		} {
			out.Reset()
			errb.Reset()
			if code := run(args, &out, &errb); code != 2 {
				t.Errorf("%v: legacy exit = %d, want 2 (unknown flag)", args[:3], code)
			}
		}
		after, _ := os.ReadFile(p)
		if string(before) != string(after) {
			t.Errorf("rejected legacy invocations modified the file")
		}
	})
}
