//go:build !linux

package realitynode

import "errors"

// defaultPortRangeReader has no meaningful production implementation on a
// non-Linux build: this package's runtime targets are Debian/Ubuntu only
// (spec §1). It exists purely so `GOOS=darwin go build ./...` succeeds
// without silently degrading — calling it at runtime returns a clear error
// rather than fabricating a port range.
func defaultPortRangeReader() (lo, hi int, err error) {
	return 0, 0, errors.New("realitynode: /proc/sys/net/ipv4/ip_local_port_range is only available on linux")
}
