package system

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// listeningPorts returns the set of TCP ports the host is currently listening on
// via a loopback or wildcard address — i.e. ports reachable by dialing
// 127.0.0.1. Callers disclose this set to the relay (KindListPorts) for
// live-status highlighting, so they must filter it through local policy first
// (see handleListPorts): the listing alone tells the relay which services
// this machine runs. Linux-only (parses /proc/net/tcp*); returns nil
// elsewhere, so on macOS/other the TUI shows no live status.
// TODO(macos): fall back to `lsof -iTCP -sTCP:LISTEN` / netstat.
func listeningPorts() []int {
	set := map[int]struct{}{}
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		scanProcNet(path, set)
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
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

// handleListPorts writes the listening ports the relay may know about, as a
// JSON array, and returns. The listing is itself a disclosure — it enumerates
// the services this machine runs — so each port faces the same proxyAllowed
// decision a dial would, the request is audited, and a policy that cannot be
// read discloses nothing at all.
func (d *system) handleListPorts(stream io.Writer) {
	pol, policyOK := d.livePolicy()
	if !policyOK {
		d.audit.record(auditEntry{Event: "list-ports", Detail: "policy unreadable", Allowed: false})
		if err := json.NewEncoder(stream).Encode([]int{}); err != nil {
			d.logf("list ports encode: %v", err)
		}
		return
	}
	ports := listeningPorts()
	allowed := make([]int, 0, len(ports))
	for _, port := range ports {
		if ok, _ := pol.proxyAllowed(port); ok {
			allowed = append(allowed, port)
		}
	}
	d.audit.record(auditEntry{Event: "list-ports", Allowed: true})
	if err := json.NewEncoder(stream).Encode(allowed); err != nil {
		d.logf("list ports encode: %v", err)
	}
}
