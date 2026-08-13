//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicodes/ormos/relay"
)

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
	header := relay.StreamHeader{Kind: relay.KindTerminal, SessionID: "probe", Cwd: "$HOME", Cols: 80, Rows: 24}
	if _, err := d.terminal(header); err == nil || !strings.Contains(err.Error(), "$") {
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
	if err := relay.WriteHeader(client, relay.StreamHeader{Kind: relay.KindProxy, Port: port}); err != nil {
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
	if err := relay.WriteHeader(client, relay.StreamHeader{Kind: relay.KindProxy, Port: port}); err != nil {
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
	if err := relay.WriteHeader(client, relay.StreamHeader{Kind: relay.KindProxy, Port: port}); err != nil {
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
	if err := relay.WriteHeader(client, relay.StreamHeader{Kind: relay.KindProxy, Port: port}); err != nil {
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
	header := relay.StreamHeader{Kind: relay.KindTerminal, SessionID: "silent", Cols: 80, Rows: 24}
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

	h := relay.StreamHeader{Kind: relay.KindTerminal, SessionID: "idle", Cols: 80, Rows: 24}
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
	// Drain whatever the shell says, so the agent's writer never blocks.
	go func() {
		for {
			if _, err := sealed.ReadFrame(); err != nil {
				return
			}
		}
	}()

	// An idle terminal is not a broken one.
	time.Sleep(4 * terminalHandshakeTO)
	if err := sealed.WriteFrame(relay.EncodeData([]byte("echo still-here\n"))); err != nil {
		t.Fatalf("an idle terminal was dropped %s after its handshake: %v", 4*terminalHandshakeTO, err)
	}
	t.Cleanup(func() {
		for _, s := range d.terminals {
			s.close()
		}
	})
}

// The header name is a contract between two binaries that both fail silently
// when it drifts: the relay would simply never call SetSystemPubKey, browsers
// would keep sealing against a stale key, and terminals would stop opening with
// no error logged anywhere. relay.PublicKeyHeader is the single spelling; this
// holds the agent's own copy to it until that copy is deleted.
func TestPublicKeyHeaderMatchesTheProtocol(t *testing.T) {
	if publicKeyHeader != relay.PublicKeyHeader {
		t.Fatalf("the agent sends %q but the protocol says %q", publicKeyHeader, relay.PublicKeyHeader)
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
	go func() {
		d.handleProxy(agent, agent, port)
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
