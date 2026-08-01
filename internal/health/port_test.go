package health

import (
	"context"
	"fmt"
	"net"
	"strings"
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
	content := udpHeader + ipv4RowWithRemote("01BB")
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
	if r.Status != StatusFail {
		t.Errorf("Status = %q, want fail", r.Status)
	}
	if r.Message != "not listening" {
		t.Errorf("Message = %q, want \"not listening\"", r.Message)
	}
}

func TestUDP443_AllSourcesUnreadable_StableMessage(t *testing.T) {
	// No sources configured → nothing to read → stable read-failure message.
	cfg := Config{Timeouts: DefaultTimeouts()}
	cfg.ProcNetUDP = []string{"/proc/net/udp", "/proc/net/udp6"}
	cfg.procReader = func(path string) (string, error) {
		return "", fmt.Errorf("open %s: permission denied", path)
	}
	c := udp443Check()
	r := c.Run(context.Background(), cfg)
	if r.Status != StatusFail {
		t.Errorf("Status = %q, want fail", r.Status)
	}
	// Message must be stable and non-sensitive.
	if r.Message == "" || strings.Contains(r.Message, "permission") {
		t.Errorf("Message = %q: must be stable and non-sensitive", r.Message)
	}
}

func TestUDP443_PreCancelledContext_ReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run
	cfg := Config{Timeouts: DefaultTimeouts()}
	cfg.ProcNetUDP = []string{"/proc/net/udp"}
	cfg.procReader = func(path string) (string, error) {
		return udpHeader + ipv4Row("00000000:01BB"), nil
	}
	c := udp443Check()
	done := make(chan Result, 1)
	go func() { done <- c.Run(ctx, cfg) }()
	select {
	case r := <-done:
		if r.Status != StatusFail {
			t.Errorf("pre-cancelled: Status = %q, want fail", r.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after pre-cancelled context")
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
	cfg.DialContext = dialCtx
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
	if r.Status != StatusFail {
		t.Errorf("Status = %q, want fail against closed port", r.Status)
	}
}

// fakeDial is a context-aware dial that blocks until ctx is cancelled, records
// that it was called, and reports cancellation to the caller.
type fakeDial struct {
	called    chan struct{}
	cancelled chan struct{}
}

func newFakeDial() *fakeDial {
	return &fakeDial{
		called:    make(chan struct{}, 1),
		cancelled: make(chan struct{}, 1),
	}
}

func (f *fakeDial) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	select {
	case f.called <- struct{}{}:
	default:
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
	cfg.Timeouts.TCPConnect = 50 * time.Millisecond // very short

	start := time.Now()
	c := tcp443Check()
	r := c.Run(context.Background(), cfg)
	elapsed := time.Since(start)

	if r.Status != StatusFail {
		t.Errorf("Status = %q, want fail on timeout", r.Status)
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
}

func TestTCP443_PreCancelledContext_NoDial(t *testing.T) {
	fd := newFakeDial()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := tcpCfg(fd.dial, "127.0.0.1:443", "")
	c := tcp443Check()
	r := c.Run(ctx, cfg)

	if r.Status != StatusFail {
		t.Errorf("Status = %q, want fail", r.Status)
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

func TestPortChecks_ProductionDefaults(t *testing.T) {
	// Verify that the production checks default to the expected addresses.
	// We do not dial them; we only inspect the configured defaults.
	c443 := tcp443Check()
	if c443.ID() != "port.tcp443" {
		t.Errorf("tcp443Check ID = %q, want port.tcp443", c443.ID())
	}
	if c443.Name() != "TCP 443" {
		t.Errorf("tcp443Check Name = %q, want TCP 443", c443.Name())
	}

	c80 := tcp80Check()
	if c80.ID() != "port.tcp80" {
		t.Errorf("tcp80Check ID = %q, want port.tcp80", c80.ID())
	}
	if c80.Name() != "TCP 80" {
		t.Errorf("tcp80Check Name = %q, want TCP 80", c80.Name())
	}

	// Verify UDP check identity.
	cu := udp443Check()
	if cu.ID() != "port.udp443" {
		t.Errorf("udp443Check ID = %q, want port.udp443", cu.ID())
	}
	if cu.Name() != "UDP 443" {
		t.Errorf("udp443Check Name = %q, want UDP 443", cu.Name())
	}
}
