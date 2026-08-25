//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nicodes/ormos/relay"
)

func fencedHeader(h relay.StreamHeader) relay.StreamHeader {
	if h.Kind == relay.KindTerminal && h.TerminalRecordID == "" {
		h.TerminalRecordID = h.SessionID
		h.TerminalGeneration = 1
	}
	h.ActionFence = strings.Repeat("a", 40)
	h.NotAfterMilli = time.Now().Add(5 * time.Second).UnixMilli()
	return h
}

func acceptedFenceDeadline(t *testing.T, h relay.StreamHeader) time.Time {
	t.Helper()
	deadline, err := relay.AcceptStreamFence(h, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return deadline
}

// closeTerminals shuts down every session a test left open, in the one shape
// that is safe: snapshot the map under terminalMu, then close outside it.
//
// Both halves matter. Reading d.terminals unlocked races the tunnel goroutines
// the test started, which is a -race flake waiting for the detach path to start
// deleting eagerly (nicodes/ormos-be#429). Closing while HOLDING the lock is
// worse than a race: terminalSession.close takes owner.terminalMu itself to
// unregister, so the same goroutine re-acquires a sync.Mutex it already holds
// and parks for ever. That is a self-deadlock, not a deadlock against another
// party -- the session's PTY reader also calls close and then parks on
// closeOnce, so it is a casualty of the stuck goroutine rather than its
// counterparty.
//
// enforcePolicy (terminal_sessions.go) takes exactly this shape in production,
// for exactly this reason.
func closeTerminals(d *system) {
	d.terminalMu.Lock()
	open := make([]*terminalSession, 0, len(d.terminals))
	for _, s := range d.terminals {
		open = append(open, s)
	}
	d.terminalMu.Unlock()
	for _, s := range open {
		s.close()
	}
}

func TestExpandHomeExpandsTildeAndEnv(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	t.Setenv("ORMOS_TEST_ROOT", "code")
	if got := expandHome("~"); got != home {
		t.Fatalf("expandHome(~) = %q, want %q", got, home)
	}
	if got := expandHome("~/app"); got != filepath.Join(home, "app") {
		t.Fatalf("expandHome(~/app) = %q", got)
	}
	// Local policy configuration keeps $VAR expansion.
	if got := expandHome("~/$ORMOS_TEST_ROOT/app"); got != filepath.Join(home, "code", "app") {
		t.Fatalf("expandHome(~/$ORMOS_TEST_ROOT/app) = %q", got)
	}
}

// The relay may ask for any directory string; what it may not do is have this
// machine's environment expanded for it. "$SECRET/work" answering differently
// from a literal path tells the relay whether SECRET is set, and to what.
func TestExpandRelayCwdRejectsEnvVars(t *testing.T) {
	t.Setenv("ORMOS_TEST_ROOT", "code")
	for _, p := range []string{"$ORMOS_TEST_ROOT/app", "~/work/$ORMOS_TEST_ROOT", "/opt/$ORMOS_TEST_ROOT", "${ORMOS_TEST_ROOT}"} {
		if _, err := expandRelayCwd(p); err == nil {
			t.Fatalf("expandRelayCwd(%q) must refuse a residual $", p)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	for p, want := range map[string]string{
		"~":          home,
		"~/work/app": filepath.Join(home, "work", "app"),
		"/opt/app":   "/opt/app",
		"relative":   "relative",
	} {
		got, err := expandRelayCwd(p)
		if err != nil || got != want {
			t.Fatalf("expandRelayCwd(%q) = %q, %v; want %q", p, got, err, want)
		}
	}
}

// End to end through the terminal handler: a cwd the relay could use to probe
// the environment is refused, not silently downgraded to the default
// directory — a shell in the default directory is still a shell.
func TestTerminalRefusesDollarCwd(t *testing.T) {
	withTempConfigDir(t) // no policy file: everything would otherwise be allowed
	d := &system{terminals: make(map[string]*terminalSession), audit: newAuditor()}
	header := fencedHeader(relay.StreamHeader{Kind: relay.KindTerminal, SessionID: "probe", Cwd: "$HOME", Cols: 80, Rows: 24})
	if _, err := d.terminal(header, acceptedFenceDeadline(t, header)); err == nil || !strings.Contains(err.Error(), "$") {
		t.Fatalf("terminal($HOME) error = %v, want a refusal", err)
	}
	if d.terminals["probe"] != nil {
		t.Fatal("a refused cwd must not leave a session behind")
	}
}

// A stream that never sends a header used to hold its slot forever:
// ReadHeader blocks until a newline, and slots are capped. With the deadline
// the stream is dropped and the slot freed.
func TestServeStreamDropsHeaderlessStream(t *testing.T) {
	prev := headerReadTO
	headerReadTO = 100 * time.Millisecond
	t.Cleanup(func() { headerReadTO = prev })

	agent, client := net.Pipe()
	defer client.Close()
	d := &system{}
	done := make(chan struct{})
	go func() {
		d.serveStream(agent)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stream that never sent a header was not dropped")
	}
}

func TestShutdownRequiresLiveAgentFence(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})
	stopped := make(chan struct{}, 1)
	d.setCancel(func() { stopped <- struct{}{} })

	send := func(header relay.StreamHeader) relay.ActionAck {
		agent, client := net.Pipe()
		done := make(chan struct{})
		go func() {
			d.serveStream(agent)
			close(done)
		}()
		if err := relay.WriteHeader(client, header); err != nil {
			t.Fatal(err)
		}
		ack, err := relay.ReadActionAck(client)
		if err != nil {
			t.Fatalf("reading shutdown ack: %v", err)
		}
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("shutdown stream did not finish")
		}
		if err := relay.ValidateActionAck(header, ack); err != nil {
			t.Fatalf("shutdown ack: %v", err)
		}
		return ack
	}

	expired := fencedHeader(relay.StreamHeader{Kind: relay.KindShutdown})
	expired.NotAfterMilli = time.Now().Add(-time.Millisecond).UnixMilli()
	if ack := send(expired); ack.Status != relay.ActionAckExpired {
		t.Fatalf("expired shutdown ack = %q", ack.Status)
	}
	select {
	case <-stopped:
		t.Fatal("expired shutdown fence stopped the agent")
	default:
	}

	refused := fencedHeader(relay.StreamHeader{Kind: relay.KindShutdown})
	refused.ActionFence = "short"
	if ack := send(refused); ack.Status != relay.ActionAckRefused {
		t.Fatalf("refused shutdown ack = %q", ack.Status)
	}

	if ack := send(fencedHeader(relay.StreamHeader{Kind: relay.KindShutdown})); ack.Status != relay.ActionAckSuccess {
		t.Fatalf("live shutdown ack = %q", ack.Status)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("live shutdown fence did not stop the agent")
	}
}

func TestShutdownRechecksFenceImmediatelyBeforeExecution(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})
	stopped := make(chan struct{}, 1)
	d.setCancel(func() { stopped <- struct{}{} })
	parked := make(chan struct{})
	release := make(chan struct{})
	d.beforeShutdownAction = func() {
		close(parked)
		<-release
	}

	agent, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		d.serveStream(agent)
		close(done)
	}()
	header := fencedHeader(relay.StreamHeader{Kind: relay.KindShutdown})
	header.NotAfterMilli = time.Now().Add(150 * time.Millisecond).UnixMilli()
	if err := relay.WriteHeader(client, header); err != nil {
		t.Fatal(err)
	}
	<-parked
	time.Sleep(time.Until(time.UnixMilli(header.NotAfterMilli)) + 25*time.Millisecond)
	close(release)
	ack, err := relay.ReadActionAck(client)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Status != relay.ActionAckExpired || relay.ValidateActionAck(header, ack) != nil {
		t.Fatalf("parked shutdown ack = %+v", ack)
	}
	select {
	case <-stopped:
		t.Fatal("shutdown executed after its fence expired")
	default:
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expired shutdown stream did not finish")
	}
}

type failingAckConn struct {
	net.Conn
	reader *bytes.Reader
}

func (c *failingAckConn) Read(p []byte) (int, error)       { return c.reader.Read(p) }
func (c *failingAckConn) Write([]byte) (int, error)        { return 0, errors.New("tunnel lost") }
func (c *failingAckConn) Close() error                     { return nil }
func (c *failingAckConn) SetReadDeadline(time.Time) error  { return nil }
func (c *failingAckConn) SetWriteDeadline(time.Time) error { return nil }

func TestShutdownAckFailureLeavesAgentRunning(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})
	stopped := make(chan struct{}, 1)
	d.setCancel(func() { stopped <- struct{}{} })
	header := fencedHeader(relay.StreamHeader{Kind: relay.KindShutdown})
	var wire bytes.Buffer
	if err := relay.WriteHeader(&wire, header); err != nil {
		t.Fatal(err)
	}
	d.serveStream(&failingAckConn{reader: bytes.NewReader(wire.Bytes())})
	select {
	case <-stopped:
		t.Fatal("failed success acknowledgment cancelled the agent")
	default:
	}
}

func TestSuccessfulShutdownAckImmediatelyFulfillsRootCancellation(t *testing.T) {
	var order []string
	if !commitShutdown(func() { order = append(order, "cancel") }, func() bool {
		order = append(order, "ack")
		return true
	}) {
		t.Fatal("successful ACK did not commit shutdown")
	}
	if got := strings.Join(order, ","); got != "ack,cancel" {
		t.Fatalf("shutdown commit order = %q, want ack,cancel", got)
	}
}

type stalledAckConn struct {
	net.Conn
	reader    *bytes.Reader
	started   chan struct{}
	release   chan struct{}
	writeDone chan struct{}
	mu        sync.Mutex
	deadline  time.Time
}

func (c *stalledAckConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *stalledAckConn) Write(p []byte) (int, error) {
	close(c.started)
	<-c.release // deliberately ignores both SetWriteDeadline and Close
	close(c.writeDone)
	return len(p), nil
}
func (c *stalledAckConn) Close() error                    { return nil }
func (c *stalledAckConn) SetReadDeadline(time.Time) error { return nil }
func (c *stalledAckConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.deadline = deadline
	c.mu.Unlock()
	return nil
}

func (c *stalledAckConn) writeDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline
}

func TestShutdownAckCannotCommitPastNotAfterWhenWriteStalls(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})
	base := time.Now()
	var elapsed atomic.Int64
	d.actionNow = func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
	stopped := make(chan struct{}, 1)
	d.setCancel(func() { stopped <- struct{}{} })
	header := fencedHeader(relay.StreamHeader{Kind: relay.KindShutdown})
	header.NotAfterMilli = base.Add(500 * time.Millisecond).UnixMilli()
	actionDeadline, err := relay.AcceptStreamFence(header, d.actionTime())
	if err != nil {
		t.Fatal(err)
	}
	conn := &stalledAckConn{
		reader: bytes.NewReader(nil), started: make(chan struct{}),
		release: make(chan struct{}), writeDone: make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		if commitShutdown(d.shutdownCancel(), func() bool {
			return d.writeShutdownAck(conn, header, actionDeadline, relay.ActionAckSuccess)
		}) {
			// commitShutdown itself invokes cancellation; no additional action.
		}
		close(done)
	}()
	<-conn.started
	elapsed.Store(int64(actionDeadline.Sub(base) + time.Millisecond))
	close(conn.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stalled ACK was not bounded by NotAfter")
	}
	if got, want := conn.writeDeadline(), time.UnixMilli(header.NotAfterMilli); !got.Equal(want) {
		t.Fatalf("ACK deadline = %s, want NotAfter %s", got, want)
	}
	select {
	case <-stopped:
		t.Fatal("ACK that stalled past NotAfter cancelled the agent")
	default:
	}
	select {
	case <-conn.writeDone:
	case <-time.After(time.Second):
		t.Fatal("late ACK writer did not retire")
	}
}

func TestShutdownAckDeadlineUsesOneSecondWhenFenceIsLater(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})
	header := fencedHeader(relay.StreamHeader{Kind: relay.KindShutdown})
	release := make(chan struct{})
	close(release)
	conn := &stalledAckConn{
		reader: bytes.NewReader(nil), started: make(chan struct{}),
		release: release, writeDone: make(chan struct{}),
	}
	before := time.Now()
	if !d.writeShutdownAck(conn, header, acceptedFenceDeadline(t, header), relay.ActionAckSuccess) {
		t.Fatal("immediate ACK did not complete")
	}
	after := time.Now()
	deadline := conn.writeDeadline()
	if deadline.Before(before.Add(900*time.Millisecond)) || deadline.After(after.Add(shutdownAckWriteTO)) {
		t.Fatalf("ACK deadline = %s, want now + %s", deadline, shutdownAckWriteTO)
	}
}

func TestShutdownAckAsyncWritersAreCapped(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})
	header := fencedHeader(relay.StreamHeader{Kind: relay.KindShutdown})
	header.NotAfterMilli = time.Now().Add(300 * time.Millisecond).UnixMilli()
	actionDeadline := acceptedFenceDeadline(t, header)
	conns := make([]*stalledAckConn, 0, maxAsyncShutdownAckWrites)
	results := make(chan bool, maxAsyncShutdownAckWrites)
	for range maxAsyncShutdownAckWrites {
		conn := &stalledAckConn{
			reader: bytes.NewReader(nil), started: make(chan struct{}),
			release: make(chan struct{}), writeDone: make(chan struct{}),
		}
		conns = append(conns, conn)
		go func() { results <- d.writeShutdownAck(conn, header, actionDeadline, relay.ActionAckSuccess) }()
	}
	for _, conn := range conns {
		select {
		case <-conn.started:
		case <-time.After(time.Second):
			t.Fatal("capped ACK writer did not start")
		}
	}
	extra := &stalledAckConn{
		reader: bytes.NewReader(nil), started: make(chan struct{}),
		release: make(chan struct{}), writeDone: make(chan struct{}),
	}
	extraHeader := fencedHeader(relay.StreamHeader{Kind: relay.KindShutdown})
	if d.writeShutdownAck(extra, extraHeader, acceptedFenceDeadline(t, extraHeader), relay.ActionAckSuccess) {
		t.Fatal("ACK beyond the global writer cap succeeded")
	}
	select {
	case <-extra.started:
		t.Fatal("ACK beyond the global writer cap spawned a writer")
	default:
	}
	for range maxAsyncShutdownAckWrites {
		select {
		case committed := <-results:
			if committed {
				t.Fatal("stalled ACK unexpectedly committed")
			}
		case <-time.After(time.Second):
			t.Fatal("stalled capped ACK caller did not return")
		}
	}
	for _, conn := range conns {
		close(conn.release)
		select {
		case <-conn.writeDone:
		case <-time.After(time.Second):
			t.Fatal("stalled capped ACK writer did not retire")
		}
	}
}

func TestShutdownAckCrossesWebSocketBeforeRootCancellationClosesTunnel(t *testing.T) {
	withTempConfigDir(t)
	received := make(chan relay.ActionAck)
	tunnelClosed := make(chan struct{})
	serverErr := make(chan error, 1)
	header := fencedHeader(relay.StreamHeader{Kind: relay.KindShutdown})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/system/terminal-sessions" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"sessions":[]}`)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		sess, err := relay.ClientSession(relay.NetConn(context.Background(), ws))
		if err != nil {
			serverErr <- err
			return
		}
		defer sess.Close()
		stream, err := sess.Open()
		if err != nil {
			serverErr <- err
			return
		}
		defer stream.Close()
		if err := relay.WriteHeader(stream, header); err != nil {
			serverErr <- err
			return
		}
		ack, err := relay.ReadActionAck(stream)
		if err != nil {
			serverErr <- err
			return
		}
		received <- ack
		<-sess.CloseChan()
		close(tunnelClosed)
	}))
	defer srv.Close()

	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	d := newSystem(systemConfig{
		RelayURL:     "ws" + strings.TrimPrefix(srv.URL, "http"),
		PairingToken: "test-pairing-token",
	})
	cancelInvoked := make(chan struct{})
	d.setCancel(func() {
		close(cancelInvoked)
		cancelRoot()
	})
	result := make(chan error, 1)
	go func() {
		_, err := d.connectAndServe(root)
		result <- err
	}()

	select {
	case err := <-serverErr:
		t.Fatal(err)
	case ack := <-received:
		if ack.Status != relay.ActionAckSuccess || relay.ValidateActionAck(header, ack) != nil {
			t.Fatalf("shutdown acknowledgment = %+v", ack)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backend did not receive shutdown acknowledgment")
	}
	select {
	case <-cancelInvoked:
	case <-time.After(time.Second):
		t.Fatal("root cancellation was not invoked after acknowledgment")
	}
	select {
	case <-tunnelClosed:
	case err := <-serverErr:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("tunnel stayed open after acknowledged shutdown")
	}
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("agent tunnel did not return after root cancellation")
	}
}

func TestProxyFenceBoundsBothDialsAndPostDialBoundary(t *testing.T) {
	withTempConfigDir(t)
	const port = 3000

	t.Run("parked before dial", func(t *testing.T) {
		d := newSystem(systemConfig{})
		d.mu.Lock()
		d.ports = []PortStatus{{Port: port}}
		d.mu.Unlock()
		parked := make(chan struct{})
		release := make(chan struct{})
		d.beforeProxyDial = func() { close(parked); <-release }
		var calls atomic.Int32
		d.proxyDialContext = func(context.Context, string, string) (net.Conn, error) {
			calls.Add(1)
			return nil, fmt.Errorf("unexpected dial")
		}
		agent, client := net.Pipe()
		defer client.Close()
		done := make(chan struct{})
		header := fencedHeader(relay.StreamHeader{Kind: relay.KindProxy, Port: port})
		header.NotAfterMilli = time.Now().Add(150 * time.Millisecond).UnixMilli()
		actionDeadline := acceptedFenceDeadline(t, header)
		go func() { d.handleProxy(agent, agent, header, actionDeadline); agent.Close(); close(done) }()
		<-parked
		time.Sleep(time.Until(time.UnixMilli(header.NotAfterMilli)) + 25*time.Millisecond)
		close(release)
		<-done
		if calls.Load() != 0 {
			t.Fatalf("expired proxy made %d dials", calls.Load())
		}
	})

	t.Run("blocked dials inherit not-after", func(t *testing.T) {
		d := newSystem(systemConfig{})
		d.mu.Lock()
		d.ports = []PortStatus{{Port: port}}
		d.mu.Unlock()
		var calls atomic.Int32
		d.proxyDialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			if calls.Add(1) == 1 {
				return nil, fmt.Errorf("IPv4 dropped")
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		agent, client := net.Pipe()
		defer client.Close()
		go func() { _, _ = io.Copy(io.Discard, client) }()
		header := fencedHeader(relay.StreamHeader{Kind: relay.KindProxy, Port: port})
		header.NotAfterMilli = time.Now().Add(150 * time.Millisecond).UnixMilli()
		done := make(chan struct{})
		start := time.Now()
		actionDeadline := acceptedFenceDeadline(t, header)
		go func() { d.handleProxy(agent, agent, header, actionDeadline); agent.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("blocked proxy dial outlived NotAfter")
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("proxy dial returned after %s", elapsed)
		}
		if calls.Load() != 2 {
			t.Fatalf("loopback dial attempts = %d, want both addresses", calls.Load())
		}
	})

	t.Run("parked after dial closes late connection", func(t *testing.T) {
		d := newSystem(systemConfig{})
		d.mu.Lock()
		d.ports = []PortStatus{{Port: port}}
		d.mu.Unlock()
		local, peer := net.Pipe()
		defer peer.Close()
		d.proxyDialContext = func(context.Context, string, string) (net.Conn, error) { return local, nil }
		parked := make(chan struct{})
		release := make(chan struct{})
		d.afterProxyDial = func() { close(parked); <-release }
		agent, client := net.Pipe()
		defer client.Close()
		header := fencedHeader(relay.StreamHeader{Kind: relay.KindProxy, Port: port})
		header.NotAfterMilli = time.Now().Add(150 * time.Millisecond).UnixMilli()
		done := make(chan struct{})
		actionDeadline := acceptedFenceDeadline(t, header)
		go func() { d.handleProxy(agent, agent, header, actionDeadline); agent.Close(); close(done) }()
		<-parked
		time.Sleep(time.Until(time.UnixMilli(header.NotAfterMilli)) + 25*time.Millisecond)
		close(release)
		<-done
		_ = peer.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := peer.Read(make([]byte, 1)); err == nil {
			t.Fatal("post-deadline proxy connection remained open")
		}
	})
}

func TestProxyRollbackDuringStallCannotExtendAcceptedFence(t *testing.T) {
	withTempConfigDir(t)
	const port = 3000
	d := newSystem(systemConfig{})
	d.mu.Lock()
	d.ports = []PortStatus{{Port: port}}
	d.mu.Unlock()
	accepted := time.Now()
	var elapsed atomic.Int64
	d.actionNow = func() time.Time { return accepted.Add(time.Duration(elapsed.Load())) }
	header := fencedHeader(relay.StreamHeader{Kind: relay.KindProxy, Port: port})
	header.NotAfterMilli = accepted.Add(200 * time.Millisecond).UnixMilli()
	actionDeadline, err := relay.AcceptStreamFence(header, d.actionTime())
	if err != nil {
		t.Fatal(err)
	}
	// Equivalent to a wall rollback after acceptance: the unchanged absolute
	// timestamp would now appear much farther away if the handler reconstructed
	// it. The carried monotonic deadline must remain the sole authority.
	header.NotAfterMilli = accepted.Add(time.Hour).UnixMilli()
	parked := make(chan struct{})
	release := make(chan struct{})
	d.beforeProxyDial = func() { close(parked); <-release }
	var dials atomic.Int32
	d.proxyDialContext = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, fmt.Errorf("unexpected dial")
	}
	agent, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() { d.handleProxy(agent, agent, header, actionDeadline); agent.Close(); close(done) }()
	<-parked
	elapsed.Store(int64(250 * time.Millisecond))
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("wall rollback extended a stalled proxy fence")
	}
	if dials.Load() != 0 {
		t.Fatalf("wall rollback permitted %d post-deadline dials", dials.Load())
	}
}

func TestTerminalRechecksFenceAfterHandshakeBeforeCreate(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{Shell: "/bin/sh"})
	parked := make(chan struct{})
	release := make(chan struct{})
	d.beforeTerminalAction = func() { close(parked); <-release }
	header := fencedHeader(relay.StreamHeader{
		Kind: relay.KindTerminal, SessionID: "parked-create", Cols: 80, Rows: 24,
	})
	header.NotAfterMilli = time.Now().Add(200 * time.Millisecond).UnixMilli()
	agent, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() { d.serveStream(agent); close(done) }()
	if err := relay.WriteHeader(client, header); err != nil {
		t.Fatal(err)
	}
	clientKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.WriteClientHello(client, clientKey.PublicKey().Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := relay.ReadServerHello(client); err != nil {
		t.Fatal(err)
	}
	<-parked
	time.Sleep(time.Until(time.UnixMilli(header.NotAfterMilli)) + 25*time.Millisecond)
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expired terminal create did not return")
	}
	if d.terminals[header.SessionID] != nil {
		t.Fatal("terminal PTY was created after NotAfter")
	}
}

func TestTerminalRechecksFenceBeforeReattach(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{Shell: "/bin/sh"})
	d.terminals["parked-reattach"] = &terminalSession{id: "parked-reattach", owner: d, cwd: ""}
	parked := make(chan struct{})
	release := make(chan struct{})
	d.beforeTerminalAction = func() { close(parked); <-release }
	header := fencedHeader(relay.StreamHeader{
		Kind: relay.KindTerminal, SessionID: "parked-reattach", Cols: 80, Rows: 24,
	})
	header.NotAfterMilli = time.Now().Add(150 * time.Millisecond).UnixMilli()
	done := make(chan error, 1)
	deadline := acceptedFenceDeadline(t, header)
	go func() { _, err := d.terminal(header, deadline); done <- err }()
	<-parked
	time.Sleep(time.Until(time.UnixMilli(header.NotAfterMilli)) + 25*time.Millisecond)
	close(release)
	if err := <-done; !relay.IsStreamFenceExpired(err) {
		t.Fatalf("reattach error = %v, want expired fence", err)
	}
}

// The deadline covers the header only: a stream that announced itself
// promptly must keep working past it — a proxy pipe's only pacing after the
// header is the idle bound, which bytes must keep resetting.
func TestServeStreamClearsHeaderDeadline(t *testing.T) {
	prev := headerReadTO
	headerReadTO = 150 * time.Millisecond
	t.Cleanup(func() { headerReadTO = prev })

	withTempConfigDir(t)
	port := echoServer(t)
	d := newSystem(systemConfig{})
	d.mu.Lock()
	d.ports = []PortStatus{{Port: port}}
	d.mu.Unlock()

	agent, client := net.Pipe()
	defer client.Close()
	go d.serveStream(agent)
	if err := relay.WriteHeader(client, fencedHeader(relay.StreamHeader{Kind: relay.KindProxy, Port: port})); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * headerReadTO)
	msg := []byte("still alive")
	go func() { _, _ = client.Write(msg) }() // net.Pipe write blocks until read
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("proxy pipe after the header deadline: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo = %q, want %q", buf, msg)
	}
}

// The same hole #211 closed in the terminal handshake, on the last dispatch
// target that still had it: a relay that passes the policy checks could open
// a proxy stream, send the header, and then say nothing, pinning a stream
// slot and a local socket for as long as the agent runs. With the idle bound
// the pipe is dropped and both are freed. Proved by pinning, per the standard
// in #211: without the bound this never returns.
func TestProxyPipeDropsASilentPeer(t *testing.T) {
	prev := proxyIdleTO
	proxyIdleTO = 150 * time.Millisecond
	t.Cleanup(func() { proxyIdleTO = prev })

	withTempConfigDir(t)
	port := echoServer(t)
	d := newSystem(systemConfig{})
	d.mu.Lock()
	d.ports = []PortStatus{{Port: port}}
	d.mu.Unlock()

	agent, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		d.serveStream(agent)
		close(done)
	}()
	if err := relay.WriteHeader(client, fencedHeader(relay.StreamHeader{Kind: relay.KindProxy, Port: port})); err != nil {
		t.Fatal(err)
	}
	// ...and then nothing, ever.
	start := time.Now()
	select {
	case <-done:
		// Bounded both ways: returning EARLY would mean something other than
		// the idle bound ended the pipe, and a future guard placed before the
		// copies would keep this green with the bound gone.
		if elapsed := time.Since(start); elapsed < proxyIdleTO {
			t.Fatalf("the pipe ended after %s, before the idle bound; it was not the bound that dropped it", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a proxy pipe whose peer went silent was not dropped")
	}
}

// The other half, and the one that fails as a user-visible disconnect rather
// than as a red test: the bound must RESET on transferred bytes, not run from
// when the pipe opened. A proxy pipe is honestly long-lived — a previewed
// app's WebSocket, a sparse event stream — so a pipe with traffic every
// proxyIdleTO/2 must still be alive after four idle bounds.
func TestProxyPipeResetsItsIdleDeadlineOnBytes(t *testing.T) {
	prev := proxyIdleTO
	proxyIdleTO = 150 * time.Millisecond
	t.Cleanup(func() { proxyIdleTO = prev })

	withTempConfigDir(t)
	port := echoServer(t)
	d := newSystem(systemConfig{})
	d.mu.Lock()
	d.ports = []PortStatus{{Port: port}}
	d.mu.Unlock()

	agent, client := net.Pipe()
	defer client.Close()
	go d.serveStream(agent)
	if err := relay.WriteHeader(client, fencedHeader(relay.StreamHeader{Kind: relay.KindProxy, Port: port})); err != nil {
		t.Fatal(err)
	}
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	deadline := time.Now().Add(4 * proxyIdleTO)
	for time.Now().Before(deadline) {
		if _, err := client.Write([]byte("x")); err != nil {
			t.Fatalf("a busy pipe was dropped inside the idle bound: %v", err)
		}
		if _, err := io.ReadFull(client, make([]byte, 1)); err != nil {
			t.Fatalf("echo on a busy pipe: %v", err)
		}
		time.Sleep(proxyIdleTO / 2)
	}
}

// One-way traffic is progress too: an SSE response or a WebSocket whose
// server is the only side currently speaking must re-arm BOTH ends of the
// pipe. No client payload follows the stream header here; repeated bytes from
// the local service alone keep the pipe alive past four idle bounds.
func TestProxyPipeOneWayTrafficResetsItsIdleDeadline(t *testing.T) {
	prev := proxyIdleTO
	proxyIdleTO = 150 * time.Millisecond
	t.Cleanup(func() { proxyIdleTO = prev })

	withTempConfigDir(t)
	port := pulseServer(t, proxyIdleTO/2)
	d := newSystem(systemConfig{})
	d.mu.Lock()
	d.ports = []PortStatus{{Port: port}}
	d.mu.Unlock()

	agent, client := net.Pipe()
	defer client.Close()
	go d.serveStream(agent)
	if err := relay.WriteHeader(client, fencedHeader(relay.StreamHeader{Kind: relay.KindProxy, Port: port})); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	deadline := time.Now().Add(4 * proxyIdleTO)
	for time.Now().Before(deadline) {
		if _, err := io.ReadFull(client, make([]byte, 1)); err != nil {
			t.Fatalf("one-way traffic did not keep the proxy pipe alive: %v", err)
		}
	}
}

// A reset is shared state across TWO conns, not two independent safe calls.
// Hold the first reset halfway through (stream advanced, local not yet), launch
// a concurrent reset, and prove the lock covers the complete operation. Once
// released, the second reset calculates a later deadline and leaves BOTH conns
// at it. Without serialization the old activity can resume last and overwrite
// local with its stale deadline.
func TestProxyIdleDeadlineSerializesConcurrentResets(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	times := make(chan time.Time, 2)
	times <- base
	times <- base.Add(time.Minute)

	stream := &deadlineConn{}
	localEntered := make(chan struct{})
	releaseLocal := make(chan struct{})
	local := &deadlineConn{beforeFirstSet: func() {
		close(localEntered)
		<-releaseLocal
	}}
	idle := newProxyIdleDeadline(5*time.Minute, stream, local)
	idle.now = func() time.Time { return <-times }

	firstDone := make(chan struct{})
	go func() {
		idle.reset()
		close(firstDone)
	}()
	<-localEntered // first reset is halfway through its two-conn transaction

	// The mutex must remain held until local receives the same deadline.
	if idle.mu.TryLock() {
		idle.mu.Unlock()
		t.Fatal("the shared reset lock was released between the two SetDeadline calls")
	}

	secondStarted := make(chan struct{})
	secondDone := make(chan struct{})
	go func() {
		close(secondStarted)
		idle.reset()
		close(secondDone)
	}()
	<-secondStarted
	close(releaseLocal)
	<-firstDone
	<-secondDone

	want := base.Add(time.Minute + 5*time.Minute)
	if got := stream.deadline(); !got.Equal(want) {
		t.Fatalf("stream deadline = %s, want latest activity deadline %s", got, want)
	}
	if got := local.deadline(); !got.Equal(want) {
		t.Fatalf("local deadline = %s, want latest activity deadline %s", got, want)
	}
}

// deadlineConn is the smallest controllable net.Conn for the concurrent-reset
// test. Only SetDeadline is called; embedding net.Conn supplies the rest of the
// interface without pretending those methods participate in the test.
type deadlineConn struct {
	net.Conn
	mu             sync.Mutex
	set            time.Time
	sets           int
	beforeFirstSet func()
}

func (c *deadlineConn) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.sets++
	first := c.sets == 1
	hook := c.beforeFirstSet
	c.mu.Unlock()
	if first && hook != nil {
		hook()
	}
	c.mu.Lock()
	c.set = deadline
	c.mu.Unlock()
	return nil
}

func (c *deadlineConn) deadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.set
}

// echoServer stands up a loopback TCP echoer and returns its port: the local
// end a proxy pipe test dials, with an answer for whatever it is sent.
func echoServer(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// pulseServer writes one byte at interval for as long as its client remains
// connected; it never reads, making the test traffic strictly one-way.
func pulseServer(t *testing.T, interval time.Duration) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			if _, err := c.Write([]byte("x")); err != nil {
				return
			}
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// The threat model includes a compromised relay. Such a relay could open a
// terminal stream, send the header — which satisfied headerReadTO — and then
// never send the client hello, because serveStream cleared its deadline exactly
// one read too early. Each such stream pinned a goroutine and one of the
// relay.MaxTunnelStreams slots for ever, until the agent could serve nothing.
func TestTerminalHandshakeDeadlineDropsASilentPeer(t *testing.T) {
	prev := terminalHandshakeTO
	terminalHandshakeTO = 150 * time.Millisecond
	t.Cleanup(func() { terminalHandshakeTO = prev })

	withTempConfigDir(t)
	agent, client := net.Pipe()
	defer client.Close()
	d := &system{terminals: make(map[string]*terminalSession), audit: newAuditor()}
	done := make(chan struct{})
	go func() {
		d.serveStream(agent)
		close(done)
	}()
	header := fencedHeader(relay.StreamHeader{Kind: relay.KindTerminal, SessionID: "silent", Cols: 80, Rows: 24})
	if err := relay.WriteHeader(client, header); err != nil {
		t.Fatal(err)
	}
	// ...and then nothing, ever.
	start := time.Now()
	select {
	case <-done:
		// Bounded both ways: returning EARLY would mean something other than
		// the deadline ended the stream, and a future guard placed before
		// ReadClientHello would keep this green with the deadline gone.
		if elapsed := time.Since(start); elapsed < terminalHandshakeTO {
			t.Fatalf("the stream ended after %s, before the handshake deadline; it was not the deadline that dropped it", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a terminal stream that never sent its client hello was not dropped")
	}
}

// The other half, and the one that fails as a user-visible disconnect rather
// than as a red test: the deadline must be CLEARED once the handshake is done.
// Leaving it armed kills every terminal, busy or not, terminalHandshakeTO after
// it opens.
//
// The keystroke after the idle wait is asserted to come back OPENED, not merely
// written. A WriteFrame that returns nil says the bytes left this process; it
// says nothing about whether the peer holds the key to open them, so it is not
// evidence of a working seal (nicodes/ormos-be#423). What this liveness check
// needs is that the far end acted on the frame, which the echo is.
//
// It is still a deadline test, not a key test. The keys here are both generated
// in this function, so nothing about the key the agent PUBLISHES is exercised —
// TestTheAdvertisedKeyIsTheKeyTerminalsAreSealedWith is what covers that.
func TestIdleTerminalSurvivesTheHandshakeDeadline(t *testing.T) {
	prev := terminalHandshakeTO
	terminalHandshakeTO = 300 * time.Millisecond
	t.Cleanup(func() { terminalHandshakeTO = prev })

	withTempConfigDir(t)
	agentKey, err := relay.GenerateAgentKey()
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := relay.GenerateAgentKey()
	if err != nil {
		t.Fatal(err)
	}
	d := &system{key: agentKey, terminals: make(map[string]*terminalSession), audit: newAuditor()}
	d.cfg.Shell = "/bin/sh"
	agent, client := net.Pipe()
	defer client.Close()
	go d.serveStream(agent)

	h := fencedHeader(relay.StreamHeader{Kind: relay.KindTerminal, SessionID: "idle", Cols: 80, Rows: 24})
	if err := relay.WriteHeader(client, h); err != nil {
		t.Fatal(err)
	}
	if err := relay.WriteClientHello(client, clientKey.PublicKey().Bytes()); err != nil {
		t.Fatal(err)
	}
	salt, err := relay.ReadServerHello(client)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := relay.DeriveSessionKeys(clientKey, agentKey.PublicKey().Bytes(), salt, h.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := relay.NewSealedStream(client, client, keys.ClientToAgent, keys.AgentToClient)
	if err != nil {
		t.Fatal(err)
	}
	// Drain whatever the shell says, so the agent's writer never blocks — and
	// keep what was successfully OPENED, because that is what the assertion
	// below needs. A drain that discarded both the frames and their errors is
	// what let this test look like it covered the seal.
	opened := make(chan []byte, terminalSendQueue)
	go func() {
		defer close(opened)
		for {
			frame, err := sealed.ReadFrame()
			if err != nil {
				return
			}
			select {
			case opened <- frame.Data:
			default: // full: still draining, still never blocking the writer
			}
		}
	}()

	// An idle terminal is not a broken one.
	time.Sleep(4 * terminalHandshakeTO)
	const marker = "still-here"
	if err := sealed.WriteFrame(relay.EncodeData([]byte("echo " + marker + "\n"))); err != nil {
		t.Fatalf("an idle terminal was dropped %s after its handshake: %v", 4*terminalHandshakeTO, err)
	}
	t.Cleanup(func() { closeTerminals(d) })
	// The write above only left this process. The terminal is live once the PTY
	// has echoed the keystroke back and this side has opened it.
	giveUp := time.After(30 * time.Second)
	var seen []byte
	for !bytes.Contains(seen, []byte(marker)) {
		select {
		case data, ok := <-opened:
			if !ok {
				t.Fatalf("the sealed stream ended before the keystroke came back; opened %q so far", seen)
			}
			seen = append(seen, data...)
		case <-giveUp:
			t.Fatalf("the keystroke was written but never came back opened; opened %q so far", seen)
		}
	}
}

// proxyResponse runs one refused proxy stream and returns the exact bytes the
// agent wrote, alongside the parsed response.
//
// Both, because they catch different things. Parsing catches a malformed status
// line. The RAW bytes catch everything the parser is too forgiving about: a
// missing Connection: close, a bare LF where the protocol wants CRLF, an
// under-declared Content-Length. net/http accepts all three -- it bounds the
// body BY the declared Content-Length, so `len(body) == resp.ContentLength` is
// true by construction and can never fail.
func proxyResponse(t *testing.T, d *system, port int) (*http.Response, string, string) {
	t.Helper()
	agent, client := net.Pipe()
	defer client.Close()
	if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	header := fencedHeader(relay.StreamHeader{Kind: relay.KindProxy, Port: port})
	actionDeadline := acceptedFenceDeadline(t, header)
	go func() {
		d.handleProxy(agent, agent, header, actionDeadline)
		// serveStream's deferred Close, which is what ends the response.
		agent.Close()
	}()
	raw, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("reading the agent's refusal: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), nil)
	if err != nil {
		t.Fatalf("the agent's refusal is not a readable HTTP response: %v (%q)", err, raw)
	}
	t.Cleanup(func() { resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the refusal body: %v", err)
	}
	return resp, string(body), string(raw)
}

// wantProxyError is the response every refusal must produce, spelled out once
// here so the test does not reuse the helper's own format string to check the
// helper. A mistake in writeProxyError has to disagree with something.
func wantProxyError(status, body string) string {
	return "HTTP/1.1 " + status + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"X-Content-Type-Options: nosniff\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
		"Connection: close\r\n" +
		"\r\n" + body
}

// Every refusal goes out through writeProxyError, so these pin the exact bytes
// the browser receives. Four hand-assembled copies of that format string was
// four places a future edit lands on three.
func TestProxyRefusalsAreWellFormedHTTP(t *testing.T) {
	withTempConfigDir(t)
	// A relay URL that refuses instantly, so the "not exposed" branch below
	// fails its lookup fast instead of waiting out a real timeout.
	closed := closedLoopbackPort(t)
	d := newSystem(systemConfig{RelayURL: fmt.Sprintf("ws://127.0.0.1:%d", closed)})

	t.Run("refused by local policy", func(t *testing.T) {
		// A privileged port: the built-in guard refuses it with no policy file.
		resp, body, raw := proxyResponse(t, d, 22)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("answered %s, want 403", resp.Status)
		}
		// "refuses port 22" rather than "Local policy", which both 403 bodies
		// start with -- a regression into the fail-closed unreadable-policy
		// branch would otherwise stay green.
		want := wantProxyError("403 Forbidden", "Local policy on this system refuses port 22.\n")
		if raw != want {
			t.Errorf("raw response:\n%q\nwant:\n%q", raw, want)
		}
		if !strings.Contains(body, "refuses port 22") {
			t.Errorf("the refusal does not say which port: %q", body)
		}
	})

	t.Run("not exposed", func(t *testing.T) {
		// An ordinary dev port local policy allows, which the relay does not
		// list -- and the relay cannot be reached to ask, so it fails closed.
		resp, _, raw := proxyResponse(t, d, 3000)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("answered %s, want 403", resp.Status)
		}
		want := wantProxyError("403 Forbidden", "Port 3000 is not exposed on this system.\n")
		if raw != want {
			t.Errorf("raw response:\n%q\nwant:\n%q", raw, want)
		}
	})

	t.Run("nothing listening", func(t *testing.T) {
		// Allowed and exposed, but dead: a 502 with an explanation, because
		// what the user sees is an iframe and a bare EOF renders blank.
		dead := closedLoopbackPort(t)
		d.mu.Lock()
		d.ports = []PortStatus{{Port: dead}}
		d.mu.Unlock()

		resp, _, raw := proxyResponse(t, d, dead)
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("answered %s, want 502", resp.Status)
		}
		want := wantProxyError("502 Bad Gateway", fmt.Sprintf(
			"Nothing is listening on port %d on this system.\nStart your app on that port and reload.\n", dead))
		if raw != want {
			t.Errorf("raw response:\n%q\nwant:\n%q", raw, want)
		}
	})

	t.Run("policy unreadable", func(t *testing.T) {
		// The fourth call site: a policy file that will not parse denies
		// everything, and says so rather than serving.
		dir := withTempConfigDir(t)
		if err := os.WriteFile(filepath.Join(dir, policyFileName), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		d := &system{terminals: make(map[string]*terminalSession), audit: newAuditor()}

		resp, _, raw := proxyResponse(t, d, 3000)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("answered %s, want 403", resp.Status)
		}
		want := wantProxyError("403 Forbidden",
			"Local policy on this system could not be read; nothing is being served.\n")
		if raw != want {
			t.Errorf("raw response:\n%q\nwant:\n%q", raw, want)
		}
	})
}

// closedLoopbackPort returns a loopback port with nothing listening on it: a
// connection there is refused immediately rather than timing out.
//
// Binding :0 and closing is the only way to name a port the OS agrees is free,
// so this is the one non-deterministic thing in these tests: if the kernel
// hands the same ephemeral port to something else in the gap, a subtest here
// goes red for a reason that is not handleProxy. Rare enough to live with,
// worth knowing before going looking in the wrong place.
func closedLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// The literal pin in relay/protocol_test.go records what the handshake header
// names ARE. It cannot see whether the agent still sends them, and that was the
// larger half of the risk: until this test, deleting either entry from
// connectAndServe's HTTPHeader map left the whole suite green, and the
// consequence is exactly the silent failure the pin's own comment describes —
// the relay never calls SetSystemPubKey so browsers keep sealing to a stale key,
// or the fence version goes missing and absence is the reserved v0.1.5 sentinel,
// so every current agent reads as released-legacy and quietly gives up its
// action fences. This test is what closes that, so it is worth keeping rather
// than consolidating away.
//
// So the dial is driven for real and the request read on the other side. Header
// lookup is canonicalised here, deliberately: HTTP header names are
// case-insensitive on the wire, so this asserts the header ARRIVES while the
// literal pin asserts what it is spelled.
func TestAgentDialAdvertisesItsKeyAndFenceVersion(t *testing.T) {
	withTempConfigDir(t)
	headers := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case headers <- r.Header.Clone():
		default:
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Nothing is served: the dial itself is the subject.
		ws.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	d := newSystem(systemConfig{
		RelayURL:     "ws" + strings.TrimPrefix(srv.URL, "http"),
		PairingToken: "test-pairing-token",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The tunnel dying immediately is expected and not what is under test.
	_, _ = d.connectAndServe(ctx)

	select {
	case h := <-headers:
		// The VALUE, not merely presence: "browsers keep sealing to a stale
		// key" is the outcome an advertised-but-wrong key produces, so an
		// assertion that only checked for non-empty would name a failure it
		// could not see.
		//
		// One direction only, to be exact about it: this pins the advertised
		// header to d.key, which is the key DeriveSessionKeys reads
		// (terminal_sessions.go). A divergence introduced on the SEAL side --
		// sealing with something other than d.key while the dial keeps
		// advertising it -- is not visible here; that is what
		// TestTheAdvertisedKeyIsTheKeyTerminalsAreSealedWith below covers, by
		// deriving the browser's side from the advertised header rather than
		// from d.key (nicodes/ormos-be#423).
		if got, want := h.Get(relay.PublicKeyHeader), encodePublicKey(d.key); got != want {
			t.Errorf("the dial advertised %s = %q, want the key this agent seals with, %q: the relay stores what it is told, so a wrong or absent key means browsers seal to a key the agent cannot open and terminals stop opening with no error anywhere",
				relay.PublicKeyHeader, got, want)
		}
		if got := h.Get(relay.StreamFenceVersionHeader); got != relay.StreamFenceVersion {
			t.Errorf("the dial carried %s = %q, want %q: absence is the reserved v0.1.5 sentinel, so a missing header does not fail — it downgrades this agent to legacy and gives up the action fences",
				relay.StreamFenceVersionHeader, got, relay.StreamFenceVersion)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relay never received a dial")
	}
}

// The severe case of the class, and the only one that breaks a single
// self-consistent build rather than a mixed-version pair: the key the dial
// ADVERTISES and the key handleTerminal SEALS with are two separate reads of
// d.key, and nothing tied them together. Sealing with a freshly generated key
// per handshake while connectAndServe kept advertising d.key left the entire
// agent suite green (nicodes/ormos-be#423). The relay stores whatever key it is
// told and browsers seal to that, so the consequence is every terminal on every
// system failing immediately on a rejected AEAD open of frames that arrived
// looking perfectly well-formed — with no error saying why.
//
// So both halves run against one system and the seal does the asserting. The
// dial is real; its header value is decoded the way the relay decodes it; and
// the browser's side of the handshake derives from THAT value and from nothing
// else. Diverge the two keys and the derived directions diverge with them, so
// the first frame the agent sends cannot be opened.
//
// What this covers:
//
//   - the advertised key is the key DeriveSessionKeys receives, exercised in
//     both directions of one live sealed stream: the reset frame attach queues
//     is opened here (agent -> browser), and a sealed keystroke reaches the PTY
//     and comes back echoed (browser -> agent).
//   - that the advertised value decodes as padded standard base64 of a
//     SealKeySize key. That is a consequence of needing the bytes, not the
//     subject, and it is an uneven detector, so it must not be read as a pin on
//     the encoding: losing the PADDING is caught every time (43 characters,
//     which the standard decoder refuses outright), while swapping the ALPHABET
//     alone is caught only when the key happens to encode with a '+' or a '/' --
//     73.11% of 20,000 random X25519 keys, measured locally. A literal pin for
//     the alphabet is nicodes/ormos-be#421, which is not on this branch.
//
// What it does NOT cover:
//
//   - anything on the relay's side. The relay is a different binary in a
//     different repository; that it stores this value and hands it to the
//     browser unchanged cannot be seen from here.
//   - the out-of-band fingerprint (relay.Fingerprint, printed on startup and
//     shown in the dashboard). It is derived from the same key at a different
//     call site, and a divergence there is a comparison a user fails rather
//     than a terminal that cannot open.
//   - a browser that keeps sealing to a PREVIOUSLY advertised key. This drives
//     one dial and one handshake against one key; key rotation across a
//     reconnect is the republish this test's dial performs, not a case it
//     asserts about.
func TestTheAdvertisedKeyIsTheKeyTerminalsAreSealedWith(t *testing.T) {
	withTempConfigDir(t)
	headers := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/system/terminal-sessions" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"sessions":[{"id":"advertised-key","state":"running","generation":1}]}`)
			return
		}
		select {
		case headers <- r.Header.Clone():
		default:
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	d := newSystem(systemConfig{
		RelayURL:     "ws" + strings.TrimPrefix(srv.URL, "http"),
		PairingToken: "test-pairing-token",
		Shell:        "/bin/sh",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The tunnel dying immediately is expected: the dial published the key, and
	// the terminal below is served over a pipe rather than over that tunnel.
	_, _ = d.connectAndServe(ctx)

	var advertised []byte
	select {
	case h := <-headers:
		value := h.Get(relay.PublicKeyHeader)
		var err error
		advertised, err = base64.StdEncoding.DecodeString(value)
		if err != nil {
			t.Fatalf("the advertised %s = %q is not padded standard base64: %v — the relay decodes it with that alphabet, so it would store no key at all",
				relay.PublicKeyHeader, value, err)
		}
		if len(advertised) != relay.SealKeySize {
			t.Fatalf("the advertised %s decoded to %d bytes, want %d", relay.PublicKeyHeader, len(advertised), relay.SealKeySize)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relay never received a dial")
	}

	agent, client := net.Pipe()
	defer client.Close()
	go d.serveStream(agent)
	t.Cleanup(func() { closeTerminals(d) })
	// One deadline over the whole exchange. A seal-side divergence shows up as a
	// failed open, but a peer that cannot open what it reads may also simply stop
	// talking, and this test must report that rather than hang.
	if err := client.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}

	h := fencedHeader(relay.StreamHeader{Kind: relay.KindTerminal, SessionID: "advertised-key", Cols: 80, Rows: 24})
	if err := relay.WriteHeader(client, h); err != nil {
		t.Fatal(err)
	}
	clientKey, err := relay.GenerateAgentKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.WriteClientHello(client, clientKey.PublicKey().Bytes()); err != nil {
		t.Fatal(err)
	}
	salt, err := relay.ReadServerHello(client)
	if err != nil {
		t.Fatal(err)
	}
	// The advertised bytes, deliberately -- not d.key, and not agentKey from a
	// helper. This line is the whole test: a browser has nothing but what the
	// relay was told, so deriving from anything else here would be asserting
	// against the agent's private state instead of against its wire contract.
	keys, err := relay.DeriveSessionKeys(clientKey, advertised, salt, h.SessionID)
	if err != nil {
		t.Fatalf("a browser could not derive against the advertised key: %v", err)
	}
	sealed, err := relay.NewSealedStream(client, client, keys.ClientToAgent, keys.AgentToClient)
	if err != nil {
		t.Fatal(err)
	}

	// attach queues a screen reset and an activity frame before anything is
	// typed, so the agent -> browser direction is testable without waiting on
	// the shell. Opening it is the assertion; a WriteFrame that returns nil is
	// not, because a write succeeds whether or not the peer holds the key.
	// The diagnosis is conditioned on the error rather than asserted over it. A
	// failed authentication IS a key divergence; an EOF or a timeout says only
	// that the stream ended, and handleTerminal's other early returns (a refused
	// fence, a policy refusal, a PTY that would not start) look identical from
	// out here.
	if _, err := sealed.ReadFrame(); err != nil {
		cause := "the stream ended without a sealing failure, so this may not be a key problem at all: " +
			"a refused fence, a policy refusal or a PTY that would not start look identical from here, " +
			"and the agent's log line says which"
		if errors.Is(err, relay.ErrSealFailed) {
			cause = "the agent sealed with a key other than the one it published, which fails every terminal " +
				"on every system immediately, with no error saying why"
		}
		t.Fatalf("a browser sealing to the advertised %s could not open the agent's first frame: %v — %s",
			relay.PublicKeyHeader, err, cause)
	}

	// And the other direction, end to end through the PTY: sealed input the
	// agent can actually open, echoed back as sealed output this side can open.
	const marker = "ormos-seal-both-ways"
	if err := sealed.WriteFrame(relay.EncodeData([]byte("echo " + marker + "\n"))); err != nil {
		t.Fatalf("writing a sealed keystroke: %v", err)
	}
	var seen []byte
	for !bytes.Contains(seen, []byte(marker)) {
		frame, err := sealed.ReadFrame()
		if err != nil {
			// Same discipline as above, without the ErrSealFailed branch: the
			// agent's send key cannot change mid-stream, so by this point a
			// sealing failure on THIS side has already fired the assertion above
			// and only a stream that ended can arrive here. Which is exactly why
			// the message names a suspect instead of convicting one -- the agent
			// stops reading records it cannot open, and it also stops reading
			// when the session ended for reasons that have nothing to do with a
			// key.
			t.Fatalf("a sealed keystroke never came back through the PTY (%v); opened %q so far — the agent "+
				"stops reading a stream whose records it cannot open, so a key other than the one it "+
				"advertised is the first suspect; a session that ended for another reason looks identical "+
				"from here, and the agent's log line distinguishes them",
				err, seen)
		}
		seen = append(seen, frame.Data...)
	}
}
