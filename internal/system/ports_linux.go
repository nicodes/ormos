//go:build linux && !android

package system

import (
	"bufio"
	"os"
	"sort"
	"strconv"
	"strings"
)

// listeningPorts returns the set of TCP ports the host is currently listening
// on via a loopback or wildcard address. The Linux implementation intentionally
// retains the established /proc parser and its best-effort read behavior.
func listeningPorts() ([]int, error) {
	set := map[int]struct{}{}
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		scanProcNet(path, set)
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Ints(out)
	return out, nil
}

// scanProcNet parses a /proc/net/tcp{,6} file, adding LISTEN ports bound to a
// loopback or wildcard local address into set.
func scanProcNet(path string, set map[int]struct{}) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Scan() // header row
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// columns: sl local_address rem_address st ...
		if len(fields) < 4 {
			continue
		}
		if fields[3] != "0A" { // 0A = TCP_LISTEN
			continue
		}
		local := fields[1] // "IP:PORT" in hex
		colon := strings.LastIndexByte(local, ':')
		if colon < 0 {
			continue
		}
		ipHex, portHex := local[:colon], local[colon+1:]
		if !isLoopbackOrWildcard(ipHex) {
			continue
		}
		if port, err := strconv.ParseInt(portHex, 16, 32); err == nil && port > 0 {
			set[int(port)] = struct{}{}
		}
	}
}

// isLoopbackOrWildcard reports whether the hex-encoded local IP (little-endian
// per /proc convention) is 127.0.0.1, 0.0.0.0, ::1, or :: — the addresses that
// are reachable (or trivially so) via a 127.0.0.1 dial.
func isLoopbackOrWildcard(ipHex string) bool {
	switch strings.ToUpper(ipHex) {
	case "0100007F": // 127.0.0.1 (IPv4, little-endian bytes)
		return true
	case "00000000": // 0.0.0.0 (IPv4 wildcard)
		return true
	case "00000000000000000000000001000000": // ::1 (IPv6 loopback)
		return true
	case "00000000000000000000000000000000": // :: (IPv6 wildcard)
		return true
	}
	return false
}
