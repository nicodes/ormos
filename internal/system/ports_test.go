//go:build unix

package system

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// listPorts runs handleListPorts against a live system whose policy file
// (when policyJSON is non-empty) is written into a fresh config dir, and
// returns the decoded port list.
func listPorts(t *testing.T, policyJSON string) []int {
	t.Helper()
	dir := withTempConfigDir(t)
	if policyJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, "policy.json"), []byte(policyJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d := &system{terminals: make(map[string]*terminalSession), audit: newAuditor()}
	var buf bytes.Buffer
	d.handleListPorts(&buf)
	var ports []int
	if err := json.Unmarshal(buf.Bytes(), &ports); err != nil {
		t.Fatalf("list ports output did not decode: %v (%q)", err, buf.String())
	}
	return ports
}

func containsPort(ports []int, port int) bool {
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

// The listing tells the relay which services this machine runs, so a port the
// machine would refuse to dial must not show up in it either.
func TestListPortsFilteredThroughPolicy(t *testing.T) {
	if runtime.GOOS != "linux" {
		// listeningPorts reads /proc/net/tcp*, so off Linux it reports nothing
		// and every assertion below would pass by finding an empty list. A
		// vacuous green is worse than a skip: it would claim this is checked on
		// macOS when the feature it checks does not run there at all. The gap
		// itself is filed separately.
		t.Skip("listeningPorts parses /proc/net/tcp*; there is nothing to filter off Linux")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot open a loopback listener: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if got := listPorts(t, ""); !containsPort(got, port) {
		t.Fatalf("with no policy the listing should contain the test listener :%d (got %v)", port, got)
	}
	if got := listPorts(t, fmt.Sprintf(`{"deniedPorts": [%d]}`, port)); containsPort(got, port) {
		t.Fatalf("deniedPorts must hide :%d from the listing", port)
	}
	if got := listPorts(t, `{"allowedPorts": [3000]}`); containsPort(got, port) {
		t.Fatalf("an authoritative allowedPorts must hide everything else (:%d present)", port)
	}
}

// A policy that cannot be read denies everything else on this machine; the
// port listing must not be the one place that still answers.
func TestListPortsUnreadablePolicyDisclosesNothing(t *testing.T) {
	dir := withTempConfigDir(t)
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &system{terminals: make(map[string]*terminalSession), audit: newAuditor()}
	var buf bytes.Buffer
	d.handleListPorts(&buf)
	if got := strings.TrimSpace(buf.String()); got != "[]" {
		t.Fatalf("unreadable policy listing = %s, want []", got)
	}

	log, err := os.ReadFile(filepath.Join(dir, "sessions.log"))
	if err != nil {
		t.Fatalf("the request was not audited: %v", err)
	}
	var entry auditEntry
	if err := json.Unmarshal(bytes.TrimSpace(log), &entry); err != nil {
		t.Fatalf("audit line did not decode: %v", err)
	}
	if entry.Event != "list-ports" || entry.Allowed {
		t.Fatalf("audit entry = %+v, want a denied list-ports", entry)
	}
}
