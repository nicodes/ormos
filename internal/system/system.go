package system

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/nicodes/ormos/relay"
)

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

	mu         sync.Mutex
	connected  bool
	sessions   int
	logs       []string     // ring buffer of recent log lines
	ports      []PortStatus // configured ports across this system's projects
	listening  []int        // host's currently-listening loopback ports (cached)
	echoStderr bool
	cancel     context.CancelFunc // stops the whole agent (relay-requested shutdown)
	events     chan struct{}      // relay→TUI "data changed" nudges (coalesced)
	terminalMu sync.Mutex
	terminals  map[string]*terminalSession

	policy  policy   // local restrictions the agent enforces itself
	audit   *auditor // append-only record of relay-requested actions
	lastGot []int    // last successfully fetched configured ports (fail-closed fallback)

	// key is this machine's long-lived X25519 private key. Terminal frames are
	// sealed against it, so the server carries them without being able to read
	// them — see relay/seal.go.
	key *ecdh.PrivateKey
}

// setCancel lets the agent shut itself down when the relay asks it to.
func (d *system) setCancel(cancel context.CancelFunc) {
	d.mu.Lock()
	d.cancel = cancel
	d.mu.Unlock()
}

// handleShutdown is invoked when the relay opens a KindShutdown stream (the user
// clicked Stop or Forget in the UI). It cancels the root context so Run/TUI exit
// and the process stops.
func (d *system) handleShutdown() {
	d.logf("shutdown requested by relay; exiting")
	d.mu.Lock()
	c := d.cancel
	d.mu.Unlock()
	if c != nil {
		c()
	}
}

const logRing = 200

func newSystem(cfg systemConfig) *system {
	d := &system{
		cfg: cfg, events: make(chan struct{}, 1),
		terminals: make(map[string]*terminalSession),
		audit:     newAuditor(),
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
	//
	// This was the missing half of the sealing work -- loadOrCreateKey was
	// written and never called, so d.key was nil and every connect dereferenced
	// it. Both users of the key would have failed: publishing the public half on
	// the handshake, and deriving session keys in terminal_sessions.go.
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
		fmt.Fprintln(os.Stderr, line)
	}
}

// Status is a snapshot of system state for the TUI.
//
// It carries only what a render actually reads. It used to also copy the whole
// log ring and the configured-port slice on every call — 500ms, forever — for
// two fields nothing outside a test ever looked at. The log ring now has a real
// consumer and is fetched by the tail through RecentLogs; the port slice does
// not, because the TUI lists projects and ports from the relay's own reply
// (model.projects) and needs Live only to mark which of them are up.
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
	// maxTunnelRead caps a single tunnel WS message. yamux writes at most one
	// stream window per message (256 KiB by default), so this is generous;
	// it only has to be finite.
	maxTunnelRead = 4 << 20
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
		listen := listeningPorts()
		live := make(map[int]bool, len(listen))
		for _, p := range listen {
			live[p] = true
		}
		d.mu.Lock()
		d.listening = listen
		d.mu.Unlock()

		infos, err := d.fetchConfiguredPorts(ctx)
		if err != nil {
			d.logf("ports poll failed: %v", err)
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
		return nil, fmt.Errorf("relay returned %s", resp.Status)
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
	if port < 1 || port > 65535 {
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
			return nil, fmt.Errorf("%s", e.Error)
		}
		return nil, fmt.Errorf("relay returned %s", resp.Status)
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
	// Reaching here with a cleartext remote relay means the operator opted in
	// via ORMOS_INSECURE=1 (runSystem refuses it otherwise) — say so loudly,
	// every reconnect.
	if insecureRemoteRelay(d.cfg.RelayURL) {
		d.logf("WARNING: %s is a remote cleartext relay; the pairing token is sent unencrypted (allowed by ORMOS_INSECURE=1) — use wss://", d.cfg.RelayURL)
	}
	go d.pollPorts(ctx)
	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		began := time.Now()
		connected, err := d.connectAndServe(ctx)
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
			// Published on every connect rather than once at pairing, so an
			// agent whose key is new — or which predates sealing — starts
			// working again by reconnecting instead of being re-paired.
			publicKeyHeader: {encodePublicKey(d.key)},
		},
	})
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	// Bound a single tunnel message. Frames are yamux-sized (well under this);
	// the finite cap avoids an unbounded read while still fitting real traffic.
	conn.SetReadLimit(maxTunnelRead)

	netConn := relay.NetConn(ctx, conn)
	sess, err := relay.ServerSession(netConn)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "yamux setup failed")
		return false, fmt.Errorf("yamux: %w", err)
	}
	defer sess.Close()

	d.setConnected(true)
	d.logf("tunnel established")
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
