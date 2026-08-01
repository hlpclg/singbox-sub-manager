package health

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var tokenRegex = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// readCloser is an internal seam for testing file I/O errors and closure.
type readCloser interface {
	Read(p []byte) (n int, err error)
	Close() error
}

var (
	osStat = os.Stat
	osOpen = func(name string) (readCloser, error) {
		return os.Open(name)
	}
)

// ValidToken enforces the exact regex in the design for subscription tokens.
func ValidToken(token string) bool {
	return tokenRegex.MatchString(token)
}

// ---------------------------------------------------------------------------
// Token Check
// ---------------------------------------------------------------------------

type tokenCheckImpl struct{}

func tokenCheck() Check { return tokenCheckImpl{} }

func (tokenCheckImpl) ID() string   { return "subscription.token" }
func (tokenCheckImpl) Name() string { return "subscription token" }

func (c tokenCheckImpl) Run(ctx context.Context, cfg Config) Result {
	if ctx.Err() != nil {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "cancelled"}
	}
	if cfg.TokenErr != nil {
		// Logically missing or unreadable token file.
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "invalid or missing"}
	}
	if !ValidToken(cfg.Token) {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "invalid or missing"}
	}
	return Result{ID: c.ID(), Name: c.Name(), Status: StatusPass, Message: "present"}
}

// ---------------------------------------------------------------------------
// File Checks
// ---------------------------------------------------------------------------

type fileCheck struct {
	id       string
	name     string
	filename string
}

func clashCheck() Check {
	return fileCheck{
		id:       "subscription.clash",
		name:     "clash.yaml",
		filename: "clash.yaml",
	}
}

func srCheck() Check {
	return fileCheck{
		id:       "subscription.sr",
		name:     "sr.txt",
		filename: "sr.txt",
	}
}

func (c fileCheck) ID() string   { return c.id }
func (c fileCheck) Name() string { return c.name }

// Run executes the file check.
// Local file opens and reads are synchronous stdlib operations that cannot be
// interrupted by a context. We check context.Err() before starting I/O, rather
// than inventing a timeout goroutine that would leak if blocked on a stuck mount.
func (c fileCheck) Run(ctx context.Context, cfg Config) Result {
	if ctx.Err() != nil {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "cancelled"}
	}
	if cfg.TokenErr != nil || !ValidToken(cfg.Token) {
		// Cascade failure when token is unavailable or errored out.
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "token unavailable"}
	}

	path := filepath.Join(cfg.SubscriptionRoot, cfg.Token, c.filename)
	info, err := osStat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "missing"}
		}
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "unreadable"}
	}
	if !info.Mode().IsRegular() {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "unreadable"}
	}
	if info.Size() == 0 {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "empty"}
	}

	// Must actually open and read at least one byte to verify readability.
	f, err := osOpen(path)
	if err != nil {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "unreadable"}
	}
	defer f.Close()

	var buf [1]byte
	_, err = f.Read(buf[:])
	if err != nil {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "unreadable"}
	}

	return Result{ID: c.ID(), Name: c.Name(), Status: StatusPass, Message: fmt.Sprintf("present, %s", formatSize(info.Size()))}
}

// formatSize produces compact human-readable sizes (e.g., 312 B, 8.2 KB).
func formatSize(bytes int64) string {
	const unit = 1000
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
