package health

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// UDP 443 listener check
// ---------------------------------------------------------------------------

// udp443CheckImpl checks that a local UDP socket is listening on port 443 by
// parsing /proc/net/udp and /proc/net/udp6 (or injected sources in tests).
//
// This check reports only that a local UDP socket uses port 443. It does NOT
// claim Hysteria2 or remote-node health.
//
// Note: Go stdlib local file reads of /proc pseudo-files are not cancellable.
// We treat these as bounded reads (the kernel copies a small fixed table) and
// check context before each source, not inside the read itself.
type udp443CheckImpl struct{}

func udp443Check() Check { return udp443CheckImpl{} }

func (udp443CheckImpl) ID() string   { return "port.udp443" }
func (udp443CheckImpl) Name() string { return "UDP 443" }

func (c udp443CheckImpl) Run(ctx context.Context, cfg Config) Result {
	// Check context before starting any reads.
	if ctx.Err() != nil {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "cancelled"}
	}

	sources := cfg.ProcNetUDP
	if len(sources) == 0 {
		sources = []string{"/proc/net/udp", "/proc/net/udp6"}
	}

	reader := cfg.procReader
	if reader == nil {
		reader = func(path string) (string, error) {
			b, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			return string(b), nil
		}
	}

	allUnreadable := true
	for _, src := range sources {
		// Check context between source reads (per spec).
		if ctx.Err() != nil {
			return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "cancelled"}
		}
		content, err := reader(src)
		if err != nil {
			// Missing first source must not prevent checking the next one.
			continue
		}
		allUnreadable = false
		if parseUDPPort443(content) {
			return Result{ID: c.ID(), Name: c.Name(), Status: StatusPass, Message: "listening"}
		}
	}

	if allUnreadable {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "proc source unavailable"}
	}
	return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "not listening"}
}

// parseUDPPort443 returns true if any row in the /proc/net/udp[6] content has
// a local-address column whose port field equals 443 (hex "01BB").
//
// Format: "sl  local_address rem_address …"
//   - local_address is "IIIIIIII:PPPP" (IPv4) or "IIII…:PPPP" (IPv6); port
//     is the part after the colon in column 1, exactly 4 uppercase hex digits.
//
// Only the local-address column (column index 1) is inspected. Values like
// "101BB" or "01B" are rejected because they are not exactly 4 hex digits.
func parseUDPPort443(content string) bool {
	const target = "01BB" // 443 decimal
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		localAddr := fields[1]
		colon := strings.LastIndex(localAddr, ":")
		if colon < 0 {
			continue
		}
		portHex := localAddr[colon+1:]
		if strings.EqualFold(portHex, target) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// TCP 443 and TCP 80 reachability checks
// ---------------------------------------------------------------------------

// tcpPortCheck dials the configured loopback TCP address and reports reachable
// on success. It uses a context bounded by cfg.Timeouts.TCPConnect.
type tcpPortCheck struct {
	id, name    string
	defaultAddr string
	addrFrom    func(cfg Config) string
}

func tcp443Check() Check {
	return tcpPortCheck{
		id:          "port.tcp443",
		name:        "TCP 443",
		defaultAddr: "127.0.0.1:443",
		addrFrom:    func(cfg Config) string { return cfg.TCP443Addr },
	}
}

func tcp80Check() Check {
	return tcpPortCheck{
		id:          "port.tcp80",
		name:        "TCP 80",
		defaultAddr: "127.0.0.1:80",
		addrFrom:    func(cfg Config) string { return cfg.TCP80Addr },
	}
}

func (c tcpPortCheck) ID() string   { return c.id }
func (c tcpPortCheck) Name() string { return c.name }

func (c tcpPortCheck) Run(ctx context.Context, cfg Config) Result {
	// Honour pre-cancelled context without dialling.
	if ctx.Err() != nil {
		return Result{ID: c.ID(), Name: c.Name(), Status: StatusFail, Message: "cancelled"}
	}

	addr := c.addrFrom(cfg)
	if addr == "" {
		addr = c.defaultAddr
	}

	timeout := cfg.Timeouts.TCPConnect
	if timeout <= 0 {
		timeout = DefaultTimeouts().TCPConnect
	}

	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dial := cfg.DialContext
	if dial == nil {
		d := &net.Dialer{Timeout: timeout}
		dial = d.DialContext
	}

	conn, err := dial(dialCtx, "tcp", addr)
	if err != nil {
		return Result{ID: c.id, Name: c.name, Status: StatusFail, Message: dialErrMessage(err)}
	}
	conn.Close()
	return Result{ID: c.id, Name: c.name, Status: StatusPass, Message: "reachable"}
}

// dialErrMessage returns a stable, non-sensitive failure message for TCP dial
// errors. It deliberately does not include the address or underlying OS detail
// so callers cannot extract configuration from error text.
func dialErrMessage(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case isContextErr(err):
		return "timeout or cancelled"
	default:
		return fmt.Sprintf("connection failed: %s", sanitiseDialErr(err))
	}
}

// isContextErr reports whether err signals context cancellation or deadline.
func isContextErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "context") ||
		strings.Contains(msg, "deadline") ||
		strings.Contains(msg, "canceled") ||
		strings.Contains(msg, "cancelled")
}

// sanitiseDialErr strips the address portion from net.OpError messages to
// avoid leaking configuration in error output.
func sanitiseDialErr(err error) string {
	if ne, ok := err.(*net.OpError); ok {
		return ne.Op + " error"
	}
	// For context errors and other cases return a generic string.
	s := err.Error()
	if idx := strings.Index(s, ":"); idx > 0 {
		// Trim after first colon to avoid including addresses.
		s = strings.TrimSpace(s[:idx])
	}
	return s
}

// ---------------------------------------------------------------------------
// Ensure unused import (time) is used; used implicitly via net.Dialer.Timeout.
// ---------------------------------------------------------------------------
var _ = time.Second // suppress "imported and not used" if compiler opts differ
