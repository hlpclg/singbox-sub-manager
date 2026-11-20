//go:build linux

package realitynode

import (
	"fmt"
	"os"
	"strings"
)

// defaultPortRangeReader reads /proc/sys/net/ipv4/ip_local_port_range
// (spec §8.1), the production PortRangeReader on the only OS this package
// ever runs against in production (spec §1: Debian/Ubuntu). Its file
// content is two whitespace-separated integers, e.g. "32768\t60999\n".
func defaultPortRangeReader() (lo, hi int, err error) {
	const path = "/proc/sys/net/ipv4/ip_local_port_range"
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		return 0, 0, fmt.Errorf("realitynode: read %s: %w", path, rerr)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("realitynode: %s: unexpected content %q", path, string(data))
	}
	if _, serr := fmt.Sscanf(fields[0], "%d", &lo); serr != nil {
		return 0, 0, fmt.Errorf("realitynode: %s: invalid lower bound %q: %w", path, fields[0], serr)
	}
	if _, serr := fmt.Sscanf(fields[1], "%d", &hi); serr != nil {
		return 0, 0, fmt.Errorf("realitynode: %s: invalid upper bound %q: %w", path, fields[1], serr)
	}
	return lo, hi, nil
}
