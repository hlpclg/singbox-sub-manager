package health

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// fakeFS builds a readFile function backed by an in-memory map.
func fakeFS(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if content, ok := files[path]; ok {
			return []byte(content), nil
		}
		return nil, os.ErrNotExist
	}
}

// ---------------------------------------------------------------------------
// ProdDefaults
// ---------------------------------------------------------------------------

func TestProdDefaults(t *testing.T) {
	d := ProdDefaults()
	if d.SingboxConfig != "/etc/sing-box/config.json" {
		t.Errorf("SingboxConfig = %q", d.SingboxConfig)
	}
	if d.CaddyConfig != "/etc/caddy/Caddyfile" {
		t.Errorf("CaddyConfig = %q", d.CaddyConfig)
	}
	if d.SubscriptionRoot != "/var/www/proxy-sub" {
		t.Errorf("SubscriptionRoot = %q", d.SubscriptionRoot)
	}
	if d.TokenFile != "/var/lib/singbox-sub-manager/token" {
		t.Errorf("TokenFile = %q", d.TokenFile)
	}
	if d.LegacyTokenFile != "/etc/proxy-sub-token" {
		t.Errorf("LegacyTokenFile = %q", d.LegacyTokenFile)
	}
	if d.InstallJSON != "/etc/singbox-sub-manager/install.json" {
		t.Errorf("InstallJSON = %q", d.InstallJSON)
	}
}

// ---------------------------------------------------------------------------
// ResolveConfig — domain precedence
// ---------------------------------------------------------------------------

func TestResolveConfig_FlagDomainHighestPriority(t *testing.T) {
	fs := fakeFS(map[string]string{
		"/etc/singbox-sub-manager/install.json": `{"domain":"from-state.example.com"}`,
		"/etc/caddy/Caddyfile":                  "from-caddy.example.com {\n}\n",
		"/var/lib/singbox-sub-manager/token":    "abcdefghijklmnop",
	})
	cfg := ResolveConfig("flag.example.com", fs)
	if cfg.Domain != "flag.example.com" {
		t.Fatalf("Domain = %q, want flag.example.com", cfg.Domain)
	}
}

func TestResolveConfig_InstallJSONDomain(t *testing.T) {
	fs := fakeFS(map[string]string{
		"/etc/singbox-sub-manager/install.json": `{"domain":"state.example.com"}`,
		"/etc/caddy/Caddyfile":                  "caddy.example.com {\n}\n",
		"/var/lib/singbox-sub-manager/token":    "abcdefghijklmnop",
	})
	cfg := ResolveConfig("", fs)
	if cfg.Domain != "state.example.com" {
		t.Fatalf("Domain = %q, want state.example.com", cfg.Domain)
	}
}

func TestResolveConfig_CaddyfileFallback(t *testing.T) {
	fs := fakeFS(map[string]string{
		// No install.json.
		"/etc/caddy/Caddyfile":               "sub.example.com {\n  root * /var/www/proxy-sub\n  file_server\n}\n",
		"/var/lib/singbox-sub-manager/token": "abcdefghijklmnop",
	})
	cfg := ResolveConfig("", fs)
	if cfg.Domain != "sub.example.com" {
		t.Fatalf("Domain = %q, want sub.example.com", cfg.Domain)
	}
}

func TestResolveConfig_NoDomainAnywhere(t *testing.T) {
	fs := fakeFS(map[string]string{
		"/var/lib/singbox-sub-manager/token": "abcdefghijklmnop",
	})
	cfg := ResolveConfig("", fs)
	if cfg.Domain != "" {
		t.Fatalf("Domain = %q, want empty", cfg.Domain)
	}
}

// ---------------------------------------------------------------------------
// ResolveConfig — path precedence from install.json
// ---------------------------------------------------------------------------

func TestResolveConfig_PathsFromInstallJSON(t *testing.T) {
	fs := fakeFS(map[string]string{
		"/etc/singbox-sub-manager/install.json": `{
			"domain": "x.com",
			"singbox_config": "/custom/singbox.json",
			"caddy_config": "/custom/Caddyfile",
			"subscription_root": "/custom/sub",
			"token_file": "/custom/token"
		}`,
		"/custom/token": "validtoken1234567",
	})
	cfg := ResolveConfig("", fs)
	if cfg.SingboxConfig != "/custom/singbox.json" {
		t.Errorf("SingboxConfig = %q", cfg.SingboxConfig)
	}
	if cfg.CaddyConfig != "/custom/Caddyfile" {
		t.Errorf("CaddyConfig = %q", cfg.CaddyConfig)
	}
	if cfg.SubscriptionRoot != "/custom/sub" {
		t.Errorf("SubscriptionRoot = %q", cfg.SubscriptionRoot)
	}
	if cfg.TokenFile != "/custom/token" {
		t.Errorf("TokenFile = %q", cfg.TokenFile)
	}
}

func TestResolveConfig_PartialInstallJSON(t *testing.T) {
	// Only domain set; paths should fall back to defaults.
	fs := fakeFS(map[string]string{
		"/etc/singbox-sub-manager/install.json": `{"domain":"partial.example.com"}`,
		"/var/lib/singbox-sub-manager/token":    "abcdefghijklmnop",
	})
	cfg := ResolveConfig("", fs)
	d := ProdDefaults()
	if cfg.SingboxConfig != d.SingboxConfig {
		t.Errorf("SingboxConfig = %q, want default %q", cfg.SingboxConfig, d.SingboxConfig)
	}
	if cfg.CaddyConfig != d.CaddyConfig {
		t.Errorf("CaddyConfig = %q, want default %q", cfg.CaddyConfig, d.CaddyConfig)
	}
	if cfg.SubscriptionRoot != d.SubscriptionRoot {
		t.Errorf("SubscriptionRoot = %q, want default %q", cfg.SubscriptionRoot, d.SubscriptionRoot)
	}
}

func TestResolveConfig_MalformedInstallJSON(t *testing.T) {
	// Malformed JSON should fall back gracefully, not panic.
	fs := fakeFS(map[string]string{
		"/etc/singbox-sub-manager/install.json": `{broken json`,
		"/etc/caddy/Caddyfile":                  "fallback.example.com {\n}\n",
		"/var/lib/singbox-sub-manager/token":    "abcdefghijklmnop",
	})
	cfg := ResolveConfig("", fs)
	// Falls through to Caddyfile.
	if cfg.Domain != "fallback.example.com" {
		t.Fatalf("Domain = %q, want fallback.example.com", cfg.Domain)
	}
	// Paths should be production defaults.
	d := ProdDefaults()
	if cfg.SingboxConfig != d.SingboxConfig {
		t.Errorf("SingboxConfig = %q, want default", cfg.SingboxConfig)
	}
}

func TestResolveConfig_MissingInstallJSON(t *testing.T) {
	fs := fakeFS(map[string]string{
		"/etc/caddy/Caddyfile":               "fallback2.example.com {\n}\n",
		"/var/lib/singbox-sub-manager/token": "abcdefghijklmnop",
	})
	cfg := ResolveConfig("", fs)
	if cfg.Domain != "fallback2.example.com" {
		t.Fatalf("Domain = %q, want fallback2.example.com", cfg.Domain)
	}
}

// ---------------------------------------------------------------------------
// ResolveConfig — token resolution
// ---------------------------------------------------------------------------

func TestResolveConfig_ValidToken(t *testing.T) {
	fs := fakeFS(map[string]string{
		"/var/lib/singbox-sub-manager/token": "abcdefghijklmnop",
	})
	cfg := ResolveConfig("", fs)
	if cfg.Token != "abcdefghijklmnop" {
		t.Errorf("Token = %q", cfg.Token)
	}
	if cfg.TokenErr != nil {
		t.Errorf("TokenErr = %v", cfg.TokenErr)
	}
}

func TestResolveConfig_TokenWithWhitespace(t *testing.T) {
	fs := fakeFS(map[string]string{
		"/var/lib/singbox-sub-manager/token": "  abcdefghijklmnop  \n",
	})
	cfg := ResolveConfig("", fs)
	if cfg.Token != "abcdefghijklmnop" {
		t.Errorf("Token = %q, want trimmed", cfg.Token)
	}
}

func TestResolveConfig_LegacyTokenFallback(t *testing.T) {
	fs := fakeFS(map[string]string{
		// Primary missing, legacy present.
		"/etc/proxy-sub-token": "legacytoken12345678",
	})
	cfg := ResolveConfig("", fs)
	if cfg.Token != "legacytoken12345678" {
		t.Errorf("Token = %q, want legacytoken12345678", cfg.Token)
	}
	if cfg.TokenErr != nil {
		t.Errorf("TokenErr = %v", cfg.TokenErr)
	}
}

func TestResolveConfig_InvalidTokenPrimaryValidLegacy(t *testing.T) {
	fs := fakeFS(map[string]string{
		"/var/lib/singbox-sub-manager/token": "short", // too short
		"/etc/proxy-sub-token":               "validlegacytoken1234",
	})
	cfg := ResolveConfig("", fs)
	if cfg.Token != "validlegacytoken1234" {
		t.Errorf("Token = %q, want validlegacytoken1234", cfg.Token)
	}
}

func TestResolveConfig_BothTokensMissing(t *testing.T) {
	fs := fakeFS(map[string]string{})
	cfg := ResolveConfig("", fs)
	if cfg.Token != "" {
		t.Errorf("Token = %q, want empty", cfg.Token)
	}
	if cfg.TokenErr == nil {
		t.Fatal("TokenErr should be non-nil when both token files are missing")
	}
}

func TestResolveConfig_InvalidTokenBothPaths(t *testing.T) {
	fs := fakeFS(map[string]string{
		"/var/lib/singbox-sub-manager/token": "x", // too short
		"/etc/proxy-sub-token":               "y", // too short
	})
	cfg := ResolveConfig("", fs)
	if cfg.Token != "" {
		t.Errorf("Token = %q, want empty", cfg.Token)
	}
	if cfg.TokenErr == nil {
		t.Fatal("TokenErr should be non-nil for invalid tokens")
	}
}

func TestResolveConfig_TokenFromCustomPath(t *testing.T) {
	fs := fakeFS(map[string]string{
		"/etc/singbox-sub-manager/install.json": `{"token_file":"/custom/token"}`,
		"/custom/token":                         "customtoken1234567890",
	})
	cfg := ResolveConfig("", fs)
	if cfg.Token != "customtoken1234567890" {
		t.Errorf("Token = %q, want customtoken1234567890", cfg.Token)
	}
}

// ---------------------------------------------------------------------------
// ResolveConfig — secret redaction
// ---------------------------------------------------------------------------

func TestResolveConfig_TokenNotInErrors(t *testing.T) {
	// Even when token files exist but are invalid, error messages must not
	// contain token contents or file paths.
	fs := fakeFS(map[string]string{
		"/var/lib/singbox-sub-manager/token": "secretvalue12345678",
		"/etc/proxy-sub-token":               "anothersecret123456",
	})
	// Make both invalid (they are actually valid, so let's use short ones).
	fs2 := fakeFS(map[string]string{
		"/var/lib/singbox-sub-manager/token": "bad",
		"/etc/proxy-sub-token":               "bad",
	})
	cfg := ResolveConfig("", fs2)
	if cfg.TokenErr == nil {
		t.Fatal("expected TokenErr")
	}
	errMsg := cfg.TokenErr.Error()
	if strings.Contains(errMsg, "bad") {
		t.Errorf("error contains token value: %q", errMsg)
	}
	if strings.Contains(errMsg, "/var/lib") {
		t.Errorf("error contains token path: %q", errMsg)
	}
	if strings.Contains(errMsg, "/etc/proxy-sub-token") {
		t.Errorf("error contains legacy path: %q", errMsg)
	}

	// Also check when a valid token is resolved — no leakage in messages.
	cfg2 := ResolveConfig("", fs)
	if cfg2.TokenErr != nil {
		t.Errorf("unexpected TokenErr: %v", cfg2.TokenErr)
	}
	_ = cfg2 // success path, no error to check
}

// ---------------------------------------------------------------------------
// ResolveConfig — timeouts
// ---------------------------------------------------------------------------

func TestResolveConfig_DefaultTimeouts(t *testing.T) {
	fs := fakeFS(map[string]string{})
	cfg := ResolveConfig("", fs)
	expected := DefaultTimeouts()
	if cfg.Timeouts != expected {
		t.Errorf("Timeouts = %+v, want %+v", cfg.Timeouts, expected)
	}
}

// ---------------------------------------------------------------------------
// parseCaddyfileDomain
// ---------------------------------------------------------------------------

func TestParseCaddyfileDomain(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "simple site block",
			content: "sub.example.com {\n  root * /var/www\n  file_server\n}\n",
			want:    "sub.example.com",
		},
		{
			name: "global options then site",
			content: `{
  email admin@example.com
  http_port 80
}
sub.example.com {
  root * /var/www
  file_server
}
`,
			want: "sub.example.com",
		},
		{
			name: "comments before site",
			content: `# This is a comment
# Another comment
sub.example.com {
  file_server
}
`,
			want: "sub.example.com",
		},
		{
			name: "multiple site blocks",
			content: `first.example.com {
  file_server
}
second.example.com {
  file_server
}
`,
			want: "first.example.com",
		},
		{
			name:    "scheme rejected",
			content: "https://sub.example.com {\n}\n",
			want:    "",
		},
		{
			name:    "http scheme rejected",
			content: "http://sub.example.com {\n}\n",
			want:    "",
		},
		{
			name:    "path rejected",
			content: "sub.example.com/path {\n}\n",
			want:    "",
		},
		{
			name:    "wildcard rejected",
			content: "*.example.com {\n}\n",
			want:    "",
		},
		{
			name:    "port rejected",
			content: "sub.example.com:8080 {\n}\n",
			want:    "",
		},
		{
			name:    "bare port rejected",
			content: ":8080 {\n}\n",
			want:    "",
		},
		{
			name:    "directive not a domain",
			content: "respond {\n}\n",
			want:    "",
		},
		{
			name:    "empty content",
			content: "",
			want:    "",
		},
		{
			name:    "only global block",
			content: "{\n  email admin@x.com\n}\n",
			want:    "",
		},
		{
			name: "nested blocks in global",
			content: `{
  servers {
    protocols h1 h2
  }
}
sub.example.com {
  file_server
}
`,
			want: "sub.example.com",
		},
		{
			name: "real Caddyfile from install script",
			content: `{
  email admin@example.com
  http_port 80
  servers {
    protocols h1 h2
  }
}
sub.example.com {
  root * /var/www/proxy-sub
  file_server
}
`,
			want: "sub.example.com",
		},
		{
			name: "scheme site then bare site",
			content: `https://bad.example.com {
  file_server
}
good.example.com {
  file_server
}
`,
			want: "good.example.com",
		},
		{
			name:    "only comments",
			content: "# just comments\n# nothing else\n",
			want:    "",
		},
		{
			name:    "spaces in label rejected",
			content: "a.com b.com {\n}\n",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCaddyfileDomain(tt.content)
			if got != tt.want {
				t.Errorf("parseCaddyfileDomain() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// validateSiteLabel
// ---------------------------------------------------------------------------

func TestValidateSiteLabel(t *testing.T) {
	tests := []struct {
		label string
		want  string
	}{
		{"sub.example.com", "sub.example.com"},
		{"a.b", "a.b"},
		{"", ""},
		{"https://x.com", ""},
		{"x.com/path", ""},
		{"*.x.com", ""},
		{"x.com:443", ""},
		{":443", ""},
		{"respond", ""},     // no dot → directive
		{"localhost", ""},   // no dot
		{"a.com b.com", ""}, // space
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.label), func(t *testing.T) {
			got := validateSiteLabel(tt.label)
			if got != tt.want {
				t.Errorf("validateSiteLabel(%q) = %q, want %q", tt.label, got, tt.want)
			}
		})
	}
}
