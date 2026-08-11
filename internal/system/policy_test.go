package system

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// testAuditor returns an auditor over a fresh temp dir with a small bound, so
// the roll can be exercised without writing megabytes per case.
func testAuditor(t *testing.T) *auditor {
	t.Helper()
	return &auditor{path: filepath.Join(t.TempDir(), auditFileName), max: 512}
}

// fillAuditLog writes n bytes into the log, standing in for history already there.
func fillAuditLog(t *testing.T, a *auditor, n int, marker string) {
	t.Helper()
	body := marker + strings.Repeat("x", n-len(marker))
	if err := os.WriteFile(a.path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The audit log is written on every relay-requested action and lives in
// ~/.config, where nothing rotates anything. Unbounded, it grows for the life
// of the install.
func TestAuditLogRollsPastItsBound(t *testing.T) {
	a := testAuditor(t)
	fillAuditLog(t, a, int(a.bound()), "old")
	a.record(auditEntry{Event: "proxy", Port: 3000, Allowed: true})

	// The entry that triggered the roll must survive it: an action taken with
	// no local record of it is the failure this file exists to prevent.
	fresh, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatalf("the log was not recreated after the roll: %v", err)
	}
	if int64(len(fresh)) >= a.bound() {
		t.Fatalf("the log is still %d bytes; it was not rolled", len(fresh))
	}
	if !strings.Contains(string(fresh), `"event":"proxy"`) {
		t.Errorf("the entry that triggered the roll was lost: %q", fresh)
	}
	// A roll is not a write failure, so auditing must still be on.
	if a.off {
		t.Error("the auditor latched off across a successful roll")
	}

	// One generation of history, kept whole.
	rolled, err := os.ReadFile(a.path + auditRollSuffix)
	if err != nil {
		t.Fatalf("the previous generation was not kept: %v", err)
	}
	if !strings.HasPrefix(string(rolled), "old") || int64(len(rolled)) != a.bound() {
		t.Error("the rolled generation is not the file that was there before")
	}
}

// The bound is a threshold, not an approximation: one byte either side of it
// must behave differently, or the constant describes nothing.
func TestAuditLogRollBoundary(t *testing.T) {
	under := testAuditor(t)
	fillAuditLog(t, under, int(under.bound())-1, "under")
	under.record(auditEntry{Event: "proxy", Allowed: true})
	if _, err := os.Stat(under.path + auditRollSuffix); !os.IsNotExist(err) {
		t.Error("a log one byte under the bound must not roll")
	}

	at := testAuditor(t)
	fillAuditLog(t, at, int(at.bound()), "at")
	at.record(auditEntry{Event: "proxy", Allowed: true})
	if _, err := os.Stat(at.path + auditRollSuffix); err != nil {
		t.Errorf("a log exactly at the bound must roll: %v", err)
	}
}

// One generation, not a numbered series: a rotation scheme that accumulates
// files is the same unbounded growth spread over more inodes.
func TestAuditLogKeepsOnlyOneGeneration(t *testing.T) {
	a := testAuditor(t)
	for i, marker := range []string{"first", "second"} {
		fillAuditLog(t, a, int(a.bound()), marker)
		a.record(auditEntry{Event: "proxy", Port: 3000 + i, Allowed: true})
	}

	rolled, err := os.ReadFile(a.path + auditRollSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(rolled), "second") {
		t.Error("the second roll did not replace the first generation")
	}
	if entries, err := filepath.Glob(a.path + ".*"); err != nil || len(entries) != 1 {
		t.Errorf("want exactly one generation of history, got %v (%v)", entries, err)
	}
}

// Under the bound nothing is rolled and nothing is lost: the ordinary case is
// still a plain append.
func TestAuditLogAppendsUnderTheBound(t *testing.T) {
	a := testAuditor(t)
	a.record(auditEntry{Event: "terminal", Allowed: true})
	a.record(auditEntry{Event: "proxy", Port: 3000, Allowed: true})

	data, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Fatalf("want 2 entries appended, got %d: %q", n, data)
	}
	if _, err := os.Stat(a.path + auditRollSuffix); !os.IsNotExist(err) {
		t.Error("a log under the bound must not be rolled")
	}
}

// Two auditors on one path is the shape of two agent processes sharing a state
// directory, which --config makes reachable and nothing prevents on the default
// path. Unsynchronised, one process renames the other's FRESH log over
// sessions.log.1 and the real history is unlinked: no history at all, and a
// one-line "previous generation".
func TestConcurrentAuditorsDoNotDestroyHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, auditFileName)
	for attempt := range 40 {
		a := &auditor{path: path, max: 512}
		b := &auditor{path: path, max: 512}
		body := "history" + strings.Repeat("x", 512-len("history"))
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(path + auditRollSuffix)

		var wg sync.WaitGroup
		wg.Add(2)
		for _, au := range []*auditor{a, b} {
			go func() {
				defer wg.Done()
				au.record(auditEntry{Event: "proxy", Port: 3000, Allowed: true})
			}()
		}
		wg.Wait()

		rolled, err := os.ReadFile(path + auditRollSuffix)
		if err != nil {
			t.Fatalf("attempt %d: the history was not kept at all: %v", attempt, err)
		}
		if !strings.HasPrefix(string(rolled), "history") {
			t.Fatalf("attempt %d: the kept generation is %d bytes of %q, not the history that was there",
				attempt, len(rolled), string(rolled[:min(16, len(rolled))]))
		}
		// Both entries have to survive too -- one of them lands in the rolled
		// file and one in the fresh one, or both in the fresh one, but neither
		// may vanish.
		fresh, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if got := strings.Count(string(rolled)+string(fresh), `"event":"proxy"`); got != 2 {
			t.Fatalf("attempt %d: %d of 2 entries survived the concurrent roll", attempt, got)
		}
	}
}

// The relay chooses part of what lands in Detail, and a StreamHeader may be
// 64 KiB. Untruncated, a hundred junk requests roll the log twice and erase
// what the relay did before them -- so bounding the log would have handed a
// compromised relay a way to flush the evidence.
func TestAuditDetailIsTruncated(t *testing.T) {
	a := testAuditor(t)
	a.record(auditEntry{Event: "terminal", Detail: strings.Repeat("A", 64<<10), Allowed: false})

	data, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > maxAuditDetail*2 {
		t.Errorf("one entry cost %d bytes; the relay can flush the log with a handful of them", len(data))
	}
	if !strings.Contains(string(data), "…") {
		t.Error("the truncation is not marked, so a reader cannot tell the detail was cut")
	}
}

// A rename that cannot happen must not cost the entry, and must not turn
// auditing off: a log a little over its bound is a far smaller problem than an
// action with no record of it.
func TestAuditLogSurvivesARefusedRoll(t *testing.T) {
	dir := t.TempDir()
	a := &auditor{path: filepath.Join(dir, auditFileName), max: 512}
	fillAuditLog(t, a, int(a.bound()), "old")
	// A read-only directory refuses the rename (and any create), but the
	// existing file is still open-able for append.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	a.record(auditEntry{Event: "proxy", Port: 3000, Allowed: true})

	data, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"event":"proxy"`) {
		t.Error("the entry was dropped because the roll could not happen")
	}
	if a.off {
		t.Error("a refused roll must not disable auditing")
	}
}

// A symlink at sessions.log would have its TARGET's size decide the roll --
// point it at anything large and the first entry rolls the log -- and the
// rename would then move the planted link into sessions.log.1.
func TestAuditLogRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "planted")
	if err := os.WriteFile(elsewhere, []byte("not the audit log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, auditFileName)
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	a := &auditor{path: path, max: 512}
	a.record(auditEntry{Event: "proxy", Port: 3000, Allowed: true})

	data, err := os.ReadFile(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"event":"proxy"`) {
		t.Error("the entry was written through a symlink")
	}
}

// The log records what a possibly-compromised relay asked this machine to do.
// A mode loosened out of band must be corrected on the same read that finds it,
// the way identity.key is -- and must not be carried into the rolled file.
func TestAuditLogModeIsCorrected(t *testing.T) {
	a := testAuditor(t)
	fillAuditLog(t, a, int(a.bound()), "old")
	if err := os.Chmod(a.path, 0o644); err != nil {
		t.Fatal(err)
	}
	a.record(auditEntry{Event: "proxy", Port: 3000, Allowed: true})

	for _, p := range []string{a.path, a.path + auditRollSuffix} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if perm := st.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s is %04o; it carries this machine's session history", p, perm)
		}
	}
}

// An open that fails must not latch auditing off for the life of the process.
// Latching was meant for a file that cannot be written at all; applied to the
// open it turned a transient EMFILE or a momentarily full disk into silence
// for every action afterwards.
func TestAuditLogRecoversFromATransientOpenFailure(t *testing.T) {
	dir := t.TempDir()
	a := &auditor{path: filepath.Join(dir, auditFileName), max: 512}

	// Nothing can be created or opened in a directory with no permissions.
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	a.record(auditEntry{Event: "proxy", Port: 3000, Allowed: true})
	if a.off {
		t.Fatal("a failed open disabled auditing permanently")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	a.record(auditEntry{Event: "proxy", Port: 3001, Allowed: true})
	data, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatalf("nothing was written after the condition cleared: %v", err)
	}
	if !strings.Contains(string(data), `"port":3001`) {
		t.Errorf("the entry after recovery was dropped: %q", data)
	}
}
