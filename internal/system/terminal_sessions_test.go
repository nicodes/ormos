package system

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/nicodes/ormos/relay"
	"golang.org/x/sys/unix"
)

// sealedPair builds both ends of one sealed connection over a net.Pipe, the way
// handleTerminal does after the client hello: the agent seals with
// AgentToClient, the browser opens with the same key.
func sealedPair(t *testing.T, sessionID string) (agent *sealedConn, client *relay.SealedStream) {
	t.Helper()
	agentKey, err := relay.GenerateAgentKey()
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := relay.GenerateAgentKey()
	if err != nil {
		t.Fatal(err)
	}
	salt, err := relay.GenerateServerSalt()
	if err != nil {
		t.Fatal(err)
	}
	agentKeys, err := relay.DeriveSessionKeys(agentKey, clientKey.PublicKey().Bytes(), salt, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	clientKeys, err := relay.DeriveSessionKeys(clientKey, agentKey.PublicKey().Bytes(), salt, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	server, browser := net.Pipe()
	agentStream, err := relay.NewSealedStream(server, server, agentKeys.AgentToClient, agentKeys.ClientToAgent)
	if err != nil {
		t.Fatal(err)
	}
	clientStream, err := relay.NewSealedStream(browser, browser, clientKeys.ClientToAgent, clientKeys.AgentToClient)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close(); browser.Close() })
	return newSealedConn(server, agentStream), clientStream
}

// waitConns blocks until the session holds exactly n connections.
func waitConns(t *testing.T, s *terminalSession, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		s.mu.Lock()
		got := len(s.conns)
		s.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session holds %d connections, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// A browser that stops draining must cost itself and nothing else. Before the
// per-connection writer this loop went through the session lock and a blocking
// socket write: the first output parked for terminalWriteTO with the PTY
// undrained, so the shell's own write() blocked and every other attached client
// stopped receiving too.
func TestTerminalOutputNeverBlocksOnAStalledClient(t *testing.T) {
	s := &terminalSession{id: "stall", done: make(chan struct{})}

	// net.Pipe is unbuffered and nobody reads the stalled client's end, so its
	// very first write parks until the deadline.
	stalled, _ := sealedPair(t, "stall")
	live, counter := countingConn(t)
	go s.attach(stalled)
	go s.attach(live)
	waitConns(t, s, 2)

	// Enough to overrun terminalSendBytes on the stalled connection — 8 KiB a
	// frame fills the 1 MiB budget in 128 — without making the draining one do
	// more sealing than a two-vCPU runner can finish promptly under -race.
	const chunks, chunkSize = 150, 8 << 10
	data := make([]byte, chunkSize)
	start := time.Now()
	for range chunks {
		s.output(data)
	}
	elapsed := time.Since(start)
	// One blocking write on the stalled connection would already have cost
	// terminalWriteTO; the whole workload has to finish well inside it.
	if elapsed > terminalWriteTO/2 {
		t.Fatalf("%d PTY chunks took %s with one stalled client; the reader is still waiting on a socket", chunks, elapsed)
	}

	// The stalled connection goes, but on its own write deadline rather than on
	// its backlog — so this waits out terminalWriteTO — and the draining one is
	// what is left.
	waitConns(t, s, 1)
	s.mu.Lock()
	_, stalledKept := s.conns[stalled]
	_, liveKept := s.conns[live]
	s.mu.Unlock()
	if stalledKept || !liveKept {
		t.Fatalf("survivors: stalled=%v draining=%v; the wrong connection was ended", stalledKept, liveKept)
	}

	// And the survivor is still being served: whatever it missed while the
	// other one stalled, it takes new output now.
	before, _, _ := counter.totals()
	s.output([]byte("still here"))
	deadline := time.Now().Add(30 * time.Second)
	for {
		if records, _, _ := counter.totals(); records > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the draining connection stopped receiving after another client stalled")
		}
		time.Sleep(2 * time.Millisecond)
	}

	s.mu.Lock()
	if s.expires != nil {
		s.expires.Stop()
	}
	s.mu.Unlock()
}

// A connection that falls behind must be resynchronised, not disconnected. The
// queue bound measures backlog, and a backlog means either a stuck socket or a
// writer goroutine that has not been scheduled — indistinguishable from here,
// and on a single-core host the second is ordinary. Dropping that kind costs a
// healthy session a fresh handshake and a full replay at the exact moment the
// link is busiest.
func TestTerminalSlowClientIsResyncedNotDropped(t *testing.T) {
	s := &terminalSession{id: "slow", done: make(chan struct{})}
	// History first, so the attach replay is NOT a bare reset and cannot be
	// mistaken for the catch-up frame this test is looking for.
	s.output([]byte("before the burst"))

	slow, client := sealedPair(t, "slow")
	go s.attach(slow)
	waitConns(t, s, 1)

	// A client that reads, but far slower than the PTY produces.
	seen := make(chan string, 512)
	go func() {
		for {
			frame, err := client.ReadFrame()
			if err != nil {
				return
			}
			if frame.Data != nil {
				select {
				case seen <- string(frame.Data):
				default:
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	// Comfortably more than terminalSendBytes, so the backlog must collapse.
	data := make([]byte, 8<<10)
	for i := range data {
		data[i] = 'x'
	}
	for range 300 {
		s.output(data)
	}

	// It is still attached — this is the whole point.
	s.mu.Lock()
	_, kept := s.conns[slow]
	s.mu.Unlock()
	if !kept {
		t.Fatal("a client that was merely slow was disconnected instead of resynchronised")
	}

	// And it was handed a catch-up frame: a screen reset followed by history,
	// so it redraws rather than resuming against a screen that moved on
	// without it.
	deadline := time.After(30 * time.Second)
	for {
		select {
		case frame := <-seen:
			// A catch-up carries the reset AND the burst; the attach replay
			// carries the reset and only what preceded it. Requiring a byte
			// from the burst is what tells the two apart.
			if strings.HasPrefix(frame, terminalResyncPrefix) && strings.Contains(frame, "xxxxxxxx") {
				s.mu.Lock()
				if s.expires != nil {
					s.expires.Stop()
				}
				s.mu.Unlock()
				return
			}
		case <-deadline:
			t.Fatal("a client that fell a whole budget behind never received a catch-up frame")
		}
	}
}

func TestTerminalSessionReattachesWithReplay(t *testing.T) {
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inputRead.Close()
	defer inputWrite.Close()
	s := &terminalSession{id: "test", ptmx: inputWrite, done: make(chan struct{})}
	firstAgent, firstClient := sealedPair(t, "test")
	go s.attach(firstAgent)

	if frame, err := firstClient.ReadFrame(); err != nil {
		t.Fatalf("read initial replay: %v", err)
	} else if got := string(frame.Data); got != terminalResyncPrefix {
		t.Fatalf("initial replay = %q, want a bare screen reset", got)
	}
	if _, err := firstClient.ReadFrame(); err != nil {
		t.Fatalf("read initial activity: %v", err)
	}

	go s.output([]byte("survives disconnect"))
	frame, err := firstClient.ReadFrame()
	if err != nil {
		t.Fatalf("read attached output: %v", err)
	}
	if got := string(frame.Data); got != "survives disconnect" {
		t.Fatalf("attached output = %q", got)
	}
	_ = firstAgent.Close()

	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		detached := len(s.conns) == 0
		s.mu.Unlock()
		if detached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session did not detach")
		}
		time.Sleep(time.Millisecond)
	}

	// A reattach negotiates a fresh key, so the second connection shares no
	// crypto state with the first — which is the property that lets the replay
	// buffer be re-sent safely.
	secondAgent, secondClient := sealedPair(t, "test")
	go s.attach(secondAgent)
	frame, err = secondClient.ReadFrame()
	if err != nil {
		t.Fatalf("read replay after reattach: %v", err)
	}
	// The replay is preceded by a screen reset so the browser redraws rather
	// than appending it to whatever it still had on screen.
	if got, want := string(frame.Data), terminalResyncPrefix+"survives disconnect"; got != want {
		t.Fatalf("reattached replay = %q, want %q", got, want)
	}
	if _, err := secondClient.ReadFrame(); err != nil {
		t.Fatalf("read reattached activity: %v", err)
	}

	input := []byte("input after reattach")
	if err := secondClient.WriteFrame(relay.EncodeData(input)); err != nil {
		t.Fatalf("write reattached input: %v", err)
	}
	read := make(chan []byte, 1)
	go func() {
		got := make([]byte, len(input))
		if _, err := io.ReadFull(inputRead, got); err != nil {
			read <- nil
			return
		}
		read <- got
	}()
	select {
	case got := <-read:
		if string(got) != string(input) {
			t.Fatalf("reattached input = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("reattached input did not reach PTY")
	}

	s.mu.Lock()
	if s.closed {
		t.Fatal("transport disconnect closed terminal session")
	}
	if s.expires != nil {
		s.expires.Stop()
	}
	s.mu.Unlock()
}

func TestTerminalReplayIsBounded(t *testing.T) {
	s := &terminalSession{done: make(chan struct{})}
	data := make([]byte, terminalReplayBytes+100)
	for i := range data {
		data[i] = byte(i)
	}
	s.output(data)
	if len(s.buffer) != terminalReplayBytes {
		t.Fatalf("replay length = %d, want %d", len(s.buffer), terminalReplayBytes)
	}
	if s.buffer[0] != data[100] {
		t.Fatal("replay did not retain the newest output")
	}
}

func TestTerminalSessionBroadcastsToMultipleClients(t *testing.T) {
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inputRead.Close()
	defer inputWrite.Close()
	s := &terminalSession{id: "shared", ptmx: inputWrite, done: make(chan struct{})}

	// Two connections to one session, each with its own key and counter — the
	// broadcast has to seal separately for each rather than reuse one record.
	clients := make([]*relay.SealedStream, 0, 2)
	for range 2 {
		agent, client := sealedPair(t, "shared")
		clients = append(clients, client)
		go s.attach(agent)
		if _, err := client.ReadFrame(); err != nil {
			t.Fatalf("read initial replay: %v", err)
		}
		if _, err := client.ReadFrame(); err != nil {
			t.Fatalf("read initial activity: %v", err)
		}
	}

	results := make(chan string, len(clients))
	for _, client := range clients {
		go func() {
			frame, err := client.ReadFrame()
			if err != nil {
				results <- ""
				return
			}
			results <- string(frame.Data)
		}()
	}
	s.output([]byte("shared output"))
	for range clients {
		if got := <-results; got != "shared output" {
			t.Fatalf("broadcast output = %q", got)
		}
	}
}

// A session's id can be asked for with any cwd on the header: the requested
// directory passes the policy check, then the lookup returns a shell rooted
// somewhere the policy would never have allowed. Reattach has to re-decide
// against the directory the session actually holds.
func TestTerminalReattachRechecksSessionCwd(t *testing.T) {
	dir := withTempConfigDir(t)
	allowed := filepath.Join(dir, "allowed")
	outside := t.TempDir()
	if err := os.Mkdir(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	policyJSON := fmt.Sprintf(`{"allowedRoots": [%q]}`, allowed)
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), []byte(policyJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	d := &system{terminals: make(map[string]*terminalSession), audit: newAuditor()}
	d.terminals["sess"] = &terminalSession{id: "sess", cwd: outside, done: make(chan struct{})}

	// The request names an allowed directory; the pre-existing session is
	// rooted outside it. Before the fix this returned the session.
	header := relay.StreamHeader{Kind: relay.KindTerminal, SessionID: "sess", Cwd: allowed, Cols: 80, Rows: 24}
	if _, err := d.terminal(header); err == nil {
		t.Fatal("reattach to a session rooted outside allowedRoots must be refused")
	} else if !strings.Contains(err.Error(), "does not allow terminals") {
		t.Fatalf("reattach error = %v, want a policy refusal", err)
	}
	if s := d.terminals["sess"]; s == nil {
		t.Fatal("a refused reattach must not drop the session")
	}

	// Sanity: a session rooted under the allowed root reattaches fine.
	d.terminals["sess"].cwd = allowed
	s, err := d.terminal(header)
	if err != nil {
		t.Fatalf("reattach under allowedRoots refused: %v", err)
	}
	if s != d.terminals["sess"] {
		t.Fatal("reattach did not return the pre-existing session")
	}
}

// processState reads a pid's state from /proc: "" if the process is gone,
// "Z" if it is a zombie (dead, awaiting a reap that may never come under a
// non-reaping init), anything else if it is alive.
func processState(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// comm is parenthesised and may contain spaces; the state follows the
	// final ')'.
	return string(data[strings.LastIndexByte(string(data), ')')+2])
}

func processGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		switch state := processState(pid); state {
		case "", "Z":
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still alive (%s) after terminal close", pid, processState(pid))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startGroupSession launches a shell on a real PTY running script, which must
// write the background child's pid to pidfile. It returns the session and the
// child's pid.
func startGroupSession(t *testing.T, script string) (*terminalSession, int) {
	t.Helper()
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "child.pid")
	cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf(script, pidfile))
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	pgid, err := unix.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	d := &system{terminals: make(map[string]*terminalSession)}
	s := &terminalSession{id: "grouptest", owner: d, ptmx: ptmx, cmd: cmd, pgid: pgid, done: make(chan struct{})}
	d.terminals[s.id] = s

	var child int
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(pidfile); err == nil {
			if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &child); err == nil && child > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("child pid file never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The background child of a non-interactive shell stays in the shell's
	// process group — that is the property close relies on.
	if childPgid, err := unix.Getpgid(child); err != nil || childPgid != pgid {
		t.Fatalf("child pgid = %d (%v), want the shell's group %d", childPgid, err, pgid)
	}
	return s, child
}

// Killing only the shell leaves its children running; enforcePolicy then
// "ends" a terminal whose processes are still alive.
func TestTerminalCloseKillsProcessGroup(t *testing.T) {
	s, child := startGroupSession(t, "sleep 600 & echo $! > %s; wait")
	s.close()
	processGone(t, s.cmd.Process.Pid)
	processGone(t, child)
}

// A child that ignores SIGHUP must still die: after the grace period the
// group gets SIGKILL.
func TestTerminalCloseEscalatesToSIGKILL(t *testing.T) {
	prev := terminalKillGrace
	terminalKillGrace = 200 * time.Millisecond
	t.Cleanup(func() { terminalKillGrace = prev })

	s, child := startGroupSession(t, "sh -c 'trap \"\" HUP; exec sleep 600' & echo $! > %s; wait")
	start := time.Now()
	s.close()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("close took %s; the grace period is not bounding it", elapsed)
	}
	processGone(t, child)
}

// kill has to wake a reader parked on the transport this actually runs over.
// net.Pipe, which every other test here uses, unblocks a pending Read on Close;
// a yamux stream does not — Close on an established stream only moves it to
// streamLocalClose, which prohibits further local writes while reads carry on.
// Without the read deadline in kill, a killed connection's attach goroutine,
// its relay.MaxTunnelStreams slot and everything still queued on it survive
// until the peer sends a FIN or StreamCloseTimeout fires two minutes later.
func TestSealedConnKillWakesAReaderOnAYamuxStream(t *testing.T) {
	clientTransport, serverTransport := net.Pipe()
	t.Cleanup(func() { clientTransport.Close(); serverTransport.Close() })
	client, err := relay.ClientSession(clientTransport)
	if err != nil {
		t.Fatal(err)
	}
	server, err := relay.ServerSession(serverTransport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })

	opened := make(chan error, 1)
	go func() {
		_, err := client.Open()
		opened <- err
	}()
	stream, err := server.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-opened; err != nil {
		t.Fatal(err)
	}

	key := make([]byte, relay.SealKeySize)
	sealed, err := relay.NewSealedStream(stream, stream, key, key)
	if err != nil {
		t.Fatal(err)
	}
	conn := newSealedConn(stream, sealed)

	parked := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := stream.Read(buf)
		parked <- err
	}()
	// Let the read reach the point of parking; nothing is ever sent on it.
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-parked:
		t.Fatalf("the read returned before kill (%v); this test proves nothing", err)
	default:
	}

	conn.kill()
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("kill did not wake a reader parked on a yamux stream; the connection would hold its slot until StreamCloseTimeout")
	}
}
