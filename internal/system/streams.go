//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nicodes/ormos/relay"
)

// headerReadTO bounds how long a freshly accepted stream may sit without a
// header. ReadHeader blocks until a newline arrives, and stream slots are
// finite (relay.MaxTunnelStreams), so without a deadline a relay that opens
// streams and says nothing pins every slot the agent has. A var so tests can
// shrink it.
var headerReadTO = 10 * time.Second

const (
	shutdownAckWriteTO = time.Second
	proxyConnectTO     = 10 * time.Second
	// A yamux stream deadline should interrupt a blocked Write, but its session
	// send path is outside the stream. Keep the async fallback finite even if a
	// broken transport ignores both the deadline and stream closure.
	maxAsyncShutdownAckWrites = 4
)

// expandHomeDir resolves a leading ~ (or ~/) to the agent's home directory —
// the daemon sets cmd.Dir directly, so no shell is around to expand it.
func expandHomeDir(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	} else if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	return p
}

// expandHome resolves ~ and expands $VARS, so a local policy root like
// "~/code/app" works. This is for LOCAL configuration only (policy
// allowedRoots): anything the relay sends goes through expandRelayCwd, which
// must not expand this machine's environment on the relay's behalf.
func expandHome(p string) string {
	return os.ExpandEnv(expandHomeDir(p))
}

// expandRelayCwd resolves a directory the relay asked for. It gets ~
// expansion only: running os.ExpandEnv on relay input would be an oracle for
// probing this machine's environment (did "$FOO/app" open a terminal where
// "/literal/app" did not? then FOO expanded to something real). A path that
// still contains a $ after the tilde pass is rejected outright.
func expandRelayCwd(p string) (string, error) {
	p = expandHomeDir(p)
	if strings.ContainsRune(p, '$') {
		return "", fmt.Errorf("directory %q contains a $ variable; the agent does not expand relay-supplied paths", p)
	}
	return p, nil
}

// serveStream reads a stream's header and dispatches to the right handler.
func (d *system) serveStream(stream net.Conn) {
	defer stream.Close()
	// The deadline covers only the header; a stream that has announced itself
	// gets its handler's own pacing (the terminal handshake's deadline, the
	// proxy pipe's idle bound).
	_ = stream.SetReadDeadline(time.Now().Add(headerReadTO))
	header, br, err := relay.ReadHeader(stream)
	_ = stream.SetReadDeadline(time.Time{})
	if err != nil {
		d.logf("stream header error: %v", err)
		return
	}
	actionDeadline, err := relay.AcceptStreamFence(header, d.actionTime())
	if err != nil {
		d.audit.record(auditEntry{Event: string(header.Kind), Allowed: false, Detail: err.Error()})
		d.logf("refusing %s stream: %v", header.Kind, err)
		if header.Kind == relay.KindShutdown {
			d.writeShutdownAck(stream, header, actionDeadline, fenceRefusalStatus(err))
		}
		return
	}
	switch header.Kind {
	case relay.KindTerminal:
		d.handleTerminal(stream, br, header, actionDeadline)
	case relay.KindProxy:
		d.handleProxy(stream, br, header, actionDeadline)
	case relay.KindListPorts:
		d.handleListPorts(stream)
	case relay.KindShutdown:
		if d.beforeShutdownAction != nil {
			d.beforeShutdownAction()
		}
		cancel := d.shutdownCancel()
		if cancel == nil {
			d.audit.record(auditEntry{Event: "shutdown", Allowed: false, Detail: "shutdown unavailable"})
			d.writeShutdownAck(stream, header, actionDeadline, relay.ActionAckRefused)
			return
		}
		// Keep the final authorization check immediately adjacent to the ACK.
		// A success ACK commits an infallible root cancellation; if the ACK cannot
		// be written, leave the agent running so the relay can truthfully retry.
		if err := relay.ValidateStreamFenceDeadline(actionDeadline, d.actionTime()); err != nil {
			d.audit.record(auditEntry{Event: "shutdown", Allowed: false, Detail: err.Error()})
			d.writeShutdownAck(stream, header, actionDeadline, fenceRefusalStatus(err))
			return
		}
		if !commitShutdown(cancel, func() bool {
			return d.writeShutdownAck(stream, header, actionDeadline, relay.ActionAckSuccess)
		}) {
			return
		}
		d.audit.record(auditEntry{Event: "shutdown", Allowed: true})
		d.logf("shutdown requested by relay; exiting")
	case relay.KindEvent:
		d.notifyEvent() // upstream data changed (web UI); TUI refetches
		d.reconcileTerminalSessions()
	default:
		d.logf("unknown stream kind %q", header.Kind)
	}
}

func fenceRefusalStatus(err error) relay.ActionAckStatus {
	if relay.IsStreamFenceExpired(err) {
		return relay.ActionAckExpired
	}
	return relay.ActionAckRefused
}

// commitShutdown defines the irrevocable shutdown commit point. A successful
// ACK write completed inside the action fence authorizes shutdown permanently;
// root cancellation is its infallible fulfillment. The defer guarantees that
// cancellation is the very next operation, with no logging, audit I/O, or other
// fallible work between the successful write and fulfillment.
func commitShutdown(cancel context.CancelFunc, acknowledge func() bool) (committed bool) {
	if !acknowledge() {
		return false
	}
	defer cancel()
	return true
}

type shutdownAckWriteResult struct {
	err       error
	completed time.Time
}

func (d *system) actionTime() time.Time {
	if d.actionNow != nil {
		return d.actionNow()
	}
	return time.Now()
}

func (d *system) writeShutdownAck(stream net.Conn, h relay.StreamHeader, actionDeadline time.Time, status relay.ActionAckStatus) bool {
	now := d.actionTime()
	deadline := now.Add(shutdownAckWriteTO)
	// Success carries authority and therefore must complete inside NotAfter. A
	// non-success result carries no authority: while its fence is live it uses
	// the same bound, but an already-expired/refused action still gets one second
	// to report that truthful terminal result to the relay.
	if (status == relay.ActionAckSuccess || now.Before(actionDeadline)) && actionDeadline.Before(deadline) {
		deadline = actionDeadline
	}
	if !now.Before(deadline) {
		return false
	}

	// newSystem initializes this in production. The guarded lazy path keeps
	// narrow tests that construct a system literal safe without adding a second
	// source of unbounded writer goroutines.
	d.mu.Lock()
	if d.shutdownAckSlots == nil {
		d.shutdownAckSlots = make(chan struct{}, maxAsyncShutdownAckWrites)
	}
	slots := d.shutdownAckSlots
	d.mu.Unlock()
	select {
	case slots <- struct{}{}:
	default:
		d.logf("shutdown acknowledgment refused: writer limit reached")
		return false
	}

	_ = stream.SetWriteDeadline(deadline)
	result := make(chan shutdownAckWriteResult, 1)
	go func() {
		if !d.actionTime().Before(deadline) {
			<-slots
			result <- shutdownAckWriteResult{err: context.DeadlineExceeded, completed: d.actionTime()}
			return
		}
		err := relay.WriteActionAck(stream, relay.NewActionAck(h, status))
		completed := d.actionTime()
		<-slots
		result <- shutdownAckWriteResult{err: err, completed: completed}
	}()

	accept := func(r shutdownAckWriteResult) bool {
		if r.err != nil {
			d.logf("shutdown acknowledgment failed: %v", r.err)
			return false
		}
		if !r.completed.Before(deadline) {
			_ = stream.Close()
			d.logf("shutdown acknowledgment exceeded its action fence")
			return false
		}
		return true
	}
	// Prefer a write that completed before the deadline even if this goroutine
	// was not scheduled again until the timer also became ready. The completed
	// timestamp is the commit point; a later scheduler pause cannot revoke it.
	select {
	case r := <-result:
		return accept(r)
	default:
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case r := <-result:
		return accept(r)
	case <-timer.C:
		select {
		case r := <-result:
			return accept(r)
		default:
			_ = stream.Close()
			d.logf("shutdown acknowledgment exceeded its action fence")
			return false
		}
	}
}

// scrubbedEnv returns the process environment with every ORMOS_* variable
// removed, so nothing the system was configured with can be read from inside an
// interactive shell it spawns.
func scrubbedEnv() []string {
	all := os.Environ()
	out := make([]string, 0, len(all))
	for _, kv := range all {
		if strings.HasPrefix(kv, "ORMOS_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// proxyStatus is the status line of a proxy refusal.
//
// A defined type with exactly two values rather than a plain string parameter,
// because these bytes are written verbatim into a hand-assembled HTTP response.
// A status that could become dynamic is a response-splitting hole waiting for
// its first caller, and this is a type the compiler can enforce for free.
type proxyStatus string

const (
	proxyForbidden  proxyStatus = "403 Forbidden"
	proxyBadGateway proxyStatus = "502 Bad Gateway"
)

// writeProxyError answers a proxy stream with a complete plain-text HTTP
// response and nothing else. Nothing follows it: serveStream's deferred Close
// ends the stream as soon as handleProxy returns, which is what makes the
// Connection: close honest.
//
// Hand-assembled because there is no http.Request to answer: the relay opened a
// raw byte pipe, and by the time this is called the agent has decided not to
// forward what came down it. What reaches the user is an iframe, so a bare EOF
// would render as a blank frame; a status and a sentence render as an
// explanation.
//
// NEITHER ARGUMENT MAY CONTAIN CR OR LF. They are interpolated straight into
// the response, so a newline in either would let the caller inject headers or a
// second response — the classic response split. status cannot: it is one of the
// two constants above. body is the caller's, and every call site today builds
// it from a literal plus %d of an int. Anything relay-supplied — a policy
// reason, a path, an error string — must not be put here without stripping
// control characters first.
//
// One spelling of the response, called from every refusal, so a change to its
// shape — a security header, a charset, a Connection semantics fix — lands on
// all of them rather than on three of four. nosniff is the first of those to
// arrive: this renders in an iframe on the relay's own origin, and while every
// body here is a literal today, "the browser will not second-guess the type we
// declared" is worth one header rather than an argument at each call site.
func writeProxyError(w io.Writer, status proxyStatus, body string) {
	fmt.Fprintf(w, "HTTP/1.1 %s\r\nContent-Type: text/plain; charset=utf-8\r\n"+
		"X-Content-Type-Options: nosniff\r\n"+
		"Content-Length: %d\r\nConnection: close\r\n\r\n%s", status, len(body), body)
}

// proxyIdleTO bounds how long a proxy pipe may sit without a byte moving in
// either direction. handleProxy holds a stream slot (relay.MaxTunnelStreams)
// and a local TCP connection until one side closes, and a relay that passed
// the policy checks — a compromised one is in the threat model — could hold
// both forever by simply saying nothing. A fixed deadline would kill the
// legitimate case: a proxy pipe is honestly long-lived (a previewed app's
// WebSocket, a sparse event stream), so the bound is an IDLE one that resets
// on every byte transferred. Five minutes of silence is what no healthy pipe
// does: the relay's own transport retires idle pooled connections at 30s, so
// an ordinary preview request never comes close, and a browser EventSource or
// HMR socket reconnects on its own if a genuinely quiet connection is
// dropped. A var so tests can shrink it.
var proxyIdleTO = 5 * time.Minute

// progressReader calls onProgress after every successful read, so a byte
// moving in either direction of a pipe re-arms the pipe's shared idle
// deadline.
type progressReader struct {
	r          io.Reader
	onProgress func()
}

func (p progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.onProgress()
	}
	return n, err
}

// proxyIdleDeadline owns the ONE idle deadline shared by both ends of a proxy
// pipe. A reset is a two-connection transaction: calculate the deadline and
// update both conns while holding mu. net.Conn only promises each SetDeadline
// call is concurrency-safe; without this serialization, an older reset could
// set stream, pause, a newer reset could advance both, then the older one could
// resume and move local backwards — timing out a healthy preview before the
// latest byte's full idle budget elapsed.
type proxyIdleDeadline struct {
	mu      sync.Mutex
	conns   [2]net.Conn
	timeout time.Duration
	now     func() time.Time
}

func newProxyIdleDeadline(timeout time.Duration, a, b net.Conn) *proxyIdleDeadline {
	return &proxyIdleDeadline{conns: [2]net.Conn{a, b}, timeout: timeout, now: time.Now}
}

func (d *proxyIdleDeadline) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Deliberately inside the lock: a deadline calculated before waiting for
	// another reset would already be stale when it was eventually installed.
	deadline := d.now().Add(d.timeout)
	for _, conn := range d.conns {
		_ = conn.SetDeadline(deadline)
	}
}

// handleProxy dials a local TCP port and pipes raw bytes both ways.
func (d *system) handleProxy(stream net.Conn, br io.Reader, h relay.StreamHeader, actionDeadline time.Time) {
	d.addSession(1)
	defer d.addSession(-1)
	port := h.Port

	// Two independent checks, both of which must pass. Local policy is decided
	// here and comes from this machine's own config file; the exposed-port list
	// comes from the relay and only catches a relay that is buggy or out of date,
	// not one that has been taken over.
	pol, policyOK := d.livePolicy()
	if !policyOK {
		d.audit.record(auditEntry{Event: "proxy", Port: port, Detail: "policy unreadable", Allowed: false})
		writeProxyError(stream, proxyForbidden,
			"Local policy on this system could not be read; nothing is being served.\n")
		return
	}
	if ok, reason := pol.proxyAllowed(port); !ok {
		d.audit.record(auditEntry{Event: "proxy", Port: port, Detail: reason, Allowed: false})
		d.logf("proxy refused by local policy: %s", reason)
		writeProxyError(stream, proxyForbidden,
			fmt.Sprintf("Local policy on this system refuses port %d.\n", port))
		return
	}
	if !d.proxyPortAllowed(port) {
		d.audit.record(auditEntry{Event: "proxy", Port: port, Allowed: false})
		d.logf("proxy refused: port %d is not exposed", port)
		writeProxyError(stream, proxyForbidden,
			fmt.Sprintf("Port %d is not exposed on this system.\n", port))
		return
	}

	if d.beforeProxyDial != nil {
		d.beforeProxyDial()
	}
	if err := relay.ValidateStreamFenceDeadline(actionDeadline, d.actionTime()); err != nil {
		d.audit.record(auditEntry{Event: "proxy", Port: port, Detail: err.Error(), Allowed: false})
		d.logf("proxy refused at dial boundary: %v", err)
		return
	}
	_ = stream.SetDeadline(actionDeadline)
	ctx, cancel := context.WithDeadline(context.Background(), actionDeadline)
	defer cancel()
	dial := d.proxyDialContext
	if dial == nil {
		dialer := &net.Dialer{Timeout: proxyConnectTO}
		dial = dialer.DialContext
	}
	p := strconv.Itoa(port)
	local, err := dial(ctx, "tcp", net.JoinHostPort("127.0.0.1", p))
	if err != nil {
		// Dev servers (Vite, Node, …) often bind IPv6 loopback only; try ::1 too.
		if fenceErr := relay.ValidateStreamFenceDeadline(actionDeadline, d.actionTime()); fenceErr != nil {
			err = fenceErr
		} else {
			local, err = dial(ctx, "tcp", net.JoinHostPort("::1", p))
		}
	}
	if err != nil {
		d.logf("proxy dial :%d: %v", port, err)
		writeProxyError(stream, proxyBadGateway,
			fmt.Sprintf("Nothing is listening on port %d on this system.\nStart your app on that port and reload.\n", port))
		return
	}
	defer local.Close()
	if d.afterProxyDial != nil {
		d.afterProxyDial()
	}
	if err := relay.ValidateStreamFenceDeadline(actionDeadline, d.actionTime()); err != nil || ctx.Err() != nil {
		if err == nil {
			err = ctx.Err()
		}
		d.audit.record(auditEntry{Event: "proxy", Port: port, Detail: err.Error(), Allowed: false})
		d.logf("proxy refused after dial: %v", err)
		return
	}
	_ = stream.SetDeadline(time.Time{})
	d.audit.record(auditEntry{Event: "proxy", Port: port, Allowed: true})
	d.logf("proxy session -> :%d", port)
	defer d.logf("proxy session closed (:%d)", port)

	// One shared idle deadline over both ends, re-armed by every byte that
	// moves in either direction: a peer that has stopped making progress loses
	// the slot and the local socket, while a pipe that is merely quiet between
	// bursts keeps both. Read-side progress alone suffices — every byte
	// transferred passes through exactly one of the two Reads.
	idle := newProxyIdleDeadline(proxyIdleTO, stream, local)
	idle.reset()

	done := make(chan struct{}, 2)
	// stream -> local (use buffered reader from header parse).
	go func() { io.Copy(local, progressReader{br, idle.reset}); done <- struct{}{} }()
	// local -> stream.
	go func() { io.Copy(stream, progressReader{local, idle.reset}); done <- struct{}{} }()
	<-done
}
