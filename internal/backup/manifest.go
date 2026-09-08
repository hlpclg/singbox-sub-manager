// Package backup creates and verifies archives of the singbox-sub-manager
// persistent state (node configuration, rendered subscriptions, sing-box and
// Caddy configuration, subscription token and monitor state).
package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SchemaVersion is the only manifest schema this package produces and accepts.
const SchemaVersion = 1

// ManifestName is the fixed name of the manifest entry inside an archive.
const ManifestName = "manifest.json"

// Skip reasons recorded in Manifest.SkippedReasons. The values are stable and
// may be relied on by callers and tests.
const (
	SkipNotFound              = "not_found"
	SkipUnsupportedSourceType = "unsupported_source_type"
	SkipEmptyDirectory        = "empty_directory"
)

var (
	// ErrUnsupportedSchema is returned when a manifest declares a schema
	// version this build cannot process.
	ErrUnsupportedSchema = errors.New("unsupported manifest schema version")
	// ErrUnsafePath is returned when a manifest or archive entry carries a
	// path that must never be written (absolute, traversing or duplicated).
	ErrUnsafePath = errors.New("unsafe archive path")
	// ErrUnsupportedSourceType is returned when an explicitly listed source
	// path is not a plain regular file (symlink, hard link, directory,
	// device, socket or FIFO).
	ErrUnsupportedSourceType = errors.New("unsupported source file type")
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// FileEntry describes one regular file captured in an archive. Mode, UID and
// GID are the source file's own values at pack time so that real ownership
// models such as Caddyfile's root:caddy 0640 survive a restore unchanged.
type FileEntry struct {
	Path   string
	SHA256 string
	Mode   fs.FileMode
	UID    uint32
	GID    uint32
}

// fileEntryJSON is the wire form of FileEntry; Mode is serialized as an octal
// string ("0640") as specified by the design document.
type fileEntryJSON struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Mode   string `json:"mode"`
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
}

func (e FileEntry) MarshalJSON() ([]byte, error) {
	return json.Marshal(fileEntryJSON{
		Path:   e.Path,
		SHA256: e.SHA256,
		Mode:   formatMode(e.Mode),
		UID:    e.UID,
		GID:    e.GID,
	})
}

func (e *FileEntry) UnmarshalJSON(data []byte) error {
	var raw fileEntryJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	mode, err := parseMode(raw.Mode)
	if err != nil {
		return err
	}
	e.Path = raw.Path
	e.SHA256 = raw.SHA256
	e.Mode = mode
	e.UID = raw.UID
	e.GID = raw.GID
	return nil
}

func formatMode(m fs.FileMode) string {
	return fmt.Sprintf("%04o", m.Perm())
}

func parseMode(s string) (fs.FileMode, error) {
	if s == "" {
		return 0, fmt.Errorf("manifest: empty file mode")
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("manifest: invalid file mode %q: %w", s, err)
	}
	if v > 0o777 {
		return 0, fmt.Errorf("manifest: file mode %q has bits outside permissions", s)
	}
	return fs.FileMode(v), nil
}

// Manifest is the archive index. ArchivePath is populated by Create for the
// caller's convenience and is never serialized into the archive.
type Manifest struct {
	SchemaVersion   int               `json:"schema_version"`
	CreatedAt       time.Time         `json:"created_at"`
	ProxyctlVersion string            `json:"proxyctl_version"`
	Files           []FileEntry       `json:"files"`
	Skipped         []string          `json:"skipped"`
	SkippedReasons  map[string]string `json:"skipped_reasons"`
	ArchivePath     string            `json:"-"`
}

// Validate checks the manifest's schema version and structural safety. It is
// the single source of truth for what counts as a well-formed manifest, both
// when writing an archive and before reading one back.
func (m Manifest) Validate() error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: %d", ErrUnsupportedSchema, m.SchemaVersion)
	}
	if m.CreatedAt.IsZero() {
		return fmt.Errorf("manifest: created_at is missing")
	}
	seen := make(map[string]struct{}, len(m.Files))
	for _, f := range m.Files {
		if err := ValidateLogicalPath(f.Path); err != nil {
			return err
		}
		if _, dup := seen[f.Path]; dup {
			return fmt.Errorf("%w: duplicate entry %q", ErrUnsafePath, f.Path)
		}
		seen[f.Path] = struct{}{}
		if !sha256Pattern.MatchString(f.SHA256) {
			return fmt.Errorf("manifest: invalid sha256 for %q", f.Path)
		}
		if f.Mode.Perm() != f.Mode {
			return fmt.Errorf("manifest: mode for %q carries non-permission bits", f.Path)
		}
	}
	for _, p := range m.Skipped {
		if err := ValidateLogicalPath(p); err != nil {
			return err
		}
		if _, dup := seen[p]; dup {
			return fmt.Errorf("%w: %q is both packed and skipped", ErrUnsafePath, p)
		}
		if _, ok := m.SkippedReasons[p]; !ok {
			return fmt.Errorf("manifest: skipped path %q has no reason", p)
		}
	}
	for p := range m.SkippedReasons {
		if !containsString(m.Skipped, p) {
			return fmt.Errorf("manifest: reason recorded for unlisted path %q", p)
		}
	}
	return nil
}

// ValidateLogicalPath rejects anything that must never be resolved against a
// destination root: absolute paths, traversal, and empty or unclean
// components. Every other byte is a legal POSIX file name and must survive a
// backup unchanged.
func ValidateLogicalPath(p string) error {
	if p == "" {
		return fmt.Errorf("%w: empty path", ErrUnsafePath)
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("%w: %q is absolute", ErrUnsafePath, p)
	}
	if p != path.Clean(p) {
		return fmt.Errorf("%w: %q is not a clean relative path", ErrUnsafePath, p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%w: %q has an unsafe component", ErrUnsafePath, p)
		}
	}
	return nil
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
