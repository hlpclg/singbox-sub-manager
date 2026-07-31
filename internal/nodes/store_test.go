package nodes

import (
	"os"
	"path/filepath"
	"testing"
)

func sample() []Node {
	return []Node{
		{Name: "JP", Server: "1.2.3.4", Port: 443, Password: "p1", ObfsPassword: "o1", SNI: "www.bing.com", Enabled: true},
		{Name: "US", Server: "5.6.7.8", Port: 8443, Password: "p2", ObfsPassword: "o2", SNI: "www.apple.com", Enabled: false},
	}
}

func TestWriteFileRoundTripIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nodes.conf")
	if err := WriteFile(p, sample()); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ns, format, err := Load(p)
	if err != nil || format != FormatSectioned {
		t.Fatalf("Load after write: format=%v err=%v", format, err)
	}
	if len(ns) != 2 || ns[0].Name != "JP" || ns[1].Enabled {
		t.Fatalf("round-trip mismatch: %+v", ns)
	}
	// Writing the reloaded set produces identical bytes.
	if Serialize(ns) != Serialize(sample()) {
		t.Fatalf("serialize not idempotent")
	}
}

func TestWriteFilePermsAndBackup(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nodes.conf")
	if err := os.WriteFile(p, []byte("OLD"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(p, sample()); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("perm = %o, want 600", fi.Mode().Perm())
	}
	bak, err := os.ReadFile(p + ".bak")
	if err != nil || string(bak) != "OLD" {
		t.Fatalf("backup: %q err=%v", string(bak), err)
	}
}

func TestAddDuplicateRejected(t *testing.T) {
	ns := sample()
	_, err := Add(ns, Node{Name: "JP", Server: "9.9.9.9", Port: 443, Password: "p", ObfsPassword: "o", SNI: "s", Enabled: true})
	if err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestAddInvalidRejected(t *testing.T) {
	ns := sample()
	if _, err := Add(ns, Node{Name: "bad name", Server: "9.9.9.9", Port: 443, Password: "p", ObfsPassword: "o", SNI: "s"}); err == nil {
		t.Fatal("expected invalid-name error")
	}
}

func TestReplaceRenameDuplicateRejected(t *testing.T) {
	ns := sample()
	upd := ns[0]
	upd.Name = "US" // collides with existing
	if _, err := Replace(ns, "JP", upd); err == nil {
		t.Fatal("expected rename-collision error")
	}
}

func TestReplaceRenameOK(t *testing.T) {
	ns := sample()
	upd := ns[0]
	upd.Name = "JP2"
	out, err := Replace(ns, "JP", upd)
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if _, ok := Find(out, "JP2"); !ok {
		t.Fatal("renamed node not found")
	}
	if _, ok := Find(out, "JP"); ok {
		t.Fatal("old name should be gone")
	}
}

func TestRemoveMissingRejected(t *testing.T) {
	if _, err := Remove(sample(), "NOPE"); err == nil {
		t.Fatal("expected missing error")
	}
}

func TestSetEnabled(t *testing.T) {
	out, err := SetEnabled(sample(), "US", true)
	if err != nil {
		t.Fatal(err)
	}
	i, _ := Find(out, "US")
	if !out[i].Enabled {
		t.Fatal("US should be enabled")
	}
}

func TestEnabledFilter(t *testing.T) {
	got := Enabled(sample())
	if len(got) != 1 || got[0].Name != "JP" {
		t.Fatalf("Enabled filter = %+v", got)
	}
}
