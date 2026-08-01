package health

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"strings"
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

	dial := cfg.dialContext
	if dial == nil {
		d := &net.Dialer{}
		dial = d.DialContext
	}

	conn, err := dial(dialCtx, "tcp", addr)
	if err != nil {
		return Result{ID: c.id, Name: c.name, Status: StatusFail, Message: tcpDialErrMessage(err)}
	}
	conn.Close()
	return Result{ID: c.id, Name: c.name, Status: StatusPass, Message: "reachable"}
}

// tcpDialErrMessage returns a fixed, stable, non-sensitive failure message.
// Context cancellation and deadline errors are detected via errors.Is so that
// wrapped errors are handled correctly and no error text is forwarded.
func tcpDialErrMessage(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout or cancelled"
	}
	// All other errors (connection refused, network unreachable, etc.) get a
	// fixed message so no address, port, or OS detail is leaked.
	return "connection failed"
}
