// Package realitynode implements the v0.10 Reality node lifecycle. This
// file (state.go) is Task 2's read-only contribution: asset ownership
// (spec §5.1), the manifest schema (spec §5.2), and instance-state
// classification (spec §4.3). Nothing here writes, creates, or deletes any
// path; all mutation is later Tasks' (§7's transaction engine onward).
package realitynode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
)

// InstanceStatus is the five-valued result of spec §4.3's priority-ordered
// classification.
type InstanceStatus int

const (
	StatusUnfinishedTxn InstanceStatus = iota
	StatusNotInstalled
	StatusInstalled
	StatusUninstalledKeepIdentity
	StatusInconsistent
)

// Roots are the fixed asset paths from spec §5.1. All fields are path
// roots, never read from anywhere else, so tests can inject alternate
// roots without touching the real filesystem. The lock file
// (/run/lock/proxyctl-reality.lock) is deliberately not a field here: spec
// §5.1 is explicit that it is not an instance asset and must not
// participate in state classification.
type Roots struct {
	SystemdUnit string // /etc/systemd/system/proxyctl-reality.service
	ConfigDir   string // /etc/proxyctl-reality
	Config      string // /etc/proxyctl-reality/config.json
	StateDir    string // /var/lib/proxyctl-reality
	Secrets     string // /var/lib/proxyctl-reality/secrets.json
	Instance    string // /var/lib/proxyctl-reality/instance.json
	Manifest    string // /var/lib/proxyctl-reality/manifest.json
	BinDir      string // /usr/local/lib/proxyctl-reality
	TxnDir      string // /var/lib/proxyctl-reality-txn
	// TombstoneGlob matches /var/lib/.proxyctl-reality-txn.tomb-<txn_id>
	// for any txn_id (spec §7.6); the txn_id is not known ahead of time.
	TombstoneGlob string
}

// ProdRoots returns the fixed production paths from spec §5.1.
func ProdRoots() Roots {
	return Roots{
		SystemdUnit:   "/etc/systemd/system/proxyctl-reality.service",
		ConfigDir:     "/etc/proxyctl-reality",
		Config:        "/etc/proxyctl-reality/config.json",
		StateDir:      "/var/lib/proxyctl-reality",
		Secrets:       "/var/lib/proxyctl-reality/secrets.json",
		Instance:      "/var/lib/proxyctl-reality/instance.json",
		Manifest:      "/var/lib/proxyctl-reality/manifest.json",
		BinDir:        "/usr/local/lib/proxyctl-reality",
		TxnDir:        "/var/lib/proxyctl-reality-txn",
		TombstoneGlob: "/var/lib/.proxyctl-reality-txn.tomb-*",
	}
}

// StateFS is an injectable filesystem seam so tests never touch the real
// filesystem.
type StateFS struct {
	ReadFile func(string) ([]byte, error)
	Stat     func(string) (os.FileInfo, error)
	Glob     func(pattern string) ([]string, error)
}

// OSStateFS returns a StateFS backed by the real filesystem.
func OSStateFS() StateFS {
	return StateFS{ReadFile: os.ReadFile, Stat: os.Stat, Glob: filepath.Glob}
}

const manifestSchema = 1
const dynamicFileSchema = 1

// ManifestEntry is one asset recorded in the manifest (spec §5.2). Its kind
// is inferred from which optional field is populated, exactly matching the
// three shapes the spec describes, rather than an invented discriminator:
// SHA256 set => immutable file; Schema set (non-zero) => dynamic state
// file; neither => directory.
type ManifestEntry struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
	SHA256 string `json:"sha256,omitempty"`
	Schema int    `json:"schema,omitempty"`
}

func (e ManifestEntry) isConflicting() bool   { return e.SHA256 != "" && e.Schema != 0 }
func (e ManifestEntry) isDir() bool           { return e.SHA256 == "" && e.Schema == 0 }
func (e ManifestEntry) isImmutableFile() bool { return e.SHA256 != "" && e.Schema == 0 }
func (e ManifestEntry) isDynamicFile() bool   { return e.Schema != 0 && e.SHA256 == "" }

// Manifest is the persisted state at Roots.Manifest (spec §5.2).
type Manifest struct {
	Schema          int             `json:"schema"`
	State           string          `json:"state"` // "installed" or "uninstalled"
	SingboxCurrent  string          `json:"singbox_current"`
	SingboxPrevious string          `json:"singbox_previous,omitempty"`
	Entries         []ManifestEntry `json:"entries"`
}

const (
	ManifestStateInstalled   = "installed"
	ManifestStateUninstalled = "uninstalled"
)

// SecretsFile is the schema of Roots.Secrets (spec §5.1 "身份").
type SecretsFile struct {
	Schema     int    `json:"schema"`
	UUID       string `json:"uuid"`
	PrivateKey string `json:"private_key"`
	ShortID    string `json:"short_id"`
}

// InstanceFile is the schema of Roots.Instance (spec §5.1 "实例参数").
type InstanceFile struct {
	Schema        int    `json:"schema"`
	Name          string `json:"name"`
	Server        string `json:"server"`
	ListenPort    int    `json:"listen_port"`
	SNI           string `json:"sni"`
	SelfcheckPort int    `json:"selfcheck_port"`
}

// DetectResult is the outcome of instance-state classification.
type DetectResult struct {
	Status InstanceStatus
	// Reasons lists every inconsistency found, populated only when Status
	// is StatusInconsistent.
	Reasons []string
}

func pathExists(fsys StateFS, path string) bool {
	_, err := fsys.Stat(path)
	return err == nil
}

func tombstoneExists(fsys StateFS, roots Roots) bool {
	matches, err := fsys.Glob(roots.TombstoneGlob)
	return err == nil && len(matches) > 0
}

// DetectInstanceState implements spec §4.3's priority-ordered
// classification: evaluate each row in order and stop at the first one
// whose full condition holds; a row whose condition does not fully hold is
// not a match, even if part of it looks right, and classification falls
// through toward StatusInconsistent.
func DetectInstanceState(fsys StateFS, roots Roots) DetectResult {
	// Priority 1: unfinished transaction. This is checked before anything
	// else, per spec "事务/墓碑优先".
	if pathExists(fsys, roots.TxnDir) || tombstoneExists(fsys, roots) {
		return DetectResult{Status: StatusUnfinishedTxn}
	}

	stateDirAbsent := !pathExists(fsys, roots.StateDir)
	configDirAbsent := !pathExists(fsys, roots.ConfigDir)
	unitAbsent := !pathExists(fsys, roots.SystemdUnit)
	binDirAbsent := !pathExists(fsys, roots.BinDir)

	// Priority 2: not installed. All four asset roots absent. A lock file
	// existing alone does not affect this: it is not one of the four
	// roots checked here (spec "L3文件单独存在不是实例").
	if stateDirAbsent && configDirAbsent && unitAbsent && binDirAbsent {
		return DetectResult{Status: StatusNotInstalled}
	}

	manifest, manifestErr := readManifest(fsys, roots.Manifest)

	// Priority 3: installed. Requires manifest.State == installed AND
	// every entry to validate.
	if manifestErr == nil && manifest.State == ManifestStateInstalled {
		if reasons := validateInstalledManifest(fsys, roots, manifest); len(reasons) == 0 {
			return DetectResult{Status: StatusInstalled}
		}
	}

	// Priority 4: uninstalled but identity retained. Requires
	// manifest.State == uninstalled, secrets.json/instance.json valid, and
	// unit/config/bin dir all absent.
	if manifestErr == nil && manifest.State == ManifestStateUninstalled {
		reasons := validateUninstalledManifest(fsys, roots, manifest)
		if unitAbsent && configDirAbsent && binDirAbsent && len(reasons) == 0 {
			return DetectResult{Status: StatusUninstalledKeepIdentity}
		}
	}

	// Priority 5: inconsistent. Collect every reason for the report.
	return DetectResult{Status: StatusInconsistent, Reasons: inconsistencyReasons(fsys, roots, manifest, manifestErr)}
}

func readManifest(fsys StateFS, path string) (Manifest, error) {
	data, err := fsys.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, err
	}
	if m.Schema != manifestSchema {
		return Manifest{}, errUnknownManifestSchema
	}
	for _, e := range m.Entries {
		if e.Path == path {
			return Manifest{}, errManifestContainsSelf
		}
	}
	return m, nil
}

var errUnknownManifestSchema = jsonSentinelError("unknown manifest schema")
var errManifestContainsSelf = jsonSentinelError("manifest entries include the manifest's own path")

type jsonSentinelError string

func (e jsonSentinelError) Error() string { return string(e) }

// validateInstalledManifest checks every manifest entry against the real
// filesystem (spec §5.2: mode/uid/gid for all; sha256 for immutable files;
// schema parse for dynamic files) and returns every failure found.
func validateInstalledManifest(fsys StateFS, roots Roots, m Manifest) []string {
	var reasons []string
	for _, e := range m.Entries {
		reasons = append(reasons, validateEntry(fsys, e)...)
	}
	return reasons
}

// validateUninstalledManifest checks only secrets.json and instance.json
// (spec §4.3 row 4: "state=uninstalled；secrets.json、instance.json通过
// schema、mode、owner校验"), independent of whatever the manifest's own
// Entries list happens to contain.
func validateUninstalledManifest(fsys StateFS, roots Roots, m Manifest) []string {
	var reasons []string
	reasons = append(reasons, validateDynamicFile(fsys, roots.Secrets, func(data []byte) error {
		var s SecretsFile
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		if s.Schema != dynamicFileSchema {
			return errUnknownManifestSchema
		}
		return nil
	})...)
	reasons = append(reasons, validateDynamicFile(fsys, roots.Instance, func(data []byte) error {
		var i InstanceFile
		if err := json.Unmarshal(data, &i); err != nil {
			return err
		}
		if i.Schema != dynamicFileSchema {
			return errUnknownManifestSchema
		}
		return nil
	})...)
	return reasons
}

func validateDynamicFile(fsys StateFS, path string, parse func([]byte) error) []string {
	data, err := fsys.ReadFile(path)
	if err != nil {
		return []string{path + " is missing or unreadable"}
	}
	if err := parse(data); err != nil {
		return []string{path + " failed schema validation: " + err.Error()}
	}
	return nil
}

func validateEntry(fsys StateFS, e ManifestEntry) []string {
	if e.isConflicting() {
		return []string{e.Path + " manifest entry has conflicting sha256 and schema fields"}
	}
	var reasons []string
	info, err := fsys.Stat(e.Path)
	if err != nil {
		return []string{e.Path + " is missing"}
	}
	if uint32(info.Mode().Perm()) != e.Mode {
		reasons = append(reasons, e.Path+" mode does not match manifest")
	}
	if !checkOwner(info, e.UID, e.GID) {
		reasons = append(reasons, e.Path+" owner does not match manifest")
	}

	switch {
	case e.isDir():
		if !info.IsDir() {
			reasons = append(reasons, e.Path+" is not a directory")
		}
	case e.isImmutableFile():
		sum, err := fileSHA256(fsys, e.Path)
		if err != nil {
			reasons = append(reasons, e.Path+" could not be hashed")
		} else if sum != e.SHA256 {
			reasons = append(reasons, e.Path+" content hash does not match manifest")
		}
	case e.isDynamicFile():
		if e.Schema != dynamicFileSchema {
			reasons = append(reasons, e.Path+" manifest entry has unknown schema")
		}
		// Validation is "schema解析加mode/owner校验" (spec §5.2): the
		// manifest's cached Schema number is not enough by itself, the
		// live file's own embedded schema field must still parse and be
		// recognized too, or a tampered/corrupted file with a stale
		// manifest entry would pass silently.
		reasons = append(reasons, validateDynamicFile(fsys, e.Path, func(data []byte) error {
			var s struct {
				Schema int `json:"schema"`
			}
			if err := json.Unmarshal(data, &s); err != nil {
				return err
			}
			if s.Schema != dynamicFileSchema {
				return errUnknownManifestSchema
			}
			return nil
		})...)
	}
	return reasons
}

func fileSHA256(fsys StateFS, path string) (string, error) {
	data, err := fsys.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// inconsistencyReasons builds the itemized report for StatusInconsistent,
// covering both "assets exist but manifest is missing/broken" and "manifest
// says one thing but assets say another".
func inconsistencyReasons(fsys StateFS, roots Roots, manifest Manifest, manifestErr error) []string {
	if manifestErr != nil {
		if manifestErr == errUnknownManifestSchema {
			return []string{"unknown manifest schema"}
		}
		if manifestErr == errManifestContainsSelf {
			return []string{"manifest entries include the manifest's own path"}
		}
		return []string{roots.Manifest + " is missing or unreadable"}
	}
	switch manifest.State {
	case ManifestStateInstalled:
		return validateInstalledManifest(fsys, roots, manifest)
	case ManifestStateUninstalled:
		reasons := validateUninstalledManifest(fsys, roots, manifest)
		if pathExists(fsys, roots.SystemdUnit) {
			reasons = append(reasons, roots.SystemdUnit+" exists but manifest state is uninstalled")
		}
		if pathExists(fsys, roots.ConfigDir) {
			reasons = append(reasons, roots.ConfigDir+" exists but manifest state is uninstalled")
		}
		if pathExists(fsys, roots.BinDir) {
			reasons = append(reasons, roots.BinDir+" exists but manifest state is uninstalled")
		}
		return reasons
	default:
		return []string{"manifest state is neither installed nor uninstalled"}
	}
}

// checkOwner compares os.FileInfo's platform owner to the manifest's
// recorded uid/gid. Linux and Darwin (this repo's only build targets, per
// SECURITY.md's platform scope) both expose owner via syscall.Stat_t in
// Sys(), the same mechanism internal/backup already relies on.
func checkOwner(info os.FileInfo, uid, gid int) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == uid && int(st.Gid) == gid
}
