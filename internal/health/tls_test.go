package health

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
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
}

func newTLSFixture(t *testing.T, domain string, notBefore, notAfter time.Time, status int, body string) *tlsFixture {
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
	baseTLS := &tls.Config{Certificates: []tls.Certificate{cert}}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) { return baseTLS, nil }})
	if err != nil {
		t.Fatal(err)
	}
	f := &tlsFixture{ln: ln, roots: x509.NewCertPool(), requests: make(chan *http.Request, 8), sni: make(chan string, 8)}
	ln.Close()
	ln, err = tls.Listen("tcp", f.ln.Addr().String(), &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) { f.sni <- hello.ServerName; return baseTLS, nil }})
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	f.roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	go func() {
		_ = http.Serve(&trackingListener{Listener: ln, closed: &f.closed}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.requests <- r
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
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
	closed *atomic.Int32
	once   sync.Once
}

func (c *trackingConn) Close() error {
	var err error
	c.once.Do(func() { err = c.Conn.Close(); c.closed.Add(1) })
	return err
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
	for _, secret := range []string{tlsTestToken, tlsSecret, "https://" + tlsTestDomain} {
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
		{"domain", Config{Token: tlsTestToken, Timeouts: DefaultTimeouts()}, "domain unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertTLSResult(t, httpsSubscriptionCheck().Run(context.Background(), tc.cfg), "http.subscription", "clash subscription", StatusFail, tc.message)
		})
	}
}

func TestHTTPSSubscriptionTimeoutAndCancellationCloseIdleConnections(t *testing.T) {
	blocked := make(chan struct{})
	cfg := Config{Domain: tlsTestDomain, Token: tlsTestToken, Timeouts: Timeouts{TCPConnect: 20 * time.Millisecond, HTTP: 20 * time.Millisecond}, tlsDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertTLSResult(t, httpsSubscriptionCheck().Run(ctx, cfg), "http.subscription", "clash subscription", StatusFail, "timeout or cancelled")
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
	cfg := Config{Domain: tlsTestDomain, RootCAs: x509.NewCertPool(), Timeouts: Timeouts{TCPConnect: time.Second, TLS: 20 * time.Millisecond}, tlsDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		server, client := net.Pipe()
		close(called)
		peers = append(peers, server)
		return client, nil
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertTLSResult(t, tlsCertificateCheck().Run(ctx, cfg), "tls.certificate", "TLS certificate", StatusFail, "timeout or cancelled")
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
