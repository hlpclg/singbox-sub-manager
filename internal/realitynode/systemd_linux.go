//go:build linux

package realitynode

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// procfsReader is the production ProcReader, reading /proc/net/tcp and
// /proc/<pid>/{fd,exe} directly (spec §9.2). It takes no injectable root by
// design: its only two callers in production are BuildGate's polling loop
// (which needs the real kernel state) — tests exercise the logic through
// the ProcReader interface with a fake instead of pointing this reader at a
// synthetic /proc tree.
type procfsReader struct{}

// TCPListenInode implements ProcReader by scanning /proc/net/tcp for an
// IPv4 (spec §9.2: "0.0.0.0:<listen_port>", "仅IPv4") listening socket
// bound to 0.0.0.0:port.
func (procfsReader) TCPListenInode(port int) (inode uint64, ok bool, err error) {
	data, rerr := os.ReadFile("/proc/net/tcp")
	if rerr != nil {
		return 0, false, fmt.Errorf("realitynode: read /proc/net/tcp: %w", rerr)
	}
	wantAddr := fmt.Sprintf("00000000:%04X", port)
	lines := strings.Split(string(data), "\n")
	for _, line := range lines[1:] { // skip header
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		localAddr := fields[1]
		state := fields[3]
		if localAddr != wantAddr || state != "0A" { // 0A = TCP_LISTEN
			continue
		}
		ino, perr := strconv.ParseUint(fields[9], 10, 64)
		if perr != nil {
			return 0, false, fmt.Errorf("realitynode: /proc/net/tcp: invalid inode %q", fields[9])
		}
		return ino, true, nil
	}
	return 0, false, nil
}

// PIDHasSocketInode implements ProcReader by scanning /proc/<pid>/fd for a
// symlink resolving to "socket:[<inode>]".
func (procfsReader) PIDHasSocketInode(pid int, inode uint64) (bool, error) {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("realitynode: read %s: %w", dir, err)
	}
	want := fmt.Sprintf("socket:[%d]", inode)
	for _, e := range entries {
		target, err := os.Readlink(dir + "/" + e.Name())
		if err != nil {
			continue // fd raced closed between ReadDir and Readlink; not this one
		}
		if target == want {
			return true, nil
		}
	}
	return false, nil
}

// Exe implements ProcReader via /proc/<pid>/exe.
func (procfsReader) Exe(pid int) (string, error) {
	path := fmt.Sprintf("/proc/%d/exe", pid)
	target, err := os.Readlink(path)
	if err != nil {
		return "", fmt.Errorf("realitynode: readlink %s: %w", path, err)
	}
	return target, nil
}
