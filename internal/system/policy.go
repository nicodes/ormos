package system

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
type auditor struct {
	mu   sync.Mutex
	path string
	off  bool // disabled after a write failure, so we don't log on every stream
}

func newAuditor() *auditor {
	dir, err := ormosDir()
	if err != nil {
		return &auditor{off: true}
	}
	return &auditor{path: filepath.Join(dir, auditFileName)}
}

type auditEntry struct {
	Time    string `json:"time"`
	Event   string `json:"event"`
	Detail  string `json:"detail,omitempty"`
	Port    int    `json:"port,omitempty"`
	Allowed bool   `json:"allowed"`
}

// record appends one entry. Failures disable the auditor rather than interrupt
// the session — a machine that cannot write its log should still work — but the
// caller's own log line still reports the action.
func (a *auditor) record(e auditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.off || a.path == "" {
		return
	}
	e.Time = time.Now().UTC().Format(time.RFC3339)
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		a.off = true
		return
	}
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		a.off = true
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		a.off = true
	}
}
