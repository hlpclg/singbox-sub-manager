// Package realitynode: download.go implements design spec §6 (private
// sing-box binary) and the exec-lifecycle primitives §9 and §7's
// install/reinstall steps build on: a downloader for the pinned release
// tarball, extraction of the single sing-box member into a caller-owned
// transaction-area directory (accessed only through its already-open
// directory fd, never a bypassable string path — plan Task 6's "经目录 fd，
// 不用字符串路径绕过"), and a hard-timeout, whole-process-group-reaping
// external command runner reused by systemd.go and selfcheck.go.
package realitynode

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// SingboxVersion is the compiled-in pinned sing-box release (implementation
// plan "固定上游依赖": 1.14.1, verified against real assets and behaviour in
// Task 1 — docs/reality-node-validation.md).
const SingboxVersion = "1.14.1"

// singboxSHA256 maps a Go GOARCH value ("amd64"/"arm64", spec §1's
// supported architectures) to the pinned release tarball's SHA256
// (implementation plan "固定上游依赖").
var singboxSHA256 = map[string]string{
	"amd64": "12cb2816b52febb356f6a885b740cc8758c3f30b8ae0ca8edba80f0d2d35343f",
	"arm64": "6060b42fa84c5dcaeae1799af7f61b0f1ae4855d9d5ddc9e02baba17154b3ae2",
}

// PinnedSHA256 returns the compiled-in SHA256 for goarch, and ok=false for
// any architecture other than "amd64"/"arm64" (spec §1: only these two are
// supported).
func PinnedSHA256(goarch string) (sha string, ok bool) {
	sha, ok = singboxSHA256[goarch]
	return sha, ok
}

const (
	// maxTarballBytes bounds both the raw download and the extracted member
	// (spec §6 step 2: "大小上限 200 MB"). Reads are bounded via
	// io.LimitReader so a malicious or corrupt stream cannot exhaust memory
	// or disk before the cap is enforced.
	maxTarballBytes = 200 << 20

	// downloadTimeout is spec §13.2's "sing-box 下载 120 秒".
	downloadTimeout = 120 * time.Second
	// execTimeout is spec §13.2's "每次 sing-box check、sing-box version 10
	// 秒".
	execTimeout = 10 * time.Second
	// systemctlTimeout is spec §13.2's "每次 systemctl 调用 15 秒".
	systemctlTimeout = 15 * time.Second

	downloadTmpName = ".download.tmp"
)

func singboxAssetName(version, goarch string) string {
	return fmt.Sprintf("sing-box-%s-linux-%s.tar.gz", version, goarch)
}

func singboxMemberName(version, goarch string) string {
	return fmt.Sprintf("sing-box-%s-linux-%s/sing-box", version, goarch)
}

func singboxDownloadURL(version, goarch string) string {
	return fmt.Sprintf("https://github.com/SagerNet/sing-box/releases/download/v%s/%s", version, singboxAssetName(version, goarch))
}

// Downloader fetches a URL's response body. Production is HTTPDownloader;
// tests inject a fake so no test needs real network access (spec §14.1:
// "下载器...都通过接口注入").
type Downloader interface {
	Get(ctx context.Context, url string) (io.ReadCloser, error)
}

// HTTPDownloader is the production Downloader.
type HTTPDownloader struct {
	Client *http.Client
}

func (d HTTPDownloader) Get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := d.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("realitynode: download %s: unexpected status %s", url, resp.Status)
	}
	return resp.Body, nil
}

// CommandResult is the outcome of an external command run through a
// CommandRunner.
type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// TimedOut is true when Run returned because timeout (or ctx) elapsed
	// before the process exited on its own; the process group was still
	// killed and reaped before Run returned (see ExecRunner.Run).
	TimedOut bool
	// Err is set when the command could not be started, or when it was
	// killed due to timeout/cancellation. It is nil for an ordinary nonzero
	// exit (see ExitCode instead), matching internal/health's CommandResult
	// convention.
	Err error
}

// CommandRunner runs an external command under a hard per-call timeout.
// Every implementation must guarantee that, by the time Run returns, the
// whole process (and, in production, its whole process group) has been
// killed and Wait'd — a child that outlives Run cannot be left holding a
// port or file descriptor (implementation plan review-focus table: "exec
// 超时后子进程仍存活、持 fd 或监听端口"). Production is ExecRunner; tests
// inject a fake for pure unit tests, and use a real re-exec'd helper
// process to prove the lifecycle guarantee itself (see
// download_test.go/selfcheck_test.go's fakeSingboxBinary).
type CommandRunner interface {
	Run(ctx context.Context, timeout time.Duration, name string, args ...string) CommandResult
}

// execWaitDelay bounds how long Cmd.Wait will wait for the stdout/stderr
// pipe-copying goroutines to observe EOF after the direct child has
// exited or been killed (review finding F6). Without it, a grandchild that
// inherited the write end of those pipes and outlives its parent (e.g. one
// spawned by a killed process before the SIGKILL reached it, or one that
// escaped the process group entirely) can make Wait block indefinitely
// even though the direct child is long gone — defeating this package's
// "every call has a hard timeout" invariant (G7). Go 1.20+'s Cmd.WaitDelay
// forces the pipes closed and returns once this elapses.
const execWaitDelay = 5 * time.Second

// ExecRunner is the production CommandRunner. It starts the child in its
// own process group (Setpgid) so that, on timeout or ctx cancellation, a
// single signal to -pid (which, thanks to Setpgid, is also the group's
// pgid) reaches any grandchildren the child may have spawned (e.g. a
// stubborn fake or a real sing-box that forks helpers), and always Wait()s
// before returning so the process table entry is reaped and any port/fd it
// held is released. NewTimeout lets a test observe/override how the
// per-call timeout is applied (review finding F1); nil uses
// context.WithTimeout with the real duration, unchanged from before.
type ExecRunner struct {
	NewTimeout TimeoutFunc
}

func (r ExecRunner) Run(ctx context.Context, timeout time.Duration, name string, args ...string) CommandResult {
	cctx, cancel := timeoutOrDefault(r.NewTimeout)(ctx, timeout)
	defer cancel()

	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = execWaitDelay
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb

	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return CommandResult{Err: err}
		}
		return CommandResult{Err: err, ExitCode: -1}
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case err := <-waitDone:
		res := CommandResult{Stdout: out.String(), Stderr: errb.String()}
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				res.ExitCode = ee.ExitCode()
			} else {
				res.ExitCode = -1
				res.Err = err
			}
		}
		return res
	case <-cctx.Done():
		killProcessGroup(cmd.Process)
		<-waitDone // always Wait before returning: reap the child.
		return CommandResult{
			Stdout: out.String(), Stderr: errb.String(),
			ExitCode: -1, TimedOut: true, Err: cctx.Err(),
		}
	}
}

// killProcessGroup sends SIGKILL to p's whole process group. Every process
// this package starts (ExecRunner and managedProc alike) sets
// SysProcAttr.Setpgid, which is equivalent to calling setpgid(0, 0) in the
// child: the child becomes its own group leader, so its pgid is always
// equal to its own pid. Querying Getpgid(pid) first (as an earlier version
// of this function did) is not just unnecessary but actively wrong under
// concurrency: once the direct child has already been reaped by a
// concurrent Wait, its pid can be recycled by the OS for an unrelated
// process, and Getpgid(pid) would then report that unrelated process's
// group — sending SIGKILL to it. Going straight to -p.Pid never performs
// that racy re-lookup.
func killProcessGroup(p *os.Process) {
	if p == nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
}

// DownloadResult reports where DownloadSingbox left the verified binary.
type DownloadResult struct {
	// Name is the file name (inside the dl directory passed to
	// DownloadSingbox) the extracted, hash- and type-verified sing-box
	// binary was written to, mode 0755. It is not yet version- or
	// config-checked; call VerifyVersion and (once a config exists)
	// VerifyConfig next.
	Name string
	// Version and SHA256 are the values actually used (the pinned default
	// when both inputs were empty, or the caller's explicit override).
	Version string
	SHA256  string
}

var (
	errDownloadTooLarge  = errors.New("realitynode: downloaded tarball exceeds 200 MB limit")
	errMemberTooLarge    = errors.New("realitynode: extracted sing-box member exceeds 200 MB limit")
	errMemberNotFound    = errors.New("realitynode: tarball does not contain the expected sing-box member")
	errMemberDuplicate   = errors.New("realitynode: tarball contains more than one entry for the expected sing-box member")
	errMemberWrongType   = errors.New("realitynode: expected sing-box member is not a regular file")
	errSHA256Mismatch    = errors.New("realitynode: downloaded tarball SHA256 does not match")
	errUnsupportedArch   = errors.New("realitynode: unsupported architecture")
	errVersionSHAMissing = errors.New("realitynode: singbox version and sha256 must both be set, or both left empty")
)

// DownloadSingbox implements spec §6 steps 1-2 (and, via the caller then
// invoking VerifyVersion, step 3): it downloads the sing-box release
// tarball for goarch ("amd64" or "arm64"), verifies its SHA256, and
// extracts the single sing-box binary member into dl — an already-open
// directory fd for the transaction area's dl/ directory (spec §7.1) —
// naming it "sing-box", mode 0755. It never opens, reads or writes any
// path outside dl.
//
// version and sha256Hex select the compiled-in pinned release
// (SingboxVersion / PinnedSHA256) when both are empty; reinstall's
// explicit --singbox-version/--singbox-sha256 pair (spec §6: "缺少任一个
// 都是退出码 2" — that argument-pairing rule is the CLI's job, Task 7；此处
// only enforces "both or neither") goes through the same function with
// both set explicit.
// newTimeout lets a test observe/override how the 120-second download
// budget (spec §13.2) is applied (review finding F1); nil uses
// context.WithTimeout with the real duration, matching prior behavior.
func DownloadSingbox(ctx context.Context, dl *os.File, downloader Downloader, goarch, version, sha256Hex string, newTimeout TimeoutFunc) (DownloadResult, error) {
	if (version == "") != (sha256Hex == "") {
		return DownloadResult{}, errVersionSHAMissing
	}
	if version == "" {
		pinned, ok := PinnedSHA256(goarch)
		if !ok {
			return DownloadResult{}, fmt.Errorf("%w: %q", errUnsupportedArch, goarch)
		}
		version = SingboxVersion
		sha256Hex = pinned
	}
	sha256Hex = strings.ToLower(sha256Hex)

	dirfd := int(dl.Fd())
	url := singboxDownloadURL(version, goarch)

	dctx, cancel := timeoutOrDefault(newTimeout)(ctx, downloadTimeout)
	defer cancel()

	if err := downloadAndVerifySHA(dctx, downloader, url, dirfd, sha256Hex); err != nil {
		return DownloadResult{}, err
	}
	defer unlinkAt(dirfd, downloadTmpName)

	member := singboxMemberName(version, goarch)
	name, err := extractMember(dirfd, member)
	if err != nil {
		return DownloadResult{}, err
	}
	return DownloadResult{Name: name, Version: version, SHA256: sha256Hex}, nil
}

// downloadAndVerifySHA streams url's body into dirfd/downloadTmpName
// (O_EXCL, so a leftover from a previous crashed attempt is never silently
// reused) while hashing it, then compares the digest to wantSHA (spec §6
// step 1). The tmp file is left in place on success; the caller is
// responsible for removing it once extraction is done (defer in
// DownloadSingbox).
func downloadAndVerifySHA(ctx context.Context, downloader Downloader, url string, dirfd int, wantSHA string) (err error) {
	unlinkAt(dirfd, downloadTmpName) // best-effort: clear any stale leftover
	defer func() {
		// On any failure below, the temp file (partial or complete) must
		// not linger: spec §13.3 requires every exit path to clean up
		// temporary state, and a failed download/verify is not a managed
		// path change worth preserving for diagnosis.
		if err != nil {
			unlinkAt(dirfd, downloadTmpName)
		}
	}()

	fd, oerr := unix.Openat(dirfd, downloadTmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0o600)
	if oerr != nil {
		return fmt.Errorf("realitynode: create download temp file: %w", oerr)
	}
	f := os.NewFile(uintptr(fd), downloadTmpName)
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("realitynode: close download temp file: %w", cerr)
		}
	}()

	body, gerr := downloader.Get(ctx, url)
	if gerr != nil {
		return fmt.Errorf("realitynode: download %s: %w", url, gerr)
	}
	defer body.Close()

	hasher := sha256.New()
	limited := io.LimitReader(body, maxTarballBytes+1)
	n, cerr := io.Copy(io.MultiWriter(f, hasher), limited)
	if cerr != nil {
		return fmt.Errorf("realitynode: download %s: %w", url, cerr)
	}
	if n > maxTarballBytes {
		return errDownloadTooLarge
	}
	if serr := f.Sync(); serr != nil {
		return fmt.Errorf("realitynode: sync download temp file: %w", serr)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if got != wantSHA {
		return fmt.Errorf("%w: got %s, want %s", errSHA256Mismatch, got, wantSHA)
	}
	return nil
}

// extractMember opens dirfd/downloadTmpName for reading as a gzip'd tar
// stream, requires exactly one entry named member and that it be a
// regular file no larger than 200 MB (spec §6 step 2: "只提取...这一个普
// 通文件，拒绝符号链接和其他类型，大小上限 200 MB"), and writes its content
// to dirfd/"sing-box" (mode 0755, O_EXCL so it can never silently
// overwrite a pre-existing file).
func extractMember(dirfd int, member string) (name string, err error) {
	fd, oerr := unix.Openat(dirfd, downloadTmpName, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if oerr != nil {
		return "", fmt.Errorf("realitynode: open downloaded tarball: %w", oerr)
	}
	src := os.NewFile(uintptr(fd), downloadTmpName)
	defer src.Close()

	gz, gerr := gzip.NewReader(src)
	if gerr != nil {
		return "", fmt.Errorf("realitynode: open tarball as gzip: %w", gerr)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var content []byte
	found := false
	for {
		hdr, terr := tr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			return "", fmt.Errorf("realitynode: read tarball: %w", terr)
		}
		if hdr.Name != member {
			continue // spec only cares about the one expected member
		}
		if found {
			return "", errMemberDuplicate
		}
		found = true
		if hdr.Typeflag != tar.TypeReg {
			return "", errMemberWrongType
		}
		if hdr.Size > maxTarballBytes {
			return "", errMemberTooLarge
		}
		limited := io.LimitReader(tr, maxTarballBytes+1)
		buf := &bytes.Buffer{}
		n, cerr := io.Copy(buf, limited)
		if cerr != nil {
			return "", fmt.Errorf("realitynode: extract %s: %w", member, cerr)
		}
		if n > maxTarballBytes {
			return "", errMemberTooLarge
		}
		content = buf.Bytes()
	}
	if !found {
		return "", errMemberNotFound
	}

	const finalName = "sing-box"
	unlinkAt(dirfd, finalName) // defensive: extraction only ever runs once per fresh dl/, but never silently reuse stale content
	ofd, oerr := unix.Openat(dirfd, finalName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0o755)
	if oerr != nil {
		return "", fmt.Errorf("realitynode: create %s: %w", finalName, oerr)
	}
	out := os.NewFile(uintptr(ofd), finalName)
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("realitynode: close %s: %w", finalName, cerr)
		}
	}()
	if _, werr := out.Write(content); werr != nil {
		return "", fmt.Errorf("realitynode: write %s: %w", finalName, werr)
	}
	if cerr := out.Chmod(0o755); cerr != nil {
		return "", fmt.Errorf("realitynode: chmod %s: %w", finalName, cerr)
	}
	if serr := out.Sync(); serr != nil {
		return "", fmt.Errorf("realitynode: sync %s: %w", finalName, serr)
	}
	return finalName, nil
}

func unlinkAt(dirfd int, name string) {
	err := unix.Unlinkat(dirfd, name, 0)
	_ = err // best-effort cleanup; ENOENT (already absent) is the common, fine case
}

var errVersionMismatch = errors.New("realitynode: sing-box version does not match")

// VerifyVersion implements spec §6 step 3: run "<binPath> version" and
// require the first line's whitespace-split tokens to contain wantVersion
// exactly — never a substring match, so "1.12.1" is never satisfied by a
// binary reporting "1.12.10" (spec's explicit example).
func VerifyVersion(ctx context.Context, runner CommandRunner, binPath, wantVersion string) error {
	res := runner.Run(ctx, execTimeout, binPath, "version")
	if res.Err != nil {
		return fmt.Errorf("realitynode: run %s version: %w", binPath, res.Err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("realitynode: %s version exited %d: %s", binPath, res.ExitCode, res.Stderr)
	}
	firstLine, _, _ := strings.Cut(res.Stdout, "\n")
	for _, tok := range strings.Fields(firstLine) {
		if tok == wantVersion {
			return nil
		}
	}
	return fmt.Errorf("%w: %q does not contain token %q", errVersionMismatch, firstLine, wantVersion)
}

// VerifyConfig implements spec §6 step 4 / §8.1 step 4: run
// "<binPath> check -c <configPath>" and require exit code 0.
func VerifyConfig(ctx context.Context, runner CommandRunner, binPath, configPath string) error {
	res := runner.Run(ctx, execTimeout, binPath, "check", "-c", configPath)
	if res.Err != nil {
		return fmt.Errorf("realitynode: run %s check: %w", binPath, res.Err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("realitynode: %s check -c %s exited %d: %s", binPath, configPath, res.ExitCode, res.Stderr)
	}
	return nil
}
