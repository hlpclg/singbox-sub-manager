package health

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const tlsTestDomain = "subscription.test"
const tlsTestToken = "token_1234567890"
const tlsSecret = "TLS-SECRET-MARKER"

type tlsFixture struct {
	ln       net.Listener
	roots    *x509.CertPool
	requests chan *http.Request
	sni      chan string
	closed   atomic.Int32
	mu       sync.Mutex
	dials    []string
	handler  http.Handler
}

func newTLSTestCertificate(t *testing.T, domain string, notBefore, notAfter time.Time) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, NotBefore: notBefore.Add(-time.Hour), NotAfter: notAfter.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: domain}, DNSNames: []string{domain}, NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)}))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	return cert, roots
}

func newTLSFixture(t *testing.T, domain string, notBefore, notAfter time.Time, status int, body string) *tlsFixture {
	t.Helper()
	cert, roots := newTLSTestCertificate(t, domain, notBefore, notAfter)
	baseTLS := &tls.Config{Certificates: []tls.Certificate{cert}}
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &tlsFixture{roots: roots, requests: make(chan *http.Request, 8), sni: make(chan string, 8)}
	ln := tls.NewListener(rawListener, &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) { f.sni <- hello.ServerName; return baseTLS, nil }})
	f.ln = ln
	f.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests <- r
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	go func() {
		_ = http.Serve(&trackingListener{Listener: ln, closed: &f.closed}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.handler.ServeHTTP(w, r) }))
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func waitTLSClosed(t *testing.T, f *tlsFixture) {
	t.Helper()
	deadline := time.After(time.Second)
	for f.closed.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("server connection was not closed")
		case <-time.After(time.Millisecond):
		}
	}
}

type trackingListener struct {
	net.Listener
	closed *atomic.Int32
}

func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &trackingConn{Conn: c, closed: l.closed}, nil
}

type trackingConn struct {
	net.Conn
	closed       *atomic.Int32
	once         sync.Once
	writeStarted chan struct{}
	writeOnce    sync.Once
}

func (c *trackingConn) Close() error {
	var err error
	c.once.Do(func() { err = c.Conn.Close(); c.closed.Add(1) })
	return err
}

func (c *trackingConn) Write(p []byte) (int, error) {
	if c.writeStarted != nil {
		c.writeOnce.Do(func() { close(c.writeStarted) })
	}
	return c.Conn.Write(p)
}

var errInjectedWrite = errors.New("injected write failure")

// closeAuditConn counts every Close call without deduplication. Run's own
// deferred tlsConn.Close must be the only Close observed before Run returns;
// a second Close after Run's internal reqCtx cancel proves the cancellation
// watcher outlived Run.
type closeAuditConn struct {
	net.Conn
	closes atomic.Int32
}

func (c *closeAuditConn) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

// writeFailConn fails writes once armed. Run calls SetDeadline exactly once,
// after HandshakeContext and before req.Write, so handshake writes pass
// through and the request write fails deterministically with no timing
// dependency.
type writeFailConn struct {
	*closeAuditConn
	armed atomic.Bool
}

func (c *writeFailConn) SetDeadline(d time.Time) error {
	c.armed.Store(true)
	return c.closeAuditConn.SetDeadline(d)
}

func (c *writeFailConn) Write(p []byte) (int, error) {
	if c.armed.Load() {
		return 0, errInjectedWrite
	}
	return c.closeAuditConn.Write(p)
}

// blockingCloseConn blocks its first Close call until the test releases it.
// The underlying connection is closed first so pending I/O fails, which lets
// Run reach its deferred watcher wait while the watcher is still inside
// Close. The first Close on the Run path is always the cancellation
// watcher's: Run's own deferred Close can only run after the watcher exits.
type blockingCloseConn struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingCloseConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		close(c.entered)
		<-c.release
	})
	return err
}

// assertConnectionClosedAndWatcherExited proves the connection was closed
// exactly once before Run returned and that no late watcher Close followed
// Run's internal reqCtx cancel. That Run actually waits for the watcher is
// proven separately by TestHTTPSSubscriptionRunWaitsForWatcherCloseOnCancel;
// combined with Run having returned, no second Close means the watcher had
// already exited.
func assertConnectionClosedAndWatcherExited(t *testing.T, c *closeAuditConn) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := c.closes.Load(); got != 1 {
			t.Fatalf("connection Close calls = %d, want exactly 1 (watcher must exit before Run returns)", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *tlsFixture) cfg(now time.Time) Config {
	return Config{Domain: tlsTestDomain, Token: tlsTestToken, LoopbackTLSAddr: f.ln.Addr().String(), RootCAs: f.roots, Timeouts: Timeouts{TCPConnect: time.Second, TLS: time.Second, HTTP: time.Second}, tlsDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		f.mu.Lock()
		f.dials = append(f.dials, addr)
		f.mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, f.ln.Addr().String())
	}, now: func() time.Time { return now }}
}

func assertTLSResult(t *testing.T, r Result, id, name string, status Status, message string) {
	t.Helper()
	assertResult(t, r, id, name, status, message)
	for _, secret := range []string{tlsTestToken, tlsSecret, "https://" + tlsTestDomain, "clash.yaml"} {
		if strings.Contains(r.Message, secret) {
			t.Errorf("message leaked %q: %q", secret, r.Message)
		}
	}
}

func TestHTTPSSubscription200UsesGETHostSNIAndLoopback(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	f := newTLSFixture(t, tlsTestDomain, now.Add(-time.Hour), now.Add(30*24*time.Hour), http.StatusOK, "ok")
	r := httpsSubscriptionCheck().Run(context.Background(), f.cfg(now))
	assertTLSResult(t, r, "http.subscription", "clash subscription", StatusPass, "HTTP 200")
	select {
	case req := <-f.requests:
		if req.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", req.Method)
		}
		if req.Host != tlsTestDomain {
			t.Errorf("Host = %q", req.Host)
		}
		if req.URL.Path != "/"+tlsTestToken+"/clash.yaml" {
			t.Errorf("path = %q", req.URL.Path)
		}
	case <-time.After(time.Second):
		t.Fatal("request not received")
	}
	select {
	case sni := <-f.sni:
		if sni != tlsTestDomain {
			t.Errorf("SNI = %q", sni)
		}
	case <-time.After(time.Second):
		t.Fatal("SNI not received")
	}
	waitTLSClosed(t, f)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.dials) != 1 || f.dials[0] != f.ln.Addr().String() {
		t.Fatalf("dials = %v, want loopback address", f.dials)
	}
}

func TestHTTPSSubscriptionFailuresAreRedacted(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	f := newTLSFixture(t, tlsTestDomain, now.Add(-time.Hour), now.Add(30*24*time.Hour), http.StatusTeapot, tlsSecret)
	for _, tc := range []struct {
		name    string
		cfg     Config
		message string
	}{
		{"status", f.cfg(now), "HTTP status not 200"},
		{"token", Config{Domain: tlsTestDomain, Token: "bad", Timeouts: DefaultTimeouts()}, "token unavailable"},
		{"token_error", Config{Domain: tlsTestDomain, Token: tlsTestToken, TokenErr: errors.New(tlsSecret), Timeouts: DefaultTimeouts()}, "token unavailable"},
		{"domain", Config{Token: tlsTestToken, Timeouts: DefaultTimeouts()}, "domain unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertTLSResult(t, httpsSubscriptionCheck().Run(context.Background(), tc.cfg), "http.subscription", "clash subscription", StatusFail, tc.message)
		})
	}
}

func TestHTTPSSubscriptionRedirectIsNotFollowed(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	f := newTLSFixture(t, tlsTestDomain, now.Add(-time.Hour), now.Add(30*24*time.Hour), http.StatusFound, "redirect")
	f.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://"+tlsTestDomain+"/other")
		w.WriteHeader(http.StatusFound)
	})
	assertTLSResult(t, httpsSubscriptionCheck().Run(context.Background(), f.cfg(now)), "http.subscription", "clash subscription", StatusFail, "HTTP status not 200")
}

func TestHTTPSSubscriptionTimeoutAndCancellationCloseIdleConnections(t *testing.T) {
	blocked := make(chan struct{})
	deadlineObserved := make(chan time.Duration, 1)
	cfg := Config{Domain: tlsTestDomain, Token: tlsTestToken, Timeouts: Timeouts{TCPConnect: 100 * time.Millisecond, TLS: 500 * time.Millisecond, HTTP: time.Second}, tlsDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("TCP dial context has no deadline")
		}
		deadlineObserved <- time.Until(deadline)
		<-ctx.Done()
		close(blocked)
		return nil, ctx.Err()
	}}
	assertTLSResult(t, httpsSubscriptionCheck().Run(context.Background(), cfg), "http.subscription", "clash subscription", StatusFail, "timeout or cancelled")
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("dial did not observe deadline")
	}
	select {
	case remaining := <-deadlineObserved:
		if remaining < 70*time.Millisecond || remaining > 120*time.Millisecond {
			t.Fatalf("TCP deadline remaining = %s", remaining)
		}
	case <-time.After(time.Second):
		t.Fatal("dial deadline was not observed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertTLSResult(t, httpsSubscriptionCheck().Run(ctx, cfg), "http.subscription", "clash subscription", StatusFail, "timeout or cancelled")
}

func TestHTTPSSubscriptionDefaultLoopbackAddress(t *testing.T) {
	var got string
	cfg := Config{Domain: tlsTestDomain, Token: tlsTestToken, Timeouts: Timeouts{TCPConnect: time.Second, TLS: time.Second, HTTP: time.Second}, tlsDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		got = addr
		return nil, errors.New(tlsSecret)
	}}
	assertTLSResult(t, httpsSubscriptionCheck().Run(context.Background(), cfg), "http.subscription", "clash subscription", StatusFail, "request failed")
	if got != "127.0.0.1:443" {
		t.Errorf("dial address = %q", got)
	}
}

func TestHTTPSSubscriptionParentCancellationDuringTCPDial(t *testing.T) {
	started := make(chan struct{})
	var closed atomic.Int32
	cfg := Config{Domain: tlsTestDomain, Token: tlsTestToken, Timeouts: Timeouts{TCPConnect: time.Second, TLS: time.Second, HTTP: 2 * time.Second}, tlsDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		close(started)
		<-ctx.Done()
		closed.Add(1)
		return nil, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan Result, 1)
	go func() { results <- httpsSubscriptionCheck().Run(ctx, cfg) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("TCP dial did not start")
	}
	cancel()
	select {
	case r := <-results:
		assertTLSResult(t, r, "http.subscription", "clash subscription", StatusFail, "timeout or cancelled")
	case <-time.After(time.Second):
		t.Fatal("Run did not return after TCP cancellation")
	}
	if closed.Load() != 1 {
		t.Fatalf("cancelled TCP dial completions = %d", closed.Load())
	}
}

func TestHTTPSSubscriptionParentCancellationDuringTLSHandshake(t *testing.T) {
	writeStarted := make(chan struct{})
	var clientClosed atomic.Int32
	cfg := Config{Domain: tlsTestDomain, Token: tlsTestToken, Timeouts: Timeouts{TCPConnect: time.Second, TLS: 2 * time.Second, HTTP: 3 * time.Second}, tlsDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		server, client := net.Pipe()
		t.Cleanup(func() { _ = server.Close() })
		return &trackingConn{Conn: client, closed: &clientClosed, writeStarted: writeStarted}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan Result, 1)
	go func() { results <- httpsSubscriptionCheck().Run(ctx, cfg) }()
	select {
	case <-writeStarted:
	case <-time.After(time.Second):
		t.Fatal("TLS handshake did not start")
	}
	cancel()
	select {
	case r := <-results:
		assertTLSResult(t, r, "http.subscription", "clash subscription", StatusFail, "timeout or cancelled")
	case <-time.After(time.Second):
		t.Fatal("Run did not return after TLS cancellation")
	}
	if clientClosed.Load() == 0 {
		t.Fatal("TLS client connection was not closed before Run returned")
	}
}

func TestHTTPSSubscriptionHandshakeHeaderBodyTimeoutAndParentCancellation(t *testing.T) {
	// minElapsed/maxElapsed mechanically prove which configured bound fired:
	// the run must last at least the specific timeout (it really waited) and
	// less than every other timeout present in the config, so a wrong bound
	// (TCP 500ms, TLS 500ms, HTTP 1s) or an instant failure is caught.
	for _, tc := range []struct {
		name         string
		phase        string
		cancelParent bool
		minElapsed   time.Duration
		maxElapsed   time.Duration
	}{
		// TLS = 100ms; must exclude TCP 500ms and HTTP 1s.
		{"handshake", "handshake", false, 80 * time.Millisecond, 400 * time.Millisecond},
		// HTTP = 200ms; must exclude TLS 500ms and TCP 1s.
		{"headers", "headers", false, 180 * time.Millisecond, 450 * time.Millisecond},
		{"body", "body", false, 180 * time.Millisecond, 450 * time.Millisecond},
		// Parent cancel after arrival must return promptly, not at HTTP 1s.
		{"parent_cancel", "headers", true, 0, 500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arrived, release := make(chan struct{}), make(chan struct{})
			var clientClosed atomic.Int32
			var cfg Config
			var fixture *tlsFixture
			if tc.phase == "handshake" {
				cfg = Config{Domain: tlsTestDomain, Token: tlsTestToken, Timeouts: Timeouts{TCPConnect: 500 * time.Millisecond, TLS: 100 * time.Millisecond, HTTP: time.Second}, tlsDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					server, client := net.Pipe()
					t.Cleanup(func() { _ = server.Close() })
					return &trackingConn{Conn: client, closed: &clientClosed}, nil
				}}
			} else {
				now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
				f := newTLSFixture(t, tlsTestDomain, now.Add(-time.Hour), now.Add(30*24*time.Hour), 200, "ok")
				fixture = f
				f.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					arrived <- struct{}{}
					if tc.phase == "headers" {
						<-release
						return
					}
					w.Header().Set("Content-Length", "2")
					w.WriteHeader(200)
					if flush, ok := w.(http.Flusher); ok {
						flush.Flush()
					}
					<-release
				})
				cfg = f.cfg(now)
				cfg.Timeouts.TCPConnect = time.Second
				cfg.Timeouts.TLS = 500 * time.Millisecond
				if tc.cancelParent {
					cfg.Timeouts.HTTP = time.Second
				} else {
					cfg.Timeouts.HTTP = 200 * time.Millisecond
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type timedResult struct {
				r       Result
				elapsed time.Duration
			}
			resultCh := make(chan timedResult, 1)
			go func() {
				start := time.Now()
				resultCh <- timedResult{r: httpsSubscriptionCheck().Run(ctx, cfg), elapsed: time.Since(start)}
			}()
			if tc.phase != "handshake" {
				select {
				case <-arrived:
				case <-time.After(time.Second):
					t.Fatal("server did not receive request")
				}
			}
			if tc.cancelParent {
				cancel()
			}
			select {
			case tr := <-resultCh:
				assertTLSResult(t, tr.r, "http.subscription", "clash subscription", StatusFail, "timeout or cancelled")
				if tr.elapsed < tc.minElapsed || tr.elapsed > tc.maxElapsed {
					t.Fatalf("Run elapsed = %s, want within [%s, %s]", tr.elapsed, tc.minElapsed, tc.maxElapsed)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Run did not return")
			}
			close(release)
			if tc.phase == "handshake" && clientClosed.Load() == 0 {
				t.Fatal("client connection was not closed")
			}
			if fixture != nil {
				waitTLSClosed(t, fixture)
			}
		})
	}
}

func TestHTTPSSubscriptionRequestWriteFailureIsRedactedClosedAndWatcherExits(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	f := newTLSFixture(t, tlsTestDomain, now.Add(-time.Hour), now.Add(30*24*time.Hour), http.StatusOK, "ok")
	var wconn *writeFailConn
	cfg := f.cfg(now)
	cfg.tlsDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, network, f.ln.Addr().String())
		if err != nil {
			return nil, err
		}
		wconn = &writeFailConn{closeAuditConn: &closeAuditConn{Conn: raw}}
		return wconn, nil
	}
	r := httpsSubscriptionCheck().Run(context.Background(), cfg)
	assertTLSResult(t, r, "http.subscription", "clash subscription", StatusFail, "request failed")
	assertConnectionClosedAndWatcherExited(t, wconn.closeAuditConn)
	select {
	case req := <-f.requests:
		t.Fatalf("server received a request despite the write failure: %s", req.URL.Path)
	default:
	}
	waitTLSClosed(t, f)
}

func TestHTTPSSubscriptionMalformedResponseIsRedactedClosedAndWatcherExits(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	cert, roots := newTLSTestCertificate(t, tlsTestDomain, now.Add(-time.Hour), now.Add(30*24*time.Hour))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	served := make(chan struct{})
	requests := make(chan *http.Request, 1)
	readErrs := make(chan error, 1)
	go func() {
		defer close(served)
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		srv := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err := srv.Handshake(); err != nil {
			_ = raw.Close()
			return
		}
		// Read the complete request first: a well-formed request observed
		// server-side proves the client's req.Write succeeded, so the client
		// failure below can only come from http.ReadResponse.
		req, err := http.ReadRequest(bufio.NewReader(srv))
		if err != nil {
			readErrs <- err
			_ = srv.Close()
			return
		}
		requests <- req
		_, _ = srv.Write([]byte("not an http response\r\n"))
		_ = srv.Close()
	}()
	var aconn *closeAuditConn
	cfg := Config{Domain: tlsTestDomain, Token: tlsTestToken, LoopbackTLSAddr: ln.Addr().String(), RootCAs: roots, Timeouts: Timeouts{TCPConnect: time.Second, TLS: time.Second, HTTP: time.Second}, now: func() time.Time { return now }, tlsDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
		if err != nil {
			return nil, err
		}
		aconn = &closeAuditConn{Conn: raw}
		return aconn, nil
	}}
	r := httpsSubscriptionCheck().Run(context.Background(), cfg)
	assertTLSResult(t, r, "http.subscription", "clash subscription", StatusFail, "request failed")
	assertConnectionClosedAndWatcherExited(t, aconn)
	select {
	case req := <-requests:
		if req.Method != http.MethodGet || req.URL.Path != "/"+tlsTestToken+"/clash.yaml" || req.Host != tlsTestDomain {
			t.Fatalf("server read request = %s %s (Host %q)", req.Method, req.URL.Path, req.Host)
		}
	case err := <-readErrs:
		t.Fatalf("server failed to read the request: %v", err)
	case <-time.After(time.Second):
		t.Fatal("server did not read a complete request")
	}
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("malformed-response server did not finish")
	}
}

func TestHTTPSSubscriptionRunWaitsForWatcherCloseOnCancel(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	f := newTLSFixture(t, tlsTestDomain, now.Add(-time.Hour), now.Add(30*24*time.Hour), http.StatusOK, "ok")
	arrived := make(chan struct{})
	respond := make(chan struct{})
	var respondOnce sync.Once
	releaseRespond := func() { respondOnce.Do(func() { close(respond) }) }
	f.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-respond
	})
	t.Cleanup(releaseRespond)
	bconn := &blockingCloseConn{entered: make(chan struct{}), release: make(chan struct{})}
	cfg := f.cfg(now)
	cfg.tlsDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, network, f.ln.Addr().String())
		if err != nil {
			return nil, err
		}
		bconn.Conn = raw
		return bconn, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan Result, 1)
	go func() { resultCh <- httpsSubscriptionCheck().Run(ctx, cfg) }()
	select {
	case <-arrived:
	case <-time.After(time.Second):
		t.Fatal("server did not receive request")
	}
	cancel()
	select {
	case <-bconn.entered:
	case <-time.After(time.Second):
		t.Fatal("watcher did not close the connection after cancel")
	}
	// The watcher is now blocked inside Close. If Run did not wait for the
	// watcher, its read has already failed (the underlying connection is
	// closed) and it would return here.
	select {
	case <-resultCh:
		t.Fatal("Run returned while the watcher Close was still blocked")
	case <-time.After(200 * time.Millisecond):
	}
	close(bconn.release)
	select {
	case r := <-resultCh:
		assertTLSResult(t, r, "http.subscription", "clash subscription", StatusFail, "timeout or cancelled")
	case <-time.After(time.Second):
		t.Fatal("Run did not return after the watcher Close completed")
	}
	releaseRespond()
	waitTLSClosed(t, f)
}

func TestTLSCertificateValidatesChainHostnameSNIAndLoopback(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	f := newTLSFixture(t, tlsTestDomain, now.Add(-time.Hour), now.Add(30*24*time.Hour), 200, "ok")
	r := tlsCertificateCheck().Run(context.Background(), f.cfg(now))
	assertTLSResult(t, r, "tls.certificate", "TLS certificate", StatusPass, "valid")
	select {
	case sni := <-f.sni:
		if sni != tlsTestDomain {
			t.Errorf("SNI = %q", sni)
		}
	case <-time.After(time.Second):
		t.Fatal("SNI not received")
	}
	waitTLSClosed(t, f)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.dials) != 1 || f.dials[0] != f.ln.Addr().String() {
		t.Errorf("dials=%v", f.dials)
	}
}

func TestTLSCertificateFailuresAreRedacted(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	f := newTLSFixture(t, tlsTestDomain, now.Add(-time.Hour), now.Add(30*24*time.Hour), 200, "ok")
	badHost := f.cfg(now)
	badHost.Domain = "wrong.test"
	assertTLSResult(t, tlsCertificateCheck().Run(context.Background(), badHost), "tls.certificate", "TLS certificate", StatusFail, "certificate invalid")
	untrusted := f.cfg(now)
	untrusted.RootCAs = x509.NewCertPool()
	assertTLSResult(t, tlsCertificateCheck().Run(context.Background(), untrusted), "tls.certificate", "TLS certificate", StatusFail, "certificate invalid")
	expired := newTLSFixture(t, tlsTestDomain, now.Add(-48*time.Hour), now.Add(-24*time.Hour), 200, "ok")
	assertTLSResult(t, tlsCertificateCheck().Run(context.Background(), expired.cfg(now)), "tls.certificate", "TLS certificate", StatusFail, "certificate invalid")
}

func TestTLSCertificateHandshakeFailureIsRedactedAndClosed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.Close()
			close(closed)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	cfg := Config{Domain: tlsTestDomain, LoopbackTLSAddr: ln.Addr().String(), RootCAs: x509.NewCertPool(), Timeouts: Timeouts{TCPConnect: time.Second, TLS: time.Second}}
	assertTLSResult(t, tlsCertificateCheck().Run(context.Background(), cfg), "tls.certificate", "TLS certificate", StatusFail, "certificate invalid")
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("handshake connection not closed")
	}
}

func TestTLSCertificateTimeoutAndCancellation(t *testing.T) {
	called := make(chan struct{})
	var peers []net.Conn
	var clientClosed atomic.Int32
	cfg := Config{Domain: tlsTestDomain, RootCAs: x509.NewCertPool(), Timeouts: Timeouts{TCPConnect: time.Second, TLS: 20 * time.Millisecond}, tlsDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		server, client := net.Pipe()
		close(called)
		peers = append(peers, server)
		return &trackingConn{Conn: client, closed: &clientClosed}, nil
	}}
	defer func() {
		for _, peer := range peers {
			_ = peer.Close()
		}
	}()
	assertTLSResult(t, tlsCertificateCheck().Run(context.Background(), cfg), "tls.certificate", "TLS certificate", StatusFail, "timeout or cancelled")
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("no dial")
	}
	if clientClosed.Load() == 0 {
		t.Fatal("TLS client connection was not closed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertTLSResult(t, tlsCertificateCheck().Run(ctx, cfg), "tls.certificate", "TLS certificate", StatusFail, "timeout or cancelled")
}

func TestTLSCertificateAndExpiryEmptyDomain(t *testing.T) {
	cfg := Config{Timeouts: DefaultTimeouts()}
	assertTLSResult(t, tlsCertificateCheck().Run(context.Background(), cfg), "tls.certificate", "TLS certificate", StatusFail, "domain unavailable")
	assertTLSResult(t, tlsExpiryCheck().Run(context.Background(), cfg), "tls.expiry", "TLS expiry", StatusFail, "domain unavailable")
}

func TestTLSExpiryTimeoutAndParentCancellationCloseClient(t *testing.T) {
	var clientClosed atomic.Int32
	cfg := Config{Domain: tlsTestDomain, Timeouts: Timeouts{TCPConnect: time.Second, TLS: 25 * time.Millisecond}, tlsDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		server, client := net.Pipe()
		t.Cleanup(func() { _ = server.Close() })
		return &trackingConn{Conn: client, closed: &clientClosed}, nil
	}}
	assertTLSResult(t, tlsExpiryCheck().Run(context.Background(), cfg), "tls.expiry", "TLS expiry", StatusFail, "timeout or cancelled")
	if clientClosed.Load() == 0 {
		t.Fatal("TLS expiry client connection was not closed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertTLSResult(t, tlsExpiryCheck().Run(ctx, cfg), "tls.expiry", "TLS expiry", StatusFail, "timeout or cancelled")
}

func TestTLSCertificateAndExpiryParentCancellationDuringHandshake(t *testing.T) {
	for _, tc := range []struct {
		check    Check
		id, name string
	}{
		{tlsCertificateCheck(), "tls.certificate", "TLS certificate"}, {tlsExpiryCheck(), "tls.expiry", "TLS expiry"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			var clientClosed atomic.Int32
			writeStarted := make(chan struct{})
			cfg := Config{Domain: tlsTestDomain, Timeouts: Timeouts{TCPConnect: 200 * time.Millisecond, TLS: 400 * time.Millisecond, HTTP: 800 * time.Millisecond}, tlsDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				server, client := net.Pipe()
				t.Cleanup(func() { _ = server.Close() })
				return &trackingConn{Conn: client, closed: &clientClosed, writeStarted: writeStarted}, nil
			}}
			ctx, cancel := context.WithCancel(context.Background())
			resultCh := make(chan Result, 1)
			go func() { resultCh <- tc.check.Run(ctx, cfg) }()
			select {
			case <-writeStarted:
			case <-time.After(time.Second):
				t.Fatal("TLS handshake did not start")
			}
			cancel()
			select {
			case r := <-resultCh:
				assertTLSResult(t, r, tc.id, tc.name, StatusFail, "timeout or cancelled")
			case <-time.After(time.Second):
				t.Fatal("Run did not return after cancellation")
			}
			if clientClosed.Load() == 0 {
				t.Fatal("client connection was not closed before Run returned")
			}
		})
	}
}

func TestTLSExpiryUsesInjectedClockAtExactBoundary(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		after   time.Time
		status  Status
		message string
	}{
		{"far", now.Add(30 * 24 * time.Hour), StatusPass, "expires in 30 days"}, {"near", now.Add(13 * 24 * time.Hour), StatusWarn, "expires in 13 days"}, {"boundary", now.Add(14 * 24 * time.Hour), StatusPass, "expires in 14 days"}, {"expired", now.Add(-time.Hour), StatusFail, "certificate invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTLSFixture(t, tlsTestDomain, now.Add(-48*time.Hour), tc.after, 200, "ok")
			assertTLSResult(t, tlsExpiryCheck().Run(context.Background(), f.cfg(now)), "tls.expiry", "TLS expiry", tc.status, tc.message)
		})
	}
}
