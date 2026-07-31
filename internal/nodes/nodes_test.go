package nodes

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nodes.conf")
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return p
}

func TestLoadLegacyDefaultsEnabled(t *testing.T) {
	p := writeTemp(t, "# c\nJP|1.2.3.4|443|pass|obfs|www.bing.com\n")
	ns, format, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if format != FormatLegacy {
		t.Fatalf("format = %v, want FormatLegacy", format)
	}
	if len(ns) != 1 || ns[0].Name != "JP" || !ns[0].Enabled {
		t.Fatalf("unexpected nodes: %+v", ns)
	}
}

func TestLoadSectioned(t *testing.T) {
	p := writeTemp(t, "[JP]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=pass\nOBFS_PASSWORD=obfs\nSNI=www.bing.com\nENABLED=false\n")
	ns, format, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if format != FormatSectioned {
		t.Fatalf("format = %v, want FormatSectioned", format)
	}
	if len(ns) != 1 || ns[0].Server != "1.2.3.4" || ns[0].Port != 443 || ns[0].Enabled {
		t.Fatalf("unexpected node: %+v", ns[0])
	}
}

func TestLoadSectionedEnabledDefaultsTrue(t *testing.T) {
	p := writeTemp(t, "[JP]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=pass\nOBFS_PASSWORD=obfs\nSNI=www.bing.com\n")
	ns, _, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ns[0].Enabled {
		t.Fatalf("ENABLED missing should default true")
	}
}

func TestLoadEmptyFileIsFormatEmpty(t *testing.T) {
	p := writeTemp(t, "# only a comment\n")
	ns, format, err := Load(p)
	if err != nil || format != FormatEmpty || len(ns) != 0 {
		t.Fatalf("empty: ns=%v format=%v err=%v", ns, format, err)
	}
}

func TestLoadMissingFileIsFormatEmpty(t *testing.T) {
	ns, format, err := Load(filepath.Join(t.TempDir(), "nope.conf"))
	if err != nil || format != FormatEmpty || len(ns) != 0 {
		t.Fatalf("missing: ns=%v format=%v err=%v", ns, format, err)
	}
}

func TestParseFileEmptyErrors(t *testing.T) {
	p := writeTemp(t, "\n\n")
	if _, err := ParseFile(p); err == nil {
		t.Fatal("expected no nodes found error")
	}
}

func TestLoadRejectsBadPort(t *testing.T) {
	p := writeTemp(t, "JP|1.2.3.4|70000|pass|obfs|sni\n")
	if _, _, err := Load(p); err == nil {
		t.Fatal("expected invalid port error")
	}
}

func TestLoadRejectsChangeMe(t *testing.T) {
	p := writeTemp(t, "JP|1.2.3.4|443|CHANGE_ME|obfs|sni\n")
	if _, _, err := Load(p); err == nil {
		t.Fatal("expected CHANGE_ME rejection")
	}
}

func TestLoadRejectsDuplicateName(t *testing.T) {
	p := writeTemp(t, "JP|1.2.3.4|443|p|o|s\nJP|5.6.7.8|443|p|o|s\n")
	if _, _, err := Load(p); err == nil {
		t.Fatal("expected duplicate name error")
	}
}

func TestLoadRejectsMixedFormat(t *testing.T) {
	p := writeTemp(t, "[JP]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=p\nOBFS_PASSWORD=o\nSNI=s\nUS|9.9.9.9|443|p|o|s\n")
	if _, _, err := Load(p); err == nil {
		t.Fatal("expected mixed-format error")
	}
}

func TestValidateName(t *testing.T) {
	ok := []string{"JP", "JP-HY2", "us_west.1"}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	bad := []string{"", "has space", "a|b", "a=b", "a[b", "日本"}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", n)
		}
	}
}
