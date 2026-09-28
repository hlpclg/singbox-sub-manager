//go:build !linux

package realitynode

import "errors"

// procfsReader has no meaningful production implementation on a non-Linux
// build: spec §9.2's gate reads are /proc-specific and this package's
// runtime target is Debian/Ubuntu only (spec §1). It exists purely so
// `GOOS=darwin go build ./...` succeeds without silently degrading —
// calling any method at runtime returns a clear error rather than
// fabricating an answer.
type procfsReader struct{}

var errProcUnsupported = errors.New("realitynode: /proc-based gate reads are only available on linux")

func (procfsReader) TCPListenInode(port int) (inode uint64, ok bool, err error) {
	return 0, false, errProcUnsupported
}

func (procfsReader) PIDHasSocketInode(pid int, inode uint64) (bool, error) {
	return false, errProcUnsupported
}

func (procfsReader) Exe(pid int) (string, error) {
	return "", errProcUnsupported
}
