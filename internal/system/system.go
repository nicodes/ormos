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
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/nicodes/ormos/relay"
)

var terminalSessionRandom = rand.Read

const (
	terminalSessionCreateAttempts = 8
	terminalFenceLimit            = 1024
)

var (
	terminalExitReportBackoff    = 500 * time.Millisecond
	terminalExitReportMaxBackoff = 30 * time.Second
	terminalExitReportShutdownTO = 5 * time.Second
)

type relayHTTPError struct {
	statusCode int
	message    string
}

func (e *relayHTTPError) Error() string { return e.message }

var errPairingTokenRevoked = errors.New("pairing token revoked")

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
	terminals        map[string]*terminalSession
	terminalStarting int
	// terminalFences intentionally retain the highest generation seen for each
	// record. A delayed stream must not become valid again after close/delete;
	// the relay's durable record list is the authority for deciding whether a
	// new generation may be admitted, while this monotonic fence protects the
	// interval between that decision and PTY insertion.
	terminalFences      map[string]int
	terminalAdmissionMu sync.Mutex
	terminalAdmissions  map[string]*terminalAdmissionState
	admissionSequence   atomic.Uint64
	shuttingDown        bool
	activeAdmissions    int
	admissionsIdle      chan struct{}
	exitReportsClosed   bool
	publishedTerminals  map[*terminalSession]struct{}
	publishedClose      sync.WaitGroup
	reconcileStateMu    sync.Mutex
	reconcileRunning    bool
	reconcileAgain      bool
	reconcileDone       chan struct{}
	reconcileCancel     context.CancelFunc
	resetMu             sync.Mutex
	resetDone           bool
	resetErr            error
	runCtx              context.Context
	exitReportCtx       context.Context
	exitReportCancel    context.CancelFunc
	exitReportReady     chan struct{}
	exitReports         sync.WaitGroup

	policy  policy   // local restrictions the agent enforces itself
	audit   *auditor // append-only record of relay-requested actions
	lastGot []int    // last successfully fetched configured ports (fail-closed fallback)

	// key is this machine's long-lived X25519 private key. Terminal frames are
	// sealed against it, so the server carries them without being able to read
	// them — see relay/seal.go.
	key *ecdh.PrivateKey

	// Test seams for parking execution at the external-action boundaries. They
	// remain nil in production. proxyDialContext defaults to net.Dialer.
	beforeShutdownAction               func()
	beforeProxyDial                    func()
	afterProxyDial                     func()
	beforeTerminalAction               func()
	beforeTerminalAdmission            func()
	beforePTYStart                     func()
	beforeTerminalAdmissionWait        func()
	beforeTerminalAdmissionInstall     func()
	beforeReconcileAdmissionCutoff     func()
	afterTerminalProcessTeardown       func()
	beforeTerminalExitReport           func()
	afterShutdownAdmissionInvalidation func()
	beforeExitReportContext            func()
	beforeExitReportContextPublish     func()
	beforeExitReportContextWait        func()
	proxyDialContext                   func(context.Context, string, string) (net.Conn, error)
	shutdownAckSlots                   chan struct{}
	actionNow                          func() time.Time
	connectAndServeFn                  func(context.Context) (bool, error)
	pollPortsFn                        func(context.Context)
	runCancel                          context.CancelFunc
	pairingTokenRevoked                bool
}

// setCancel lets the agent shut itself down when the relay asks it to.
func (d *system) setCancel(cancel context.CancelFunc) {
	d.mu.Lock()
	d.cancel = cancel
	d.mu.Unlock()
}

// markPairingTokenRevoked stops every loop sharing this system as soon as any
// authoritative agent endpoint returns 401. It does not clear configuration;
// the top-level owner of the state-directory lock does that before re-pairing.
func (d *system) markPairingTokenRevoked() {
	d.mu.Lock()
	d.pairingTokenRevoked = true
	cancel := d.runCancel
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (d *system) pairingRevocationError() error {
	d.mu.Lock()
	revoked := d.pairingTokenRevoked
	d.mu.Unlock()
	if revoked {
		return errPairingTokenRevoked
	}
	return nil
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

func (d *system) exitReportContextState() (context.Context, bool) {
	d.mu.Lock()
	ctx := d.exitReportCtx
	shutdownCtx := ctx != nil
	if ctx == nil {
		ctx = d.runCtx
	}
	d.mu.Unlock()
	if ctx == nil {
		return context.Background(), shutdownCtx
	}
	return ctx, shutdownCtx
}

func (d *system) waitForExitReportContext() bool {
	if d.beforeExitReportContextWait != nil {
		d.beforeExitReportContextWait()
	}
	d.mu.Lock()
	ready := d.exitReportReady
	if ready == nil {
		ready = make(chan struct{})
		d.exitReportReady = ready
	}
	d.mu.Unlock()
	timer := time.NewTimer(terminalExitReportShutdownTO + time.Duration(maxTerminalSessions)*terminalKillGrace)
	defer timer.Stop()
	select {
	case <-ready:
		return true
	case <-timer.C:
		return false
	}
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
	return d.relayDoWithClient(ctx, httpClient, method, path, body)
}

func (d *system) relayDoWithClient(ctx context.Context, client *http.Client, method, path string, body any) ([]byte, error) {
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
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxRelayResponse))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if resp.StatusCode == http.StatusUnauthorized {
			d.markPairingTokenRevoked()
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

// terminalLifecycleReady is deliberately checked by both persistence and PTY
// creation. Run performs the reset before opening the tunnel, but the TUI can
// issue a command while that first reset is still pending.
func (d *system) terminalLifecycleReady() error {
	if d.shutdownStarted() {
		return errors.New("terminal lifecycle is shutting down")
	}
	if d.cfg.RelayURL == "" {
		return nil
	}
	d.resetMu.Lock()
	defer d.resetMu.Unlock()
	if d.resetDone {
		return nil
	}
	if d.resetErr != nil {
		return fmt.Errorf("terminal lifecycle reset failed; retrying: %w", d.resetErr)
	}
	return errors.New("terminal lifecycle reset pending")
}

func (d *system) shutdownStarted() bool {
	d.terminalAdmissionMu.Lock()
	started := d.shuttingDown
	d.terminalAdmissionMu.Unlock()
	return started
}

func (d *system) reportTerminalExit(recordID string, generation int) {
	if recordID == "" || generation <= 0 || d.cfg.RelayURL == "" {
		return
	}
	d.terminalAdmissionMu.Lock()
	if d.exitReportsClosed {
		d.terminalAdmissionMu.Unlock()
		return
	}
	d.exitReports.Add(1)
	d.terminalAdmissionMu.Unlock()
	client := httpClient
	go func() {
		defer d.exitReports.Done()
		backoff := terminalExitReportBackoff
		for {
			ctx, shutdownCtx := d.exitReportContextState()
			if ctx.Err() != nil {
				if shutdownCtx {
					return
				}
				if d.shutdownStarted() {
					time.Sleep(time.Millisecond)
					continue
				}
				if !d.waitForExitReportContext() {
					return
				}
				continue
			}
			requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, err := d.relayDoWithClient(requestCtx, client, http.MethodPost, "/system/terminal-sessions/"+url.PathEscape(recordID)+"/exit", struct {
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
				if shutdownCtx {
					return
				}
				if d.shutdownStarted() {
					continue
				}
				if !d.waitForExitReportContext() {
					return
				}
				continue
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

func (d *system) waitExitReports() { d.exitReports.Wait() }

func (d *system) finishShutdown() {
	d.terminalAdmissionMu.Lock()
	if d.shuttingDown {
		d.terminalAdmissionMu.Unlock()
		return
	}
	d.shuttingDown = true
	if d.beforeExitReportContextPublish != nil {
		d.beforeExitReportContextPublish()
	}
	// Publish a usable background shutdown context immediately. A report worker
	// can already be observing runCtx cancellation before this lock is reached;
	// the ready channel lets it synchronize without dropping the report. The
	// shutdown deadline is started after terminal teardown below, not here.
	d.mu.Lock()
	d.exitReportCtx, d.exitReportCancel = context.WithCancel(context.Background())
	if d.exitReportReady == nil {
		d.exitReportReady = make(chan struct{})
	}
	close(d.exitReportReady)
	d.mu.Unlock()
	for key, state := range d.terminalAdmissions {
		state.canceled = true
		close(state.done)
		delete(d.terminalAdmissions, key)
	}
	idle := d.admissionsIdle
	if d.activeAdmissions == 0 {
		idle = nil
	}
	d.terminalAdmissionMu.Unlock()
	if d.afterShutdownAdmissionInvalidation != nil {
		d.afterShutdownAdmissionInvalidation()
	}
	if idle != nil {
		<-idle
	}

	d.terminalMu.Lock()
	sessions := make([]*terminalSession, 0, len(d.terminals))
	for _, s := range d.terminals {
		sessions = append(sessions, s)
	}
	d.terminalMu.Unlock()
	for _, s := range sessions {
		s.close()
	}
	// No admission can register after shuttingDown and the admission activity
	// drain above. Waiting is therefore safe: there is no Add concurrent with
	// Wait, and it also covers a published session removed from terminals before
	// it reached reportTerminalExit.
	d.publishedClose.Wait()
	if d.beforeExitReportContext != nil {
		d.beforeExitReportContext()
	}
	// Start the bounded report window only after all published close operations
	// have completed, so slow process teardown cannot consume report time.
	d.mu.Lock()
	reportCancel := d.exitReportCancel
	d.mu.Unlock()
	reportDeadline := time.AfterFunc(terminalExitReportShutdownTO, reportCancel)
	d.terminalAdmissionMu.Lock()
	d.exitReportsClosed = true
	d.terminalAdmissionMu.Unlock()
	d.waitExitReports()
	d.mu.Lock()
	cancel := d.exitReportCancel
	d.exitReportCancel = nil
	d.mu.Unlock()
	reportDeadline.Stop()
	if cancel != nil {
		cancel()
	}
}

func (d *system) reconcileSnapshot(ctx context.Context) error {
	if d.beforeReconcileAdmissionCutoff != nil {
		d.beforeReconcileAdmissionCutoff()
	}
	d.terminalAdmissionMu.Lock()
	cutoff := d.admissionSequence.Load()
	d.terminalAdmissionMu.Unlock()
	rows, err := d.fetchTerminalSessions(ctx)
	if err != nil {
		return err
	}
	d.invalidateTerminalAdmissions(cutoff)
	valid := make(map[string]relay.TerminalSessionInfo, len(rows))
	for _, row := range rows {
		valid[row.ID] = row
	}
	d.terminalMu.Lock()
	after := make(map[string]*terminalSession, len(d.terminals))
	for id, s := range d.terminals {
		after[id] = s
	}
	d.terminalMu.Unlock()
	for id, s := range after {
		if s.admissionSequence > cutoff {
			continue
		}
		d.terminalMu.Lock()
		if d.terminals[id] != s {
			d.terminalMu.Unlock()
			continue
		}
		row, ok := valid[id]
		if ok && row.State == relay.TerminalStateRunning && row.Generation == s.generation {
			d.terminalMu.Unlock()
			continue
		}
		d.terminalMu.Unlock()
		s.close()
	}
	return nil
}

// reconcileTerminalSessions is level-triggered: the durable list is authoritative,
// and the returned signal closes when the current worker exits.
func (d *system) reconcileTerminalSessions() <-chan struct{} {
	d.reconcileStateMu.Lock()
	if d.reconcileRunning {
		d.reconcileAgain = true
		done := d.reconcileDone
		d.reconcileStateMu.Unlock()
		return done
	}
	d.reconcileRunning = true
	d.reconcileDone = make(chan struct{})
	done := d.reconcileDone
	workerCtx, cancel := context.WithCancel(d.lifecycleContext())
	d.reconcileCancel = cancel
	d.reconcileStateMu.Unlock()
	go func() {
		defer func() {
			cancel()
			d.reconcileStateMu.Lock()
			d.reconcileRunning = false
			d.reconcileCancel = nil
			close(done)
			d.reconcileDone = nil
			d.reconcileStateMu.Unlock()
		}()
		backoff := 100 * time.Millisecond
		for {
			ctx, timeout := context.WithTimeout(workerCtx, 8*time.Second)
			err := d.reconcileSnapshot(ctx)
			timeout()
			if err != nil {
				timer := time.NewTimer(backoff)
				select {
				case <-workerCtx.Done():
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
				d.reconcileStateMu.Unlock()
				return
			}
			d.reconcileStateMu.Unlock()
		}
	}()
	return done
}

func (d *system) reconcileTerminalSessionsSync(ctx context.Context) error {
	d.reconcileStateMu.Lock()
	if d.reconcileRunning {
		cancel := d.reconcileCancel
		done := d.reconcileDone
		if cancel != nil {
			cancel()
		}
		d.reconcileStateMu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		d.reconcileStateMu.Unlock()
	}
	return d.reconcileSnapshot(ctx)
}

// createTerminalSession persists a new terminal tab before any local PTY is
// attached. A transport failure is deliberately not retried: the relay may
// have committed the record before the connection failed. Only an explicit
// duplicate response proves that generating a fresh id is safe.
func (d *system) createTerminalSession(ctx context.Context, projectID string) (relay.TerminalSessionInfo, error) {
	if err := d.terminalLifecycleReady(); err != nil {
		return relay.TerminalSessionInfo{}, err
	}
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
			if info.ID == "" || info.ProjectID != projectID || info.SessionID != sessionID || info.State != relay.TerminalStateRunning || info.Generation <= 0 {
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

func (d *system) deleteTerminalSession(ctx context.Context, recordID string, generation int) error {
	if _, err := d.relayDo(ctx, http.MethodDelete, "/system/terminal-sessions/"+url.PathEscape(recordID), map[string]int{"generation": generation}); err != nil {
		return err
	}
	return d.reconcileTerminalSessionsSync(ctx)
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
	if _, err := d.relayDo(ctx, http.MethodDelete, "/system/projects/"+projectID, nil); err != nil {
		return err
	}
	// Project deletion cascades terminal records in the relay. Reconcile now,
	// rather than waiting for a later event or poll, so local PTYs disappear
	// before the TUI reports deletion complete.
	return d.reconcileTerminalSessionsSync(ctx)
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
func (d *system) Run(ctx context.Context) error {
	defer d.finishShutdown()
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	d.mu.Lock()
	d.runCtx = runCtx
	d.runCancel = cancelRun
	d.mu.Unlock()
	// Reaching here with a cleartext remote relay means the operator opted in
	// via ORMOS_INSECURE=1 (runSystem refuses it otherwise) — say so loudly,
	// every reconnect.
	if insecureRemoteRelay(d.cfg.RelayURL) {
		d.logf("WARNING: %s is a remote cleartext relay; the pairing token is sent unencrypted (allowed by ORMOS_INSECURE=1) — use wss://", d.cfg.RelayURL)
	}
	if d.pollPortsFn != nil {
		go d.pollPortsFn(runCtx)
	} else {
		go d.pollPorts(runCtx)
	}
	backoff := minBackoff
	for {
		if runCtx.Err() != nil {
			return d.pairingRevocationError()
		}
		d.resetMu.Lock()
		resetDone := d.resetDone
		d.resetMu.Unlock()
		if !resetDone {
			resetCtx, cancel := context.WithTimeout(runCtx, 8*time.Second)
			err := d.resetTerminalSessions(resetCtx)
			cancel()
			if err != nil {
				d.resetMu.Lock()
				d.resetErr = err
				d.resetMu.Unlock()
				d.logf("terminal lifecycle reset failed: %v (retry in %s)", err, backoff)
				select {
				case <-runCtx.Done():
					return d.pairingRevocationError()
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
			d.resetErr = nil
			d.resetMu.Unlock()
		}
		began := time.Now()
		connect := d.connectAndServe
		if d.connectAndServeFn != nil {
			connect = d.connectAndServeFn
		}
		connected, err := connect(runCtx)
		if runCtx.Err() != nil {
			return d.pairingRevocationError()
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
		case <-runCtx.Done():
			return d.pairingRevocationError()
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
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: map[string][]string{
			"Authorization": {"Bearer " + d.cfg.PairingToken},
			// Always advertise the current direct-system protocol. The shared
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
		if resp != nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				d.markPairingTokenRevoked()
				return false, errPairingTokenRevoked
			}
		}
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
	if err := d.reconcileTerminalSessionsSync(ctx); err != nil {
		d.setConnected(false)
		return true, fmt.Errorf("terminal reconciliation: %w", err)
	}
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
