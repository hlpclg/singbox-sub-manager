package health

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// fakeFile implements readCloser for verifying file operations.
type fakeFile struct {
	readCloser readCloser
	readErr    error
	closeChan  chan struct{}
}

func (f *fakeFile) Read(p []byte) (n int, err error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	if f.readCloser != nil {
		return f.readCloser.Read(p)
	}
	// Simulate reading 1 byte successfully if no real file
	if len(p) > 0 {
		p[0] = 'a'
		return 1, nil
	}
	return 0, nil
}

func (f *fakeFile) Close() error {
	var err error
	if f.readCloser != nil {
		err = f.readCloser.Close()
	}
	select {
	case f.closeChan <- struct{}{}:
	default:
	}
	return err
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
			var statCalls int64
			var openCalls int64

			// Setup fakes per test
			var fakeStat func(name string) (os.FileInfo, error)
			var fakeOpen func(name string) (readCloser, error)

			cfgStat := func(name string) (os.FileInfo, error) {
				atomic.AddInt64(&statCalls, 1)
				if fakeStat != nil {
					return fakeStat(name)
				}
				return os.Stat(name)
			}
			cfgOpen := func(name string) (readCloser, error) {
				atomic.AddInt64(&openCalls, 1)
				if fakeOpen != nil {
					return fakeOpen(name)
				}
				return os.Open(name)
			}

			root := t.TempDir()
			validToken := strings.Repeat("a", 16)
			tokenDir := filepath.Join(root, validToken)
			if err := os.Mkdir(tokenDir, 0755); err != nil {
				t.Fatal(err)
			}

			targetPath := filepath.Join(tokenDir, tc.filename)
			if err := os.WriteFile(targetPath, make([]byte, tc.size), 0644); err != nil {
				t.Fatal(err)
			}

			cfg := Config{
				SubscriptionRoot: root,
				Token:            validToken,
				osStat:           cfgStat,
				osOpen:           cfgOpen,
			}

			t.Run("present", func(t *testing.T) {
				atomic.StoreInt64(&statCalls, 0)
				atomic.StoreInt64(&openCalls, 0)

				closed := make(chan struct{}, 1)
				fakeStat = func(name string) (os.FileInfo, error) {
					if name != targetPath {
						t.Errorf("Stat name = %q, want %q", name, targetPath)
					}
					return os.Stat(name)
				}
				fakeOpen = func(name string) (readCloser, error) {
					if name != targetPath {
						t.Errorf("Open name = %q, want %q", name, targetPath)
					}
					f, err := os.Open(name)
					if err != nil {
						return nil, err
					}
					return &fakeFile{readCloser: f, closeChan: closed}, nil
				}

				r := tc.check.Run(context.Background(), cfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusPass, "present, "+tc.sizeMsg)

				// Ensure Close was called on success
				select {
				case <-closed:
				default:
					t.Error("Close was not called on successful read")
				}

				if atomic.LoadInt64(&statCalls) != 1 {
					t.Errorf("statCalls = %d, want 1", atomic.LoadInt64(&statCalls))
				}
				if atomic.LoadInt64(&openCalls) != 1 {
					t.Errorf("openCalls = %d, want 1", atomic.LoadInt64(&openCalls))
				}
			})

			t.Run("token_unavailable_cascade", func(t *testing.T) {
				atomic.StoreInt64(&statCalls, 0)
				atomic.StoreInt64(&openCalls, 0)

				badCfg := Config{SubscriptionRoot: root, Token: "invalid!", osStat: cfgStat, osOpen: cfgOpen}
				r := tc.check.Run(context.Background(), badCfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusFail, "token unavailable")

				if atomic.LoadInt64(&statCalls) > 0 || atomic.LoadInt64(&openCalls) > 0 {
					t.Error("I/O functions were called despite token unavailability")
				}
			})

			t.Run("token_err_cascade", func(t *testing.T) {
				atomic.StoreInt64(&statCalls, 0)
				atomic.StoreInt64(&openCalls, 0)

				badCfg := Config{SubscriptionRoot: root, Token: validToken, TokenErr: errors.New("err"), osStat: cfgStat, osOpen: cfgOpen}
				r := tc.check.Run(context.Background(), badCfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusFail, "token unavailable")

				if atomic.LoadInt64(&statCalls) > 0 || atomic.LoadInt64(&openCalls) > 0 {
					t.Error("I/O functions were called despite TokenErr")
				}
			})

			t.Run("missing_file", func(t *testing.T) {
				atomic.StoreInt64(&statCalls, 0)
				atomic.StoreInt64(&openCalls, 0)

				fakeStat = nil
				fakeOpen = nil

				missingToken := strings.Repeat("b", 16)
				missingDir := filepath.Join(root, missingToken)
				if err := os.Mkdir(missingDir, 0755); err != nil {
					t.Fatal(err)
				}
				badCfg := Config{SubscriptionRoot: root, Token: missingToken, osStat: cfgStat, osOpen: cfgOpen}
				r := tc.check.Run(context.Background(), badCfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusFail, "missing")

				if atomic.LoadInt64(&openCalls) > 0 {
					t.Error("osOpen called when file was missing")
				}
			})

			t.Run("empty_file", func(t *testing.T) {
				atomic.StoreInt64(&statCalls, 0)
				atomic.StoreInt64(&openCalls, 0)

				emptyToken := strings.Repeat("c", 16)
				dir := filepath.Join(root, emptyToken)
				if err := os.Mkdir(dir, 0755); err != nil {
					t.Fatal(err)
				}
				emptyPath := filepath.Join(dir, tc.filename)
				if err := os.WriteFile(emptyPath, []byte{}, 0644); err != nil {
					t.Fatal(err)
				}
				badCfg := Config{SubscriptionRoot: root, Token: emptyToken, osStat: cfgStat, osOpen: cfgOpen}
				r := tc.check.Run(context.Background(), badCfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusFail, "empty")

				if atomic.LoadInt64(&openCalls) > 0 {
					t.Error("osOpen called when file was empty")
				}
			})

			t.Run("unreadable_file_dir", func(t *testing.T) {
				atomic.StoreInt64(&statCalls, 0)
				atomic.StoreInt64(&openCalls, 0)

				unreadableToken := strings.Repeat("d", 16)
				dir := filepath.Join(root, unreadableToken)
				if err := os.Mkdir(dir, 0755); err != nil {
					t.Fatal(err)
				}
				unreadablePath := filepath.Join(dir, tc.filename)
				if err := os.Mkdir(unreadablePath, 0755); err != nil {
					t.Fatal(err)
				}
				badCfg := Config{SubscriptionRoot: root, Token: unreadableToken, osStat: cfgStat, osOpen: cfgOpen}
				r := tc.check.Run(context.Background(), badCfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusFail, "unreadable")
				if strings.Contains(r.Message, unreadableToken) || strings.Contains(r.Message, root) {
					t.Errorf("Error message leaks path/token: %s", r.Message)
				}

				if atomic.LoadInt64(&openCalls) > 0 {
					t.Error("osOpen called on a directory")
				}
			})

			t.Run("open_failure", func(t *testing.T) {
				fakeStat = nil
				fakeOpen = func(name string) (readCloser, error) {
					if name != targetPath {
						t.Errorf("Open name = %q, want %q", name, targetPath)
					}
					return nil, errors.New("permission denied")
				}

				r := tc.check.Run(context.Background(), cfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusFail, "unreadable")
			})

			t.Run("read_failure", func(t *testing.T) {
				closed := make(chan struct{}, 1)
				fakeStat = nil
				fakeOpen = func(name string) (readCloser, error) {
					if name != targetPath {
						t.Errorf("Open name = %q, want %q", name, targetPath)
					}
					return &fakeFile{readErr: errors.New("i/o error"), closeChan: closed}, nil
				}

				r := tc.check.Run(context.Background(), cfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusFail, "unreadable")

				// Ensure Close was called even if Read fails
				select {
				case <-closed:
				default:
					t.Error("Close was not called on read failure")
				}
			})

			t.Run("cancelled", func(t *testing.T) {
				atomic.StoreInt64(&statCalls, 0)
				atomic.StoreInt64(&openCalls, 0)

				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				r := tc.check.Run(ctx, cfg)
				assertResult(t, r, tc.check.ID(), tc.check.Name(), StatusFail, "cancelled")

				if atomic.LoadInt64(&statCalls) > 0 || atomic.LoadInt64(&openCalls) > 0 {
					t.Error("I/O functions were called on cancelled context")
				}
			})
		})
	}
}
