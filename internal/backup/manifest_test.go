package backup

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func validManifest() Manifest {
	return Manifest{
		SchemaVersion:   SchemaVersion,
		CreatedAt:       fixedClock,
		ProxyctlVersion: "v0.6.0",
		Files: []FileEntry{
			{Path: "etc/caddy/Caddyfile", SHA256: strings.Repeat("a", 64), Mode: 0640, UID: 0, GID: 999},
		},
		Skipped:        []string{"var/lib/singbox-sub-manager/monitor-paused"},
		SkippedReasons: map[string]string{"var/lib/singbox-sub-manager/monitor-paused": SkipNotFound},
	}
}

func TestFileEntry_ModeJSONRoundTrip(t *testing.T) {
	entry := FileEntry{Path: "etc/caddy/Caddyfile", SHA256: strings.Repeat("b", 64), Mode: 0640, UID: 0, GID: 999}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"mode":"0640"`) {
		t.Errorf("encoded entry = %s, want an octal mode string", data)
	}

	var back FileEntry
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != entry {
		t.Errorf("round trip = %+v, want %+v", back, entry)
	}
}

func TestFileEntry_RejectsInvalidMode(t *testing.T) {
	for _, raw := range []string{
		`{"path":"a","sha256":"x","mode":"","uid":0,"gid":0}`,
		`{"path":"a","sha256":"x","mode":"9999","uid":0,"gid":0}`,
		`{"path":"a","sha256":"x","mode":"104000","uid":0,"gid":0}`,
	} {
		var e FileEntry
		if err := json.Unmarshal([]byte(raw), &e); err == nil {
			t.Errorf("unmarshal(%s) succeeded, want an error", raw)
		}
	}
}

func TestManifest_ValidateAcceptsWellFormed(t *testing.T) {
	if err := validManifest().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestManifest_ValidateRejectsUnsupportedSchema(t *testing.T) {
	m := validManifest()
	m.SchemaVersion = 2
	err := m.Validate()
	if !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("err = %v, want ErrUnsupportedSchema", err)
	}
}

func TestManifest_ValidateRejectsMissingCreatedAt(t *testing.T) {
	m := validManifest()
	m.CreatedAt = time.Time{}
	if err := m.Validate(); err == nil {
		t.Fatal("Validate accepted a manifest without created_at")
	}
}

func TestManifest_ValidateRejectsUnsafePaths(t *testing.T) {
	unsafe := []string{
		"/etc/caddy/Caddyfile",
		"etc/../../escape",
		"../escape",
		"etc//double",
		"etc/./here",
		"",
	}
	for _, p := range unsafe {
		m := validManifest()
		m.Files[0].Path = p
		if err := m.Validate(); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("Validate(%q) = %v, want ErrUnsafePath", p, err)
		}
		if err := ValidateLogicalPath(p); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("ValidateLogicalPath(%q) = %v, want ErrUnsafePath", p, err)
		}
	}
}

func TestManifest_ValidateAcceptsLegalPosixNames(t *testing.T) {
	// Backslashes, spaces and non-ASCII bytes are ordinary POSIX file name
	// characters; rejecting them would fail a backup over a legal file.
	for _, p := range []string{
		`etc/singbox-sub-manager/we\ird.conf`,
		"etc/singbox-sub-manager/with space.conf",
		"etc/singbox-sub-manager/名前.conf",
	} {
		if err := ValidateLogicalPath(p); err != nil {
			t.Errorf("ValidateLogicalPath(%q) = %v, want nil", p, err)
		}
	}
}

func TestManifest_ValidateRejectsDuplicateEntries(t *testing.T) {
	m := validManifest()
	m.Files = append(m.Files, m.Files[0])
	if err := m.Validate(); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
}

func TestManifest_ValidateRejectsPathBothPackedAndSkipped(t *testing.T) {
	m := validManifest()
	m.Skipped = append(m.Skipped, m.Files[0].Path)
	m.SkippedReasons[m.Files[0].Path] = SkipNotFound
	if err := m.Validate(); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
}

func TestManifest_ValidateRejectsBadChecksum(t *testing.T) {
	for _, sum := range []string{"", "abc", strings.Repeat("A", 64), strings.Repeat("z", 64)} {
		m := validManifest()
		m.Files[0].SHA256 = sum
		if err := m.Validate(); err == nil {
			t.Errorf("Validate accepted checksum %q", sum)
		}
	}
}

func TestManifest_ValidateRejectsNonPermissionMode(t *testing.T) {
	m := validManifest()
	m.Files[0].Mode |= 0o4000 << 3 // a bit outside the permission range
	if err := m.Validate(); err == nil {
		t.Fatal("Validate accepted a mode with non-permission bits")
	}
}

func TestManifest_ValidateRequiresSkipReasons(t *testing.T) {
	m := validManifest()
	delete(m.SkippedReasons, m.Skipped[0])
	if err := m.Validate(); err == nil {
		t.Fatal("Validate accepted a skipped path without a reason")
	}

	m = validManifest()
	m.SkippedReasons["var/lib/singbox-sub-manager/token"] = SkipNotFound
	if err := m.Validate(); err == nil {
		t.Fatal("Validate accepted a reason for an unlisted path")
	}
}
