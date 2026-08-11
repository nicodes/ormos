package system

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The agent does whatever the relay asks: open a stream marked "terminal" and it
// spawns a shell; mark one "proxy" and it dials a local port. It cannot tell
// which user is on the other end, or whether a human asked for this at all —
// the relay's checks are the only authorization in the system. That is a
// reasonable division of labour right up until the relay is the thing that is
// compromised, at which point it is a root shell on every paired machine, with
// no local record that it happened.
//
// This file is the agent's own say in the matter: a policy it enforces itself,
// and an append-only log the relay cannot edit.

// policyFile is the optional policy, alongside the CLI's config.
const policyFileName = "policy.json"

// auditFileName is the append-only record of what the relay asked this machine
// to do, next to the config so it is easy to find.
const auditFileName = "sessions.log"

const (
	// maxAuditBytes is the size at which the audit log is rolled: it is checked
	// when the file is opened, and the entry that finds the file over it is
	// written to the FRESH log, so the live file's real ceiling is this plus
	// one entry and the pair's is twice that.
	//
	// This is the one file the agent writes on every single action — every
	// terminal attach and reattach, every proxy session, every port listing,
	// every shutdown — and it lives in ~/.config, where nothing rotates
	// anything. Without a bound it grows for the life of the install and the
	// machine owner is the one who eventually notices.
	//
	// A few MiB is tens of thousands of entries: far more history than anyone
	// reads, and small enough that two generations of it are never worth
	// mentioning. maxAuditDetail is what keeps that true when the relay is the
	// one choosing how long an entry is.
	maxAuditBytes = 4 << 20
	// auditRollSuffix names the single generation of history kept across a
	// roll. One generation, not a numbered series: the log is evidence for the
	// machine owner about what happened recently, and a rotation scheme that
	// accumulates files is the unbounded growth this bound exists to stop,
	// spread over more inodes.
	auditRollSuffix = ".1"
	// maxAuditDetail caps the relay-supplied text in one entry. Detail carries
	// the directory the relay asked for when a request was refused, and a
	// StreamHeader may be up to relay.MaxHeaderSize — so without this a single
	// junk request costs tens of kilobytes of log, and a hundred of them roll
	// away everything that came before. It is what keeps "a few MiB is tens of
	// thousands of entries" true when the relay is the one writing them.
	maxAuditDetail = 256
	// maxAuditEntry bounds the whole marshalled line, which is what actually
	// consumes the file. Capping only the raw Detail is not enough: JSON
	// escaping expands, and the relay chooses the content — 256 bytes of quotes
	// marshals to about twice that, and 256 control bytes to about six times,
	// so a raw-only cap made "tens of thousands of entries" true for ASCII and
	// false for anything the relay picked on purpose. Bounding the line makes
	// the count hold for every input.
	maxAuditEntry = 512
	// auditLockWait bounds the whole open, lock and roll. record holds a.mu
	// across it and sits on the synchronous path that sets up every stream, so
	// this is a stall every terminal attach and proxy dial in the process pays
	// under contention. Short on purpose: missing the lock only defers a roll.
	auditLockWait = 100 * time.Millisecond
)

// policy is what this machine will agree to, independent of what the relay
// says. The zero value — no policy file — permits what the agent has always
// permitted, so an existing install behaves identically until it opts in.
type policy struct {
	// AllowedRoots restricts where terminals may be rooted. Empty means any
	// directory. Entries may use ~ and are matched against the resolved path, so
	// a terminal can be confined to ~/code even if the relay asks for ~/.ssh.
	AllowedRoots []string `json:"allowedRoots"`
	// TerminalsDisabled refuses terminal streams outright — useful for a machine
	// that should only ever serve port previews.
	TerminalsDisabled bool `json:"terminalsDisabled"`
	// AllowedPorts is the set of local ports this machine will let the relay
	// reach. When non-empty it is authoritative: nothing outside it is dialled,
	// whatever the relay says. When empty, the built-in guard below applies.
	AllowedPorts []int `json:"allowedPorts"`
	// DeniedPorts is never dialled, even if it appears in AllowedPorts.
	DeniedPorts []int `json:"deniedPorts"`
}

// sensitivePorts are never proxied unless local policy names them explicitly.
// Not an exhaustive list of dangerous things — an exhaustive list is not
// possible — just the services whose exposure is worst and which no dev server
// is ever legitimately running on.
var sensitivePorts = map[int]bool{
	1433:  true, // mssql
	2375:  true, // docker, unencrypted
	2376:  true, // docker, tls
	2379:  true, // etcd client
	2380:  true, // etcd peer
	3306:  true, // mysql
	3389:  true, // rdp
	5432:  true, // postgres
	5900:  true, // vnc
	6379:  true, // redis
	6443:  true, // kubernetes api
	9200:  true, // elasticsearch
	11211: true, // memcached
	27017: true, // mongodb
}

// proxyAllowed reports whether this machine will dial the given local port for
// the relay, and why not when it will not.
//
// This is the agent's own answer, decided from the local policy file alone. It
// has to be: the allowlist the agent fetches from the relay is not a defence
// against the relay, because a relay that has been taken over does not decline
// to answer — it answers "port 22 is exposed". The two checks are combined, not
// substituted: a port must satisfy both this and the relay's list.
//
// With no allowedPorts configured the built-in guard applies: privileged ports
// and well-known service ports are refused. That is not as good as an explicit
// allowlist and is not meant to be — it is what a machine gets before anyone has
// thought about it, and it keeps the worst case (ssh, the databases) off the
// table without breaking the ordinary one (a dev server on 3000).
func (p policy) proxyAllowed(port int) (bool, string) {
	if port < 1 || port > 65535 {
		return false, "port out of range"
	}
	for _, denied := range p.DeniedPorts {
		if denied == port {
			return false, fmt.Sprintf("port %d is in deniedPorts", port)
		}
	}
	if len(p.AllowedPorts) > 0 {
		for _, allowed := range p.AllowedPorts {
			if allowed == port {
				return true, ""
			}
		}
		return false, fmt.Sprintf("port %d is not in allowedPorts", port)
	}
	if port < 1024 {
		return false, fmt.Sprintf("port %d is privileged; add it to allowedPorts to permit it", port)
	}
	if sensitivePorts[port] {
		return false, fmt.Sprintf("port %d is a well-known service port; add it to allowedPorts to permit it", port)
	}
	return true, ""
}

func policyPath() (string, error) {
	dir, err := ormosDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, policyFileName), nil
}

// livePolicy re-reads the policy file for a decision that is about to be made.
//
// The policy is the machine owner's veto over what the relay may ask for, and a
// veto that needs a process restart to take effect is the wrong shape: someone
// tightening policy because they suspect something is wrong expects it to bite
// now. The file is a few hundred bytes and streams are not opened in a tight
// loop, so re-reading per decision costs nothing worth optimising.
//
// A file that cannot be read or parsed denies everything. At startup that same
// condition exits the process (see newSystem) — but a process that is already
// running and serving must not silently widen its permissions because someone
// fat-fingered the JSON.
func (d *system) livePolicy() (policy, bool) {
	p, err := loadPolicy()
	if err != nil {
		d.logf("refusing everything: local policy is unreadable (%v)", err)
		return policy{}, false
	}
	d.mu.Lock()
	d.policy = p
	d.mu.Unlock()
	return p, true
}

// loadPolicy reads the policy file. A missing file is not an error: it means
// "no local restrictions".
func loadPolicy() (policy, error) {
	var p policy
	path, err := policyPath()
	if err != nil {
		return p, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return p, err
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("parsing %s: %w", path, err)
	}
	return p, nil
}

// terminalAllowed reports whether a terminal may be opened in cwd, and why not
// when it may not. cwd is the already-expanded directory the relay asked for;
// an empty cwd means the shell's default, which is only allowed when no root
// restriction is configured.
func (p policy) terminalAllowed(cwd string) (bool, string) {
	if p.TerminalsDisabled {
		return false, "terminals are disabled by local policy"
	}
	if len(p.AllowedRoots) == 0 {
		return true, ""
	}
	if cwd == "" {
		return false, "local policy restricts terminals to allowedRoots, but no directory was requested"
	}
	target, err := filepath.Abs(cwd)
	if err != nil {
		return false, "could not resolve the requested directory"
	}
	// Resolve symlinks so a link inside an allowed root cannot point out of it.
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	for _, root := range p.AllowedRoots {
		allowed, err := filepath.Abs(expandHome(root))
		if err != nil {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(allowed); err == nil {
			allowed = resolved
		}
		if target == allowed || strings.HasPrefix(target, allowed+string(filepath.Separator)) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("local policy does not allow terminals under %s", cwd)
}

// auditor appends a line per relay-requested action to a local file. It is
// deliberately simple and append-only: its job is to leave evidence on the
// machine the relay is acting upon, so a session nobody asked for is visible
// after the fact.
//
// Append-only, but not unbounded: past maxAuditBytes the file is rolled to a
// single previous generation (see open). Recent history is what this evidence
// is for, and nothing else on the machine would ever have truncated it.
type auditor struct {
	mu   sync.Mutex
	path string
	off  bool // disabled after a write failure, so we don't log on every stream
	// max is the roll threshold, a field only so tests can set a small one:
	// exercising the roll at the real 4 MiB means writing 4 MiB per case.
	// Zero means maxAuditBytes.
	max int64
}

// bound returns the roll threshold in effect.
func (a *auditor) bound() int64 {
	if a.max > 0 {
		return a.max
	}
	return maxAuditBytes
}

func newAuditor() *auditor {
	dir, err := ormosDir()
	if err != nil {
		return &auditor{off: true}
	}
	a := &auditor{path: filepath.Join(dir, auditFileName)}
	a.hardenRolled()
	return a
}

// hardenRolled corrects the mode of the previous generation, once, at startup.
//
// The live log is re-checked on every open and the rename carries that mode
// across, so a rolled file this build produced is already right. What this is
// for is a sessions.log.1 left loose by an older build or a restore: nothing
// ever opens that path again, so without this it stays world-readable until the
// next roll happens to replace it — which may be never on a quiet machine. It
// holds the same session history as the live log and deserves the same mode.
func (a *auditor) hardenRolled() {
	if a.path == "" {
		return
	}
	path := a.path + auditRollSuffix
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		_ = os.Chmod(path, perm&^0o077)
	}
}

type auditEntry struct {
	Time    string `json:"time"`
	Event   string `json:"event"`
	Detail  string `json:"detail,omitempty"`
	Port    int    `json:"port,omitempty"`
	Allowed bool   `json:"allowed"`
}

// truncateRunes shortens s to at most n bytes of ORIGINAL text, cutting on a
// rune boundary, and appends an ellipsis to mark the cut — so the result is up
// to n+3 bytes, not n. The caller's budget has to allow for that. A byte-slice mid-rune leaves U+FFFD sitting next to the
// ellipsis, which reads as corruption in a file whose whole job is evidence.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := 0
	for i := range s {
		if i > n {
			break
		}
		cut = i
	}
	return s[:cut] + "…"
}

// record appends one entry. A write that fails disables the auditor rather than
// interrupting the session — a machine that cannot write its log should still
// work — but the caller's own log line still reports the action.
//
// An OPEN that fails does not latch anything off. Latching was meant for a file
// that cannot be written at all, and applying it to the open turned a transient
// EMFILE or a momentarily full disk into auditing silently disabled for the
// life of the process. Nothing is printed on the failure path, so retrying next
// time costs one cheap failed syscall per action.
func (a *auditor) record(e auditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.off || a.path == "" {
		return
	}
	e.Time = time.Now().UTC().Format(time.RFC3339)
	// The relay chooses part of what lands here — Detail carries the cwd it
	// asked for when that request was refused, and a StreamHeader may be up to
	// relay.MaxHeaderSize. Untruncated, a compromised relay could roll the log
	// twice with a hundred or so junk requests and erase every trace of what it
	// did before that. Bounding the log is what makes it lossy, so the cap on
	// what one entry may cost belongs here with it.
	e.Detail = truncateRunes(e.Detail, maxAuditDetail)
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	// The raw cap does not bound the encoded line, so if escaping blew it past
	// the limit, cut Detail back to fit and encode once more. One retry, not a
	// loop: the second Detail is short enough that even worst-case escaping
	// leaves the line under the cap.
	// The raw cap does not bound the ENCODED line, so if escaping blew it past
	// the limit, cut Detail back and encode again. Halving rather than applying
	// the worst-case escape factor to the whole remainder: the latter is safe
	// but wildly over-cuts, and it over-cuts worst on exactly the input a
	// hostile relay would choose — 256 control bytes left one rune of the
	// directory it asked for, in an entry using 16% of its allowance. This
	// converges in a handful of passes and keeps Detail near the real limit.
	for n := len(e.Detail); len(line) > maxAuditEntry && n > 0; {
		n /= 2
		e.Detail = truncateRunes(e.Detail, n)
		if line, err = json.Marshal(e); err != nil {
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return
	}
	f, err := a.open()
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		a.off = true
	}
}

// open returns the audit file ready to append to, rolling it first when it has
// grown past the bound.
//
// The roll is a rename, so the entry that triggered it is written to the fresh
// file afterwards and nothing is lost at the seam. The previous generation is
// replaced rather than kept alongside: os.Rename over an existing path is
// atomic, so there is never a moment with no history at all.
//
// The whole check-and-roll is done holding an exclusive flock on the descriptor,
// and this is the part that has to be right. a.mu serializes nothing between
// PROCESSES, and two agents sharing one state directory is reachable — --config
// makes it explicit, and nothing stops a second agent on the default path
// either. Without the lock: A stats a full log, renames it to sessions.log.1
// and starts a fresh one; B, which statted the same inode before A's rename,
// then renames A's FRESH log over sessions.log.1, unlinking the real history.
// The result is no history at all and a one-line "previous generation". flock
// is advisory, but both parties here are this same code.
//
// After taking the lock the descriptor is checked against the path: another
// process may have rolled the file while we waited, in which case this
// descriptor points at the rolled-away inode and the entry would be appended to
// the history file rather than the live one. Reopen and try again.
//
// Everything else fails towards writing the entry. A log a little over its
// bound is a much smaller problem than an action the relay took with no local
// record of it, so a stat that fails, a lock that cannot be taken, or a rename
// that is refused (a read-only directory, a permissions change) all fall
// through to appending.
func (a *auditor) open() (*os.File, error) {
	// ONE deadline for the whole call, not a fresh budget per attempt. record
	// holds a.mu across this and sits on the synchronous path that sets up
	// every stream, so per-attempt budgets multiplied into a stall of over a
	// second under contention — bounded, but still a hang the operator feels.
	deadline := time.Now().Add(auditLockWait)
	// Bounded, because every iteration either returns or has just rolled the
	// file, and a path that keeps changing under us is not worth spinning on.
	for range 3 {
		f, locked, err := a.openLocked(deadline)
		if err != nil {
			return nil, err
		}
		st, err := f.Stat()
		if err != nil || st.Size() < a.bound() {
			return f, nil
		}
		// THE ROLL ONLY HAPPENS UNDER THE LOCK. Renaming without it is the
		// round-1 data loss exactly: check-then-rename is not atomic, so a
		// second agent can rename this process's FRESH log over
		// sessions.log.1 and unlink the real history — leaving no history at
		// all and a one-line "previous generation". Appending to a file that
		// is a little over its bound costs nothing that matters; losing the
		// evidence does.
		//
		// So when the lock was not taken — contention that outlasted the
		// deadline, or a filesystem with no working flock — write the entry
		// and leave the roll to a later, uncontended call.
		if !locked {
			return f, nil
		}
		// Still holding the lock. Closing releases it, and the next iteration
		// reopens the now-fresh path and writes the entry there.
		if err := os.Rename(a.path, a.path+auditRollSuffix); err != nil {
			// A rename that cannot succeed will not start succeeding on the
			// next iteration either, and retrying costs three more opens and
			// three more lock waits on every record from here on.
			f.Close()
			f, _, err := a.openLocked(deadline)
			return f, err
		}
		f.Close()
	}
	f, _, err := a.openLocked(deadline)
	return f, err
}

// openLocked opens the audit file for appending, takes an exclusive lock on it,
// and confirms the descriptor is still the file at the path.
//
// O_NOFOLLOW and the regular-file check for the same reason the key file has
// them: a symlink planted at sessions.log would otherwise have its TARGET's
// size decide the roll — point it at anything large and the first entry rolls
// the log — and the rename would then move the planted link into
// sessions.log.1, laundering it into what the README calls a kept generation of
// history. The mode is tightened on the same open, because this file is a
// record of what a possibly-compromised relay asked this machine to do and no
// other local user has any business reading it.
func (a *auditor) openLocked(deadline time.Time) (*os.File, bool, error) {
	for range 4 {
		f, locked, err := a.openLockedOnce(deadline)
		if f != nil || err != nil {
			return f, locked, err
		}
	}
	// Four opens in a row and the path changed under every one of them. Rather
	// than drop the entry in silence — which would contradict the
	// fail-towards-writing policy above — append unlocked. A line in the wrong
	// generation is recoverable; an action the relay took with no record of it
	// is not.
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	return f, false, err
}

// lockFile takes the exclusive lock, without ever blocking indefinitely.
//
// LOCK_NB and a short bounded retry rather than a plain LOCK_EX. A blocking
// flock waits forever, and it would wait while holding a.mu, on the synchronous
// path that sets up every stream — so a second agent sharing this state
// directory that is stopped under a debugger, or stalled on a hung filesystem
// while holding the lock, would take THIS agent's terminal and proxy handling
// down with it. That is a far worse failure than the one the lock prevents.
//
// Giving up costs a deferred roll and nothing else. The caller rolls ONLY when
// this returned true, so a lock that could not be taken means the entry is
// appended to a file that is briefly over its bound and the roll waits for an
// uncontended call. Nothing is lost either way, which is what makes the short
// deadline safe.
//
// EINTR is retried rather than swallowed: a signal arriving mid-call would
// otherwise silently downgrade this to the unlocked behaviour.
func lockFile(f *os.File, deadline time.Time) bool {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		switch {
		case err == nil:
			return true
		case errors.Is(err, unix.EINTR):
			// Does not consume the budget. Go's async preemption delivers
			// SIGURG freely and unix.Flock does not retry internally, so
			// counting attempts would let a signal burst spend the whole
			// allowance in microseconds and silently drop us to unlocked.
			// A wall-clock deadline cannot be spent that way.
		case !errors.Is(err, unix.EWOULDBLOCK):
			return false // a filesystem with no working flock
		default:
			time.Sleep(2 * time.Millisecond)
		}
		if !time.Now().Before(deadline) {
			return false
		}
	}
}

// openLockedOnce returns (nil, false, nil) when the file was rolled out from
// under it and the caller should try again. The bool reports whether the lock
// is actually held, which decides whether the caller may roll.
func (a *auditor) openLockedOnce(deadline time.Time) (*os.File, bool, error) {
	// O_NONBLOCK because O_NOFOLLOW does not stop a FIFO, and opening one for
	// writing blocks until a reader appears — while holding a.mu, on the stream
	// path. The regular-file check below cannot run until the open returns, so
	// without this a fifo here hangs the agent rather than one log write.
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, false, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, false, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, false, fmt.Errorf("%s is not a regular file", a.path)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		_ = f.Chmod(perm &^ 0o077)
	}
	locked := lockFile(f, deadline)
	// Did somebody roll the file while we were waiting for the lock? Then this
	// descriptor is the rolled-away inode and the entry belongs elsewhere.
	if onDisk, err := os.Stat(a.path); err != nil || !os.SameFile(onDisk, st) {
		f.Close()
		return nil, false, nil
	}
	return f, locked, nil
}
