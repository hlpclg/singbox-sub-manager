// Package realitynode: selfcheck.go implements design spec §9.1 (dest
// probe), §9.3 (local Reality data path: E1/E2/E3) and §9.4 (egress probe).
// §9.2's "本机门禁" gate lives in systemd.go (it is built as a
// transaction.Gate, reused by both the forward install/reinstall step and
// rollback recovery — see systemd.go's BuildGate). Every exit path here
// (success, failure, cancellation, each timeout) tears down the temporary
// client process (kill+Wait) and the E3 listener before returning, per
// spec §9.3's "生命周期" paragraph and §13.3's "临时客户端进程、E3监听...在
// 任何退出路径上都要释放或删除".
package realitynode

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hlpclg/singbox-sub-manager/internal/health/remote"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
)

// FailureCategory partitions spec §9's failure space into four mutually
// exclusive buckets, distinguished by type (not error text) so a caller
// maps to an exit code without parsing messages (spec §14.1: "四类失败类
// 别...用类型区分"): CategoryDestUnreachable -> exit 1 (no modification yet,
// so this category is only ever returned by ProbeDest, called before the
// transaction begins); CategoryLocalService / CategoryProtocol -> exit 4
// (roll back); CategoryEgress -> exit 0 (warn only).
type FailureCategory int

const (
	CategoryNone FailureCategory = iota
	CategoryDestUnreachable
	CategoryLocalService
	CategoryProtocol
	CategoryEgress
)

// --- §9.1 dest probe ---

// destProbeTimeout is spec §13.2's "dest 探测 10 秒".
const destProbeTimeout = 10 * time.Second

// DestProbeResult is §9.1's outcome.
type DestProbeResult struct {
	// NegotiatedH2 is whether ALPN negotiated "h2". Spec: informational
	// only, never a failure by itself ("协商结果只作为信息显示；没有协商到
	// h2时告警").
	NegotiatedH2 bool
}

// TLSDialer opens a TLS connection honoring ctx. Production is
// defaultTLSDialer (crypto/tls against the real network); tests inject a
// fake so the default test suite needs no network access (spec §14.1) —
// typically one that ignores addr and dials a local test TLS server
// instead, while still handing it the real *tls.Config so SNI/ALPN
// behaviour is exercised for real.
type TLSDialer func(ctx context.Context, network, addr string, cfg *tls.Config) (*tls.Conn, error)

func defaultTLSDialer(ctx context.Context, network, addr string, cfg *tls.Config) (*tls.Conn, error) {
	d := &tls.Dialer{Config: cfg}
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return conn.(*tls.Conn), nil
}

// ProbeDest implements spec §9.1: TLS 1.3 handshake to <sni>:443 with
// ServerName=sni, system root verification, ALPN offering h2 and
// http/1.1 without requiring h2. Any failure (including timeout) is
// reported as CategoryDestUnreachable. newTimeout lets a test observe/
// override how the 10-second budget (spec §13.2) is applied (review
// finding F1); nil uses context.WithTimeout with the real duration.
func ProbeDest(ctx context.Context, sni string, dialer TLSDialer, newTimeout TimeoutFunc) (DestProbeResult, FailureCategory, error) {
	if dialer == nil {
		dialer = defaultTLSDialer
	}
	cctx, cancel := timeoutOrDefault(newTimeout)(ctx, destProbeTimeout)
	defer cancel()

	cfg := &tls.Config{ServerName: sni, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2", "http/1.1"}}
	conn, err := dialer(cctx, "tcp", net.JoinHostPort(sni, "443"), cfg)
	if err != nil {
		return DestProbeResult{}, CategoryDestUnreachable, fmt.Errorf("realitynode: dest probe %s:443: %w", sni, err)
	}
	defer conn.Close()
	return DestProbeResult{NegotiatedH2: conn.ConnectionState().NegotiatedProtocol == "h2"}, CategoryNone, nil
}

// --- §9.3 local Reality data path / §9.4 egress ---

// DataPathConfig configures RunSelfcheck.
type DataPathConfig struct {
	// ListenPort is E1: the Reality service, already running, listening on
	// 0.0.0.0:ListenPort.
	ListenPort int
	// SelfcheckPort is E3: proxyctl binds 127.0.0.1:SelfcheckPort itself.
	SelfcheckPort int

	SNI        string
	UUID       string
	PrivateKey string // base64 raw-url; public key is derived here, never stored
	ShortID    string

	// ClientBinaryPath is the target manifest's current private sing-box
	// (spec §9.3: "二进制是目标manifest中当前版本的私有sing-box").
	ClientBinaryPath string
	// ClientConfigDir is the transaction area directory the temporary
	// client config is written into (0600, spec §9.3), already open (via
	// os.Open on its real path, so Name() gives a usable path for exec);
	// the caller deletes it with the rest of the transaction area.
	ClientConfigDir *os.File
	// ClientConfigName is the file name to use inside ClientConfigDir.
	ClientConfigName string

	// EgressURL is spec §9.4's probe target. Defaults to
	// "https://www.gstatic.com/generate_204"; tests point this at a local
	// fake so the default test suite needs no network access — this is an
	// injectable knob like the others §14.1 requires, not a change to the
	// production default.
	EgressURL string

	// ClientStartGrace bounds how long RunSelfcheck waits, per E2 attempt,
	// for the temporary client to start listening before treating the
	// attempt as failed (spec §9.3: "客户端因端口被占而启动失败时,换一个端
	// 口重试"). The spec does not fix this figure; production defaults to
	// 2 seconds, tests override it to keep runs fast — this is this Task's
	// own implementation choice, not a spec-mandated timeout, so it is not
	// itself subject to the "fake clock" requirement.
	ClientStartGrace time.Duration

	Listen            func(network, address string) (net.Listener, error)
	FreePort          func() (int, error)
	HTTPClientFactory func(proxyPort int) *http.Client
	RandBytes         func(n int) ([]byte, error)

	DestDialer TLSDialer // for the one dest retest on protocol failure (§9 row 3)

	// NewTimeout lets a test observe/override how the two §13.2 budgets
	// this struct owns (30s local data path, 10s egress) are applied
	// (review finding F1), and is also what makes the local-data-path
	// budget span E2's retries AND the nonce request as a single shared
	// context (review finding F4) — nil uses context.WithTimeout with the
	// real durations.
	NewTimeout TimeoutFunc

	// Occupant identifies who holds a TCP listener, used to explain an E3
	// bind failure (spec §9.3: "提示端口被谁占用", review finding F3). nil
	// uses the production Linux /proc-based reader (a no-op stub on other
	// platforms).
	Occupant OccupantReader
}

// OccupantReader identifies which process currently holds a TCP listener,
// used only to make an E3 bind-failure diagnostic more useful (spec §9.3).
// It is best-effort: any error or "can't determine" reports ok=false, and
// callers must never treat that as a harder failure than the bind error
// itself already is.
type OccupantReader interface {
	FindTCPListenerOwner(host string, port int) (pid int, comm string, ok bool)
}

func (c DataPathConfig) occupant() OccupantReader {
	if c.Occupant != nil {
		return c.Occupant
	}
	return procfsOccupantReader{}
}

func (c DataPathConfig) listen() func(network, address string) (net.Listener, error) {
	if c.Listen != nil {
		return c.Listen
	}
	return net.Listen
}

func (c DataPathConfig) freePort() (int, error) {
	if c.FreePort != nil {
		return c.FreePort()
	}
	return defaultFreePort()
}

func defaultFreePort() (int, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func (c DataPathConfig) httpClient(proxyPort int) *http.Client {
	if c.HTTPClientFactory != nil {
		return c.HTTPClientFactory(proxyPort)
	}
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", proxyPort))
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
}

func (c DataPathConfig) randBytes(n int) ([]byte, error) {
	if c.RandBytes != nil {
		return c.RandBytes(n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (c DataPathConfig) startGrace() time.Duration {
	if c.ClientStartGrace > 0 {
		return c.ClientStartGrace
	}
	return 2 * time.Second
}

func (c DataPathConfig) egressURL() string {
	if c.EgressURL != "" {
		return c.EgressURL
	}
	return "https://www.gstatic.com/generate_204"
}

// SelfcheckOutcome is RunSelfcheck's result.
type SelfcheckOutcome struct {
	Category FailureCategory // CategoryNone on full (9.3+9.4) success
	Err      error

	EgressOK bool
	// EgressCategory is CategoryEgress when the egress probe failed
	// (review finding F9): a caller can see this failure by type without
	// parsing EgressWarn, even though it never changes the overall
	// Category/Err (spec §9 row 4: "只告警,不影响退出码").
	EgressCategory FailureCategory
	EgressWarn     string

	// DestRetest and DestRetestErr are populated only when Category ==
	// CategoryProtocol (spec §9 row 3: "报告中附一次dest复测结果") and the
	// retest was actually attempted (DestRetestSkipped is false).
	DestRetest    *DestProbeResult
	DestRetestErr error
	// DestRetestSkipped is true when RunSelfcheck's own ctx parameter had
	// already ended (cancelled, e.g. SIGINT, or its own deadline exceeded)
	// by the time the retest would run (review finding R1): retesting
	// against an already-dead context would just fail immediately with
	// "context canceled/deadline exceeded" — misreporting a cancellation
	// as a dest-reachability result — and would delay rollback for as
	// long as the retest's own budget, up to 10 seconds (spec §13.3: a
	// pending rollback must not be held up by best-effort diagnostics).
	// The retest is skipped outright in this case, not attempted with a
	// substitute detached context.
	DestRetestSkipped bool

	// E3Occupant* are populated (best effort) only when Category ==
	// CategoryLocalService because binding E3 itself failed (spec §9.3:
	// "提示端口被谁占用", review finding F3).
	E3OccupantFound bool
	E3OccupantPID   int
	E3OccupantComm  string
}

const (
	e2MaxAttempts = 3
	// dataPathRequestTimeout is spec §13.2's "本地数据通路 30 秒".
	dataPathRequestTimeout = 30 * time.Second
	// egressRequestTimeout is spec §13.2's "外网探测 10 秒".
	egressRequestTimeout = 10 * time.Second
)

// RunSelfcheck implements spec §9.3 then, on success, §9.4, reusing the
// same temporary client and E2 port for both. It always tears down the E3
// listener and the temporary client (kill+Wait) before returning,
// regardless of outcome, cancellation, or timeout.
func RunSelfcheck(ctx context.Context, cfg DataPathConfig) SelfcheckOutcome {
	pubKey, err := DerivePublicKey(cfg.PrivateKey)
	if err != nil {
		return SelfcheckOutcome{Category: CategoryProtocol, Err: fmt.Errorf("realitynode: derive public key: %w", err)}
	}

	// E3: proxyctl itself holds this listener for the duration of the
	// selfcheck (spec §9.3's E3 row).
	e3Addr := fmt.Sprintf("127.0.0.1:%d", cfg.SelfcheckPort)
	listener, err := cfg.listen()("tcp4", e3Addr)
	if err != nil {
		out := SelfcheckOutcome{Category: CategoryLocalService, Err: fmt.Errorf("realitynode: bind E3 %s: %w", e3Addr, err)}
		// Best effort (review finding F3, extended by R3): explain who
		// holds the port. A listener bound to the wildcard address
		// 0.0.0.0:<port> also makes 127.0.0.1:<port> unbindable (real OS
		// behavior), so check 127.0.0.1 first, then 0.0.0.0, rather than
		// only ever looking at the specific address we tried to bind.
		// Never treated as a harder failure when neither can be
		// determined.
		occ := cfg.occupant()
		pid, comm, ok := occ.FindTCPListenerOwner("127.0.0.1", cfg.SelfcheckPort)
		if !ok {
			pid, comm, ok = occ.FindTCPListenerOwner("0.0.0.0", cfg.SelfcheckPort)
		}
		if ok {
			out.E3OccupantFound = true
			out.E3OccupantPID = pid
			out.E3OccupantComm = comm
		}
		return out
	}
	defer listener.Close()

	nonce, err := cfg.randBytes(32)
	if err != nil {
		return SelfcheckOutcome{Category: CategoryProtocol, Err: fmt.Errorf("realitynode: generate nonce: %w", err)}
	}
	nonceHex := hex.EncodeToString(nonce)
	pathBytes, err := cfg.randBytes(16)
	if err != nil {
		return SelfcheckOutcome{Category: CategoryProtocol, Err: fmt.Errorf("realitynode: generate nonce path: %w", err)}
	}
	randomPath := "/" + hex.EncodeToString(pathBytes)

	e3srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != randomPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(nonceHex))
	})}
	var e3wg sync.WaitGroup
	e3wg.Add(1)
	go func() {
		defer e3wg.Done()
		_ = e3srv.Serve(listener)
	}()
	defer func() {
		_ = e3srv.Close() // forceful, immediate close of any live connections
		e3wg.Wait()
	}()

	// spec §13.2's "本地数据通路 30 秒" covers the whole phase — E2's up to
	// three start attempts AND the nonce request share this one context,
	// not a fresh budget each (review finding F4): a slow sequence of E2
	// retries must not let the request phase start with a full new 30
	// seconds of its own.
	dpCtx, dpCancel := timeoutOrDefault(cfg.NewTimeout)(ctx, dataPathRequestTimeout)
	defer dpCancel()

	proc, e2Port, catg, err := startClientWithRetry(dpCtx, cfg, pubKey)
	if err != nil {
		return SelfcheckOutcome{Category: catg, Err: err}
	}
	defer proc.stop()

	client := cfg.httpClient(e2Port)
	defer client.CloseIdleConnections()

	nonceURL := fmt.Sprintf("http://127.0.0.1:%d%s", cfg.SelfcheckPort, randomPath)
	if err := fetchExpect(dpCtx, client, nonceURL, nonceHex); err != nil {
		out := SelfcheckOutcome{Category: CategoryProtocol, Err: fmt.Errorf("realitynode: local data path check: %w", err)}
		// review finding R1 (a correction to F9's original fix): the
		// retest must use RunSelfcheck's own ctx parameter — not dpCtx,
		// and not a fully detached context.Background() either. dpCtx may
		// have ended purely because the data-path phase's OWN 30-second
		// budget elapsed while ctx itself is still perfectly alive (e.g.
		// a slow-but-legitimate protocol failure) — in that case the
		// retest must still run, using ctx, so it is genuinely a "recheck
		// dest right now" rather than a detached probe with no relation
		// to why we got here. But when ctx itself has already ended
		// (SIGINT, or the caller's own deadline), retesting against it
		// would only fail immediately with "context canceled/deadline
		// exceeded" and delay a pending rollback for up to the retest's
		// own 10-second budget (spec §13.3) — so in that case the retest
		// is skipped outright, not attempted against a dead or detached
		// context.
		if ctx.Err() != nil {
			out.DestRetestSkipped = true
		} else {
			retest, retestErr := probeDestBestEffort(ctx, cfg)
			out.DestRetest = retest
			out.DestRetestErr = retestErr
		}
		return out
	}

	// §9.4: reuse the same client/E2 for the egress probe, with its own
	// independent 10-second budget (spec §13.2) derived from the original
	// ctx — not dpCtx, which may already be close to its own deadline
	// after a slow-but-successful data-path phase. Failure only warns; it
	// never changes the overall Category or Err (spec §9 row 4), but is
	// still surfaced by type via EgressCategory (review finding F9).
	out := SelfcheckOutcome{Category: CategoryNone}
	egressCtx, cancel := timeoutOrDefault(cfg.NewTimeout)(ctx, egressRequestTimeout)
	defer cancel()
	req, rerr := http.NewRequestWithContext(egressCtx, http.MethodGet, cfg.egressURL(), nil)
	if rerr != nil {
		out.EgressCategory = CategoryEgress
		out.EgressWarn = rerr.Error()
		return out
	}
	resp, gerr := client.Do(req)
	if gerr != nil {
		out.EgressCategory = CategoryEgress
		out.EgressWarn = gerr.Error()
		return out
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		out.EgressCategory = CategoryEgress
		out.EgressWarn = fmt.Sprintf("unexpected status %s", resp.Status)
		return out
	}
	out.EgressOK = true
	return out
}

// probeDestBestEffort re-probes dest (spec §9 row 3: "报告中附一次dest复测
// 结果"). Callers must only invoke this when ctx.Err() is nil (review
// finding R1) — it is RunSelfcheck's own ctx parameter, still alive at the
// call site, not dpCtx and not a detached context.Background(). ProbeDest
// still applies its own 10-second dest budget on top (optionally observed/
// overridden by the same NewTimeout a test injected for the rest of this
// config, for consistency).
func probeDestBestEffort(ctx context.Context, cfg DataPathConfig) (*DestProbeResult, error) {
	res, _, err := ProbeDest(ctx, cfg.SNI, cfg.DestDialer, cfg.NewTimeout)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// fetchExpect issues one GET to target through client and requires status
// 200 with a body equal to want. ctx must already carry whatever budget
// applies (RunSelfcheck passes dpCtx, spec §13.2's shared 30-second local
// data path budget — review finding F4).
func fetchExpect(ctx context.Context, client *http.Client, target string, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	if string(body) != want {
		return fmt.Errorf("unexpected body %q", string(body))
	}
	return nil
}

// startClientWithRetry implements spec §9.3's E2 retry rule: proxyctl picks
// a free port, writes the temporary client config, starts the client
// binary, and waits (up to cfg.startGrace()) for it to start listening. On
// failure (process exits early, or never starts listening in time) it
// retries with a new port up to e2MaxAttempts times; exhausting all
// attempts (for a reason other than ctx itself ending) is
// CategoryLocalService. ctx ending (cancelled, or the shared 30-second
// data-path budget elapsing — review finding F4) is reported as
// CategoryProtocol instead, with ctx.Err() wrapped so errors.Is can still
// identify it (review finding F9) — it is a data-path-level timeout/
// cancellation, not evidence of a local-service port conflict.
func startClientWithRetry(ctx context.Context, cfg DataPathConfig, pubKey string) (*managedProc, int, FailureCategory, error) {
	var lastErr error
	for attempt := 0; attempt < e2MaxAttempts; attempt++ {
		if cerr := ctx.Err(); cerr != nil {
			return nil, 0, CategoryProtocol, fmt.Errorf("realitynode: local data path: %w", cerr)
		}

		port, err := cfg.freePort()
		if err != nil {
			lastErr = err
			continue
		}

		node := nodes.Node{
			Name: "reality-selfcheck", Type: nodes.TypeVlessReality,
			Server: "127.0.0.1", Port: cfg.ListenPort, SNI: cfg.SNI, Enabled: true,
			UUID: cfg.UUID, PublicKey: pubKey, ShortID: cfg.ShortID,
		}
		cfgData, err := remote.GenerateConfig(node, port)
		if err != nil {
			return nil, 0, CategoryProtocol, fmt.Errorf("realitynode: generate temp client config: %w", err)
		}
		if err := writeClientConfig(cfg.ClientConfigDir, cfg.ClientConfigName, cfgData); err != nil {
			return nil, 0, CategoryLocalService, fmt.Errorf("realitynode: write temp client config: %w", err)
		}
		configPath := cfg.ClientConfigDir.Name() + "/" + cfg.ClientConfigName

		proc, err := startProcess(cfg.ClientBinaryPath, "run", "-c", configPath)
		if err != nil {
			lastErr = fmt.Errorf("realitynode: start temp client: %w", err)
			continue
		}
		if waitForPortOpen(ctx, port, cfg.startGrace(), proc) {
			return proc, port, CategoryNone, nil
		}
		proc.stop()
		if cerr := ctx.Err(); cerr != nil {
			return nil, 0, CategoryProtocol, fmt.Errorf("realitynode: local data path: %w", cerr)
		}
		lastErr = fmt.Errorf("realitynode: temp client did not start listening on port %d within %s", port, cfg.startGrace())
	}
	return nil, 0, CategoryLocalService, fmt.Errorf("realitynode: temp client failed after %d attempts: %w", e2MaxAttempts, lastErr)
}

func waitForPortOpen(ctx context.Context, port int, grace time.Duration, proc *managedProc) bool {
	deadline := time.Now().Add(grace)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for {
		conn, err := net.DialTimeout("tcp4", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		select {
		case <-proc.exited():
			return false
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if time.Now().After(deadline) {
				return false
			}
		}
	}
}

// --- managed subprocess (the temporary client) ---

// managedProc is a started external process this package always
// terminates via process-group kill+Wait on every exit path (plan
// review-focus: "exec超时后子进程仍存活、持fd或监听端口"). Stop is
// idempotent.
type managedProc struct {
	cmd      *exec.Cmd
	done     chan struct{}
	stopOnce sync.Once
}

func startProcess(name string, args ...string) (*managedProc, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return &managedProc{cmd: cmd, done: done}, nil
}

// exited returns a channel that closes once the process has exited on its
// own (not via stop()).
func (p *managedProc) exited() <-chan struct{} { return p.done }

func (p *managedProc) stop() {
	p.stopOnce.Do(func() {
		killProcessGroup(p.cmd.Process)
		<-p.done
	})
}

// writeClientConfig writes data to name inside dir (mode 0600, spec §9.3),
// through dir's directory fd rather than a bypassable string path,
// replacing any stale leftover from a previous E2 retry attempt.
func writeClientConfig(dir *os.File, name string, data []byte) (err error) {
	dirfd := int(dir.Fd())
	unlinkAt(dirfd, name)
	fd, oerr := unix.Openat(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0o600)
	if oerr != nil {
		return fmt.Errorf("realitynode: create %s: %w", name, oerr)
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("realitynode: close %s: %w", name, cerr)
		}
	}()
	if _, werr := f.Write(data); werr != nil {
		return fmt.Errorf("realitynode: write %s: %w", name, werr)
	}
	return f.Sync()
}
