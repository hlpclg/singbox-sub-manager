package realitynode

// service_platform_test.go: table-driven coverage of spec §8.1's platform
// precheck (OS release, architecture, systemd) — plan Task 7 checklist
// item P1-3(e).
import (
	"errors"
	"testing"
)

func TestCheckPlatform(t *testing.T) {
	ok := func() bool { return true }
	no := func() bool { return false }
	rel := func(id, ver string) func() (string, string, error) {
		return func() (string, string, error) { return id, ver, nil }
	}
	cases := []struct {
		name    string
		env     Env
		wantErr bool
	}{
		{"debian 12 amd64", Env{GOARCH: "amd64", SystemdAvailable: ok, OSRelease: rel("debian", "12")}, false},
		{"debian 12 arm64", Env{GOARCH: "arm64", SystemdAvailable: ok, OSRelease: rel("debian", "12")}, false},
		{"ubuntu 22.04", Env{GOARCH: "amd64", SystemdAvailable: ok, OSRelease: rel("ubuntu", "22.04")}, false},
		{"ubuntu 24.04", Env{GOARCH: "arm64", SystemdAvailable: ok, OSRelease: rel("ubuntu", "24.04")}, false},
		{"explicit linux", Env{GOOS: "linux", GOARCH: "amd64", SystemdAvailable: ok, OSRelease: rel("debian", "12")}, false},
		{"ubuntu 20.04 unsupported", Env{GOARCH: "amd64", SystemdAvailable: ok, OSRelease: rel("ubuntu", "20.04")}, true},
		{"ubuntu 23.10 unsupported", Env{GOARCH: "amd64", SystemdAvailable: ok, OSRelease: rel("ubuntu", "23.10")}, true},
		{"debian 11 unsupported", Env{GOARCH: "amd64", SystemdAvailable: ok, OSRelease: rel("debian", "11")}, true},
		{"debian 13 unsupported", Env{GOARCH: "amd64", SystemdAvailable: ok, OSRelease: rel("debian", "13")}, true},
		{"fedora unsupported", Env{GOARCH: "amd64", SystemdAvailable: ok, OSRelease: rel("fedora", "40")}, true},
		{"alpine unsupported", Env{GOARCH: "amd64", SystemdAvailable: ok, OSRelease: rel("alpine", "3.20")}, true},
		{"empty id unsupported", Env{GOARCH: "amd64", SystemdAvailable: ok, OSRelease: rel("", "")}, true},
		{"os-release unreadable", Env{GOARCH: "amd64", SystemdAvailable: ok, OSRelease: func() (string, string, error) { return "", "", errors.New("no such file") }}, true},
		{"arch 386", Env{GOARCH: "386", SystemdAvailable: ok, OSRelease: rel("debian", "12")}, true},
		{"arch riscv64", Env{GOARCH: "riscv64", SystemdAvailable: ok, OSRelease: rel("debian", "12")}, true},
		{"arch empty", Env{GOARCH: "", SystemdAvailable: ok, OSRelease: rel("debian", "12")}, true},
		{"darwin", Env{GOOS: "darwin", GOARCH: "arm64", SystemdAvailable: ok, OSRelease: rel("debian", "12")}, true},
		{"no systemd", Env{GOARCH: "amd64", SystemdAvailable: no, OSRelease: rel("debian", "12")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			err := checkPlatform(&env)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkPlatform() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestParseOSRelease(t *testing.T) {
	cases := []struct {
		in      string
		id, ver string
	}{
		{"ID=debian\nVERSION_ID=\"12\"\nNAME=\"Debian GNU/Linux\"\n", "debian", "12"},
		{"NAME=\"Ubuntu\"\nID=ubuntu\nVERSION_ID=\"24.04\"\n", "ubuntu", "24.04"},
		{"ID=\"alpine\"\nVERSION_ID=3.20.1\n", "alpine", "3.20.1"},
		{"", "", ""},
		{"# comment\nGARBAGE\n", "", ""},
	}
	for _, tc := range cases {
		id, ver := ParseOSRelease(tc.in)
		if id != tc.id || ver != tc.ver {
			t.Errorf("ParseOSRelease(%q) = %q, %q; want %q, %q", tc.in, id, ver, tc.id, tc.ver)
		}
	}
}
