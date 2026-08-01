package health

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// UDP 443 tests
// ---------------------------------------------------------------------------

// udpCfg builds a Config that injects in-memory proc content for UDP checks.
func udpCfg(sources map[string]string) Config {
	cfg := Config{Timeouts: DefaultTimeouts()}
	for path := range sources {
		cfg.ProcNetUDP = append(cfg.ProcNetUDP, path)
	}
	cfg.procReader = func(path string) (string, error) {
		if content, ok := sources[path]; ok {
			return content, nil
		}
		return "", fmt.Errorf("open %s: no such file or directory", path)
	}
	return cfg
}

// ipv4Row is a minimal /proc/net/udp line with local address "IIIIIIII:PPPP" in column 1.
func ipv4Row(localAddr string) string {
	return "  0: " + localAddr + " 00000000:0000 07 00000000:00000000 00:00000000 00000000  1000      0 0 2 0\n"
}

// ipv4RowWithRemote returns a row whose local port is 0000 and remote address
// has the given "IIIIIIII:PPPP" value.
func ipv4RowWithRemote(remoteAddr string) string {
	return "  0: 00000000:0000 " + remoteAddr + " 07 00000000:00000000 00:00000000 00000000  1000      0 0 2 0\n"
}

const udpHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func TestUDP443_IPv4RowPasses(t *testing.T) {
	// 443 decimal = 0x1BB
	content := udpHeader + ipv4Row("00000000:01BB")
	cfg := udpCfg(map[string]string{"/proc/net/udp": content})
	c := udp443Check()
	r := c.Run(context.Background(), cfg)

	if r.ID != "port.udp443" {
		t.Errorf("ID = %q, want port.udp443", r.ID)
	}
	if r.Name != "UDP 443" {
		t.Errorf("Name = %q, want UDP 443", r.Name)
	}
	if r.Status != StatusPass {
		t.Errorf("Status = %q, want pass", r.Status)
	}
	if r.Message != "listening" {
		t.Errorf("Message = %q, want listening", r.Message)
	}
}

func TestUDP443_IPv6RowPassesWhenIPv4Missing(t *testing.T) {
	// IPv4 source is missing; IPv6 source has port 443.
	ipv6Content := udpHeader +
		"  0: 00000000000000000000000001000000:01BB 00000000000000000000000000000000:0000 07 00000000:00000000 00:00000000 00000000  1000      0 0 2 0\n"
	cfg := udpCfg(map[string]string{"/proc/net/udp6": ipv6Content})
	c := udp443Check()
	r := c.Run(context.Background(), cfg)
	if r.Status != StatusPass {
		t.Errorf("Status = %q, want pass; message = %q", r.Status, r.Message)
	}
}

func TestUDP443_RemoteAddrPortDoesNotPass(t *testing.T) {
	// Remote address column has 01BB but local port is 0000 → must not pass.
	content := udpHeader + ipv4RowWithRemote("00000000:01BB")
	cfg := udpCfg(map[string]string{"/proc/net/udp": content})
	c := udp443Check()
	r := c.Run(context.Background(), cfg)
	if r.Status != StatusFail {
		t.Errorf("Status = %q, want fail (remote-only match should not pass)", r.Status)
	}
}

func TestUDP443_MalformedPortsDoNotPass(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		// "101BB" – too many hex digits, not exactly 4
		{"101BB_tooLong", udpHeader + ipv4Row("00000000:101BB")},
		// "01B" – too short (3 chars)
		{"01B_tooShort", udpHeader + ipv4Row("00000000:01B")},
		// Blank line only
		{"blankLine", "\n"},
		// Header only
		{"headerOnly", udpHeader},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := udpCfg(map[string]string{"/proc/net/udp": tc.content})
			c := udp443Check()
			r := c.Run(context.Background(), cfg)
			if r.Status == StatusPass {
				t.Errorf("Status = pass unexpectedly for %q; message = %q", tc.name, r.Message)
			}
		})
	}
}

func TestUDP443_NoMatch_ReturnsNotListening(t *testing.T) {
	// Readable file but no port 443 entry.
	content := udpHeader + ipv4Row("00000000:0050") // port 80
	cfg := udpCfg(map[string]string{"/proc/net/udp": content})
	c := udp443Check()
	r := c.Run(context.Background(), cfg)

	if r.ID != "port.udp443" {
		t.Errorf("ID = %q, want port.udp443", r.ID)
	}
	if r.Name != "UDP 443" {
		t.Errorf("Name = %q, want UDP 443", r.Name)
	}
	if r.Status != StatusFail {
		t.Errorf("Status = %q, want fail", r.Status)
	}
	if r.Message != "not listening" {
		t.Errorf("Message = %q, want \"not listening\"", r.Message)
	}
}

func TestUDP443_AllSourcesUnreadable_StableMessage(t *testing.T) {
	// All sources unreadable → stable read-failure message.
	cfg := Config{Timeouts: DefaultTimeouts()}
	cfg.ProcNetUDP = []string{"/proc/net/udp", "/proc/net/udp6"}
	cfg.procReader = func(path string) (string, error) {
		return "", fmt.Errorf("open %s: permission denied", path)
	}
	c := udp443Check()
	r := c.Run(context.Background(), cfg)

	if r.ID != "port.udp443" {
		t.Errorf("ID = %q, want port.udp443", r.ID)
	}
	if r.Name != "UDP 443" {
		t.Errorf("Name = %q, want UDP 443", r.Name)
	}
	if r.Status != StatusFail {
		t.Errorf("Status = %q, want fail", r.Status)
	}
	// Message must be the stable fixed string and must not contain OS detail.
	if r.Message != "proc source unavailable" {
		t.Errorf("Message = %q, want \"proc source unavailable\"", r.Message)
	}
	if strings.Contains(r.Message, "permission") {
		t.Errorf("Message = %q: must not contain sensitive OS detail", r.Message)
	}
}

func TestUDP443_PreCancelledContext_ReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run

	var readerCalls int64
	cfg := Config{Timeouts: DefaultTimeouts()}
	cfg.ProcNetUDP = []string{"/proc/net/udp"}
	cfg.procReader = func(path string) (string, error) {
		atomic.AddInt64(&readerCalls, 1)
		return udpHeader + ipv4Row("00000000:01BB"), nil
	}
	c := udp443Check()
	done := make(chan Result, 1)
	go func() { done <- c.Run(ctx, cfg) }()

	var r Result
	select {
	case r = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after pre-cancelled context")
	}

	// Exact field assertions.
	if r.ID != "port.udp443" {
		t.Errorf("ID = %q, want port.udp443", r.ID)
	}
	if r.Name != "UDP 443" {
		t.Errorf("Name = %q, want UDP 443", r.Name)
	}
	if r.Status != StatusFail {
		t.Errorf("Status = %q, want fail", r.Status)
	}
	if r.Message != "cancelled" {
		t.Errorf("Message = %q, want cancelled", r.Message)
	}
	// procReader must never be called when context is already cancelled.
	if n := atomic.LoadInt64(&readerCalls); n != 0 {
		t.Errorf("procReader called %d time(s), want 0", n)
	}
}

func TestUDP443_FirstSourceMissingSecondReadable(t *testing.T) {
	// First source unreadable, second has port 443 → must PASS.
	ipv6Content := udpHeader +
		"  0: 00000000000000000000000001000000:01BB 00000000000000000000000000000000:0000 07 00000000:00000000 00:00000000 00000000  1000      0 0 2 0\n"
	sources := map[string]string{
		"/proc/net/udp6": ipv6Content,
		// /proc/net/udp intentionally absent → procReader returns error
	}
	cfg := udpCfg(sources)
	cfg.ProcNetUDP = []string{"/proc/net/udp", "/proc/net/udp6"}
	c := udp443Check()
	r := c.Run(context.Background(), cfg)
	if r.Status != StatusPass {
		t.Errorf("Status = %q, want pass when first source missing but second readable", r.Status)
	}
}

// ---------------------------------------------------------------------------
// TCP check tests
// ---------------------------------------------------------------------------

// tcpCfg builds a Config with the given dial seam and per-check addresses.
func tcpCfg(dialCtx dialContextFunc, addr443, addr80 string) Config {
	cfg := Config{Timeouts: DefaultTimeouts()}
	cfg.dialContext = dialCtx
	cfg.TCP443Addr = addr443
	cfg.TCP80Addr = addr80
	return cfg
}

func TestTCP443_SuccessAgainstEphemeralListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cfg := tcpCfg(nil, ln.Addr().String(), "")
	c := tcp443Check()
	r := c.Run(context.Background(), cfg)

	if r.ID != "port.tcp443" {
		t.Errorf("ID = %q, want port.tcp443", r.ID)
	}
	if r.Name != "TCP 443" {
		t.Errorf("Name = %q, want TCP 443", r.Name)
	}
	if r.Status != StatusPass {
		t.Errorf("Status = %q, want pass; message = %q", r.Status, r.Message)
	}
	if r.Message != "reachable" {
		t.Errorf("Message = %q, want reachable", r.Message)
	}
}

func TestTCP80_UsesOwnAddress(t *testing.T) {
	// Two listeners on different ephemeral ports; TCP80 must connect to addr80.
	ln80, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln80.Close()

	// addr443 points to a closed port (the listener is not started).
	cfg := tcpCfg(nil, "127.0.0.1:1", ln80.Addr().String())
	c := tcp80Check()
	r := c.Run(context.Background(), cfg)

	if r.ID != "port.tcp80" {
		t.Errorf("ID = %q, want port.tcp80", r.ID)
	}
	if r.Name != "TCP 80" {
		t.Errorf("Name = %q, want TCP 80", r.Name)
	}
	if r.Status != StatusPass {
		t.Errorf("Status = %q, want pass; message = %q", r.Status, r.Message)
	}
	if r.Message != "reachable" {
		t.Errorf("Message = %q, want reachable", r.Message)
	}
}

func TestTCP443_ClosedAddress_Fails(t *testing.T) {
	// Use ephemeral listener then close it immediately.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	cfg := tcpCfg(nil, addr, "")
	c := tcp443Check()
	r := c.Run(context.Background(), cfg)

	if r.ID != "port.tcp443" {
		t.Errorf("ID = %q, want port.tcp443", r.ID)
	}
	if r.Name != "TCP 443" {
		t.Errorf("Name = %q, want TCP 443", r.Name)
	}
	if r.Status != StatusFail {
		t.Errorf("Status = %q, want fail against closed port", r.Status)
	}
	if r.Message != "connection failed" {
		t.Errorf("Message = %q, want \"connection failed\"", r.Message)
	}
}

// fakeDial is a context-aware dial that blocks until ctx is cancelled and
// records the address it was called with and the deadline set on ctx.
type fakeDial struct {
	called    chan struct{}
	cancelled chan struct{}
	dialAddr  chan string
	deadline  chan time.Time
}

func newFakeDial() *fakeDial {
	return &fakeDial{
		called:    make(chan struct{}, 1),
		cancelled: make(chan struct{}, 1),
		dialAddr:  make(chan string, 1),
		deadline:  make(chan time.Time, 1),
	}
}

func (f *fakeDial) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	select {
	case f.called <- struct{}{}:
	default:
	}
	select {
	case f.dialAddr <- addr:
	default:
	}
	if dl, ok := ctx.Deadline(); ok {
		select {
		case f.deadline <- dl:
		default:
		}
	}
	<-ctx.Done()
	select {
	case f.cancelled <- struct{}{}:
	default:
	}
	return nil, ctx.Err()
}

func TestTCP443_ContextAwareDial_TimesOut(t *testing.T) {
	fd := newFakeDial()
	cfg := tcpCfg(fd.dial, "127.0.0.1:443", "")
	cfg.Timeouts.TCPConnect = 50 * time.Millisecond

	start := time.Now()
	c := tcp443Check()
	r := c.Run(context.Background(), cfg)
	elapsed := time.Since(start)

	// Exact field assertions.
	if r.ID != "port.tcp443" {
		t.Errorf("ID = %q, want port.tcp443", r.ID)
	}
	if r.Name != "TCP 443" {
		t.Errorf("Name = %q, want TCP 443", r.Name)
	}
	if r.Status != StatusFail {
		t.Errorf("Status = %q, want fail on timeout", r.Status)
	}
	if r.Message != "timeout or cancelled" {
		t.Errorf("Message = %q, want \"timeout or cancelled\"", r.Message)
	}

	// Must return within generous margin of the timeout.
	if elapsed > 2*time.Second {
		t.Errorf("Run blocked for %v, expected prompt return", elapsed)
	}
	// The fake must have observed cancellation.
	select {
	case <-fd.cancelled:
	case <-time.After(time.Second):
		t.Error("fake dial did not observe cancellation")
	}
	// The deadline set on ctx must be consistent with cfg.Timeouts.TCPConnect.
	select {
	case dl := <-fd.deadline:
		remaining := time.Until(dl)
		// Allow generous margin: deadline must be within [0, 2×timeout] from now.
		// At this point elapsed time has passed, so we check it was in the future
		// relative to start (i.e., deadline > start).
		if dl.Before(start) {
			t.Errorf("dial deadline %v was before test start %v", dl, start)
		}
		// And must not be more than 2× the configured timeout from test start.
		maxDeadline := start.Add(2 * cfg.Timeouts.TCPConnect)
		if dl.After(maxDeadline) {
			t.Errorf("dial deadline %v exceeds 2× TCPConnect from start; remaining from now = %v", dl, remaining)
		}
	case <-time.After(time.Second):
		t.Error("fake dial did not report a deadline")
	}
}

func TestTCP443_PreCancelledContext_NoDial(t *testing.T) {
	fd := newFakeDial()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := tcpCfg(fd.dial, "127.0.0.1:443", "")
	c := tcp443Check()
	r := c.Run(ctx, cfg)

	// Exact field assertions.
	if r.ID != "port.tcp443" {
		t.Errorf("ID = %q, want port.tcp443", r.ID)
	}
	if r.Name != "TCP 443" {
		t.Errorf("Name = %q, want TCP 443", r.Name)
	}
	if r.Status != StatusFail {
		t.Errorf("Status = %q, want fail", r.Status)
	}
	if r.Message != "cancelled" {
		t.Errorf("Message = %q, want \"cancelled\"", r.Message)
	}
	// Dial must NOT have been called since ctx was already cancelled.
	select {
	case <-fd.called:
		t.Error("fake dial was called despite pre-cancelled context")
	default:
	}
}

// trackConn records whether Close was called.
type trackConn struct {
	net.Conn
	closed chan struct{}
}

func (c *trackConn) Close() error {
	select {
	case c.closed <- struct{}{}:
	default:
	}
	return c.Conn.Close()
}

func TestTCP443_SuccessfulDial_ClosesCalled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	closed := make(chan struct{}, 1)
	realDialer := &net.Dialer{}
	dialCtx := func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := realDialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &trackConn{Conn: conn, closed: closed}, nil
	}

	cfg := tcpCfg(dialCtx, ln.Addr().String(), "")
	c := tcp443Check()
	r := c.Run(context.Background(), cfg)

	if r.Status != StatusPass {
		t.Errorf("Status = %q, want pass", r.Status)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Error("connection was not closed after successful dial")
	}
}

func TestPortChecks_DefaultAddresses(t *testing.T) {
	// Verify production default dial addresses by running the checks with a
	// fake dial that records the target address and immediately returns an error.
	// This exercises the real Run path including the defaultAddr fallback.

	type recorded struct {
		addr string
	}
	makeRecorder := func(rec *recorded) dialContextFunc {
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			rec.addr = addr
			return nil, context.Canceled // stable, non-nil → triggers FAIL
		}
	}

	t.Run("tcp443_default_addr", func(t *testing.T) {
		var rec recorded
		// Zero TCP443Addr → must fall back to 127.0.0.1:443.
		cfg := Config{
			Timeouts:    DefaultTimeouts(),
			dialContext: makeRecorder(&rec),
			// TCP443Addr intentionally left empty
		}
		c := tcp443Check()
		r := c.Run(context.Background(), cfg)

		if rec.addr != "127.0.0.1:443" {
			t.Errorf("dialled %q, want 127.0.0.1:443", rec.addr)
		}
		if r.ID != "port.tcp443" {
			t.Errorf("ID = %q, want port.tcp443", r.ID)
		}
		if r.Name != "TCP 443" {
			t.Errorf("Name = %q, want TCP 443", r.Name)
		}
		if r.Status != StatusFail {
			t.Errorf("Status = %q, want fail", r.Status)
		}
	})

	t.Run("tcp80_default_addr", func(t *testing.T) {
		var rec recorded
		// Zero TCP80Addr → must fall back to 127.0.0.1:80.
		cfg := Config{
			Timeouts:    DefaultTimeouts(),
			dialContext: makeRecorder(&rec),
			// TCP80Addr intentionally left empty
		}
		c := tcp80Check()
		r := c.Run(context.Background(), cfg)

		if rec.addr != "127.0.0.1:80" {
			t.Errorf("dialled %q, want 127.0.0.1:80", rec.addr)
		}
		if r.ID != "port.tcp80" {
			t.Errorf("ID = %q, want port.tcp80", r.ID)
		}
		if r.Name != "TCP 80" {
			t.Errorf("Name = %q, want TCP 80", r.Name)
		}
		if r.Status != StatusFail {
			t.Errorf("Status = %q, want fail", r.Status)
		}
	})

	t.Run("udp443_identity", func(t *testing.T) {
		cu := udp443Check()
		if cu.ID() != "port.udp443" {
			t.Errorf("udp443Check ID = %q, want port.udp443", cu.ID())
		}
		if cu.Name() != "UDP 443" {
			t.Errorf("udp443Check Name = %q, want UDP 443", cu.Name())
		}
	})
}
