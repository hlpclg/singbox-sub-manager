// Package role provides read-only detection of a host's subscription
// center role, per design spec §4.2
// (docs/superpowers/specs/2026-09-18-v0.10-reality-node-install-design.md).
// It never creates, modifies, or deletes any path.
package role

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/hlpclg/singbox-sub-manager/internal/health"
)

// Status is the three-valued result of subscription-center detection.
type Status int

const (
	StatusNo Status = iota
	StatusUncertain
	StatusYes
)

// Paths are the fixed inputs to subscription-center detection (spec §4.2).
// All fields are path roots, not read from anywhere else, so callers can
// inject alternate roots in tests without touching the real filesystem.
type Paths struct {
	// InstallJSON is the fixed path to install.json.
	InstallJSON string
	// DefaultTokenFile is the production default token path (used only for
	// the "否" all-three-absent check; the "是" check instead reads the
	// token_file field out of install.json itself).
	DefaultTokenFile string
	// DefaultNodesConf is the fixed default nodes.conf path.
	DefaultNodesConf string
}

// ProdPaths returns the fixed production paths from spec §4.2.
func ProdPaths() Paths {
	defs := health.ProdDefaults()
	return Paths{
		InstallJSON:      defs.InstallJSON,
		DefaultTokenFile: defs.TokenFile,
		// Fixed by spec; must match cmd/proxyctl's defaultNodesPath.
		DefaultNodesConf: "/etc/singbox-sub-manager/nodes.conf",
	}
}

// FS is an injectable filesystem seam so tests never touch the real
// filesystem. OSFS returns the real implementation.
type FS struct {
	ReadFile func(string) ([]byte, error)
	Stat     func(string) (os.FileInfo, error)
}

// OSFS returns an FS backed by the real filesystem.
func OSFS() FS {
	return FS{ReadFile: os.ReadFile, Stat: os.Stat}
}

// installState mirrors health's install.json schema (kept independent so
// this package's only dependency on health is ValidToken and ProdDefaults).
type installState struct {
	Domain           string `json:"domain"`
	SubscriptionRoot string `json:"subscription_root"`
	TokenFile        string `json:"token_file"`
}

// Result is the outcome of subscription-center detection.
type Result struct {
	Status Status
	// Reasons lists every missing or invalid item, populated only when
	// Status is StatusUncertain.
	Reasons []string
}

func notExist(fsys FS, path string) bool {
	_, err := fsys.Stat(path)
	return err != nil && os.IsNotExist(err)
}

// DetectSubscriptionCenter implements the spec §4.2 table exactly:
//
//   - "是": install.json parses; domain non-empty; subscription_root is an
//     absolute path to an existing directory; token_file is readable and,
//     trimmed, passes health.ValidToken; the default nodes.conf exists and
//     is a regular file.
//   - "否": install.json, the default token file, and the default
//     nodes.conf all do not exist.
//   - "不确定": everything else, with every missing or invalid item listed.
func DetectSubscriptionCenter(fsys FS, paths Paths) Result {
	if notExist(fsys, paths.InstallJSON) && notExist(fsys, paths.DefaultTokenFile) && notExist(fsys, paths.DefaultNodesConf) {
		return Result{Status: StatusNo}
	}

	var reasons []string

	data, err := fsys.ReadFile(paths.InstallJSON)
	if err != nil {
		reasons = append(reasons, "install.json missing or unreadable")
	} else {
		var st installState
		if jsonErr := json.Unmarshal(data, &st); jsonErr != nil {
			reasons = append(reasons, "install.json unparseable")
		} else {
			if st.Domain == "" {
				reasons = append(reasons, "domain is empty")
			}
			if !filepath.IsAbs(st.SubscriptionRoot) {
				reasons = append(reasons, "subscription_root is not an absolute path")
			} else if info, statErr := fsys.Stat(st.SubscriptionRoot); statErr != nil || !info.IsDir() {
				reasons = append(reasons, "subscription_root does not exist or is not a directory")
			}
			if st.TokenFile == "" {
				reasons = append(reasons, "token_file is empty")
			} else if tokenData, tokErr := fsys.ReadFile(st.TokenFile); tokErr != nil {
				reasons = append(reasons, "token_file is unreadable")
			} else if !health.ValidToken(strings.TrimSpace(string(tokenData))) {
				reasons = append(reasons, "token_file content is not a valid token")
			}
		}
	}

	if info, statErr := fsys.Stat(paths.DefaultNodesConf); statErr != nil {
		reasons = append(reasons, "default nodes.conf does not exist")
	} else if !info.Mode().IsRegular() {
		reasons = append(reasons, "default nodes.conf is not a regular file")
	}

	if len(reasons) == 0 {
		return Result{Status: StatusYes}
	}
	return Result{Status: StatusUncertain, Reasons: reasons}
}
