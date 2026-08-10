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
	agentKeys, err := relay.DeriveSessionKeys(agentKey, clientKey.PublicKey().Bytes(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	clientKeys, err := relay.DeriveSessionKeys(clientKey, agentKey.PublicKey().Bytes(), sessionID)
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
	return &sealedConn{Conn: server, stream: agentStream}, clientStream
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

	if _, err := firstClient.ReadFrame(); err != nil {
		t.Fatalf("read initial replay: %v", err)
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
	if got := string(frame.Data); got != "survives disconnect" {
		t.Fatalf("reattached replay = %q", got)
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
