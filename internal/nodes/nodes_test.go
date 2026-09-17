package nodes

import (
	"os"
	"path/filepath"
	"strings"
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

func TestLoadSectionedEnabledFalseDisables(t *testing.T) {
	p := writeTemp(t, "[JP]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=pass\nOBFS_PASSWORD=obfs\nSNI=www.bing.com\nENABLED=false\n")
	ns, _, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ns[0].Enabled {
		t.Fatalf("ENABLED=false should disable")
	}
}

func TestLoadSectionedEnabledCaseInsensitiveTrue(t *testing.T) {
	for _, v := range []string{"TRUE", "True", "true"} {
		p := writeTemp(t, "[JP]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=pass\nOBFS_PASSWORD=obfs\nSNI=www.bing.com\nENABLED="+v+"\n")
		ns, _, err := Load(p)
		if err != nil {
			t.Fatalf("Load(ENABLED=%s): %v", v, err)
		}
		if !ns[0].Enabled {
			t.Fatalf("ENABLED=%s should enable", v)
		}
	}
}

func TestLoadSectionedEnabledInvalidValueRejected(t *testing.T) {
	p := writeTemp(t, "[JP]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=pass\nOBFS_PASSWORD=obfs\nSNI=www.bing.com\nENABLED=flase\n")
	if _, _, err := Load(p); err == nil {
		t.Fatal("expected error for invalid ENABLED value 'flase'")
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

func TestLoadRejectsChangeMeInServer(t *testing.T) {
	p := writeTemp(t, "[JP]\nSERVER=CHANGE_ME\nPORT=443\nPASSWORD=p\nOBFS_PASSWORD=o\nSNI=s\n")
	if _, _, err := Load(p); err == nil {
		t.Fatal("expected CHANGE_ME rejection for SERVER field")
	}
}

func TestLoadRejectsChangeMeInSNI(t *testing.T) {
	p := writeTemp(t, "[JP]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=p\nOBFS_PASSWORD=o\nSNI=CHANGE_ME\n")
	if _, _, err := Load(p); err == nil {
		t.Fatal("expected CHANGE_ME rejection for SNI field")
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

func TestValidateHysteria2EmptyTypeDefaultsToHysteria2(t *testing.T) {
	n := Node{Name: "JP", Server: "1.2.3.4", Port: 443, Password: "p", ObfsPassword: "o", SNI: "s", Enabled: true}
	if err := Validate(n); err != nil {
		t.Fatalf("Validate with empty Type should default to hysteria2: %v", err)
	}
}

func TestValidateVlessRealityAccepted(t *testing.T) {
	n := Node{
		Name: "JP-Reality", Type: TypeVlessReality, Server: "1.2.3.4", Port: 443, SNI: "www.bing.com",
		UUID: "12345678-1234-1234-1234-123456789abc", PublicKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8", ShortID: "0123456789abcdef",
		Enabled: true,
	}
	if err := Validate(n); err != nil {
		t.Fatalf("Validate(valid vless-reality node) = %v, want nil", err)
	}
}

func TestValidateVlessRealityShortIDOptional(t *testing.T) {
	n := Node{
		Name: "JP-Reality", Type: TypeVlessReality, Server: "1.2.3.4", Port: 443, SNI: "www.bing.com",
		UUID: "12345678-1234-1234-1234-123456789abc", PublicKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
		Enabled: true,
	}
	if err := Validate(n); err != nil {
		t.Fatalf("Validate(empty ShortID) = %v, want nil", err)
	}
}

func TestValidateVlessRealityRejectsHysteria2Fields(t *testing.T) {
	n := Node{
		Name: "JP-Reality", Type: TypeVlessReality, Server: "1.2.3.4", Port: 443, SNI: "s",
		UUID: "12345678-1234-1234-1234-123456789abc", PublicKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
		Password: "leaked",
	}
	if err := Validate(n); err == nil {
		t.Fatal("expected error when a vless-reality node carries a Password")
	}
}

func TestValidateHysteria2RejectsVlessRealityFields(t *testing.T) {
	n := Node{
		Name: "JP", Server: "1.2.3.4", Port: 443, Password: "p", ObfsPassword: "o", SNI: "s",
		UUID: "12345678-1234-1234-1234-123456789abc",
	}
	if err := Validate(n); err == nil {
		t.Fatal("expected error when a hysteria2 node carries a UUID")
	}
}

func TestValidateVlessRealityRejectsBadUUID(t *testing.T) {
	n := Node{
		Name: "JP-Reality", Type: TypeVlessReality, Server: "1.2.3.4", Port: 443, SNI: "s",
		UUID: "not-a-uuid", PublicKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
	}
	if err := Validate(n); err == nil {
		t.Fatal("expected error for malformed UUID")
	}
}

func TestValidateVlessRealityRejectsPaddedPublicKey(t *testing.T) {
	n := Node{
		Name: "JP-Reality", Type: TypeVlessReality, Server: "1.2.3.4", Port: 443, SNI: "s",
		UUID: "12345678-1234-1234-1234-123456789abc", PublicKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
	}
	if err := Validate(n); err == nil {
		t.Fatal("expected error for padded base64 public key")
	}
}

func TestValidateVlessRealityRejectsWrongLengthPublicKey(t *testing.T) {
	n := Node{
		Name: "JP-Reality", Type: TypeVlessReality, Server: "1.2.3.4", Port: 443, SNI: "s",
		UUID: "12345678-1234-1234-1234-123456789abc", PublicKey: "AAECAwQFBgcICQoLDA0ODw",
	}
	if err := Validate(n); err == nil {
		t.Fatal("expected error for a public key that does not decode to 32 bytes")
	}
}

func TestValidateVlessRealityRejectsOddLengthShortID(t *testing.T) {
	n := Node{
		Name: "JP-Reality", Type: TypeVlessReality, Server: "1.2.3.4", Port: 443, SNI: "s",
		UUID: "12345678-1234-1234-1234-123456789abc", PublicKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
		ShortID: "abc",
	}
	if err := Validate(n); err == nil {
		t.Fatal("expected error for odd-length ShortID")
	}
}

func TestValidateChangeMeBeforeFormatCheck(t *testing.T) {
	n := Node{
		Name: "JP-Reality", Type: TypeVlessReality, Server: "1.2.3.4", Port: 443, SNI: "s",
		UUID: "CHANGE_ME_UUID", PublicKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
	}
	err := Validate(n)
	if err == nil || !strings.Contains(err.Error(), "CHANGE_ME") {
		t.Fatalf("Validate(CHANGE_ME_UUID) = %v, want a CHANGE_ME placeholder error (not a UUID-format error)", err)
	}
}

func TestValidateUnknownTypeRejected(t *testing.T) {
	n := Node{Name: "JP", Type: NodeType("tuic"), Server: "1.2.3.4", Port: 443, SNI: "s"}
	if err := Validate(n); err == nil {
		t.Fatal("expected error for unknown node type")
	}
}

func TestLoadSectionedVlessReality(t *testing.T) {
	p := writeTemp(t, "[JP-Reality]\nTYPE=vless-reality\nSERVER=1.2.3.4\nPORT=443\nUUID=12345678-1234-1234-1234-123456789abc\nPUBLIC_KEY=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8\nSHORT_ID=0123456789abcdef\nSNI=www.bing.com\nENABLED=true\n")
	ns, format, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if format != FormatSectioned {
		t.Fatalf("format = %v, want FormatSectioned", format)
	}
	if len(ns) != 1 || ns[0].Type != TypeVlessReality || ns[0].UUID != "12345678-1234-1234-1234-123456789abc" {
		t.Fatalf("unexpected node: %+v", ns[0])
	}
}

func TestLoadSectionedWithoutTypeDefaultsHysteria2(t *testing.T) {
	p := writeTemp(t, "[JP]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=pass\nOBFS_PASSWORD=obfs\nSNI=www.bing.com\n")
	ns, _, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ns[0].Type != TypeHysteria2 {
		t.Fatalf("Type = %q, want %q", ns[0].Type, TypeHysteria2)
	}
}

func TestLoadSectionedRejectsPasswordOnVlessReality(t *testing.T) {
	p := writeTemp(t, "[JP-Reality]\nTYPE=vless-reality\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=leaked\nUUID=12345678-1234-1234-1234-123456789abc\nPUBLIC_KEY=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8\nSNI=s\n")
	if _, _, err := Load(p); err == nil {
		t.Fatal("expected error: PASSWORD is not a valid key for type vless-reality")
	}
}

func TestLoadSectionedRejectsUUIDOnHysteria2(t *testing.T) {
	p := writeTemp(t, "[JP]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=p\nOBFS_PASSWORD=o\nUUID=12345678-1234-1234-1234-123456789abc\nSNI=s\n")
	if _, _, err := Load(p); err == nil {
		t.Fatal("expected error: UUID is not a valid key for implicit type hysteria2")
	}
}

func TestLoadSectionedRejectsLateTypeInImplicitMode(t *testing.T) {
	p := writeTemp(t, "[JP]\nSERVER=1.2.3.4\nPORT=443\nTYPE=hysteria2\nPASSWORD=p\nOBFS_PASSWORD=o\nSNI=s\n")
	if _, _, err := Load(p); err == nil {
		t.Fatal("expected error: TYPE must be the first key")
	}
}

func TestLoadSectionedRejectsDuplicateType(t *testing.T) {
	p := writeTemp(t, "[JP-Reality]\nTYPE=vless-reality\nSERVER=1.2.3.4\nPORT=443\nTYPE=vless-reality\nUUID=12345678-1234-1234-1234-123456789abc\nPUBLIC_KEY=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8\nSNI=s\n")
	if _, _, err := Load(p); err == nil {
		t.Fatal("expected error: duplicate TYPE key")
	}
}

func TestLoadSectionedRejectsDuplicateKey(t *testing.T) {
	p := writeTemp(t, "[JP]\nSERVER=1.2.3.4\nSERVER=5.6.7.8\nPORT=443\nPASSWORD=p\nOBFS_PASSWORD=o\nSNI=s\n")
	if _, _, err := Load(p); err == nil {
		t.Fatal("expected error: duplicate SERVER key")
	}
}

func TestLoadSectionedRejectsEmptyTypeValue(t *testing.T) {
	p := writeTemp(t, "[JP]\nTYPE=\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=p\nOBFS_PASSWORD=o\nSNI=s\n")
	if _, _, err := Load(p); err == nil {
		t.Fatal("expected error: empty TYPE value")
	}
}

func TestLoadSectionedRejectsUnknownTypeValue(t *testing.T) {
	p := writeTemp(t, "[JP]\nTYPE=tuic\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=p\nOBFS_PASSWORD=o\nSNI=s\n")
	if _, _, err := Load(p); err == nil {
		t.Fatal("expected error: unknown TYPE value")
	}
}

func TestLoadSectionedRejectsEmptySection(t *testing.T) {
	p := writeTemp(t, "[JP]\n\n[US]\nSERVER=1.2.3.4\nPORT=443\nPASSWORD=p\nOBFS_PASSWORD=o\nSNI=s\n")
	if _, _, err := Load(p); err == nil {
		t.Fatal("expected error: section JP has no keys")
	}
}

func TestLoadSectionedMissingPortReportsPort(t *testing.T) {
	p := writeTemp(t, "[JP-Reality]\nTYPE=vless-reality\nSNI=s\n")
	_, _, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "PORT") {
		t.Fatalf("Load = %v, want an error mentioning missing PORT", err)
	}
}
