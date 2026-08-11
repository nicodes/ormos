package system

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoPolicyAllowsAnything(t *testing.T) {
	var p policy // zero value == no policy file
	for _, cwd := range []string{"", "/etc", "/home/someone/code"} {
		if ok, reason := p.terminalAllowed(cwd); !ok {
			t.Fatalf("terminalAllowed(%q) = false (%s), want true with no policy", cwd, reason)
		}
	}
}

func TestTerminalsCanBeDisabled(t *testing.T) {
	p := policy{TerminalsDisabled: true}
	if ok, _ := p.terminalAllowed("/home/someone/code"); ok {
		t.Fatal("terminals must be refused when disabled by policy")
	}
}

func TestAllowedRootsConfineTerminals(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "project")
	outside := t.TempDir()
	p := policy{AllowedRoots: []string{root}}

	for _, dir := range []string{root, inside} {
		if ok, reason := p.terminalAllowed(dir); !ok {
			t.Fatalf("terminalAllowed(%q) = false (%s), want true", dir, reason)
		}
	}
	for _, dir := range []string{outside, "/etc", ""} {
		if ok, _ := p.terminalAllowed(dir); ok {
			t.Fatalf("terminalAllowed(%q) = true, want false", dir)
		}
	}
}

// A prefix comparison alone would accept a sibling directory whose name merely
// starts with the allowed root.
func TestAllowedRootsRejectSiblingPrefix(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "code")
	sibling := filepath.Join(base, "code-secrets")
	p := policy{AllowedRoots: []string{root}}

	if ok, _ := p.terminalAllowed(sibling); ok {
		t.Fatalf("%q must not be treated as inside %q", sibling, root)
	}
}

// The relay-sourced exposed-port list cannot defend against the relay, so the
// agent needs an answer of its own. These are the cases where "the relay said
// so" must not be enough.
func TestProxyAllowedGuardsSensitivePortsByDefault(t *testing.T) {
	var p policy // no policy file at all

	for _, port := range []int{3000, 5173, 8080, 1024, 49152} {
		if ok, reason := p.proxyAllowed(port); !ok {
			t.Fatalf("port %d is an ordinary dev port and should be dialled: %s", port, reason)
		}
	}
	// ssh, the databases, the container runtimes: nothing a dev server runs on,
	// and the worst things to hand over.
	for _, port := range []int{22, 25, 80, 443, 1023, 3306, 5432, 6379, 27017, 2375, 6443} {
		if ok, _ := p.proxyAllowed(port); ok {
			t.Fatalf("port %d must not be dialled without an explicit local opt-in", port)
		}
	}
	for _, port := range []int{0, -1, 65536} {
		if ok, _ := p.proxyAllowed(port); ok {
			t.Fatalf("port %d is not a port", port)
		}
	}
}

// With allowedPorts set, it is the whole answer — including for ports the
// built-in guard would otherwise refuse.
func TestProxyAllowedHonoursExplicitAllowlist(t *testing.T) {
	p := policy{AllowedPorts: []int{3000, 5432}}

	for _, port := range []int{3000, 5432} {
		if ok, reason := p.proxyAllowed(port); !ok {
			t.Fatalf("port %d is explicitly allowed: %s", port, reason)
		}
	}
	// An ordinary dev port that would pass the default guard is still refused,
	// because the allowlist is authoritative once it exists.
	if ok, _ := p.proxyAllowed(8080); ok {
		t.Fatal("allowedPorts must be exhaustive when set")
	}
}

func TestProxyAllowedDeniedPortsWin(t *testing.T) {
	p := policy{AllowedPorts: []int{3000}, DeniedPorts: []int{3000}}
	if ok, _ := p.proxyAllowed(3000); ok {
		t.Fatal("deniedPorts must override allowedPorts")
	}
}

// The audit log is written on every relay-requested action and lives in
// ~/.config, where nothing rotates anything. Unbounded, it grows for the life
// of the install.
func TestAuditLogRollsPastItsBound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, auditFileName)
	a := &auditor{path: path}

	old := strings.Repeat("x", maxAuditBytes) + "\n"
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	a.record(auditEntry{Event: "proxy", Port: 3000, Allowed: true})

	// The entry that triggered the roll must survive it: an action taken with
	// no local record of it is the failure this file exists to prevent.
	fresh, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the log was not recreated after the roll: %v", err)
	}
	if int64(len(fresh)) >= maxAuditBytes {
		t.Fatalf("the log is still %d bytes; it was not rolled", len(fresh))
	}
	if !strings.Contains(string(fresh), `"event":"proxy"`) {
		t.Errorf("the entry that triggered the roll was lost: %q", fresh)
	}

	// One generation of history, kept whole.
	rolled, err := os.ReadFile(path + auditRollSuffix)
	if err != nil {
		t.Fatalf("the previous generation was not kept: %v", err)
	}
	if string(rolled) != old {
		t.Error("the rolled generation is not the file that was there before")
	}
	// The history the relay must not be able to read stays 0600 across the
	// rename, the same as the live file.
	st, err := os.Stat(path + auditRollSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("rolled log is %04o; it carries the same session history as the live one", perm)
	}
}

// One generation, not a numbered series: a rotation scheme that accumulates
// files is the same unbounded growth spread over more inodes.
func TestAuditLogKeepsOnlyOneGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, auditFileName)
	a := &auditor{path: path}

	for i, marker := range []string{"first", "second"} {
		if err := os.WriteFile(path, []byte(marker+strings.Repeat("x", maxAuditBytes)), 0o600); err != nil {
			t.Fatal(err)
		}
		a.record(auditEntry{Event: "proxy", Port: 3000 + i, Allowed: true})
	}

	rolled, err := os.ReadFile(path + auditRollSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(rolled), "second") {
		t.Error("the second roll did not replace the first generation")
	}
	if entries, err := filepath.Glob(path + ".*"); err != nil || len(entries) != 1 {
		t.Errorf("want exactly one generation of history, got %v (%v)", entries, err)
	}
}

// Under the bound nothing is rolled and nothing is lost: the ordinary case is
// still a plain append.
func TestAuditLogAppendsUnderTheBound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, auditFileName)
	a := &auditor{path: path}

	a.record(auditEntry{Event: "terminal", Allowed: true})
	a.record(auditEntry{Event: "proxy", Port: 3000, Allowed: true})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Fatalf("want 2 entries appended, got %d: %q", n, data)
	}
	if _, err := os.Stat(path + auditRollSuffix); !os.IsNotExist(err) {
		t.Error("a log under the bound must not be rolled")
	}
}
