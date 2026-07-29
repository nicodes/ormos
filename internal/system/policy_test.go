package system

import (
	"path/filepath"
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
