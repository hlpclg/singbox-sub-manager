package realitynode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenServerConfigParams is the fixed set of test parameters shared by
// TestGenerateServerConfigGolden (default test set, byte-for-byte against
// testdata/probe/server_config.golden.json) and the reality_integration-
// tagged TestGenerateServerConfigMatchesTask1Structure (review finding
// F7): PrivateKey is a test constant (32 zero bytes, base64 raw-url
// encoded) — never a real key — and every other field is a fixed,
// deterministic value so both tests exercise the exact same shape.
func goldenServerConfigParams() ServerConfigParams {
	return ServerConfigParams{
		ListenPort:    443,
		SelfcheckPort: 54321,
		SNI:           "www.microsoft.com",
		UUID:          "11111111-1111-4111-8111-111111111111",
		PrivateKey:    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		ShortID:       "0123456789abcdef",
		LocalCIDRs:    []string{"203.0.113.9/32"}, // RFC 5737 TEST-NET-3, not a real address
	}
}

// TestGenerateServerConfigGolden implements review finding F7's first
// requirement: a byte-for-byte comparison against a frozen fixture,
// exercising the FULL structure (fixed reject list, inbound, reality
// handshake, flow, log, outbounds, final, selfcheck/local-reject rules) —
// not just rule count and position, which is all the pre-existing test
// checked.
func TestGenerateServerConfigGolden(t *testing.T) {
	got, err := GenerateServerConfig(goldenServerConfigParams())
	if err != nil {
		t.Fatalf("GenerateServerConfig: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "probe", "server_config.golden.json"))
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("GenerateServerConfig output does not match testdata/probe/server_config.golden.json byte-for-byte.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// --- TestConfigAndAddresses ---

func TestConfigAndAddresses(t *testing.T) {
	t.Run("ValidateInstallParams", func(t *testing.T) {
		cases := []struct {
			name    string
			p       InstallParams
			wantErr bool
		}{
			{"valid ipv4 server", InstallParams{Name: "n1", Server: "1.2.3.4", Port: 443, SNI: "www.microsoft.com"}, false},
			{"valid hostname server", InstallParams{Name: "n1", Server: "example.com", Port: 443, SNI: "www.microsoft.com"}, false},
			{"name prefix dash rejected", InstallParams{Name: "-bad", Server: "1.2.3.4", Port: 443, SNI: "example.com"}, true},
			{"name bad charset rejected", InstallParams{Name: "bad name", Server: "1.2.3.4", Port: 443, SNI: "example.com"}, true},
			{"server empty rejected", InstallParams{Name: "n1", Server: "", Port: 443, SNI: "example.com"}, true},
			{"server ipv6 rejected", InstallParams{Name: "n1", Server: "::1", Port: 443, SNI: "example.com"}, true},
			{"server hostname 253 chars ok", InstallParams{Name: "n1", Server: hostnameOfLength(253), Port: 443, SNI: "example.com"}, false},
			{"server hostname 254 chars rejected", InstallParams{Name: "n1", Server: hostnameOfLength(254), Port: 443, SNI: "example.com"}, true},
			{"server label 63 chars ok", InstallParams{Name: "n1", Server: strings.Repeat("a", 63) + ".com", Port: 443, SNI: "example.com"}, false},
			{"server label 64 chars rejected", InstallParams{Name: "n1", Server: strings.Repeat("a", 64) + ".com", Port: 443, SNI: "example.com"}, true},
			{"server label leading dash rejected", InstallParams{Name: "n1", Server: "-a.com", Port: 443, SNI: "example.com"}, true},
			{"server label trailing dash rejected", InstallParams{Name: "n1", Server: "a-.com", Port: 443, SNI: "example.com"}, true},
			{"server no dot rejected", InstallParams{Name: "n1", Server: "localhost", Port: 443, SNI: "example.com"}, true},
			{"port zero rejected", InstallParams{Name: "n1", Server: "1.2.3.4", Port: 0, SNI: "example.com"}, true},
			{"port too big rejected", InstallParams{Name: "n1", Server: "1.2.3.4", Port: 65536, SNI: "example.com"}, true},
			{"port max ok", InstallParams{Name: "n1", Server: "1.2.3.4", Port: 65535, SNI: "example.com"}, false},
			{"sni ipv4 literal rejected", InstallParams{Name: "n1", Server: "1.2.3.4", Port: 443, SNI: "1.2.3.4"}, true},
			{"sni ipv6 literal rejected", InstallParams{Name: "n1", Server: "1.2.3.4", Port: 443, SNI: "::1"}, true},
			{"sni valid hostname ok", InstallParams{Name: "n1", Server: "1.2.3.4", Port: 443, SNI: "www.microsoft.com"}, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := ValidateInstallParams(tc.p)
				if tc.wantErr && err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !tc.wantErr && err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			})
		}
	})

	t.Run("LocalAddressCIDRs collects interfaces, ipv4 server literal, and DNS results", func(t *testing.T) {
		ifaces := func() ([]net.Addr, error) {
			return []net.Addr{
				&net.IPNet{IP: net.ParseIP("10.0.0.5"), Mask: net.CIDRMask(24, 32)},
				&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
				&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)}, // loopback IP even on a "non-loopback" iface entry: must still be excluded
			}, nil
		}
		resolve := func(ctx context.Context, host string) ([]net.IP, error) {
			t.Fatalf("resolve should not be called for an IPv4 literal server")
			return nil, nil
		}
		cidrs, err := LocalAddressCIDRs(context.Background(), "203.0.113.9", ifaces, resolve)
		if err != nil {
			t.Fatalf("LocalAddressCIDRs: %v", err)
		}
		want := []string{"10.0.0.5/32", "203.0.113.9/32", "fe80::1/128"}
		if !equalStrings(cidrs, want) {
			t.Fatalf("cidrs = %v, want %v", cidrs, want)
		}
	})

	t.Run("LocalAddressCIDRs resolves hostname server and includes all A/AAAA (NAT address entering reject set)", func(t *testing.T) {
		ifaces := func() ([]net.Addr, error) { return nil, nil }
		resolveCalled := false
		resolve := func(ctx context.Context, host string) ([]net.IP, error) {
			resolveCalled = true
			if host != "example.com" {
				t.Fatalf("resolve called with %q, want example.com", host)
			}
			return []net.IP{net.ParseIP("198.51.100.7"), net.ParseIP("2001:db8::1")}, nil
		}
		cidrs, err := LocalAddressCIDRs(context.Background(), "example.com", ifaces, resolve)
		if err != nil {
			t.Fatalf("LocalAddressCIDRs: %v", err)
		}
		if !resolveCalled {
			t.Fatalf("resolve was not called for hostname server")
		}
		want := []string{"198.51.100.7/32", "2001:db8::1/128"}
		if !equalStrings(cidrs, want) {
			t.Fatalf("cidrs = %v, want %v", cidrs, want)
		}
	})

	t.Run("LocalAddressCIDRs empty when no interfaces and ipv4 server", func(t *testing.T) {
		ifaces := func() ([]net.Addr, error) { return nil, nil }
		resolve := func(ctx context.Context, host string) ([]net.IP, error) { return nil, nil }
		cidrs, err := LocalAddressCIDRs(context.Background(), "1.2.3.4", ifaces, resolve)
		if err != nil {
			t.Fatalf("LocalAddressCIDRs: %v", err)
		}
		want := []string{"1.2.3.4/32"}
		if !equalStrings(cidrs, want) {
			t.Fatalf("cidrs = %v, want %v", cidrs, want)
		}
	})

	t.Run("GenerateServerConfig matches Task 1's real sing-box-1.14.1-verified structure", func(t *testing.T) {
		data, err := GenerateServerConfig(ServerConfigParams{
			ListenPort: 443, SelfcheckPort: 54321, SNI: "www.microsoft.com",
			UUID: "11111111-1111-4111-8111-111111111111", PrivateKey: "abc", ShortID: "0123456789abcdef",
			LocalCIDRs: []string{"10.0.0.5/32"},
		})
		if err != nil {
			t.Fatalf("GenerateServerConfig: %v", err)
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("output is not valid JSON: %v", err)
		}
		route := parsed["route"].(map[string]interface{})
		rules := route["rules"].([]interface{})
		if len(rules) != 4 {
			t.Fatalf("rules count = %d, want 4 (selfcheck allow, fixed reject, local reject, ip_is_private)", len(rules))
		}
		selfcheckRule := rules[0].(map[string]interface{})
		if selfcheckRule["outbound"] != "direct" {
			t.Fatalf("first rule is not the selfcheck allow rule: %v", selfcheckRule)
		}
		lastRule := rules[3].(map[string]interface{})
		if lastRule["ip_is_private"] != true {
			t.Fatalf("last rule is not ip_is_private catch-all: %v", lastRule)
		}
		localRule := rules[2].(map[string]interface{})
		localCIDRs := localRule["ip_cidr"].([]interface{})
		if len(localCIDRs) != 1 || localCIDRs[0] != "10.0.0.5/32" {
			t.Fatalf("local reject rule = %v", localRule)
		}
	})

	t.Run("GenerateServerConfig omits local reject rule when LocalCIDRs empty", func(t *testing.T) {
		data, err := GenerateServerConfig(ServerConfigParams{
			ListenPort: 443, SelfcheckPort: 54321, SNI: "www.microsoft.com",
			UUID: "11111111-1111-4111-8111-111111111111", PrivateKey: "abc", ShortID: "0123456789abcdef",
		})
		if err != nil {
			t.Fatalf("GenerateServerConfig: %v", err)
		}
		var parsed map[string]interface{}
		json.Unmarshal(data, &parsed)
		rules := parsed["route"].(map[string]interface{})["rules"].([]interface{})
		if len(rules) != 3 {
			t.Fatalf("rules count = %d, want 3 when LocalCIDRs is empty", len(rules))
		}
	})

	t.Run("RenderUnit matches Task 1's fixture byte-for-byte", func(t *testing.T) {
		fixture, err := os.ReadFile(filepath.Join("testdata", "probe", "reality.service"))
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		want := strings.Replace(string(fixture), "<v>", "1.14.1", 1)
		got := RenderUnit("1.14.1")
		if string(got) != want {
			t.Fatalf("RenderUnit output does not match testdata/probe/reality.service with <v> substituted:\n--- got ---\n%s\n--- want ---\n%s", got, want)
		}
	})
}

// hostnameOfLength builds a structurally valid hostname (labels 1-63 chars
// of 'a', at least one '.') of exactly n characters, so length-boundary
// tests exercise the overall 253-char limit independent of per-label
// validity.
func hostnameOfLength(n int) string {
	var labels []string
	remaining := n
	for remaining > 63 {
		take := 60
		labels = append(labels, strings.Repeat("a", take))
		remaining -= take + 1 // +1 for the '.' that will join it to the next label
	}
	labels = append(labels, strings.Repeat("a", remaining))
	return strings.Join(labels, ".")
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- TestSelfcheckPortSelection ---

func TestSelfcheckPortSelection(t *testing.T) {
	t.Run("retries when a candidate fails to bind", func(t *testing.T) {
		portRange := func() (int, int, error) { return 20000, 60000, nil }
		var seen []int
		canBind := func(port int) bool {
			seen = append(seen, port)
			return port == 14999 // only the second candidate can bind
		}
		randSeq := []int{5000, 4999} // -> 15000 (rejected by canBind), then 14999
		i := 0
		randIntn := func(n int) int {
			v := randSeq[i]
			i++
			return v
		}
		port, err := SelectSelfcheckPort(443, portRange, canBind, randIntn)
		if err != nil {
			t.Fatalf("SelectSelfcheckPort: %v", err)
		}
		if port != 14999 {
			t.Fatalf("port = %d, want 14999", port)
		}
		if len(seen) != 2 || seen[0] != 15000 || seen[1] != 14999 {
			t.Fatalf("canBind calls = %v, want [15000 14999]", seen)
		}
	})

	t.Run("avoids listenPort even when it falls inside the candidate range", func(t *testing.T) {
		portRange := func() (int, int, error) { return 20000, 60000, nil }
		canBind := func(port int) bool { return true }
		calls := 0
		randIntn := func(n int) int {
			calls++
			if calls == 1 {
				return 15000 - 10000 // candidate == listenPort, must be skipped
			}
			return 16000 - 10000
		}
		port, err := SelectSelfcheckPort(15000, portRange, canBind, randIntn)
		if err != nil {
			t.Fatalf("SelectSelfcheckPort: %v", err)
		}
		if port == 15000 {
			t.Fatalf("port must not equal listenPort")
		}
	})

	t.Run("empty range rejected", func(t *testing.T) {
		portRange := func() (int, int, error) { return 10000, 60000, nil } // lo-1 = 9999 < 10000
		_, err := SelectSelfcheckPort(443, portRange, func(int) bool { return true }, func(int) int { return 0 })
		if !errors.Is(err, errEmptyPortRange) {
			t.Fatalf("err = %v, want errEmptyPortRange", err)
		}
	})

	t.Run("ten failed bind attempts rejected", func(t *testing.T) {
		portRange := func() (int, int, error) { return 20000, 60000, nil }
		attempts := 0
		canBind := func(int) bool { attempts++; return false }
		_, err := SelectSelfcheckPort(443, portRange, canBind, func(int) int { return 0 })
		if !errors.Is(err, errSelfcheckPortFailure) {
			t.Fatalf("err = %v, want errSelfcheckPortFailure", err)
		}
		if attempts != 10 {
			t.Fatalf("attempts = %d, want 10", attempts)
		}
	})

	t.Run("port range read error propagates", func(t *testing.T) {
		portRange := func() (int, int, error) { return 0, 0, errUnsupportedArch }
		_, err := SelectSelfcheckPort(443, portRange, func(int) bool { return true }, func(int) int { return 0 })
		if err == nil {
			t.Fatalf("expected error")
		}
	})
}
