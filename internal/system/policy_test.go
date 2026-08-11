//go:build linux || darwin

package system

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

// Capping the raw Detail does not cap the LINE, and the relay chooses the
// content: JSON escaping turns 256 quotes into ~2x and 256 control bytes into
// ~6x, so a raw-only cap made the entry-count claim true for ASCII and false
// for anything picked on purpose. The bound has to hold for the worst input.
func TestAuditEntryIsBoundedForEveryInput(t *testing.T) {
	for name, detail := range map[string]string{
		"plain":     strings.Repeat("A", 64<<10),
		"quotes":    strings.Repeat(`"`, 64<<10),
		"controls":  strings.Repeat("\x01", 64<<10),
		"backslash": strings.Repeat(`\`, 64<<10),
		"multibyte": strings.Repeat("日", 64<<10),
	} {
		a := testAuditor(t)
		a.record(auditEntry{Event: "terminal", Detail: detail, Allowed: false})
		data, err := os.ReadFile(a.path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(data) > maxAuditEntry+1 { // +1 for the newline
			t.Errorf("%s: one entry cost %d bytes, over the %d cap", name, len(data), maxAuditEntry)
		}
		// And it must still be one valid JSON object.
		var e auditEntry
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &e); err != nil {
			t.Errorf("%s: the truncated entry is not valid JSON: %v (%q)", name, err, data)
		}
	}
}

// A truncated non-ASCII cwd must not end mid-rune: json.Marshal substitutes
// U+FFFD, which reads as corruption in a file whose whole job is evidence.
func TestAuditDetailTruncatesOnRuneBoundaries(t *testing.T) {
	a := testAuditor(t)
	a.record(auditEntry{Event: "terminal", Detail: strings.Repeat("日", 400), Allowed: false})
	data, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	var e auditEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &e); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(e.Detail, '�') {
		t.Errorf("the detail was cut mid-rune: %q", e.Detail)
	}
}

// A blocking flock would wait forever while holding a.mu, on the synchronous
// path that sets up every stream — so a second agent stopped under a debugger
// while holding the lock would take this agent's terminals down with it. The
// entry is written unlocked instead.
func TestAuditRecordDoesNotBlockOnAHeldLock(t *testing.T) {
	a := testAuditor(t)
	fillAuditLog(t, a, 16, "old")

	// Hold the lock from another descriptor, the way another process would.
	holder, err := os.OpenFile(a.path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		a.record(auditEntry{Event: "proxy", Port: 3000, Allowed: true})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("record blocked on a held lock; every stream in the agent queues behind this")
	}

	data, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"event":"proxy"`) {
		t.Error("the entry was dropped rather than written unlocked")
	}
}

// The roll must never happen without the lock. Renaming unlocked is the round-1
// data loss exactly: check-then-rename is not atomic, so a second agent can
// rename this process's FRESH log over sessions.log.1 and unlink the real
// history. When the lock cannot be taken the entry is still written — to a file
// briefly over its bound — and the roll waits for an uncontended call.
func TestOverBoundLogIsNotRolledWithoutTheLock(t *testing.T) {
	a := testAuditor(t)
	fillAuditLog(t, a, int(a.bound()), "history")

	// Hold the lock the way another agent process would.
	holder, err := os.OpenFile(a.path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	a.record(auditEntry{Event: "proxy", Port: 3000, Allowed: true})

	if _, err := os.Stat(a.path + auditRollSuffix); !os.IsNotExist(err) {
		t.Error("the log was rolled while another owner held the lock; history can be unlinked this way")
	}
	data, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "history") {
		t.Error("the history was replaced")
	}
	if !strings.Contains(string(data), `"event":"proxy"`) {
		t.Error("the entry was dropped rather than appended over the bound")
	}

	// Once the contention clears, the roll happens on the next record.
	holder.Close()
	a.record(auditEntry{Event: "proxy", Port: 3001, Allowed: true})
	rolled, err := os.ReadFile(a.path + auditRollSuffix)
	if err != nil {
		t.Fatalf("the deferred roll never happened: %v", err)
	}
	if !strings.HasPrefix(string(rolled), "history") {
		t.Error("the deferred roll kept the wrong generation")
	}
}

// A FIFO at sessions.log would block the open until a reader appears — while
// holding a.mu, on the path that sets up every stream. The test fails by not
// finishing.
func TestAuditLogFifoDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, auditFileName)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create a fifo here: %v", err)
	}
	a := &auditor{path: path, max: 512}

	done := make(chan struct{})
	go func() {
		a.record(auditEntry{Event: "proxy", Port: 3000, Allowed: true})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("record blocked on a fifo; every stream in the agent queues behind this")
	}
}

// A rolled generation left loose by an older build is only ever replaced, never
// re-opened — so its mode is corrected at the roll, where the path is known.
func TestRolledLogModeIsCorrected(t *testing.T) {
	a := testAuditor(t)
	// A previous generation left world-readable by an older build. Nothing ever
	// opens this path again, so if startup does not correct it, nothing will
	// until a roll happens to replace it -- which on a quiet machine is never.
	if err := os.WriteFile(a.path+auditRollSuffix, []byte("older\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(a.path+auditRollSuffix, 0o644); err != nil {
		t.Fatal(err)
	}
	a.hardenRolled()

	st, err := os.Stat(a.path + auditRollSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("the rolled log is %04o; it carries this machine's session history", perm)
	}
}

// A symlink standing at sessions.log.1 is left alone — hardenRolled corrects
// the file this machine rolled, not whatever a planted link points at.
func TestRolledLogSymlinkIsNotFollowed(t *testing.T) {
	a := testAuditor(t)
	elsewhere := filepath.Join(t.TempDir(), "target.log")
	if err := os.WriteFile(elsewhere, []byte("elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, a.path+auditRollSuffix); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	a.hardenRolled()

	st, err := os.Stat(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o644 {
		t.Errorf("the symlink's target was chmodded to %04o; hardenRolled must not follow links", perm)
	}
}

// The relay chooses Detail, so it also chooses how much of it survives
// truncation. Applying the worst-case escape factor to the whole remainder left
// one rune of the directory the relay asked for, in an entry using a sixth of
// its allowance — a compromised relay could erase the evidence about itself
// while staying inside the bound.
func TestAuditDetailKeepsItsAllowance(t *testing.T) {
	for name, detail := range map[string]string{
		"controls": strings.Repeat("\x01", 64<<10),
		"quotes":   strings.Repeat(`"`, 64<<10),
	} {
		a := testAuditor(t)
		a.record(auditEntry{Event: "terminal", Detail: detail, Allowed: false})
		data, err := os.ReadFile(a.path)
		if err != nil {
			t.Fatal(err)
		}
		var e auditEntry
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &e); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len([]rune(e.Detail)) < 16 {
			t.Errorf("%s: only %d runes of detail survived in a %d-byte entry; the evidence is gone",
				name, len([]rune(e.Detail)), len(data))
		}
	}
}

// The two sides of the allowedRoots comparison must be resolved to the same
// degree. EvalSymlinks resolves nothing when any component is missing, and the
// relay may ask for a directory that does not exist yet -- so an existing root
// reached through a symlink used to be compared against an unresolved target
// and refuse everything under itself. macOS finds this immediately (/var is a
// link to /private/var); on Linux it needs a symlinked home to show up.
func TestAllowedRootsResolveBothSidesEqually(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "via-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	p := policy{AllowedRoots: []string{link}}

	// A directory that does not exist yet, named through the real path, under
	// a root named through the link. Both spellings are the same directory.
	if ok, reason := p.terminalAllowed(filepath.Join(real, "not-created-yet")); !ok {
		t.Errorf("a not-yet-created directory under the allowed root was refused: %s", reason)
	}
	if ok, reason := p.terminalAllowed(filepath.Join(link, "not-created-yet")); !ok {
		t.Errorf("the same directory named through the link was refused: %s", reason)
	}
}

// The other direction: resolving the existing prefix means a link that leaves
// the allowed root is caught even when the path continues past it into
// something that does not exist. Unresolved, that was a plain string prefix of
// the root and was allowed.
func TestAllowedRootsFollowALinkOutOfTheRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	p := policy{AllowedRoots: []string{root}}

	if ok, _ := p.terminalAllowed(filepath.Join(escape, "missing")); ok {
		t.Fatal("a link out of the allowed root must not be followed, even into a path that does not exist")
	}
}
