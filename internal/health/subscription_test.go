package health

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Helpers provided by port_test.go

func TestValidToken(t *testing.T) {
	cases := []struct {
		token string
		valid bool
	}{
		{strings.Repeat("a", 15), false},  // too short
		{strings.Repeat("a", 16), true},   // min
		{strings.Repeat("a", 128), true},  // max
		{strings.Repeat("a", 129), false}, // too long
		{"abcde12345_A-Z-_", true},        // valid chars
		{"abcde12345_A-Z-!", false},       // invalid char
		{"", false},                       // empty
	}

	for _, tc := range cases {
		t.Run(tc.token, func(t *testing.T) {
			got := ValidToken(tc.token)
			if got != tc.valid {
				t.Errorf("ValidToken(%q) = %v, want %v", tc.token, got, tc.valid)
			}
		})
	}
}

func TestTokenCheck(t *testing.T) {
	c := tokenCheck()

	t.Run("valid", func(t *testing.T) {
		cfg := Config{Token: strings.Repeat("a", 16)}
		r := c.Run(context.Background(), cfg)
		assertResult(t, r, "subscription.token", "subscription token", StatusPass, "present")
	})

	t.Run("invalid_chars", func(t *testing.T) {
		cfg := Config{Token: "abcde12345_A-Z-!"}
		r := c.Run(context.Background(), cfg)
		assertResult(t, r, "subscription.token", "subscription token", StatusFail, "invalid or missing")
	})

	t.Run("empty", func(t *testing.T) {
		cfg := Config{Token: ""}
		r := c.Run(context.Background(), cfg)
		assertResult(t, r, "subscription.token", "subscription token", StatusFail, "invalid or missing")
	})

	t.Run("token_err_with_valid_string", func(t *testing.T) {
		// Even if string is valid, if TokenErr is present it should fail
		cfg := Config{Token: strings.Repeat("a", 16), TokenErr: errors.New("read error")}
		r := c.Run(context.Background(), cfg)
		if r.Status != StatusFail {
			t.Errorf("Status = %q, want fail", r.Status)
		}
		if r.Message != "invalid or missing" {
			t.Errorf("Message = %q, want invalid or missing", r.Message)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cfg := Config{Token: strings.Repeat("a", 16)}
		r := c.Run(ctx, cfg)
		assertResult(t, r, "subscription.token", "subscription token", StatusFail, "cancelled")
	})
}

func TestFileChecksMatrix(t *testing.T) {
	checks := []struct {
		check    Check
		filename string
		sizeMsg  string
		size     int
	}{
		{clashCheck(), "clash.yaml", "8.2 KB", 8192},
		{srCheck(), "sr.txt", "312 B", 312},
	}

	for _, tc := range checks {
		t.Run(tc.filename, func(t *testing.T) {
			root := t.TempDir()
			validToken := strings.Repeat("a", 16)
			tokenDir := filepath.Join(root, validToken)
			if err := os.Mkdir(tokenDir, 0755); err != nil {
				t.Fatal(err)
			}

			// Pre-create the valid file
			targetPath := filepath.Join(tokenDir, tc.filename)
			if err := os.WriteFile(targetPath, make([]byte, tc.size), 0644); err != nil {
				t.Fatal(err)
			}

			cfg := Config{
				SubscriptionRoot: root,
				Token:            validToken,
			}

			t.Run("present", func(t *testing.T) {
				r := tc.check.Run(context.Background(), cfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusPass, "present, "+tc.sizeMsg)
			})

			t.Run("token_unavailable_cascade", func(t *testing.T) {
				badCfg := Config{SubscriptionRoot: root, Token: "invalid!"}
				r := tc.check.Run(context.Background(), badCfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusFail, "token unavailable")
			})

			t.Run("token_err_cascade", func(t *testing.T) {
				badCfg := Config{SubscriptionRoot: root, Token: validToken, TokenErr: errors.New("err")}
				r := tc.check.Run(context.Background(), badCfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusFail, "token unavailable")
			})

			t.Run("missing_file", func(t *testing.T) {
				missingToken := strings.Repeat("b", 16)
				missingDir := filepath.Join(root, missingToken)
				if err := os.Mkdir(missingDir, 0755); err != nil {
					t.Fatal(err)
				}
				badCfg := Config{SubscriptionRoot: root, Token: missingToken}
				r := tc.check.Run(context.Background(), badCfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusFail, "missing")
			})

			t.Run("empty_file", func(t *testing.T) {
				emptyToken := strings.Repeat("c", 16)
				dir := filepath.Join(root, emptyToken)
				if err := os.Mkdir(dir, 0755); err != nil {
					t.Fatal(err)
				}
				emptyPath := filepath.Join(dir, tc.filename)
				if err := os.WriteFile(emptyPath, []byte{}, 0644); err != nil {
					t.Fatal(err)
				}
				badCfg := Config{SubscriptionRoot: root, Token: emptyToken}
				r := tc.check.Run(context.Background(), badCfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusFail, "empty")
			})

			t.Run("unreadable_file_dir", func(t *testing.T) {
				// unreadable due to being a directory
				unreadableToken := strings.Repeat("d", 16)
				dir := filepath.Join(root, unreadableToken)
				if err := os.Mkdir(dir, 0755); err != nil {
					t.Fatal(err)
				}
				unreadablePath := filepath.Join(dir, tc.filename)
				if err := os.Mkdir(unreadablePath, 0755); err != nil {
					t.Fatal(err)
				}
				badCfg := Config{SubscriptionRoot: root, Token: unreadableToken}
				r := tc.check.Run(context.Background(), badCfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusFail, "unreadable")
				if strings.Contains(r.Message, unreadableToken) || strings.Contains(r.Message, root) {
					t.Errorf("Error message leaks path/token: %s", r.Message)
				}
			})

			t.Run("unreadable_file_perms", func(t *testing.T) {
				if os.Getuid() == 0 {
					t.Skip("skipping permission test as root")
				}
				// unreadable due to permissions
				unreadableToken := strings.Repeat("e", 16)
				dir := filepath.Join(root, unreadableToken)
				if err := os.Mkdir(dir, 0755); err != nil {
					t.Fatal(err)
				}
				unreadablePath := filepath.Join(dir, tc.filename)
				// Create file with 000 permissions
				if err := os.WriteFile(unreadablePath, make([]byte, 10), 0000); err != nil {
					t.Fatal(err)
				}
				badCfg := Config{SubscriptionRoot: root, Token: unreadableToken}
				r := tc.check.Run(context.Background(), badCfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusFail, "unreadable")
			})

			t.Run("cancelled", func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				r := tc.check.Run(ctx, cfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusFail, "cancelled")
			})
		})
	}
}
