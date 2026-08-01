package health

import (
	"context"
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

	t.Run("token_err", func(t *testing.T) {
		cfg := Config{TokenErr: os.ErrNotExist}
		r := c.Run(context.Background(), cfg)
		// Should fail, and not leak token.
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

func TestFileChecks(t *testing.T) {
	root := t.TempDir()
	validToken := strings.Repeat("a", 16)
	tokenDir := filepath.Join(root, validToken)
	if err := os.Mkdir(tokenDir, 0755); err != nil {
		t.Fatal(err)
	}

	clashPath := filepath.Join(tokenDir, "clash.yaml")
	if err := os.WriteFile(clashPath, make([]byte, 8192), 0644); err != nil {
		t.Fatal(err)
	}

	srPath := filepath.Join(tokenDir, "sr.txt")
	if err := os.WriteFile(srPath, make([]byte, 312), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		SubscriptionRoot: root,
		Token:            validToken,
	}

	t.Run("clash_present", func(t *testing.T) {
		c := clashCheck()
		r := c.Run(context.Background(), cfg)
		assertResult(t, r, "subscription.clash", "clash.yaml", StatusPass, "present, 8.2 KB")
	})

	t.Run("sr_present", func(t *testing.T) {
		c := srCheck()
		r := c.Run(context.Background(), cfg)
		assertResult(t, r, "subscription.sr", "sr.txt", StatusPass, "present, 312 B")
	})

	t.Run("token_unavailable_cascade", func(t *testing.T) {
		badCfg := Config{SubscriptionRoot: root, Token: "invalid!"}
		c := clashCheck()
		r := c.Run(context.Background(), badCfg)
		assertResult(t, r, "subscription.clash", "clash.yaml", StatusFail, "token unavailable")
	})

	t.Run("missing_file", func(t *testing.T) {
		emptyDir := filepath.Join(root, "empty")
		if err := os.Mkdir(emptyDir, 0755); err != nil {
			t.Fatal(err)
		}
		badCfg := Config{SubscriptionRoot: root, Token: strings.Repeat("b", 16)}
		c := clashCheck()
		r := c.Run(context.Background(), badCfg)
		assertResult(t, r, "subscription.clash", "clash.yaml", StatusFail, "missing")
	})

	t.Run("empty_file", func(t *testing.T) {
		emptyToken := strings.Repeat("c", 16)
		dir := filepath.Join(root, emptyToken)
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
		clashPath := filepath.Join(dir, "clash.yaml")
		if err := os.WriteFile(clashPath, []byte{}, 0644); err != nil {
			t.Fatal(err)
		}
		badCfg := Config{SubscriptionRoot: root, Token: emptyToken}
		c := clashCheck()
		r := c.Run(context.Background(), badCfg)
		assertResult(t, r, "subscription.clash", "clash.yaml", StatusFail, "empty")
	})

	t.Run("unreadable_file", func(t *testing.T) {
		unreadableToken := strings.Repeat("d", 16)
		dir := filepath.Join(root, unreadableToken)
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
		// Make a directory where a file should be, causing IsRegular() to fail,
		// simulating an unreadable/invalid file type.
		clashPath := filepath.Join(dir, "clash.yaml")
		if err := os.Mkdir(clashPath, 0755); err != nil {
			t.Fatal(err)
		}
		badCfg := Config{SubscriptionRoot: root, Token: unreadableToken}
		c := clashCheck()
		r := c.Run(context.Background(), badCfg)
		assertResult(t, r, "subscription.clash", "clash.yaml", StatusFail, "unreadable")
		if strings.Contains(r.Message, unreadableToken) || strings.Contains(r.Message, root) {
			t.Errorf("Error message leaks path/token: %s", r.Message)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c := clashCheck()
		r := c.Run(ctx, cfg)
		assertResult(t, r, "subscription.clash", "clash.yaml", StatusFail, "cancelled")
	})
}
