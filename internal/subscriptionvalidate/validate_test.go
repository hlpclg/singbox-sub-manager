package subscriptionvalidate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"github.com/hlpclg/singbox-sub-manager/internal/render"
)

func hy2(name string) nodes.Node {
	return nodes.Node{Name: name, Type: nodes.TypeHysteria2, Server: "203.0.113.5", Port: 443,
		Password: "pw-" + name, ObfsPassword: "ob-" + name, SNI: "www.example.com", Enabled: true}
}

func reality(name string) nodes.Node {
	return nodes.Node{Name: name, Type: nodes.TypeVlessReality, Server: "203.0.113.9", Port: 443,
		UUID: "11111111-2222-4333-8444-555555555555", PublicKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
		ShortID: "ab12", SNI: "www.microsoft.com", Enabled: true}
}

// The accepted output is exactly what render.Write puts in clash.yaml and
// sr.txt, so a publish that validated these bytes publishes what `merge`
// would have written.
func TestValidateReturnsRenderWriteBytes(t *testing.T) {
	ns := []nodes.Node{hy2("JP"), reality("US")}
	got, err := Validate(ns)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	dir := t.TempDir()
	if err := render.Write(dir, ns); err != nil {
		t.Fatal(err)
	}
	clash, _ := os.ReadFile(filepath.Join(dir, "clash.yaml"))
	sr, _ := os.ReadFile(filepath.Join(dir, "sr.txt"))
	if got.Clash != string(clash) || got.Shadowrocket != string(sr) {
		t.Fatal("Validate rendered bytes differ from render.Write output")
	}
}

// Each rejection reuses the exact message `proxyctl validate` prints after
// its "validation failed: " prefix.
func TestValidateRejections(t *testing.T) {
	changeMe := hy2("A")
	changeMe.Password = "CHANGE_ME"
	badPort := hy2("B")
	badPort.Port = 0
	reserved := hy2("GitHub")
	cases := []struct {
		name    string
		ns      []nodes.Node
		wantErr string
		is      error
	}{
		{"empty set", nil, "no enabled nodes", ErrNoEnabledNodes},
		{"invalid node", []nodes.Node{badPort}, "invalid port 0", nil},
		{"reserved name", []nodes.Node{reserved}, "conflicts with a reserved policy group", nil},
		{"placeholder secret", []nodes.Node{changeMe}, "CHANGE_ME", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Validate(tc.ns)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.is)
			}
		})
	}
}

func TestClashYAMLStructuralChecks(t *testing.T) {
	base := "proxies:\n  - name: N1\nproxy-groups:\n  - name: G1\n    proxies:\n      - N1\nrules:\n  - MATCH,G1\n"
	if err := ClashYAML(base); err != nil {
		t.Fatalf("valid doc rejected: %v", err)
	}
	bad := map[string]string{
		"invalid yaml":     "proxies: [",
		"reserved proxy":   "proxies:\n  - name: DIRECT\n",
		"undefined member": "proxy-groups:\n  - name: G1\n    proxies:\n      - nope\n",
		"undefined target": "proxies:\n  - name: N1\nproxy-groups:\n  - name: G1\n    proxies:\n      - N1\nrules:\n  - MATCH,ghost\n",
		"too few fields":   "rules:\n  - FOO,bar\n",
		"unused provider":  "rule-providers:\n  rp:\n    type: http\n",
	}
	for name, doc := range bad {
		if err := ClashYAML(doc); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}
