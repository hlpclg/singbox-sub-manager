//go:build !linux

package realitynode

// procfsOccupantReader has no meaningful production implementation on a
// non-Linux build: this package's runtime target is Debian/Ubuntu only
// (spec §1), and the occupant lookup is /proc-specific. It exists purely
// so `GOOS=darwin go build ./...` succeeds without silently degrading —
// FindTCPListenerOwner always reports ok=false rather than fabricating an
// answer, which is exactly how the Linux implementation also reports "an
// occupant could not be determined" (this lookup is best-effort by
// design, spec §9.3), so no caller needs a platform-specific code path.
type procfsOccupantReader struct{}

func (procfsOccupantReader) FindTCPListenerOwner(host string, port int) (pid int, comm string, ok bool) {
	return 0, "", false
}
