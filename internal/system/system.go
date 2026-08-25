//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/nicodes/ormos/relay"
)

var terminalSessionRandom = rand.Read

const (
	terminalSessionCreateAttempts = 8
)

var (
	terminalExitReportBackoff    = 500 * time.Millisecond
	terminalExitReportMaxBackoff = 30 * time.Second
)

type relayHTTPError struct {
	statusCode int
	message    string
}

func (e *relayHTTPError) Error() string { return e.message }

// PortStatus is one configured exposed port for this system, with whether it
// is currently being served (a local process is listening on it).
type PortStatus struct {
	Project string
	Port    int
	Label   string
	Live    bool
}

// system maintains the outbound tunnel to the relay and serves streams.
type system struct {
	cfg systemConfig

	mu               sync.Mutex
	connected        bool
	sessions         int
	logs             []string     // ring buffer of recent log lines
	ports            []PortStatus // configured ports across this system's projects
	listening        []int        // host's currently-listening loopback ports (cached)
	echoStderr       bool
	echoWriter       io.Writer
	cancel           context.CancelFunc // stops the whole agent (relay-requested shutdown)
	events           chan struct{}      // relay→TUI "data changed" nudges (coalesced)
	terminalMu       sync.Mutex
	lifecycleMu      sync.Mutex
	terminals        map[string]*terminalSession
	reconcileStateMu sync.Mutex
	reconcileRunning bool
	reconcileAgain   bool
	resetMu          sync.Mutex
	resetDone        bool
	runCtx           context.Context

	policy  policy   // local restrictions the agent enforces itself
	audit   *auditor // append-only record of relay-requested actions
	lastGot []int    // last successfully fetched configured ports (fail-closed fallback)

	// key is this machine's long-lived X25519 private key. Terminal frames are
	// sealed against it, so the server carries them without being able to read
	// them — see relay/seal.go.
	key *ecdh.PrivateKey

	// Test seams for parking execution at the external-action boundaries. They
	// remain nil in production. proxyDialContext defaults to net.Dialer.
	beforeShutdownAction func()
	beforeProxyDial      func()
	afterProxyDial       func()
	beforeTerminalAction func()
	proxyDialContext     func(context.Context, string, string) (net.Conn, error)
	shutdownAckSlots     chan struct{}
	actionNow            func() time.Time
	connectAndServeFn    func(context.Context) (bool, error)
	pollPortsFn          func(context.Context)
}

// setCancel lets the agent shut itself down when the relay asks it to.
func (d *system) setCancel(cancel context.CancelFunc) {
	d.mu.Lock()
	d.cancel = cancel
	d.mu.Unlock()
}

func (d *system) lifecycleContext() context.Context {
	d.mu.Lock()
	ctx := d.runCtx
	d.mu.Unlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// shutdownCancel returns the root cancellation installed by Run. Looking it up
// does not execute the shutdown: the success acknowledgment must cross the
// tunnel first, otherwise cancelling this context can tear down WebSocket/yamux
// before the relay receives the terminal result.
func (d *system) shutdownCancel() context.CancelFunc {
	d.mu.Lock()
	c := d.cancel
	d.mu.Unlock()
	return c
}

const logRing = 200

func newSystem(cfg systemConfig) *system {
	d := &system{
		cfg: cfg, events: make(chan struct{}, 1),
		terminals:        make(map[string]*terminalSession),
		audit:            newAuditor(),
		echoWriter:       os.Stderr,
		shutdownAckSlots: make(chan struct{}, maxAsyncShutdownAckWrites),
	}
	p, err := loadPolicy()
	if err != nil {
		// A malformed policy must not silently degrade into "no policy".
		fmt.Fprintf(os.Stderr, "error: reading local policy: %v\n", err)
		os.Exit(1)
	}
	d.policy = p

	// The terminal key, for the same reason and in the same way: an agent
	// without one cannot seal a frame, and must not start pretending it can.
	k, warnings, err := loadOrCreateKey()
	// Warnings first, and even on the error path: "the key was world-readable"
	// is the most important thing that happened here, and it must not be lost
	// because the read then failed for some other reason.
	for _, w := range warnings {
		// Both destinations, deliberately. The ring is what the dashboard
		// renders, and in TUI mode the alt screen wipes stderr a moment after
		// this runs; stderr is what a headless start shows, and EchoToStderr is
		// not switched on until runSystem, after this function returns.
		d.logf("warning: %s", w)
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: reading terminal key: %v\n", err)
		os.Exit(1)
	}
	d.key = k
	return d
}

// notifyEvent signals (non-blocking, coalescing) that upstream data changed and
// the TUI should refetch. A full buffer already means "refetch pending".
func (d *system) notifyEvent() {
	select {
	case d.events <- struct{}{}:
	default:
	}
}

// Events returns the channel signalled when the relay pushes a data-change nudge.
func (d *system) Events() <-chan struct{} { return d.events }

// EchoToStderr controls whether log lines are also printed to stderr (headless
// mode). The TUI renders the tail of the ring buffer instead, in its ACTIVITY
// pane — see RecentLogs and the TUI's View.
func (d *system) EchoToStderr(v bool) {
	d.mu.Lock()
	d.echoStderr = v
	d.mu.Unlock()
}

func (d *system) logf(format string, args ...any) {
	line := time.Now().Format("15:04:05") + " " + fmt.Sprintf(format, args...)
	d.mu.Lock()
	d.logs = append(d.logs, line)
	if len(d.logs) > logRing {
		d.logs = d.logs[len(d.logs)-logRing:]
	}
	echo := d.echoStderr
	d.mu.Unlock()
	if echo {
		fmt.Fprintln(d.echoWriter, line)
	}
}

// Status is a snapshot of system state for the TUI.
//
// It carries only what a render actually reads. It used to also copy the whole
// log ring and d.ports on every call — 500ms, forever — into a Status.Logs and
// a Status.Ports nothing outside a test ever looked at. The ring now has a real
// consumer and is fetched by the tail through RecentLogs; Status.Ports is gone,
// because the TUI lists projects and ports from the relay's own reply
// (model.projects) and needs Live only to mark which of them are up.
//
// d.ports itself stays: cachedPortConfigured reads it on the proxy port-allow
// path. It is only the per-render COPY of it that had no reader.
type Status struct {
	Connected bool
	Sessions  int
	RelayURL  string
	Live      map[int]bool // host's currently-listening ports (for live highlighting)
}

// Snapshot returns a copy of the current state. Live is built from the cached
// listening-ports set so the TUI needn't rescan /proc on every render tick.
func (d *system) Snapshot() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	live := make(map[int]bool, len(d.listening))
	for _, p := range d.listening {
		live[p] = true
	}
	return Status{
		Connected: d.connected,
		Sessions:  d.sessions,
		RelayURL:  d.cfg.RelayURL,
		Live:      live,
	}
}

// RecentLogs returns the last n log lines, oldest first.
//
// The tail rather than the whole ring: the TUI shows a fixed handful of lines
// and copying the other ~190 twice a second bought nothing. In headless mode
// the same lines go to stderr; in TUI mode this is the only way they are ever
// seen, which is the point — before the ACTIVITY pane existed, "tunnel error:
// ..." was written to a buffer nothing displayed and the dashboard just sat
// there saying offline with no reason given.
func (d *system) RecentLogs(n int) []string {
	if n <= 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if n > len(d.logs) {
		n = len(d.logs)
	}
	if n == 0 {
		return nil
	}
	tail := make([]string, n)
	copy(tail, d.logs[len(d.logs)-n:])
	return tail
}

func (d *system) setConnected(v bool) {
	d.mu.Lock()
	d.connected = v
	d.mu.Unlock()
}

func (d *system) addSession(delta int) {
	d.mu.Lock()
	d.sessions += delta
	d.mu.Unlock()
}

const (
	minBackoff     = time.Second      // initial reconnect backoff
	maxBackoff     = 30 * time.Second // cap on reconnect backoff
	portsPollEvery = 5 * time.Second  // how often port status is refreshed
	portsFetchTO   = 8 * time.Second  // per-call timeout for an on-demand ports fetch
)

// httpClient is the shared client for all relay HTTP calls (provision, ports,
// project/port CRUD). Per-call contexts bound individual requests; this bounds
// the worst case.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// maxRelayResponse caps a relay response body. Real responses are a handful
// of ports or projects — kilobytes — so 1 MiB is generous; it only has to be
// finite, so a relay answering with an endless body cannot make the agent
// buffer without limit (every decode of a relay response goes through here or
// through a LimitReader with this cap).
const maxRelayResponse = 1 << 20

// decodeRelayJSON decodes one bounded JSON response body from the relay.
func decodeRelayJSON(r io.Reader, v any) error {
	return json.NewDecoder(io.LimitReader(r, maxRelayResponse)).Decode(v)
}

// pollPorts periodically fetches this system's configured ports from the
// relay and cross-references them against the locally-listening ports to mark
// each live/idle, for the TUI. Transient errors keep the last-good list.
func (d *system) pollPorts(ctx context.Context) {
	tick := func() {
		// Policy is re-read here too, so an edit that revokes a directory takes
		// effect on sessions already open rather than only on the next attach.
		d.enforcePolicy()

		// Scan the host's listening ports once and cache them (the TUI reads this
		// snapshot instead of rescanning /proc every render tick).
		listen, err := listeningPorts()
		if err != nil {
			d.logf("ports discovery failed: %q", err)
			d.mu.Lock()
			listen = append([]int(nil), d.listening...)
			d.mu.Unlock()
		} else {
			d.mu.Lock()
			d.listening = listen
			d.mu.Unlock()
		}
		live := make(map[int]bool, len(listen))
		for _, p := range listen {
			live[p] = true
		}
		infos, err := d.fetchConfiguredPorts(ctx)
		if err != nil {
			// fetchConfiguredPorts can include the relay's HTTP reason phrase.
			// Sanitize this argument before logf's headless stderr echo; the TUI
			// independently sanitizes the same ring when it renders it.
			d.logf("ports poll failed: %s", sanitizeRelayOutput(err.Error()))
			return
		}
		statuses := make([]PortStatus, 0, len(infos))
		configured := make([]int, 0, len(infos))
		for _, in := range infos {
			statuses = append(statuses, PortStatus{Project: in.Project, Port: in.Port, Label: in.Label, Live: live[in.Port]})
			configured = append(configured, in.Port)
		}
		d.mu.Lock()
		d.ports = statuses
		d.lastGot = configured
		d.mu.Unlock()
	}

	tick()
	t := time.NewTicker(portsPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// fetchConfiguredPorts GETs the system's configured ports from the relay,
// authenticated by the pairing token.
func (d *system) fetchConfiguredPorts(ctx context.Context) ([]relay.PortInfo, error) {
	url := httpBaseFromWS(d.cfg.RelayURL) + "/system/ports"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.cfg.PairingToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("relay returned %s", sanitizeRelayOutput(resp.Status))
	}
	var out struct {
		Ports []relay.PortInfo `json:"ports"`
	}
	if err := decodeRelayJSON(resp.Body, &out); err != nil {
		return nil, err
	}
	return out.Ports, nil
}

// cachedPortConfigured reports whether port is in the last-fetched configured
// set (populated by pollPorts).
func (d *system) cachedPortConfigured(port int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range d.ports {
		if p.Port == port {
			return true
		}
	}
	return false
}

// proxyPortAllowed reports whether the port appears in the set of ports the
// relay says this system has exposed. It consults the cached set first and, on a
// miss, fetches fresh so a just-added port isn't wrongly refused.
//
// NOTE what this is and is not. The list comes from the relay, so it catches a
// relay that is buggy, racing, or out of date. It is NOT a defence against a
// relay that has been taken over: such a relay does not refuse to answer, it
// answers "port 22 is exposed" and this returns true. policy.proxyAllowed is
// the check that does not consult the relay, and both must pass.
//
// A failed fetch falls back to the last list this agent successfully retrieved,
// and denies when it has never retrieved one.
func (d *system) proxyPortAllowed(port int) bool {
	if !relay.ValidPort(port) {
		return false
	}
	if d.cachedPortConfigured(port) {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), portsFetchTO)
	defer cancel()
	infos, err := d.fetchConfiguredPorts(ctx)
	if err != nil {
		d.mu.Lock()
		known, everFetched := d.lastGot, d.lastGot != nil
		d.mu.Unlock()
		if !everFetched {
			d.logf("proxy denied: cannot reach the relay to check exposed ports (:%d)", port)
			return false
		}
		for _, p := range known {
			if p == port {
				return true
			}
		}
		return false
	}
	for _, in := range infos {
		if in.Port == port {
			return true
		}
	}
	return false
}

// relayDo performs a pairing-token-authenticated request to the relay and, for
// non-2xx responses, returns an error carrying the relay's {"error":...} message.
func (d *system) relayDo(ctx context.Context, method, path string, body any) ([]byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	url := httpBaseFromWS(d.cfg.RelayURL) + path
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.cfg.PairingToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxRelayResponse))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return nil, &relayHTTPError{statusCode: resp.StatusCode, message: e.Error}
		}
		return nil, &relayHTTPError{statusCode: resp.StatusCode, message: fmt.Sprintf("relay returned %s", resp.Status)}
	}
	return data, nil
}

// fetchSystemInfo GETs this system's name/hostname/online from the relay.
func (d *system) fetchSystemInfo(ctx context.Context) (*relay.SystemInfo, error) {
	data, err := d.relayDo(ctx, http.MethodGet, "/system/info", nil)
	if err != nil {
		return nil, err
	}
	var info relay.SystemInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// renameSystem sets this system's display name (label).
func (d *system) renameSystem(ctx context.Context, name string) error {
	_, err := d.relayDo(ctx, http.MethodPatch, "/system/info", map[string]string{"name": name})
	return err
}

// fetchProjects lists this system's projects (with ports) from the relay.
func (d *system) fetchProjects(ctx context.Context) ([]relay.ProjectInfo, error) {
	data, err := d.relayDo(ctx, http.MethodGet, "/system/projects", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Projects []relay.ProjectInfo `json:"projects"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

// fetchTerminalSessions lists the persisted terminal tabs for this system.
func (d *system) fetchTerminalSessions(ctx context.Context) ([]relay.TerminalSessionInfo, error) {
	data, err := d.relayDo(ctx, http.MethodGet, "/system/terminal-sessions", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Sessions []relay.TerminalSessionInfo `json:"sessions"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

func (d *system) resetTerminalSessions(ctx context.Context) error {
	_, err := d.relayDo(ctx, http.MethodPost, "/system/terminal-sessions/reset", nil)
	return err
}

func (d *system) reportTerminalExit(recordID string, generation int) {
	if recordID == "" || generation <= 0 || d.cfg.RelayURL == "" {
		return
	}
	go func() {
		ctx := d.lifecycleContext()
		backoff := terminalExitReportBackoff
		for {
			requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, err := d.relayDo(requestCtx, http.MethodPost, "/system/terminal-sessions/"+url.PathEscape(recordID)+"/exit", struct {
				Generation int `json:"generation"`
			}{generation})
			cancel()
			if err == nil {
				return
			}
			var he *relayHTTPError
			if errors.As(err, &he) && (he.statusCode == http.StatusNotFound || he.statusCode == http.StatusConflict) {
				return
			}
			if errors.As(err, &he) && he.statusCode >= 400 && he.statusCode < 500 &&
				he.statusCode != http.StatusRequestTimeout && he.statusCode != http.StatusTooManyRequests {
				d.logf("terminal exit report: %v", err)
				return
			}
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if backoff < terminalExitReportMaxBackoff {
				backoff *= 2
				if backoff > terminalExitReportMaxBackoff {
					backoff = terminalExitReportMaxBackoff
				}
			}
		}
	}()
}

// reconcileTerminalSessions is level-triggered: the durable list is authoritative,
// and the lock serializes overlapping event-triggered fetches.
func (d *system) reconcileTerminalSessions() {
	d.reconcileStateMu.Lock()
	if d.reconcileRunning {
		d.reconcileAgain = true
		d.reconcileStateMu.Unlock()
		return
	}
	d.reconcileRunning = true
	d.reconcileStateMu.Unlock()
	go func() {
		backoff := 100 * time.Millisecond
		for {
			select {
			case <-d.lifecycleContext().Done():
				return
			default:
			}
			d.lifecycleMu.Lock()
			ctx, cancel := context.WithTimeout(d.lifecycleContext(), 8*time.Second)
			rows, err := d.fetchTerminalSessions(ctx)
			cancel()
			doomed := make([]*terminalSession, 0)
			if err == nil {
				valid := make(map[string]relay.TerminalSessionInfo, len(rows))
				for _, row := range rows {
					valid[row.ID] = row
				}
				d.terminalMu.Lock()
				for id, s := range d.terminals {
					row, ok := valid[id]
					if !ok || row.State != relay.TerminalStateRunning || row.Generation != s.generation {
						doomed = append(doomed, s)
					}
				}
				d.terminalMu.Unlock()
			}
			d.lifecycleMu.Unlock()
			for _, s := range doomed {
				s.close()
			}
			if err != nil {
				timer := time.NewTimer(backoff)
				select {
				case <-d.lifecycleContext().Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				if backoff < 2*time.Second {
					backoff *= 2
					if backoff > 2*time.Second {
						backoff = 2 * time.Second
					}
				}
			} else {
				backoff = 100 * time.Millisecond
			}
			d.reconcileStateMu.Lock()
			again := d.reconcileAgain || err != nil
			d.reconcileAgain = false
			if !again {
				d.reconcileRunning = false
				d.reconcileStateMu.Unlock()
				return
			}
			d.reconcileStateMu.Unlock()
		}
	}()
}

// createTerminalSession persists a new terminal tab before any local PTY is
// attached. A transport failure is deliberately not retried: the relay may
// have committed the record before the connection failed. Only an explicit
// duplicate response proves that generating a fresh id is safe.
func (d *system) createTerminalSession(ctx context.Context, projectID string) (relay.TerminalSessionInfo, error) {
	for range terminalSessionCreateAttempts {
		sessionID, err := newTerminalSessionID()
		if err != nil {
			return relay.TerminalSessionInfo{}, fmt.Errorf("generate terminal session id: %w", err)
		}
		data, err := d.relayDo(ctx, http.MethodPost, "/system/terminal-sessions", struct {
			ProjectID string `json:"project_id"`
			SessionID string `json:"session_id"`
		}{ProjectID: projectID, SessionID: sessionID})
		if err == nil {
			var info relay.TerminalSessionInfo
			if err := json.Unmarshal(data, &info); err != nil {
				return relay.TerminalSessionInfo{}, fmt.Errorf("decode terminal session: %w", err)
			}
			if info.ID == "" || info.State != relay.TerminalStateRunning || info.Generation <= 0 || info.SessionID == "" {
				return relay.TerminalSessionInfo{}, fmt.Errorf("create returned invalid terminal session")
			}
			return info, nil
		}
		var httpErr *relayHTTPError
		if !errors.As(err, &httpErr) || httpErr.statusCode != http.StatusConflict {
			return relay.TerminalSessionInfo{}, err
		}
	}
	return relay.TerminalSessionInfo{}, fmt.Errorf("could not allocate a unique terminal session id")
}

func (d *system) restartTerminalSession(ctx context.Context, recordID string) (relay.TerminalSessionInfo, error) {
	data, err := d.relayDo(ctx, http.MethodPost, "/system/terminal-sessions/"+url.PathEscape(recordID)+"/restart", nil)
	if err != nil {
		return relay.TerminalSessionInfo{}, err
	}
	var info relay.TerminalSessionInfo
	if err := json.Unmarshal(data, &info); err == nil && info.ID != "" {
		return info, nil
	}
	var out struct {
		Session relay.TerminalSessionInfo `json:"session"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return relay.TerminalSessionInfo{}, err
	}
	return out.Session, nil
}

func (d *system) deleteTerminalSession(ctx context.Context, recordID string) error {
	_, err := d.relayDo(ctx, http.MethodDelete, "/system/terminal-sessions/"+url.PathEscape(recordID), nil)
	return err
}

func newTerminalSessionID() (string, error) {
	var raw [18]byte // 144 random bits; base64url encodes without punctuation or padding.
	n, err := terminalSessionRandom(raw[:])
	if err != nil {
		return "", err
	}
	if n != len(raw) {
		return "", io.ErrUnexpectedEOF
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// createProject creates a project and returns nothing but an error.
func (d *system) createProject(ctx context.Context, name, rootDir string) error {
	_, err := d.relayDo(ctx, http.MethodPost, "/system/projects",
		map[string]string{"name": name, "root_dir": rootDir})
	return err
}

// updateProject renames a project and/or changes its root dir (empty = keep).
func (d *system) updateProject(ctx context.Context, projectID, name, rootDir string) error {
	_, err := d.relayDo(ctx, http.MethodPatch, "/system/projects/"+projectID,
		map[string]string{"name": name, "root_dir": rootDir})
	return err
}

// deleteProject deletes a project (and, via cascade, its ports).
func (d *system) deleteProject(ctx context.Context, projectID string) error {
	_, err := d.relayDo(ctx, http.MethodDelete, "/system/projects/"+projectID, nil)
	return err
}

// addPort adds an exposed port to a project.
func (d *system) addPort(ctx context.Context, projectID string, port int, label string) error {
	_, err := d.relayDo(ctx, http.MethodPost, "/system/projects/"+projectID+"/ports",
		map[string]any{"port": port, "label": label})
	return err
}

// updatePort changes an exposed port's label.
func (d *system) updatePort(ctx context.Context, portID, label string) error {
	_, err := d.relayDo(ctx, http.MethodPatch, "/system/ports/"+portID,
		map[string]string{"label": label})
	return err
}

// deletePort removes an exposed port.
func (d *system) deletePort(ctx context.Context, portID string) error {
	_, err := d.relayDo(ctx, http.MethodDelete, "/system/ports/"+portID, nil)
	return err
}

// Run connects to the relay and serves the tunnel, reconnecting with backoff
// until ctx is cancelled.
func (d *system) Run(ctx context.Context) {
	d.mu.Lock()
	d.runCtx = ctx
	d.mu.Unlock()
	// Reaching here with a cleartext remote relay means the operator opted in
	// via ORMOS_INSECURE=1 (runSystem refuses it otherwise) — say so loudly,
	// every reconnect.
	if insecureRemoteRelay(d.cfg.RelayURL) {
		d.logf("WARNING: %s is a remote cleartext relay; the pairing token is sent unencrypted (allowed by ORMOS_INSECURE=1) — use wss://", d.cfg.RelayURL)
	}
	if d.pollPortsFn != nil {
		go d.pollPortsFn(ctx)
	} else {
		go d.pollPorts(ctx)
	}
	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		d.resetMu.Lock()
		resetDone := d.resetDone
		d.resetMu.Unlock()
		if !resetDone {
			resetCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			err := d.resetTerminalSessions(resetCtx)
			cancel()
			if err != nil {
				d.logf("terminal lifecycle reset failed: %v (retry in %s)", err, backoff)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff < maxBackoff {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
				}
				continue
			}
			d.resetMu.Lock()
			d.resetDone = true
			d.resetMu.Unlock()
		}
		began := time.Now()
		connect := d.connectAndServe
		if d.connectAndServeFn != nil {
			connect = d.connectAndServeFn
		}
		connected, err := connect(ctx)
		if ctx.Err() != nil {
			return
		}
		lifetime := time.Since(began)
		if tunnelHealthy(connected, lifetime) {
			backoff = minBackoff // a long-lived tunnel resets the backoff
			if err == nil {
				continue // clean close of a healthy tunnel — reconnect promptly
			}
		}
		// Anything else backs off — including a CLEAN close of a young tunnel:
		// a relay that accepts and immediately drops us would otherwise set
		// the pace of a tight TLS handshake loop with zero delay.
		if err != nil {
			d.logf("tunnel error: %v (retry in %s)", err, backoff)
		} else {
			d.logf("tunnel closed after %s (retry in %s)", lifetime.Round(time.Millisecond), backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff + jitter(backoff)):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// tunnelHealthyFloor is how long a tunnel must live for its end to count as
// an ordinary event (a deploy, a load balancer cycling the connection).
// Shorter than that and the close is treated as churn: the reconnect backs
// off like any failure, clean or not.
const tunnelHealthyFloor = 30 * time.Second

// tunnelHealthy reports whether a tunnel that just ended lived long enough to
// reset the reconnect backoff (and, when it closed cleanly, to reconnect with
// no delay at all).
func tunnelHealthy(connected bool, lifetime time.Duration) bool {
	return connected && lifetime >= tunnelHealthyFloor
}

// jitter returns a value in [0, d/2) to spread reconnects so a relay restart
// doesn't trigger every system to reconnect in lockstep. Clock-derived (not
// security-sensitive).
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(time.Now().UnixNano() % int64(d/2))
}

// connectAndServe establishes one tunnel and blocks until it closes. It reports
// whether the connection was established (so the caller can reset its backoff)
// alongside any error.
func (d *system) connectAndServe(ctx context.Context) (connected bool, err error) {
	url := d.cfg.RelayURL + "/system/connect"
	d.logf("connecting to %s", url)
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: map[string][]string{
			"Authorization": {"Bearer " + d.cfg.PairingToken},
			// Always advertise the current fenced+ACK protocol. The shared
			// LegacyV0 constant represents absence for old released binaries;
			// it is not a downgrade this agent may request.
			relay.StreamFenceVersionHeader: {relay.StreamFenceVersion},
			// Published on every connect rather than once at pairing, so an
			// agent whose key is new — or which predates sealing — starts
			// working again by reconnecting instead of being re-paired.
			relay.PublicKeyHeader: {encodePublicKey(d.key)},
		},
	})
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	// No SetReadLimit here, deliberately. relay.NetConn presents this connection
	// as a net.Conn, and websocket.NetConn disables the read limit to do so: a
	// byte stream may legitimately span messages, so a per-message cap cannot
	// apply to one. Anything set here would be cleared immediately after.
	//
	// The tunnel's memory ceiling is yamux's rather than the WebSocket layer's.
	// netConn.Read reads into the caller's buffer, so message size never becomes
	// allocation size, and what a peer may hold across the tunnel is
	// relay.MaxTunnelWindowBytes — pinned by the guard in
	// relay/transport_test.go, which is where to change it.
	netConn := relay.NetConn(ctx, conn)
	sess, err := relay.ServerSession(netConn)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "yamux setup failed")
		return false, fmt.Errorf("yamux: %w", err)
	}
	defer sess.Close()

	d.setConnected(true)
	d.logf("tunnel established")
	// Reconcile while the tunnel is established, before accepting streams, so a
	// missed lifecycle nudge is repaired immediately on every reconnect.
	d.reconcileTerminalSessions()
	defer func() {
		d.setConnected(false)
		d.logf("tunnel closed")
	}()

	// Bound how many streams the relay may have in flight against this machine.
	// yamux imposes no limit of its own, and every stream costs something real
	// here — a PTY, a TCP connection to a local service, a frame buffer — so
	// without a cap a relay that has gone wrong (or hostile) can exhaust the
	// agent simply by opening streams.
	slots := make(chan struct{}, relay.MaxTunnelStreams)
	for {
		stream, err := sess.Accept()
		if err != nil {
			return true, nil // connected; session closed
		}
		select {
		case slots <- struct{}{}:
		default:
			d.logf("refused a stream: %d already in flight", relay.MaxTunnelStreams)
			stream.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			d.serveStream(stream)
		}()
	}
}
