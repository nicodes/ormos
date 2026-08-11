//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
// promptly must keep working past it (a proxy pipe has no pacing of its own).
func TestServeStreamClearsHeaderDeadline(t *testing.T) {
	prev := headerReadTO
	headerReadTO = 150 * time.Millisecond
	t.Cleanup(func() { headerReadTO = prev })

	agent, client := net.Pipe()
	defer client.Close()
	d := &system{}
	go d.serveStream(agent) // KindPing echoes until closed
	if err := relay.WriteHeader(client, relay.StreamHeader{Kind: relay.KindPing}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * headerReadTO)
	msg := []byte("still alive")
	go func() { _, _ = client.Write(msg) }() // net.Pipe write blocks until read
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("ping echo after the header deadline: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo = %q, want %q", buf, msg)
	}
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

// proxyResponse runs one refused proxy stream and parses what the agent wrote
// back as an HTTP response. Parsing rather than string-matching is the point:
// a Content-Length that disagrees with the body, or a malformed status line,
// fails here instead of rendering as a blank iframe for the user.
func proxyResponse(t *testing.T, d *system, port int) (*http.Response, string) {
	t.Helper()
	agent, client := net.Pipe()
	defer client.Close()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	go func() {
		d.handleProxy(agent, agent, port)
		agent.Close()
	}()
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("the agent's refusal is not a readable HTTP response: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the refusal body: %v", err)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("Content-Length is %d but the body is %d bytes", resp.ContentLength, len(body))
	}
	return resp, string(body)
}

// Every refusal goes out through writeProxyError, so these pin the shape of
// the response the browser actually receives. Four hand-assembled copies of
// that format string is four places a future edit lands on three.
func TestProxyRefusalsAreWellFormedHTTP(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})

	// Refused by the built-in guard: a privileged port, no local opt-in.
	resp, body := proxyResponse(t, d, 22)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("policy refusal answered %s, want 403", resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type is %q", ct)
	}
	if !strings.Contains(body, "Local policy") {
		t.Errorf("the refusal does not say why: %q", body)
	}

	// Allowed, exposed, but nothing is listening: a 502 with an explanation,
	// because what the user sees is an iframe and a bare EOF renders blank.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := l.Addr().(*net.TCPAddr).Port
	l.Close()
	d.mu.Lock()
	d.ports = []PortStatus{{Port: dead}}
	d.mu.Unlock()

	resp, body = proxyResponse(t, d, dead)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("a dead port answered %s, want 502", resp.Status)
	}
	if !strings.Contains(body, "Nothing is listening") {
		t.Errorf("the 502 does not say why: %q", body)
	}
}
