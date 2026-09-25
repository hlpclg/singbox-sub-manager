package role

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// memFS is an in-memory FS for pure-unit table tests. It has no notion of
// permission errors; TestRoleMatrix covers pure existence/content
// combinations, and TestRoleReadOnlyRealFS separately proves the package
// never mutates the real filesystem it reads.
type memFS struct {
	files map[string][]byte
	dirs  map[string]bool
}

type memFileInfo struct {
	name  string
	isDir bool
	mode  os.FileMode
}

func (i memFileInfo) Name() string       { return i.name }
func (i memFileInfo) Size() int64        { return 0 }
func (i memFileInfo) Mode() os.FileMode  { return i.mode }
func (i memFileInfo) ModTime() time.Time { return time.Time{} }
func (i memFileInfo) IsDir() bool        { return i.isDir }
func (i memFileInfo) Sys() interface{}   { return nil }

func (m memFS) fs() FS {
	return FS{
		ReadFile: func(path string) ([]byte, error) {
			if data, ok := m.files[path]; ok {
				return data, nil
			}
			return nil, os.ErrNotExist
		},
		Stat: func(path string) (os.FileInfo, error) {
			if m.dirs[path] {
				return memFileInfo{name: filepath.Base(path), isDir: true, mode: os.ModeDir | 0o755}, nil
			}
			if data, ok := m.files[path]; ok {
				_ = data
				return memFileInfo{name: filepath.Base(path), isDir: false, mode: 0o644}, nil
			}
			return nil, os.ErrNotExist
		},
	}
}

const (
	installPath = "/etc/singbox-sub-manager/install.json"
	tokenPath   = "/var/lib/singbox-sub-manager/token"
	nodesPath   = "/etc/singbox-sub-manager/nodes.conf"
	subRoot     = "/var/www/proxy-sub"
)

func testPaths() Paths {
	return Paths{InstallJSON: installPath, DefaultTokenFile: tokenPath, DefaultNodesConf: nodesPath}
}

const validToken = "abcdefghijklmnop" // 16 chars, matches health.ValidToken's [16,128] bound

func validInstallJSON() []byte {
	return []byte(`{"domain":"sub.example.com","subscription_root":"` + subRoot + `","token_file":"` + tokenPath + `"}`)
}

func TestRoleMatrix(t *testing.T) {
	tests := []struct {
		name       string
		files      map[string][]byte
		dirs       map[string]bool
		wantStatus Status
		wantReason string // substring expected somewhere in Reasons; "" = don't check
	}{
		{
			name:       "all three absent is No",
			files:      map[string][]byte{},
			dirs:       map[string]bool{},
			wantStatus: StatusNo,
		},
		{
			name: "fully valid is Yes",
			files: map[string][]byte{
				installPath: validInstallJSON(),
				tokenPath:   []byte(validToken),
				nodesPath:   []byte("[node1]\n"),
			},
			dirs:       map[string]bool{subRoot: true},
			wantStatus: StatusYes,
		},
		{
			name: "only nodes.conf present (accidental node-host create) is Uncertain",
			files: map[string][]byte{
				nodesPath: []byte("[node1]\n"),
			},
			wantStatus: StatusUncertain,
			wantReason: "install.json missing",
		},
		{
			name: "install.json unparseable",
			files: map[string][]byte{
				installPath: []byte("{not json"),
				tokenPath:   []byte(validToken),
				nodesPath:   []byte("x"),
			},
			dirs:       map[string]bool{subRoot: true},
			wantStatus: StatusUncertain,
			wantReason: "install.json unparseable",
		},
		{
			name: "domain empty",
			files: map[string][]byte{
				installPath: []byte(`{"domain":"","subscription_root":"` + subRoot + `","token_file":"` + tokenPath + `"}`),
				tokenPath:   []byte(validToken),
				nodesPath:   []byte("x"),
			},
			dirs:       map[string]bool{subRoot: true},
			wantStatus: StatusUncertain,
			wantReason: "domain is empty",
		},
		{
			name: "subscription_root not absolute",
			files: map[string][]byte{
				installPath: []byte(`{"domain":"d.example.com","subscription_root":"relative/path","token_file":"` + tokenPath + `"}`),
				tokenPath:   []byte(validToken),
				nodesPath:   []byte("x"),
			},
			wantStatus: StatusUncertain,
			wantReason: "subscription_root is not an absolute path",
		},
		{
			name: "subscription_root missing directory",
			files: map[string][]byte{
				installPath: []byte(`{"domain":"d.example.com","subscription_root":"` + subRoot + `","token_file":"` + tokenPath + `"}`),
				tokenPath:   []byte(validToken),
				nodesPath:   []byte("x"),
			},
			// subRoot deliberately not in dirs
			wantStatus: StatusUncertain,
			wantReason: "subscription_root does not exist",
		},
		{
			name: "token_file empty",
			files: map[string][]byte{
				installPath: []byte(`{"domain":"d.example.com","subscription_root":"` + subRoot + `","token_file":""}`),
				nodesPath:   []byte("x"),
			},
			dirs:       map[string]bool{subRoot: true},
			wantStatus: StatusUncertain,
			wantReason: "token_file is empty",
		},
		{
			name: "token_file unreadable",
			files: map[string][]byte{
				installPath: []byte(`{"domain":"d.example.com","subscription_root":"` + subRoot + `","token_file":"/nowhere/token"}`),
				nodesPath:   []byte("x"),
			},
			dirs:       map[string]bool{subRoot: true},
			wantStatus: StatusUncertain,
			wantReason: "token_file is unreadable",
		},
		{
			name: "token content too short fails ValidToken",
			files: map[string][]byte{
				installPath: []byte(`{"domain":"d.example.com","subscription_root":"` + subRoot + `","token_file":"` + tokenPath + `"}`),
				tokenPath:   []byte("short"),
				nodesPath:   []byte("x"),
			},
			dirs:       map[string]bool{subRoot: true},
			wantStatus: StatusUncertain,
			wantReason: "not a valid token",
		},
		{
			name: "token content trimmed of whitespace still validates",
			files: map[string][]byte{
				installPath: []byte(`{"domain":"d.example.com","subscription_root":"` + subRoot + `","token_file":"` + tokenPath + `"}`),
				tokenPath:   []byte("  " + validToken + "\n"),
				nodesPath:   []byte("x"),
			},
			dirs:       map[string]bool{subRoot: true},
			wantStatus: StatusYes,
		},
		{
			name: "default nodes.conf missing",
			files: map[string][]byte{
				installPath: validInstallJSON(),
				tokenPath:   []byte(validToken),
			},
			dirs:       map[string]bool{subRoot: true},
			wantStatus: StatusUncertain,
			wantReason: "default nodes.conf does not exist",
		},
		{
			name: "default nodes.conf is a directory not a regular file",
			files: map[string][]byte{
				installPath: validInstallJSON(),
				tokenPath:   []byte(validToken),
			},
			dirs:       map[string]bool{subRoot: true, nodesPath: true},
			wantStatus: StatusUncertain,
			wantReason: "not a regular file",
		},
		{
			name: "multiple failures all listed",
			files: map[string][]byte{
				installPath: []byte(`{"domain":"","subscription_root":"relative","token_file":""}`),
			},
			wantStatus: StatusUncertain,
			wantReason: "domain is empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := memFS{files: tt.files, dirs: tt.dirs}
			result := DetectSubscriptionCenter(m.fs(), testPaths())
			if result.Status != tt.wantStatus {
				t.Fatalf("status = %v, want %v (reasons: %v)", result.Status, tt.wantStatus, result.Reasons)
			}
			if tt.wantStatus == StatusUncertain && len(result.Reasons) == 0 {
				t.Fatalf("expected reasons to be populated for Uncertain, got none")
			}
			if tt.wantStatus != StatusUncertain && len(result.Reasons) != 0 {
				t.Fatalf("expected no reasons for status %v, got %v", tt.wantStatus, result.Reasons)
			}
			if tt.wantReason != "" {
				var found bool
				for _, r := range result.Reasons {
					if strings.Contains(r, tt.wantReason) {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("expected a reason containing %q, got %v", tt.wantReason, result.Reasons)
				}
			}
		})
	}
}

// TestRoleTokenLengthBoundary exercises health.ValidToken's exact [16,128]
// length boundary and an illegal-character case through the role
// detection path (not just health's own tests), since it's what
// DetectSubscriptionCenter's "是" classification depends on.
func TestRoleTokenLengthBoundary(t *testing.T) {
	repeat := func(n int) string {
		s := make([]byte, n)
		for i := range s {
			s[i] = 'a'
		}
		return string(s)
	}

	tests := []struct {
		name       string
		token      string
		wantStatus Status
	}{
		{"15 chars is too short", repeat(15), StatusUncertain},
		{"16 chars is the minimum valid length", repeat(16), StatusYes},
		{"128 chars is the maximum valid length", repeat(128), StatusYes},
		{"129 chars is too long", repeat(129), StatusUncertain},
		{"illegal character (space) at valid length", repeat(15) + " ", StatusUncertain},
		{"illegal character (slash) at valid length", repeat(15) + "/", StatusUncertain},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := memFS{
				files: map[string][]byte{
					installPath: validInstallJSON(),
					tokenPath:   []byte(tt.token),
					nodesPath:   []byte("x"),
				},
				dirs: map[string]bool{subRoot: true},
			}
			result := DetectSubscriptionCenter(m.fs(), testPaths())
			if result.Status != tt.wantStatus {
				t.Fatalf("token %q (%d bytes): status = %v, want %v (reasons: %v)", tt.token, len(tt.token), result.Status, tt.wantStatus, result.Reasons)
			}
		})
	}
}

// TestRoleReadOnlyRealFS proves detection never mutates what it reads: run
// against a real temporary filesystem, capture byte content and mtimes
// before and after, and assert they are identical.
func TestRoleReadOnlyRealFS(t *testing.T) {
	dir := t.TempDir()
	installPath := filepath.Join(dir, "install.json")
	tokenPath := filepath.Join(dir, "token")
	nodesPath := filepath.Join(dir, "nodes.conf")
	subRoot := filepath.Join(dir, "proxy-sub")

	if err := os.Mkdir(subRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	installData := []byte(`{"domain":"sub.example.com","subscription_root":"` + subRoot + `","token_file":"` + tokenPath + `"}`)
	if err := os.WriteFile(installPath, installData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(validToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodesPath, []byte("[node1]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	snapshot := func() map[string]struct {
		data  []byte
		mtime time.Time
	} {
		out := map[string]struct {
			data  []byte
			mtime time.Time
		}{}
		for _, p := range []string{installPath, tokenPath, nodesPath} {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			out[p] = struct {
				data  []byte
				mtime time.Time
			}{data, info.ModTime()}
		}
		return out
	}

	before := snapshot()

	result := DetectSubscriptionCenter(OSFS(), Paths{InstallJSON: installPath, DefaultTokenFile: tokenPath, DefaultNodesConf: nodesPath})
	if result.Status != StatusYes {
		t.Fatalf("status = %v, want Yes (reasons: %v)", result.Status, result.Reasons)
	}

	after := snapshot()
	for p, b := range before {
		a := after[p]
		if string(a.data) != string(b.data) {
			t.Fatalf("%s: content changed", p)
		}
		if !a.mtime.Equal(b.mtime) {
			t.Fatalf("%s: mtime changed", p)
		}
	}
}
