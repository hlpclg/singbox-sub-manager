package realitynode

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- test doubles ---

type fakeDownloader struct {
	body              []byte
	err               error
	blockUntilCtxDone bool
}

func (f fakeDownloader) Get(ctx context.Context, url string) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.blockUntilCtxDone {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return io.NopCloser(bytes.NewReader(f.body)), nil
}

// buildTarGz builds a gzip'd tar archive from entries, in order.
type tarEntry struct {
	name     string
	content  []byte
	typeflag byte
	linkname string
	size     int64 // overrides len(content) when non-zero, to test a lying header
}

func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0o755,
			Typeflag: e.typeflag,
			Linkname: e.linkname,
		}
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		size := int64(len(e.content))
		if e.size != 0 {
			size = e.size
		}
		hdr.Size = size
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if e.size != 0 {
				// tar requires exactly Size bytes written; when testing an
				// oversized member we don't care about actual content, so
				// stream zero-filled bytes matching the declared size.
				if _, err := io.CopyN(tw, zeroReader{}, size); err != nil {
					t.Fatalf("write filler content: %v", err)
				}
			} else if _, err := tw.Write(e.content); err != nil {
				t.Fatalf("write content: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// zeroReader is an infinite source of zero bytes, used only to build a
// large-but-cheap synthetic tar entry for the "member too large" test case.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func openDirFD(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

const testVersion = "9.9.9"
const testArch = "amd64"

func memberFor(t *testing.T) string { return singboxMemberName(testVersion, testArch) }

// --- TestDownloadMatrix ---

func TestDownloadMatrix(t *testing.T) {
	binContent := []byte("#!/bin/sh\necho fake sing-box\n")

	t.Run("success", func(t *testing.T) {
		dir := t.TempDir()
		dl := openDirFD(t, dir)
		tgz := buildTarGz(t, []tarEntry{
			{name: memberFor(t), content: binContent},
			{name: strings.TrimSuffix(memberFor(t), "/sing-box") + "/LICENSE", content: []byte("license text")},
		})
		want := sha256Hex(tgz)
		res, err := DownloadSingbox(context.Background(), dl, fakeDownloader{body: tgz}, testArch, testVersion, want, nil)
		if err != nil {
			t.Fatalf("DownloadSingbox: %v", err)
		}
		if res.Name != "sing-box" {
			t.Fatalf("Name = %q, want sing-box", res.Name)
		}
		got, err := os.ReadFile(filepath.Join(dir, "sing-box"))
		if err != nil {
			t.Fatalf("read extracted: %v", err)
		}
		if !bytes.Equal(got, binContent) {
			t.Fatalf("extracted content mismatch")
		}
		info, err := os.Stat(filepath.Join(dir, "sing-box"))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
		}
		if _, err := os.Stat(filepath.Join(dir, downloadTmpName)); !os.IsNotExist(err) {
			t.Fatalf("download temp file should be cleaned up, stat err = %v", err)
		}
	})

	t.Run("sha256 mismatch rejected, no managed path change", func(t *testing.T) {
		dir := t.TempDir()
		dl := openDirFD(t, dir)
		tgz := buildTarGz(t, []tarEntry{{name: memberFor(t), content: binContent}})
		_, err := DownloadSingbox(context.Background(), dl, fakeDownloader{body: tgz}, testArch, testVersion, strings.Repeat("0", 64), nil)
		if !errors.Is(err, errSHA256Mismatch) {
			t.Fatalf("err = %v, want errSHA256Mismatch", err)
		}
		assertDirEmpty(t, dir)
	})

	t.Run("extra duplicate member entries rejected", func(t *testing.T) {
		dir := t.TempDir()
		dl := openDirFD(t, dir)
		tgz := buildTarGz(t, []tarEntry{
			{name: memberFor(t), content: binContent},
			{name: memberFor(t), content: []byte("evil replacement")},
		})
		want := sha256Hex(tgz)
		_, err := DownloadSingbox(context.Background(), dl, fakeDownloader{body: tgz}, testArch, testVersion, want, nil)
		if !errors.Is(err, errMemberDuplicate) {
			t.Fatalf("err = %v, want errMemberDuplicate", err)
		}
		assertDirEmpty(t, dir)
	})

	t.Run("symlink member rejected", func(t *testing.T) {
		dir := t.TempDir()
		dl := openDirFD(t, dir)
		tgz := buildTarGz(t, []tarEntry{
			{name: memberFor(t), typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		})
		want := sha256Hex(tgz)
		_, err := DownloadSingbox(context.Background(), dl, fakeDownloader{body: tgz}, testArch, testVersion, want, nil)
		if !errors.Is(err, errMemberWrongType) {
			t.Fatalf("err = %v, want errMemberWrongType", err)
		}
		assertDirEmpty(t, dir)
	})

	t.Run("hardlink member rejected", func(t *testing.T) {
		dir := t.TempDir()
		dl := openDirFD(t, dir)
		tgz := buildTarGz(t, []tarEntry{
			{name: memberFor(t), typeflag: tar.TypeLink, linkname: "some/other/file"},
		})
		want := sha256Hex(tgz)
		_, err := DownloadSingbox(context.Background(), dl, fakeDownloader{body: tgz}, testArch, testVersion, want, nil)
		if !errors.Is(err, errMemberWrongType) {
			t.Fatalf("err = %v, want errMemberWrongType", err)
		}
		assertDirEmpty(t, dir)
	})

	t.Run("member too large rejected", func(t *testing.T) {
		dir := t.TempDir()
		dl := openDirFD(t, dir)
		tgz := buildTarGz(t, []tarEntry{
			{name: memberFor(t), content: binContent, size: maxTarballBytes + 1},
		})
		want := sha256Hex(tgz)
		_, err := DownloadSingbox(context.Background(), dl, fakeDownloader{body: tgz}, testArch, testVersion, want, nil)
		if !errors.Is(err, errMemberTooLarge) {
			t.Fatalf("err = %v, want errMemberTooLarge", err)
		}
		assertDirEmpty(t, dir)
	})

	t.Run("member missing rejected", func(t *testing.T) {
		dir := t.TempDir()
		dl := openDirFD(t, dir)
		tgz := buildTarGz(t, []tarEntry{{name: "sing-box-9.9.9-linux-amd64/LICENSE", content: []byte("x")}})
		want := sha256Hex(tgz)
		_, err := DownloadSingbox(context.Background(), dl, fakeDownloader{body: tgz}, testArch, testVersion, want, nil)
		if !errors.Is(err, errMemberNotFound) {
			t.Fatalf("err = %v, want errMemberNotFound", err)
		}
		assertDirEmpty(t, dir)
	})

	t.Run("download timeout", func(t *testing.T) {
		dir := t.TempDir()
		dl := openDirFD(t, dir)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := DownloadSingbox(ctx, dl, fakeDownloader{blockUntilCtxDone: true}, testArch, testVersion, strings.Repeat("0", 64), nil)
		if err == nil {
			t.Fatalf("expected timeout error")
		}
		assertDirEmpty(t, dir)
	})

	t.Run("mid-download cancellation", func(t *testing.T) {
		dir := t.TempDir()
		dl := openDirFD(t, dir)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := DownloadSingbox(ctx, dl, fakeDownloader{blockUntilCtxDone: true}, testArch, testVersion, strings.Repeat("0", 64), nil)
		if err == nil {
			t.Fatalf("expected cancellation error")
		}
		assertDirEmpty(t, dir)
	})

	t.Run("version and sha256 must both be set or both empty", func(t *testing.T) {
		dir := t.TempDir()
		dl := openDirFD(t, dir)
		_, err := DownloadSingbox(context.Background(), dl, fakeDownloader{}, testArch, testVersion, "", nil)
		if !errors.Is(err, errVersionSHAMissing) {
			t.Fatalf("err = %v, want errVersionSHAMissing", err)
		}
	})

	t.Run("pinned defaults used when version/sha256 empty", func(t *testing.T) {
		dir := t.TempDir()
		dl := openDirFD(t, dir)
		pinned, _ := PinnedSHA256("amd64")
		tgz := buildTarGz(t, []tarEntry{{name: singboxMemberName(SingboxVersion, "amd64"), content: binContent}})
		// This tarball's real SHA won't equal the pinned constant (it's a
		// synthetic archive, not the real release), so this only checks
		// that empty inputs select SingboxVersion/PinnedSHA256 rather than
		// erroring on the "must both be set" rule — the mismatch below is
		// expected and itself proves the pinned value was actually used
		// for comparison.
		_, err := DownloadSingbox(context.Background(), dl, fakeDownloader{body: tgz}, "amd64", "", "", nil)
		if !errors.Is(err, errSHA256Mismatch) {
			t.Fatalf("err = %v, want errSHA256Mismatch (comparing against pinned %s)", err, pinned)
		}
	})

	t.Run("unsupported architecture", func(t *testing.T) {
		dir := t.TempDir()
		dl := openDirFD(t, dir)
		_, err := DownloadSingbox(context.Background(), dl, fakeDownloader{}, "mips", "", "", nil)
		if !errors.Is(err, errUnsupportedArch) {
			t.Fatalf("err = %v, want errUnsupportedArch", err)
		}
	})
}

func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("expected dl/ to be empty (no managed path change), found: %v", names)
	}
}

// --- F1: the download phase's own 120-second budget ---

// TestDownloadTimeoutBudget proves (review finding F1) that DownloadSingbox
// requests exactly spec §13.2's 120-second budget, and that this budget —
// not whatever ctx the caller happens to pass — is what terminates a
// download that never completes. context.Background() (no deadline at
// all) is passed as the parent, so a real timeout here can only be
// explained by the injected, substituted-short budget actually firing.
func TestDownloadTimeoutBudget(t *testing.T) {
	dir := t.TempDir()
	dl := openDirFD(t, dir)
	var requested time.Duration
	nt := recordingShortTimeout(&requested, 50*time.Millisecond)

	start := time.Now()
	_, err := DownloadSingbox(context.Background(), dl, fakeDownloader{blockUntilCtxDone: true}, testArch, testVersion, strings.Repeat("0", 64), nt)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected a timeout error")
	}
	if requested != downloadTimeout {
		t.Fatalf("requested timeout = %s, want %s (spec §13.2 \"sing-box 下载 120 秒\")", requested, downloadTimeout)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("DownloadSingbox took %s; its own budget (substituted short here) should have fired, not a real 120s wait", elapsed)
	}
	assertDirEmpty(t, dir)
}

// --- version/check against a real re-exec'd subprocess ---

func TestVerifyVersion(t *testing.T) {
	t.Run("exact token match", func(t *testing.T) {
		bin := buildFakeBinary(t, "version", helperEnvExtra1+"=sing-box version 1.14.1")
		if err := VerifyVersion(context.Background(), ExecRunner{}, bin, "1.14.1"); err != nil {
			t.Fatalf("VerifyVersion: %v", err)
		}
	})

	t.Run("prefix version does not match (1.12.1 vs 1.12.10)", func(t *testing.T) {
		bin := buildFakeBinary(t, "version", helperEnvExtra1+"=sing-box version 1.12.10")
		err := VerifyVersion(context.Background(), ExecRunner{}, bin, "1.12.1")
		if !errors.Is(err, errVersionMismatch) {
			t.Fatalf("err = %v, want errVersionMismatch", err)
		}
	})

	// review finding F1: version's own 10-second budget, proven the same
	// way as TestDownloadTimeoutBudget, using a real hanging subprocess.
	t.Run("own 10-second budget terminates a real hanging subprocess", func(t *testing.T) {
		bin := buildFakeBinary(t, "hang")
		var requested time.Duration
		nt := recordingShortTimeout(&requested, 50*time.Millisecond)
		start := time.Now()
		err := VerifyVersion(context.Background(), ExecRunner{NewTimeout: nt}, bin, "1.14.1")
		elapsed := time.Since(start)
		if err == nil {
			t.Fatalf("expected a timeout error")
		}
		if requested != execTimeout {
			t.Fatalf("requested timeout = %s, want %s (spec §13.2 \"sing-box version 10 秒\")", requested, execTimeout)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("VerifyVersion took %s; its own budget should have fired", elapsed)
		}
	})
}

func TestVerifyConfig(t *testing.T) {
	t.Run("check passes", func(t *testing.T) {
		bin := buildFakeBinary(t, "check_ok")
		if err := VerifyConfig(context.Background(), ExecRunner{}, bin, "/some/config.json"); err != nil {
			t.Fatalf("VerifyConfig: %v", err)
		}
	})

	t.Run("check fails", func(t *testing.T) {
		bin := buildFakeBinary(t, "check_fail")
		if err := VerifyConfig(context.Background(), ExecRunner{}, bin, "/some/config.json"); err == nil {
			t.Fatalf("expected error")
		}
	})

	// review finding F1: check's own 10-second budget, proven the same way
	// as TestDownloadTimeoutBudget, using a real hanging subprocess.
	t.Run("own 10-second budget terminates a real hanging subprocess", func(t *testing.T) {
		bin := buildFakeBinary(t, "hang")
		var requested time.Duration
		nt := recordingShortTimeout(&requested, 50*time.Millisecond)
		start := time.Now()
		err := VerifyConfig(context.Background(), ExecRunner{NewTimeout: nt}, bin, "/some/config.json")
		elapsed := time.Since(start)
		if err == nil {
			t.Fatalf("expected a timeout error")
		}
		if requested != execTimeout {
			t.Fatalf("requested timeout = %s, want %s (spec §13.2 \"sing-box check 10 秒\")", requested, execTimeout)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("VerifyConfig took %s; its own budget should have fired", elapsed)
		}
	})
}

// TestExecRunnerTimeoutReapsChild proves ExecRunner.Run terminates and
// Wait()s a real child that never exits on its own (plan review-focus:
// "exec超时后子进程仍存活...").
func TestExecRunnerTimeoutReapsChild(t *testing.T) {
	bin := buildFakeBinary(t, "hang")
	start := time.Now()
	res := ExecRunner{}.Run(context.Background(), 200*time.Millisecond, bin)
	elapsed := time.Since(start)
	if !res.TimedOut {
		t.Fatalf("res.TimedOut = false, want true (res=%+v)", res)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Run took %s, want it to return promptly after the 200ms timeout", elapsed)
	}
}

// TestExecRunnerKillsProcessGroup proves ExecRunner.Run's timeout path
// kills the whole process group, not just the direct child: a real
// grandchild process holds a TCP port open and would keep holding it if
// only the direct child were killed. After Run returns, the port must be
// immediately bindable again — no fd/port leak (plan review-focus row:
// "exec超时后子进程仍存活、持fd或监听端口").
func TestExecRunnerKillsProcessGroup(t *testing.T) {
	port := freeTCPPort(t)
	bin := buildFakeBinary(t, "spawn_holder", fmt.Sprintf("%s=%d", helperEnvHoldPrt, port))

	res := ExecRunner{}.Run(context.Background(), 300*time.Millisecond, bin)
	if !res.TimedOut {
		t.Fatalf("res.TimedOut = false, want true (res=%+v)", res)
	}

	// Give the killed grandchild a brief, bounded moment to actually
	// release the port at the OS level (SIGKILL delivery/reap is not
	// instantaneous even though Run already Wait()'d the direct child).
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			l.Close()
			return // success: port is free again
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d still held after ExecRunner.Run returned (process-group kill did not reach the grandchild): %v", port, lastErr)
}

// TestExecRunnerWaitDelayPreventsIndefiniteBlock proves review finding
// F6's cmd.WaitDelay fix: a real grandchild that escapes into its own
// process group (so process-group signaling can never reach it) while
// holding the write end of ExecRunner's internal stdout-capture pipe open
// must not be able to make Run hang forever, even after the direct child
// itself has already exited normally. Without WaitDelay, cmd.Wait() blocks
// until every holder of that pipe's write end closes it — which the
// escaped grandchild here never does — so this test's own timeout
// (comfortably longer than execWaitDelay) is what proves the fix: without
// it, this test either hangs until Go's test binary timeout or fails.
func TestExecRunnerWaitDelayPreventsIndefiniteBlock(t *testing.T) {
	bin := buildFakeBinary(t, "escape_holder")
	// The outer per-call timeout is deliberately much larger than
	// execWaitDelay: the direct child ("escape_holder") exits almost
	// immediately on its own (well within this budget), so if Run still
	// takes roughly execWaitDelay to return, that can only be explained
	// by WaitDelay's own countdown (started at that early process exit),
	// not by this outer timeout firing.
	const outerTimeout = 30 * time.Second
	start := time.Now()
	res := ExecRunner{}.Run(context.Background(), outerTimeout, bin)
	elapsed := time.Since(start)
	if elapsed >= outerTimeout {
		t.Fatalf("Run took the full outer timeout (%s); WaitDelay did not bound cmd.Wait as expected (res=%+v)", elapsed, res)
	}
	if elapsed > execWaitDelay+5*time.Second {
		t.Fatalf("Run took %s (res=%+v); cmd.WaitDelay (%s) should force Wait to return close to that mark, even though an escaped grandchild keeps the stdout pipe open forever", elapsed, res, execWaitDelay)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeTCPPort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("freeTCPPort close: %v", err)
	}
	return port
}

// TestExecRunnerNormalExit checks the non-timeout path still reports
// stdout/stderr/exit code correctly (covered indirectly by
// TestVerifyVersion/TestVerifyConfig above, but this asserts ExecRunner's
// own contract directly too).
func TestExecRunnerNormalExit(t *testing.T) {
	bin := buildFakeBinary(t, "check_fail")
	res := ExecRunner{}.Run(context.Background(), 5*time.Second, bin, "check")
	if res.TimedOut {
		t.Fatalf("unexpected timeout")
	}
	if res.ExitCode != 1 {
		t.Fatalf("ExitCode = %d, want 1", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "simulated check failure") {
		t.Fatalf("Stderr = %q, missing expected message", res.Stderr)
	}
}

// TestExecRunnerNotFound checks the "binary does not exist" path.
func TestExecRunnerNotFound(t *testing.T) {
	res := ExecRunner{}.Run(context.Background(), time.Second, filepath.Join(t.TempDir(), "does-not-exist"))
	if res.Err == nil {
		t.Fatalf("expected error for missing binary")
	}
}
