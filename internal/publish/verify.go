package publish

// verify.go: the HTTPS leg of design spec §10.3 step 7 and §10.4.
//
// Verification fetches both subscription documents over HTTPS the way the
// health subscription check reaches them -- a TCP connection to the loopback
// listener (127.0.0.1:443), TLS with the subscription domain as SNI and
// certificate name, TLS 1.2 or newer, system roots -- but, unlike health's
// bool-only "HTTP 200" check, it returns the status code and the SHA-256 of
// the response body so the caller can prove that the bytes Caddy serves are
// the bytes this transaction wrote. The Host header carries the same domain,
// so a response that merely exists on the public internet, or one served by a
// different virtual host on the same box, cannot satisfy it.
//
// health keeps no exported fetch helper, and adding one would change a package
// outside this task's scope; the connection policy is therefore mirrored here
// (the same defaults, via health.DefaultTimeouts) rather than imported.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"github.com/hlpclg/singbox-sub-manager/internal/health"
)

// FetchResult is what a Fetcher learned about one HTTPS response.
type FetchResult struct {
	Status int
	// SHA256 is the lowercase hex SHA-256 of the full response body.
	SHA256 string
}

// Fetcher retrieves https://<domain><urlPath> through the local Caddy
// listener, with domain as TLS SNI and Host. Implementations must honor ctx
// (it carries the per-file 10 second budget of spec §13.2) and release every
// connection and goroutine before returning.
type Fetcher interface {
	Fetch(ctx context.Context, domain, urlPath string) (FetchResult, error)
}

// DefaultLoopbackAddr is health's default LoopbackTLSAddr.
const DefaultLoopbackAddr = "127.0.0.1:443"

// defaultMaxBody bounds how much of a response is hashed. A subscription is a
// few kilobytes; anything near this size is not one.
const defaultMaxBody = 16 << 20

// LoopbackFetcher is the production Fetcher.
type LoopbackFetcher struct {
	// Addr is the TCP address dialed instead of the domain's DNS name;
	// empty means DefaultLoopbackAddr.
	Addr string
	// RootCAs verifies the certificate; nil means the system roots.
	RootCAs *x509.CertPool
	// Timeouts bounds the TCP connect and TLS handshake phases; zero fields
	// take health.DefaultTimeouts. The whole request is additionally bounded
	// by ctx.
	Timeouts health.Timeouts
	// MaxBody overrides the response size cap (0 = 16 MiB).
	MaxBody int64
	// DialContext is a test seam; nil dials with net.Dialer.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (l LoopbackFetcher) timeouts() health.Timeouts {
	t := l.Timeouts
	d := health.DefaultTimeouts()
	if t.TCPConnect <= 0 {
		t.TCPConnect = d.TCPConnect
	}
	if t.TLS <= 0 {
		t.TLS = d.TLS
	}
	return t
}

// Fetch implements Fetcher.
func (l LoopbackFetcher) Fetch(ctx context.Context, domain, urlPath string) (FetchResult, error) {
	if err := ctx.Err(); err != nil {
		return FetchResult{}, err
	}
	t := l.timeouts()
	addr := l.Addr
	if addr == "" {
		addr = DefaultLoopbackAddr
	}
	dial := l.DialContext
	if dial == nil {
		d := &net.Dialer{}
		dial = d.DialContext
	}

	dialCtx, cancelDial := context.WithTimeout(ctx, t.TCPConnect)
	conn, err := dial(dialCtx, "tcp", addr)
	cancelDial()
	if err != nil {
		return FetchResult{}, wrapCtx(ctx, fmt.Errorf("connect to %s: %w", addr, err))
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: domain, RootCAs: l.RootCAs, MinVersion: tls.VersionTLS12})
	defer tlsConn.Close()

	// A parent context without a deadline must still interrupt a blocked
	// read or write: the watcher closes the connection when ctx ends, and
	// Fetch waits for it before returning so no goroutine outlives the call.
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			_ = tlsConn.Close()
		case <-stopWatch:
		}
	}()
	defer func() { close(stopWatch); <-watchDone }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = tlsConn.SetDeadline(deadline)
	}

	hsCtx, cancelHS := context.WithTimeout(ctx, t.TLS)
	err = tlsConn.HandshakeContext(hsCtx)
	cancelHS()
	if err != nil {
		return FetchResult{}, wrapCtx(ctx, fmt.Errorf("TLS handshake with %s: %w", domain, err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+domain+urlPath, nil)
	if err != nil {
		return FetchResult{}, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "proxyctl-publish")
	req.Close = true
	if err := req.Write(tlsConn); err != nil {
		return FetchResult{}, wrapCtx(ctx, fmt.Errorf("send request: %w", err))
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		return FetchResult{}, wrapCtx(ctx, fmt.Errorf("read response: %w", err))
	}
	defer resp.Body.Close()

	max := l.MaxBody
	if max <= 0 {
		max = defaultMaxBody
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return FetchResult{}, wrapCtx(ctx, fmt.Errorf("read body: %w", err))
	}
	if n > max {
		return FetchResult{}, fmt.Errorf("response body exceeds %d bytes", max)
	}
	// A response that raced in while the context was being cancelled (closing
	// the connection makes the peer answer) must not count: a verification
	// that was cancelled or timed out has not succeeded.
	if err := ctx.Err(); err != nil {
		return FetchResult{}, err
	}
	return FetchResult{Status: resp.StatusCode, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// wrapCtx reports the context's error (cancelled / deadline exceeded) in
// preference to the network error the closed connection surfaced as.
func wrapCtx(ctx context.Context, err error) error {
	if ce := ctx.Err(); ce != nil {
		if errors.Is(err, ce) {
			return err
		}
		return fmt.Errorf("%w (%v)", ce, err)
	}
	// The connection deadline is set from ctx's deadline and can fire a hair
	// before ctx's own timer does.
	if errors.Is(err, os.ErrDeadlineExceeded) {
		if _, ok := ctx.Deadline(); ok {
			return fmt.Errorf("%w (%v)", context.DeadlineExceeded, err)
		}
	}
	return err
}

// fileHash is one subscription document and the SHA-256 it must serve.
type fileHash struct {
	name string
	sha  string
}

// verifyHTTPS is §10.3 step 7's HTTPS half: for each file in order, within its
// own 10 second budget (spec §13.2, through the injectable clock), the
// response must be 200 and its body hash must equal the expected one. The
// first mismatch is returned.
func (e *Env) verifyHTTPS(ctx context.Context, lay Layout, want []fileHash) error {
	for _, w := range want {
		if err := ctx.Err(); err != nil {
			return err
		}
		fctx, cancel := e.timeoutFunc()(ctx, e.fileVerifyTimeout())
		res, err := e.fetcher().Fetch(fctx, lay.Domain, "/"+lay.Token+"/"+w.name)
		cancel()
		if err != nil {
			return fmt.Errorf("HTTPS fetch of %s failed: %w", w.name, err)
		}
		if res.Status != http.StatusOK {
			return fmt.Errorf("HTTPS fetch of %s returned status %d, want 200", w.name, res.Status)
		}
		if res.SHA256 != w.sha {
			return fmt.Errorf("HTTPS fetch of %s served other content than was published (body sha256 %s, want %s)", w.name, short(res.SHA256), short(w.sha))
		}
	}
	return nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// reverify is §10.4's advisory HTTPS re-check after a local rollback: the
// expected hashes are the previous publication's, files that did not exist
// before are skipped, and every problem is only a warning -- a Caddy outage
// must not turn a successful local rollback into a failure.
func (e *Env) reverify(ctx context.Context, lay Layout, previous []fileHash) []string {
	if len(previous) == 0 {
		return nil
	}
	var warnings []string
	for _, w := range previous {
		if err := e.verifyHTTPS(ctx, lay, []fileHash{w}); err != nil {
			warnings = append(warnings, fmt.Sprintf("after the rollback, the previous %s could not be confirmed over HTTPS (local state is restored): %v", w.name, err))
		}
	}
	return warnings
}
