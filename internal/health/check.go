package health

import (
	"context"
	"crypto/x509"
	"net"
	"os"
	"time"
)

// dialContextFunc is the function signature for context-aware TCP dials.
// Production code uses (*net.Dialer).DialContext; tests inject a fake.
type dialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Status is a single check's outcome.
type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

// Timeouts holds the pinned per-operation upper bounds.
// Never treat these as expected latencies; they are ceilings.
type Timeouts struct {
	TCPConnect time.Duration
	DNS        time.Duration
	TLS        time.Duration
	HTTP       time.Duration
	Command    time.Duration
	Overall    time.Duration
}

// DefaultTimeouts returns the values pinned by the spec.
func DefaultTimeouts() Timeouts {
	return Timeouts{
		TCPConnect: 2 * time.Second,
		DNS:        3 * time.Second,
		TLS:        5 * time.Second,
		HTTP:       8 * time.Second,
		Command:    10 * time.Second,
		Overall:    30 * time.Second,
	}
}

// CommandResult is the outcome of running an external command.
type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	NotFound bool  // binary not found in PATH
	Err      error // execution error other than a non-zero exit
}

// CommandRunner runs external commands. Production uses ExecRunner; tests
// inject a fake. Run must respond to ctx.Done() and return promptly after
// context cancellation.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) CommandResult
}

// Config carries everything the checks need. Zero-valued injectable seams
// (Runner, LookupHost, DiskFree, RootCAs) fall back to production behavior; tests set them to keep checks off the real system.
type Config struct {
	Domain           string
	SingboxConfig    string // /etc/sing-box/config.json
	CaddyConfig      string // /etc/caddy/Caddyfile
	SubscriptionRoot string // /var/www/proxy-sub
	TokenFile        string // /var/lib/singbox-sub-manager/token

	// Token is resolved once by ResolveConfig. Empty means unavailable; the
	// reason is in TokenErr. Checks that need the token cascade to FAIL when
	// it is empty.
	Token    string
	TokenErr error

	// Local probe seams. Empty strings mean use the production defaults.
	ProcNetUDP      []string       // default {/proc/net/udp, /proc/net/udp6}
	TCP443Addr      string         // default 127.0.0.1:443
	TCP80Addr       string         // default 127.0.0.1:80
	LoopbackTLSAddr string         // default 127.0.0.1:443 (HTTPS + TLS dial target)
	RootCAs         *x509.CertPool // nil = system roots

	// Injectable seams (nil = production implementation).
	Runner     CommandRunner
	LookupHost func(ctx context.Context, host string) ([]string, error)
	DiskFree   func(path string) (uint64, error)

	// dialContext, when non-nil, overrides the TCP dialer used by TCP port
	// checks. Tests (same package) inject a fake; production defaults to
	// net.Dialer{}.
	dialContext dialContextFunc

	// tlsDialContext is the instance-level TCP seam used by HTTPS and TLS
	// checks. It remains package-visible to keep the public Config surface
	// stable while allowing deterministic lifecycle tests.
	tlsDialContext dialContextFunc

	// now is an instance-level clock seam for certificate-expiry boundaries.
	now func() time.Time

	// procReader, when non-nil, overrides /proc/net/udp* file reading.
	// Tests inject in-memory content; production uses os.ReadFile.
	procReader func(path string) (string, error)

	// osStat and osOpen are unexported internal seams for fileCheck to allow
	// testing file I/O deterministically without affecting the public Config.
	osStat func(name string) (os.FileInfo, error)
	osOpen func(name string) (readCloser, error)

	Timeouts Timeouts
}

// Check is one health probe. Implementations of Run must select on ctx.Done()
// and return promptly when the context is cancelled; RunAll relies on this to
// enforce the overall timeout and avoid goroutine leaks.
type Check interface {
	ID() string
	Name() string
	Run(ctx context.Context, cfg Config) Result
}
