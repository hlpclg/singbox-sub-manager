package health

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ---------------------------------------------------------------------------
// Production defaults (must match the design spec exactly)
// ---------------------------------------------------------------------------

// Defaults holds the production file paths used when install.json is absent
// or incomplete.
type Defaults struct {
	SingboxConfig    string
	CaddyConfig      string
	SubscriptionRoot string
	TokenFile        string
	LegacyTokenFile  string
	InstallJSON      string
}

// ProdDefaults returns the verified production paths from the design spec.
func ProdDefaults() Defaults {
	return Defaults{
		SingboxConfig:    "/etc/sing-box/config.json",
		CaddyConfig:      "/etc/caddy/Caddyfile",
		SubscriptionRoot: "/var/www/proxy-sub",
		TokenFile:        "/var/lib/singbox-sub-manager/token",
		LegacyTokenFile:  "/etc/proxy-sub-token",
		InstallJSON:      "/etc/singbox-sub-manager/install.json",
	}
}

// ---------------------------------------------------------------------------
// install.json schema
// ---------------------------------------------------------------------------

type installState struct {
	Domain           string `json:"domain"`
	SingboxConfig    string `json:"singbox_config"`
	CaddyConfig      string `json:"caddy_config"`
	SubscriptionRoot string `json:"subscription_root"`
	TokenFile        string `json:"token_file"`
}

// ---------------------------------------------------------------------------
// ResolveConfig
// ---------------------------------------------------------------------------

// ResolveConfig builds a Config by resolving domain, paths, and token from
// the flag value, install.json, Caddyfile fallback, and production defaults.
//
// Domain precedence: flagDomain > install.json > Caddyfile first site address.
// Path precedence: install.json (when present and non-empty) > ProdDefaults.
// Token: primary path > legacy path; validated before storing.
//
// readFile is an injectable seam (nil = os.ReadFile). It must never appear in
// error messages when reading token files.
func ResolveConfig(flagDomain string, readFile func(string) ([]byte, error)) Config {
	if readFile == nil {
		readFile = os.ReadFile
	}

	defs := ProdDefaults()

	cfg := Config{
		SingboxConfig:    defs.SingboxConfig,
		CaddyConfig:      defs.CaddyConfig,
		SubscriptionRoot: defs.SubscriptionRoot,
		TokenFile:        defs.TokenFile,
		Timeouts:         DefaultTimeouts(),
	}

	// --- install.json ---
	var state installState
	stateLoaded := false
	if data, err := readFile(defs.InstallJSON); err == nil {
		if json.Unmarshal(data, &state) == nil {
			stateLoaded = true
			if state.SingboxConfig != "" {
				cfg.SingboxConfig = state.SingboxConfig
			}
			if state.CaddyConfig != "" {
				cfg.CaddyConfig = state.CaddyConfig
			}
			if state.SubscriptionRoot != "" {
				cfg.SubscriptionRoot = state.SubscriptionRoot
			}
			if state.TokenFile != "" {
				cfg.TokenFile = state.TokenFile
			}
		}
	}

	// --- Domain resolution ---
	if flagDomain != "" {
		cfg.Domain = flagDomain
	} else if stateLoaded && state.Domain != "" {
		cfg.Domain = state.Domain
	} else {
		// Fallback: parse Caddyfile for the first valid site address.
		if data, err := readFile(cfg.CaddyConfig); err == nil {
			if domain := parseCaddyfileDomain(string(data)); domain != "" {
				cfg.Domain = domain
			}
		}
	}

	// --- Token resolution ---
	cfg.Token, cfg.TokenErr = resolveToken(cfg.TokenFile, defs.LegacyTokenFile, readFile)

	return cfg
}

// resolveToken reads and validates the token from the primary path, falling
// back to the legacy path. Error messages never include token contents or
// token-bearing paths.
func resolveToken(primary, legacy string, readFile func(string) ([]byte, error)) (string, error) {
	token, err := readTokenFile(primary, readFile)
	if err == nil && ValidToken(token) {
		return token, nil
	}

	// Try legacy path.
	token, err = readTokenFile(legacy, readFile)
	if err == nil && ValidToken(token) {
		return token, nil
	}

	if err != nil {
		return "", fmt.Errorf("token file unavailable")
	}
	return "", fmt.Errorf("token format invalid")
}

// readTokenFile reads and trims the token from a file. Error messages are
// generic to avoid leaking paths.
func readTokenFile(path string, readFile func(string) ([]byte, error)) (string, error) {
	data, err := readFile(path)
	if err != nil {
		return "", fmt.Errorf("token file unavailable")
	}
	return strings.TrimSpace(string(data)), nil
}

// ---------------------------------------------------------------------------
// Caddyfile domain parser
// ---------------------------------------------------------------------------

// parseCaddyfileDomain extracts the first valid site address (bare domain)
// from a Caddyfile. It rejects:
//   - schemes (http://, https://)
//   - paths (/anything)
//   - wildcards (*.example.com)
//   - ports (:8080, domain:8080)
//   - directives (lines inside a block or known Caddy directives)
//   - comments (# ...)
//   - global option blocks ({ at the top level without a site label)
//   - malformed site labels
func parseCaddyfileDomain(content string) string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	depth := 0
	// inGlobalBlock tracks the global options block { ... } at the top.
	inGlobalBlock := false
	seenFirstBlock := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Handle closing braces.
		if line == "}" {
			if depth > 0 {
				depth--
			}
			if inGlobalBlock && depth == 0 {
				inGlobalBlock = false
			}
			continue
		}

		// If we are inside any block, skip content lines and track nested
		// braces.
		if depth > 0 {
			if strings.HasSuffix(line, "{") {
				depth++
			}
			continue
		}

		// At depth 0: look for site labels.
		// A bare "{" at the start (before any site block) is the global
		// options block.
		if line == "{" {
			if !seenFirstBlock {
				inGlobalBlock = true
			}
			depth++
			seenFirstBlock = true
			continue
		}

		// Lines like "sub.example.com {" — extract the site label.
		label := line
		if strings.HasSuffix(line, "{") {
			label = strings.TrimSpace(strings.TrimSuffix(line, "{"))
		}

		if domain := validateSiteLabel(label); domain != "" {
			return domain
		}
		seenFirstBlock = true
	}

	return ""
}

// validateSiteLabel checks whether a Caddyfile site label is a bare domain
// suitable for health checks. Returns the domain or empty string.
func validateSiteLabel(label string) string {
	if label == "" {
		return ""
	}

	// Reject schemes.
	if strings.Contains(label, "://") {
		return ""
	}

	// Reject paths.
	if strings.Contains(label, "/") {
		return ""
	}

	// Reject wildcards.
	if strings.Contains(label, "*") {
		return ""
	}

	// Reject ports (bare :port or domain:port).
	if strings.Contains(label, ":") {
		return ""
	}

	// Reject if it looks like a Caddy directive (no dots, e.g. "respond").
	if !strings.Contains(label, ".") {
		return ""
	}

	// Reject labels with spaces (multiple site addresses on one line — pick
	// only clean single-domain labels).
	if strings.Contains(label, " ") {
		return ""
	}

	return label
}
