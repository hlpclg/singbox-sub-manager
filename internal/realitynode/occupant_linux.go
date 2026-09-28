//go:build linux

package realitynode

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// procfsOccupantReader is the production OccupantReader (review finding
// F3, spec §9.3's "提示端口被谁占用"): it finds the IPv4 TCP listener bound
// to host:port in /proc/net/tcp, then scans every process's open file
// descriptors for one holding that socket's inode, and reads its comm
// name. Best effort throughout: any failure to read /proc, or not finding
// a match, reports ok=false rather than an error — callers must never
// treat "couldn't determine the occupant" as harder than the bind failure
// that prompted the lookup in the first place.
type procfsOccupantReader struct{}

// ipv4HexAddr renders host:port the way /proc/net/tcp encodes an IPv4
// local_address: the 4 address bytes as a little-endian 32-bit hex value
// (this is the documented, widely-relied-upon Linux convention — e.g.
// 127.0.0.1 always appears as "0100007F", never "7F000001", on every
// little-endian architecture Go supports, which is all of this project's
// build targets), followed by ':' and the big-endian 16-bit port in hex.
func ipv4HexAddr(host string, port int) (string, bool) {
	switch host {
	case "0.0.0.0":
		return fmt.Sprintf("00000000:%04X", port), true
	case "127.0.0.1":
		return fmt.Sprintf("0100007F:%04X", port), true
	default:
		return "", false
	}
}

func (procfsOccupantReader) FindTCPListenerOwner(host string, port int) (pid int, comm string, ok bool) {
	wantAddr, supported := ipv4HexAddr(host, port)
	if !supported {
		return 0, "", false
	}
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return 0, "", false
	}
	var inode string
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 {
		lines = lines[1:] // skip header
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		if fields[1] == wantAddr && fields[3] == "0A" { // 0A = TCP_LISTEN
			inode = fields[9]
			break
		}
	}
	if inode == "" {
		return 0, "", false
	}

	wantTarget := "socket:[" + inode + "]"
	procEntries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, "", false
	}
	for _, e := range procEntries {
		candidatePID, perr := strconv.Atoi(e.Name())
		if perr != nil {
			continue
		}
		fdDir := fmt.Sprintf("/proc/%d/fd", candidatePID)
		fds, ferr := os.ReadDir(fdDir)
		if ferr != nil {
			continue // process gone, or no permission — not our target
		}
		for _, fd := range fds {
			target, lerr := os.Readlink(fdDir + "/" + fd.Name())
			if lerr != nil {
				continue
			}
			if target == wantTarget {
				name := ""
				if b, cerr := os.ReadFile(fmt.Sprintf("/proc/%d/comm", candidatePID)); cerr == nil {
					name = strings.TrimSpace(string(b))
				}
				return candidatePID, name, true
			}
		}
	}
	return 0, "", false
}
