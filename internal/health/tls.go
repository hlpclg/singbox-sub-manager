package health

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const tlsExpiryWindow = 14 * 24 * time.Hour

type httpsSubscriptionCheckImpl struct{}

func httpsSubscriptionCheck() Check             { return httpsSubscriptionCheckImpl{} }
func (httpsSubscriptionCheckImpl) ID() string   { return "http.subscription" }
func (httpsSubscriptionCheckImpl) Name() string { return "clash subscription" }

func (c httpsSubscriptionCheckImpl) Run(ctx context.Context, cfg Config) Result {
	if ctx.Err() != nil {
		return tlsResult(c.ID(), c.Name(), StatusFail, "timeout or cancelled")
	}
	if cfg.TokenErr != nil || !ValidToken(cfg.Token) {
		return tlsResult(c.ID(), c.Name(), StatusFail, "token unavailable")
	}
	if cfg.Domain == "" {
		return tlsResult(c.ID(), c.Name(), StatusFail, "domain unavailable")
	}

	timeouts := tlsTimeouts(cfg)
	reqCtx, cancel := context.WithTimeout(ctx, timeouts.HTTP)
	defer cancel()
	target := "https://" + cfg.Domain + "/" + cfg.Token + "/clash.yaml"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return tlsResult(c.ID(), c.Name(), StatusFail, "request failed")
	}
	conn, err := tlsDial(reqCtx, cfg, timeouts.TCPConnect)
	if err != nil {
		return tlsResult(c.ID(), c.Name(), StatusFail, httpContextError(reqCtx, err))
	}
	tlsConn := tls.Client(conn, tlsConfig(cfg))
	defer tlsConn.Close()
	handshakeCtx, cancelHandshake := context.WithTimeout(reqCtx, timeouts.TLS)
	err = tlsConn.HandshakeContext(handshakeCtx)
	cancelHandshake()
	if err != nil {
		return tlsResult(c.ID(), c.Name(), StatusFail, httpContextError(reqCtx, err))
	}

	// A parent context without a deadline must still interrupt a blocked HTTP
	// write/read. The watcher owns no work after stop is closed, and Run waits
	// for it before returning.
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-reqCtx.Done():
			_ = tlsConn.Close()
		case <-stopWatch:
		}
	}()
	defer func() { close(stopWatch); <-watchDone }()
	if deadline, ok := reqCtx.Deadline(); ok {
		_ = tlsConn.SetDeadline(deadline)
	}
	if err := req.Write(tlsConn); err != nil {
		return tlsResult(c.ID(), c.Name(), StatusFail, httpContextError(reqCtx, err))
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		return tlsResult(c.ID(), c.Name(), StatusFail, httpContextError(reqCtx, err))
	}
	_, readErr := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return tlsResult(c.ID(), c.Name(), StatusFail, httpContextError(reqCtx, readErr))
	}
	if resp.StatusCode != http.StatusOK {
		return tlsResult(c.ID(), c.Name(), StatusFail, "HTTP status not 200")
	}
	return tlsResult(c.ID(), c.Name(), StatusPass, "HTTP 200")
}

type tlsCertificateCheckImpl struct{}

func tlsCertificateCheck() Check             { return tlsCertificateCheckImpl{} }
func (tlsCertificateCheckImpl) ID() string   { return "tls.certificate" }
func (tlsCertificateCheckImpl) Name() string { return "TLS certificate" }
func (c tlsCertificateCheckImpl) Run(ctx context.Context, cfg Config) Result {
	if ctx.Err() != nil {
		return tlsResult(c.ID(), c.Name(), StatusFail, "timeout or cancelled")
	}
	if cfg.Domain == "" {
		return tlsResult(c.ID(), c.Name(), StatusFail, "domain unavailable")
	}
	if _, err := verifiedCertificate(ctx, cfg); err != nil {
		return tlsResult(c.ID(), c.Name(), StatusFail, tlsErrorMessage(err))
	}
	return tlsResult(c.ID(), c.Name(), StatusPass, "valid")
}

type tlsExpiryCheckImpl struct{}

func tlsExpiryCheck() Check             { return tlsExpiryCheckImpl{} }
func (tlsExpiryCheckImpl) ID() string   { return "tls.expiry" }
func (tlsExpiryCheckImpl) Name() string { return "TLS expiry" }
func (c tlsExpiryCheckImpl) Run(ctx context.Context, cfg Config) Result {
	if ctx.Err() != nil {
		return tlsResult(c.ID(), c.Name(), StatusFail, "timeout or cancelled")
	}
	if cfg.Domain == "" {
		return tlsResult(c.ID(), c.Name(), StatusFail, "domain unavailable")
	}
	cert, err := verifiedCertificate(ctx, cfg)
	if err != nil {
		return tlsResult(c.ID(), c.Name(), StatusFail, tlsErrorMessage(err))
	}
	now := time.Now
	if cfg.now != nil {
		now = cfg.now
	}
	remaining := cert.NotAfter.Sub(now())
	if remaining <= 0 {
		return tlsResult(c.ID(), c.Name(), StatusFail, "certificate invalid")
	}
	days := int(remaining / (24 * time.Hour))
	message := "expires in " + itoa(days) + " days"
	if remaining < tlsExpiryWindow {
		return tlsResult(c.ID(), c.Name(), StatusWarn, message)
	}
	return tlsResult(c.ID(), c.Name(), StatusPass, message)
}

func verifiedCertificate(ctx context.Context, cfg Config) (*x509.Certificate, error) {
	timeouts := tlsTimeouts(cfg)
	conn, err := tlsDial(ctx, cfg, timeouts.TCPConnect)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(conn, tlsConfig(cfg))
	defer tlsConn.Close()
	handshakeCtx, cancel := context.WithTimeout(ctx, timeouts.TLS)
	defer cancel()
	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		return nil, err
	}
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return nil, errors.New("no certificate")
	}
	return state.PeerCertificates[0], nil
}

func tlsDial(ctx context.Context, cfg Config, timeout time.Duration) (net.Conn, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	addr := cfg.LoopbackTLSAddr
	if addr == "" {
		addr = "127.0.0.1:443"
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dial := cfg.tlsDialContext
	if dial == nil {
		d := &net.Dialer{}
		dial = d.DialContext
	}
	return dial(dialCtx, "tcp", addr)
}

func tlsConfig(cfg Config) *tls.Config {
	c := &tls.Config{ServerName: cfg.Domain, RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12}
	if cfg.now != nil {
		c.Time = cfg.now
	}
	return c
}

func tlsTimeouts(cfg Config) Timeouts {
	t := cfg.Timeouts
	defaults := DefaultTimeouts()
	if t.TCPConnect <= 0 {
		t.TCPConnect = defaults.TCPConnect
	}
	if t.TLS <= 0 {
		t.TLS = defaults.TLS
	}
	if t.HTTP <= 0 {
		t.HTTP = defaults.HTTP
	}
	return t
}

func tlsErrorMessage(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout or cancelled"
	}
	return "certificate invalid"
}

func httpErrorMessage(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout or cancelled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout or cancelled"
	}
	// net/http exposes TLSHandshakeTimeout as an unexported error type. Its
	// stable standard-library text is inspected only to map it to our fixed,
	// non-sensitive public diagnostic.
	if strings.Contains(err.Error(), "TLS handshake timeout") {
		return "timeout or cancelled"
	}
	return "request failed"
}

func httpContextError(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "timeout or cancelled"
	}
	return httpErrorMessage(err)
}

func tlsResult(id, name string, status Status, message string) Result {
	return Result{ID: id, Name: name, Status: status, Message: message}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
