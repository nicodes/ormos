//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/nicodes/ormos/relay"
	"golang.org/x/sys/unix"
)

func runningTerminalInfo(id string) relay.TerminalSessionInfo {
	return relay.TerminalSessionInfo{ID: id, SessionID: id, State: relay.TerminalStateRunning, Generation: 1}
}

func TestLifecycleTerminalValidationControlsPTYCreation(t *testing.T) {
	oldClient := httpClient
	t.Cleanup(func() { httpClient = oldClient })
	for _, tc := range []struct {
		name, state string
		generation  int
		wantCreate  bool
	}{
		{"missing record", "running", 1, false},
		{"exited", relay.TerminalStateExited, 1, false},
		{"stale generation", relay.TerminalStateRunning, 2, false},
		{"valid running sibling", relay.TerminalStateRunning, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTempConfigDir(t)
			d := newSystem(systemConfig{RelayURL: "ws://relay.test", Shell: "/bin/cat"})
			httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				row := `{"id":"record","session_id":"session","state":"` + tc.state + `","generation":` + fmt.Sprint(tc.generation) + `}`
				if tc.name == "missing record" {
					row = `{"id":"other","session_id":"session","state":"running","generation":1}`
				}
				return testHTTPResponse(http.StatusOK, `{"sessions":[`+row+`]}`), nil
			})}
			h := fencedHeader(relay.StreamHeader{Kind: relay.KindTerminal, Cwd: t.TempDir(), SessionID: "session", TerminalRecordID: "record", TerminalGeneration: 1, Cols: 80, Rows: 24})
			s, err := d.terminal(h, acceptedFenceDeadline(t, h))
			if (err == nil) != tc.wantCreate {
				t.Fatalf("err=%v wantCreate=%v", err, tc.wantCreate)
			}
			if tc.wantCreate {
				if s == nil {
					t.Fatal("valid lifecycle row did not create PTY")
				}
				s.close()
			} else if len(d.terminals) != 0 {
				t.Fatalf("refused request created %d PTYs", len(d.terminals))
			}
		})
	}
}

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
	got := s.replay.snapshot()
	if len(got) != terminalReplayBytes {
		t.Fatalf("replay length = %d, want %d", len(got), terminalReplayBytes)
	}
	if !bytes.Equal(got, data[100:]) {
		t.Fatal("replay did not retain the newest output")
	}
}

// The ring has to hand back exactly the last cap bytes in order, whatever
// mixture of chunk sizes produced them and however many times it has wrapped.
func TestReplayRingKeepsTheNewestBytesInOrder(t *testing.T) {
	const capacity = 64
	var written []byte
	r := &replayRing{buf: make([]byte, capacity)}
	// Chunk sizes that divide the capacity unevenly, so writes straddle the
	// wrap point rather than landing on it.
	for i, n := range []int{7, 13, 5, 40, 3, 64, 1, 100, 29, 31} {
		chunk := make([]byte, n)
		for j := range chunk {
			chunk[j] = byte(i*251 + j)
		}
		r.append(chunk)
		written = append(written, chunk...)

		want := written
		if len(want) > capacity {
			want = want[len(want)-capacity:]
		}
		if got := r.snapshot(); !bytes.Equal(got, want) {
			t.Fatalf("after appending %d bytes: snapshot = %v, want %v", n, got, want)
		}
		// The prefixed form is what the hot path actually calls, and it walks
		// the ring itself rather than deferring to snapshot.
		if got, wantPrefixed := r.snapshotAfter("RST"), append([]byte("RST"), want...); !bytes.Equal(got, wantPrefixed) {
			t.Fatalf("after appending %d bytes: snapshotAfter = %v, want %v", n, got, wantPrefixed)
		}
	}
}

// The point of the ring: steady-state append costs the chunk, not the bound.
// The old append-and-compact shifted the whole buffer on every PTY read once it
// was full, so these two capacities used to differ by their ratio.
//
// The chunk size is coprime with both capacities on purpose, so every append
// straddles the wrap point — which is what production chunks, arriving at
// arbitrary sizes up to terminalCoalesceMax, actually do.
func BenchmarkReplayRingAppend(b *testing.B) {
	chunk := make([]byte, 4093)
	for _, capacity := range []int{terminalReplayBytes, 4 * terminalReplayBytes} {
		b.Run(fmt.Sprintf("%dKiB", capacity>>10), func(b *testing.B) {
			r := &replayRing{buf: make([]byte, capacity)}
			// Fill it first: the steady state being measured is the one where
			// every append wraps and overwrites.
			for filled := 0; filled < capacity; filled += len(chunk) {
				r.append(chunk)
			}
			b.SetBytes(int64(len(chunk)))
			for b.Loop() {
				r.append(chunk)
			}
		})
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

// Local attachments deliberately use the same session fan-out as their sealed
// browser siblings. This is the control comparison: both receive one output
// from one session, rather than a local attach starting a second shell.
func TestLocalTerminalClientSharesBrowserSession(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{Shell: "/bin/cat"})
	root := t.TempDir()
	local, err := d.AttachTerminal(root, runningTerminalInfo("local-shared"), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.session.close() })
	if got := string(<-local.Output()); got != terminalResyncPrefix {
		t.Fatalf("local initial replay = %q, want reset", got)
	}

	browser, browserClient := sealedPair(t, "local-shared")
	go local.session.attach(browser)
	if _, err := browserClient.ReadFrame(); err != nil {
		t.Fatalf("read browser initial replay: %v", err)
	}
	if _, err := browserClient.ReadFrame(); err != nil {
		t.Fatalf("read browser initial activity: %v", err)
	}

	local.session.output([]byte("shared with both"))
	if got := string(<-local.Output()); got != "shared with both" {
		t.Fatalf("local output = %q", got)
	}
	frame, err := browserClient.ReadFrame()
	if err != nil {
		t.Fatalf("read browser output: %v", err)
	}
	if got := string(frame.Data); got != "shared with both" {
		t.Fatalf("browser output = %q", got)
	}

	if err := local.Write([]byte("local input\n")); err != nil {
		t.Fatalf("local input: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case output := <-local.Output():
			if strings.Contains(string(output), "local input") {
				goto resized
			}
		case <-deadline:
			t.Fatal("local input did not reach the shared PTY")
		}
	}

resized:
	if err := browserClient.WriteFrame(relay.EncodeData([]byte("browser input\n"))); err != nil {
		t.Fatalf("browser input: %v", err)
	}
	deadline = time.After(5 * time.Second)
	for {
		select {
		case output := <-local.Output():
			if strings.Contains(string(output), "browser input") {
				goto resizedByBrowser
			}
		case <-deadline:
			t.Fatal("browser input did not reach the same PTY as local input")
		}
	}

resizedByBrowser:
	if err := local.Resize(100, 40); err != nil {
		t.Fatalf("local resize: %v", err)
	}
	rows, cols, err := pty.Getsize(local.session.ptmx)
	if err != nil {
		t.Fatal(err)
	}
	if cols != 100 || rows != 40 {
		t.Fatalf("PTY size = %dx%d, want 100x40", cols, rows)
	}

	local.Detach()
	waitConns(t, local.session, 1) // browser remains; detach did not kill the shell.
	if d.terminals["local-shared"] != local.session {
		t.Fatal("local detach removed a shell still attached in the browser")
	}
}

func TestLocalTerminalCreationUsesTerminalSessionLimit(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{Shell: "/bin/cat"})
	root := t.TempDir()
	clients := make([]*TerminalClient, 0, maxTerminalSessions)
	for i := range maxTerminalSessions {
		id := fmt.Sprintf("local-session-%d", i)
		client, err := d.AttachTerminal(root, runningTerminalInfo(id), 80, 24)
		if err != nil {
			t.Fatalf("local session %d: %v", i, err)
		}
		<-client.Output()
		clients = append(clients, client)
	}
	t.Cleanup(func() {
		for _, client := range clients {
			client.session.close()
		}
	})
	if _, err := d.AttachTerminal(root, runningTerminalInfo("local-session-over-limit"), 80, 24); err == nil || !strings.Contains(err.Error(), "session limit") {
		t.Fatalf("local session limit error = %v", err)
	}

	// Browser headers enter the same terminal authority. Calling it directly is
	// the control for the already-sealed browser path: sealing happens before
	// terminal(), but neither can bypass its shared session limit.
	header := fencedHeader(relay.StreamHeader{Kind: relay.KindTerminal, Cwd: root, SessionID: "browser-session-over-limit", Cols: 80, Rows: 24})
	if _, err := d.terminal(header, acceptedFenceDeadline(t, header)); err == nil || !strings.Contains(err.Error(), "session limit") {
		t.Fatalf("browser terminal authority limit error = %v", err)
	}
}

func TestLocalTerminalClientReplayDetachAndConnectionLimit(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{Shell: "/bin/cat"})
	root := t.TempDir()
	first, err := d.AttachTerminal(root, runningTerminalInfo("local-limit"), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.session.close() })
	<-first.Output() // initial reset
	first.session.output([]byte("replayed"))
	if got := string(<-first.Output()); got != "replayed" {
		t.Fatalf("first output = %q", got)
	}
	first.Detach()
	waitConns(t, first.session, 0)
	first.session.mu.Lock()
	armed := first.session.expires != nil
	first.session.mu.Unlock()
	if !armed {
		t.Fatal("last local detach did not arm terminal TTL")
	}

	reattached, err := d.AttachTerminal(root, runningTerminalInfo("local-limit"), 100, 40)
	if err != nil {
		t.Fatal(err)
	}
	if reattached.session != first.session {
		t.Fatal("local reattach created a duplicate shell")
	}
	rows, cols, err := pty.Getsize(reattached.session.ptmx)
	if err != nil {
		t.Fatal(err)
	}
	if cols != 100 || rows != 40 {
		t.Fatalf("reattached PTY size = %dx%d, want 100x40", cols, rows)
	}
	if got, want := string(<-reattached.Output()), terminalResyncPrefix+"replayed"; got != want {
		t.Fatalf("local replay = %q, want %q", got, want)
	}
	clients := []*TerminalClient{reattached}
	for len(clients) < maxTerminalConns {
		client, err := d.AttachTerminal(root, runningTerminalInfo("local-limit"), 80, 24)
		if err != nil {
			t.Fatal(err)
		}
		<-client.Output()
		clients = append(clients, client)
	}
	if _, err := d.AttachTerminal(root, runningTerminalInfo("local-limit"), 80, 24); err == nil || !strings.Contains(err.Error(), "connections") {
		t.Fatalf("connection over limit error = %v, want connection-limit refusal", err)
	}
	for _, client := range clients {
		client.Detach()
	}
}

// A local TUI must never be held hostage by a PTY whose slave has stopped
// reading. The browser sibling remains the control: once the local attachment
// is gone, a browser can still write to this one PTY.
func TestLocalTerminalDetachReleasesInputAndDoesNotStarveBrowser(t *testing.T) {
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inputRead.Close(); _ = inputWrite.Close() })
	fd := int(inputWrite.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	filled := 0
	fill := make([]byte, terminalInputQuantum)
	for {
		n, err := unix.Write(fd, fill)
		filled += n
		if err == unix.EAGAIN {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	s := &terminalSession{id: "local-input", ptmx: inputWrite, conns: make(map[terminalConn]struct{}), input: make(chan *terminalInput, terminalInputQueue), done: make(chan struct{})}
	s.startInput()
	localConn := newLocalTerminalConn()
	if err := s.attachConn(localConn); err != nil {
		t.Fatal(err)
	}
	local := &TerminalClient{session: s, conn: localConn}

	writeDone := make(chan error, 1)
	go func() { writeDone <- local.Write(make([]byte, terminalSendBytes)) }()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("local Write returned %v before detach", err)
		}
	case <-time.After(time.Second):
		t.Fatal("local Write blocked while PTY slave was not reading")
	}

	local.Detach()
	select {
	case <-local.Done():
	case <-time.After(time.Second):
		t.Fatal("local detach did not close Done")
	}
	waitInputAccounting(t, s, localConn, 0, 0, time.Second)
	readFD := int(inputRead.Fd())
	if err := unix.SetNonblock(readFD, true); err != nil {
		t.Fatal(err)
	}
	for {
		_, err := unix.Read(readFD, make([]byte, filled))
		if err == unix.EAGAIN {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	browser, browserClient := sealedPair(t, "local-input")
	go s.attach(browser)
	if _, err := browserClient.ReadFrame(); err != nil {
		t.Fatalf("read browser replay: %v", err)
	}
	if _, err := browserClient.ReadFrame(); err != nil {
		t.Fatalf("read browser activity: %v", err)
	}
	if err := browserClient.WriteFrame(relay.EncodeData([]byte("browser input"))); err != nil {
		t.Fatalf("write browser input: %v", err)
	}
	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, len("browser input"))
		_, err := io.ReadFull(inputRead, buf)
		if err != nil {
			got <- nil
			return
		}
		got <- buf
	}()
	select {
	case data := <-got:
		if string(data) != "browser input" {
			t.Fatalf("browser input after local detach = %q", data)
		}
	case <-time.After(time.Second):
		t.Fatal("browser input was starved after local detach")
	}
	browser.kill()
}

func TestLocalTerminalWriteBoundsInputByBytes(t *testing.T) {
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inputRead.Close(); _ = inputWrite.Close() })
	s := &terminalSession{id: "local-input-bound", ptmx: inputWrite, conns: make(map[terminalConn]struct{}), input: make(chan *terminalInput, terminalInputQueue), done: make(chan struct{})}
	s.startInput()
	conn := newLocalTerminalConn()
	if err := s.attachConn(conn); err != nil {
		t.Fatal(err)
	}
	client := &TerminalClient{session: s, conn: conn}
	if err := client.Write(make([]byte, terminalInputBytes)); err != nil {
		t.Fatalf("fill bounded local input queue: %v", err)
	}
	if err := client.Write([]byte("x")); !errors.Is(err, ErrTerminalInputBackpressure) {
		t.Fatalf("input beyond byte budget error = %v, want ErrTerminalInputBackpressure", err)
	}
	client.Detach()
	waitInputAccounting(t, s, conn, 0, 0, time.Second)
}

func TestLocalTerminalWriteReservesCallsAndBytesBeforeCopy(t *testing.T) {
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inputRead.Close(); _ = inputWrite.Close() })
	fd := int(inputWrite.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	for {
		_, err := unix.Write(fd, make([]byte, 4<<10))
		if err == unix.EAGAIN {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	s := &terminalSession{ptmx: inputWrite, input: make(chan *terminalInput, terminalInputQueue), done: make(chan struct{})}
	s.startInput()
	conn := newLocalTerminalConn()
	client := &TerminalClient{session: s, conn: conn}
	var copied atomic.Int64
	conn.copyInput = func(p []byte) []byte {
		copied.Add(1)
		return append([]byte(nil), p...)
	}
	if err := client.Write([]byte("x")); err != nil {
		t.Fatalf("initial in-flight input: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for len(s.input) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("input worker did not take the in-flight call")
		}
		time.Sleep(time.Millisecond)
	}
	var accepted atomic.Int64
	accepted.Add(1) // the worker holds this first call in the exact bound.
	var group sync.WaitGroup
	for range 2 * terminalInputQueue {
		group.Add(1)
		go func() {
			defer group.Done()
			if client.Write([]byte("x")) == nil {
				accepted.Add(1)
			}
		}()
	}
	group.Wait()
	if got := accepted.Load(); got != terminalInputQueue {
		t.Fatalf("accepted local input calls = %d, want exact bound %d", got, terminalInputQueue)
	}
	if got := copied.Load(); got != accepted.Load() {
		t.Fatalf("implementation copies = %d, accepted calls = %d; rejected calls allocated", got, accepted.Load())
	}
	if got := conn.inputCalls.Load(); got != terminalInputQueue {
		t.Fatalf("reserved local input calls = %d, want %d", got, terminalInputQueue)
	}
	if got := conn.inputBytes.Load(); got != terminalInputQueue {
		t.Fatalf("reserved local input bytes = %d, want %d", got, terminalInputQueue)
	}
	conn.kill()
	waitInputAccounting(t, s, conn, 0, 0, time.Second)
}

func TestLocalTerminalWriteReservesAggregateBeforeCopyAcrossClients(t *testing.T) {
	_, inputWrite := fullNonblockingPipe(t)
	s := &terminalSession{ptmx: inputWrite, input: make(chan *terminalInput, terminalSchedulerQueue), done: make(chan struct{})}
	firstConn := newLocalTerminalConn()
	secondConn := newLocalTerminalConn()
	if err := s.attachConn(firstConn); err != nil {
		t.Fatal(err)
	}
	if err := s.attachConn(secondConn); err != nil {
		t.Fatal(err)
	}
	first := &TerminalClient{session: s, conn: firstConn}
	second := &TerminalClient{session: s, conn: secondConn}
	entered := make(chan struct{})
	release := make(chan struct{})
	firstConn.beforeInputCopy = func() {
		close(entered)
		<-release
	}
	var secondCopies atomic.Int64
	secondConn.copyInput = func(p []byte) []byte {
		secondCopies.Add(1)
		return append([]byte(nil), p...)
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Write(make([]byte, terminalInputBytes)) }()
	<-entered
	s.inputMu.Lock()
	if s.inputCalls != 1 || s.inputBytes != terminalInputBytes || s.localInputCalls != 1 || s.localInputBytes != terminalInputBytes {
		t.Fatalf("aggregate reservation before copy total=%d/%d local=%d/%d", s.inputCalls, s.inputBytes, s.localInputCalls, s.localInputBytes)
	}
	s.inputMu.Unlock()
	if err := second.Write([]byte("overflow")); !errors.Is(err, ErrTerminalInputBackpressure) {
		t.Fatalf("second client aggregate overflow = %v, want ErrTerminalInputBackpressure", err)
	}
	if got := secondCopies.Load(); got != 0 {
		t.Fatalf("aggregate-rejected second client made %d copies", got)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first reserved Write: %v", err)
	}
	s.close()
	select {
	case <-s.inputStopped:
	case <-time.After(time.Second):
		t.Fatal("close did not drain aggregate reservation")
	}
	waitInputAccounting(t, s, firstConn, 0, 0, time.Second)
	if secondConn.inputCalls.Load() != 0 || secondConn.inputBytes.Load() != 0 {
		t.Fatalf("rejected second ledger = %d/%d, want 0/0", secondConn.inputCalls.Load(), secondConn.inputBytes.Load())
	}
	firstConn.kill()
	secondConn.kill()
}

func TestReservedLocalPublicationRollsBackWhenSessionCloses(t *testing.T) {
	_, inputWrite := fullNonblockingPipe(t)
	s := &terminalSession{ptmx: inputWrite, input: make(chan *terminalInput, terminalSchedulerQueue), done: make(chan struct{})}
	local := newLocalTerminalConn()
	if err := s.attachConn(local); err != nil {
		t.Fatal(err)
	}
	client := &TerminalClient{session: s, conn: local}
	reserved := make(chan struct{})
	release := make(chan struct{})
	local.beforeInputCopy = func() {
		close(reserved)
		<-release
	}
	writeResult := make(chan error, 1)
	go func() { writeResult <- client.Write(make([]byte, terminalInputBytes)) }()
	<-reserved

	browserBlock := make([]byte, terminalInputQuantum)
	for i := range terminalBrowserInputQueue {
		if !s.submitInput(&terminalInput{p: browserBlock}) {
			t.Fatalf("fill browser reserve at call %d", i+1)
		}
	}
	s.inputMu.Lock()
	got := [6]int{s.inputCalls, s.inputBytes, s.localInputCalls, s.localInputBytes, s.browserInputCalls, s.browserInputBytes}
	s.inputMu.Unlock()
	want := [6]int{1 + terminalBrowserInputQueue, terminalSchedulerBytes, 1, terminalInputBytes, terminalBrowserInputQueue, terminalBrowserInputBytes}
	if got != want {
		t.Fatalf("reserved pre-publication ledgers = %v, want %v", got, want)
	}

	browserResult := make(chan bool, 1)
	browserDead := make(chan struct{})
	go func() { browserResult <- s.submitBrowserInput([]byte("waiting-browser"), browserDead) }()
	waitInputCapacityWaiter(t, s)
	closeResult := make(chan struct{})
	go func() {
		s.close()
		close(closeResult)
	}()
	select {
	case ok := <-browserResult:
		if ok {
			t.Fatal("browser waiter was admitted during close")
		}
	case <-time.After(time.Second):
		t.Fatal("session close did not wake browser capacity waiter")
	}
	select {
	case <-closeResult:
		t.Fatal("session close passed a reserved Write holding local inputMu")
	default:
	}

	close(release)
	if err := <-writeResult; !errors.Is(err, ErrTerminalInputBackpressure) {
		t.Fatalf("reserved publication after close = %v, want ErrTerminalInputBackpressure", err)
	}
	select {
	case <-closeResult:
	case <-time.After(time.Second):
		t.Fatal("session close deadlocked after reserved Write released local inputMu")
	}
	select {
	case <-s.inputStopped:
	case <-time.After(time.Second):
		t.Fatal("input worker did not stop after reserved publication rollback")
	}
	s.inputMu.Lock()
	final := [6]int{s.inputCalls, s.inputBytes, s.localInputCalls, s.localInputBytes, s.browserInputCalls, s.browserInputBytes}
	s.inputMu.Unlock()
	if final != [6]int{} {
		t.Fatalf("session ledgers after close = %v, want all zero", final)
	}
	if calls, inputBytes := local.inputCalls.Load(), local.inputBytes.Load(); calls != 0 || inputBytes != 0 {
		t.Fatalf("local ledgers after close = %d/%d, want 0/0", calls, inputBytes)
	}
}

func TestReservedLocalInputOwnsFutureSchedulerChannelSlot(t *testing.T) {
	_, inputWrite := fullNonblockingPipe(t)
	s := &terminalSession{ptmx: inputWrite, done: make(chan struct{})}
	primary := newLocalTerminalConn()
	overflow := newLocalTerminalConn()
	if err := s.attachConn(primary); err != nil {
		t.Fatal(err)
	}
	if err := s.attachConn(overflow); err != nil {
		t.Fatal(err)
	}
	primaryClient := &TerminalClient{session: s, conn: primary}
	overflowClient := &TerminalClient{session: s, conn: overflow}
	block := make([]byte, terminalInputQuantum)
	for i := range terminalInputQueue - 1 {
		if err := primaryClient.Write(block); err != nil {
			t.Fatalf("publish local call %d of %d: %v", i+1, terminalInputQueue-1, err)
		}
	}
	for i := range terminalBrowserInputQueue {
		if !s.submitInput(&terminalInput{p: block}) {
			t.Fatalf("publish browser call %d of %d", i+1, terminalBrowserInputQueue)
		}
	}
	if got := cap(s.input); got != terminalSchedulerQueue {
		t.Fatalf("production scheduler channel capacity = %d, want %d future-slot invariant", got, terminalSchedulerQueue)
	}
	reserved := make(chan struct{})
	release := make(chan struct{})
	primary.beforeInputCopy = func() {
		close(reserved)
		<-release
	}
	lastResult := make(chan error, 1)
	go func() { lastResult <- primaryClient.Write(block) }()
	<-reserved
	s.inputMu.Lock()
	got := [5]int{s.inputCalls, s.localInputCalls, s.browserInputCalls, s.localInputBytes, s.browserInputBytes}
	s.inputMu.Unlock()
	want := [5]int{terminalSchedulerQueue, terminalInputQueue, terminalBrowserInputQueue, terminalInputBytes, terminalBrowserInputBytes}
	if got != want {
		t.Fatalf("320th reserved ledgers = %v, want %v", got, want)
	}
	var overflowCopies atomic.Int64
	overflow.copyInput = func(p []byte) []byte {
		overflowCopies.Add(1)
		return append([]byte(nil), p...)
	}
	if err := overflowClient.Write([]byte("321st")); !errors.Is(err, ErrTerminalInputBackpressure) {
		t.Fatalf("321st local Write = %v, want ErrTerminalInputBackpressure", err)
	}
	if copies := overflowCopies.Load(); copies != 0 {
		t.Fatalf("321st rejected Write made %d copies", copies)
	}
	close(release)
	if err := <-lastResult; err != nil {
		t.Fatalf("320th Write did not publish into its reserved slot: %v", err)
	}
	s.close()
	select {
	case <-s.inputStopped:
	case <-time.After(time.Second):
		t.Fatal("close did not drain exact 320-call scheduler bound")
	}
	s.inputMu.Lock()
	final := [6]int{s.inputCalls, s.inputBytes, s.localInputCalls, s.localInputBytes, s.browserInputCalls, s.browserInputBytes}
	s.inputMu.Unlock()
	if final != [6]int{} {
		t.Fatalf("session ledgers after exact-bound close = %v, want all zero", final)
	}
	if calls, inputBytes := primary.inputCalls.Load(), primary.inputBytes.Load(); calls != 0 || inputBytes != 0 {
		t.Fatalf("primary ledgers after close = %d/%d, want 0/0", calls, inputBytes)
	}
	if calls, inputBytes := overflow.inputCalls.Load(), overflow.inputBytes.Load(); calls != 0 || inputBytes != 0 {
		t.Fatalf("overflow ledgers after close = %d/%d, want 0/0", calls, inputBytes)
	}
}

func TestLocalTerminalWriteRejectsOversizedInputWithoutCopy(t *testing.T) {
	conn := newLocalTerminalConn()
	client := &TerminalClient{conn: conn}
	var copied atomic.Int64
	conn.copyInput = func(p []byte) []byte {
		copied.Add(1)
		return append([]byte(nil), p...)
	}
	err := client.Write(make([]byte, min(relay.MaxFrameSize, terminalInputBytes)+1))
	if !errors.Is(err, ErrTerminalInputTooLarge) {
		t.Fatalf("oversized local input error = %v, want ErrTerminalInputTooLarge", err)
	}
	if errors.Is(err, ErrTerminalInputBackpressure) {
		t.Fatal("oversized input matched retryable ErrTerminalInputBackpressure")
	}
	if got := copied.Load(); got != 0 {
		t.Fatalf("oversized input made %d implementation copies", got)
	}
	conn.kill()
}

func TestLocalTerminalWriteAdmissionIsOrderedWithDetach(t *testing.T) {
	_, inputWrite := fullNonblockingPipe(t)
	conn := newLocalTerminalConn()
	s := &terminalSession{ptmx: inputWrite, input: make(chan *terminalInput, terminalInputQueue), done: make(chan struct{})}
	s.startInput()
	client := &TerminalClient{session: s, conn: conn}
	entered := make(chan struct{})
	release := make(chan struct{})
	conn.beforeInputCopy = func() {
		close(entered)
		<-release
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- client.Write([]byte("accepted before detach")) }()
	<-entered // reservation and the inputMu ownership are now established.
	detached := make(chan struct{})
	go func() { conn.kill(); close(detached) }()
	select {
	case <-detached:
		t.Fatal("detach passed a Write already holding admission")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-writeDone; err != nil {
		t.Fatalf("Write admitted before detach = %v", err)
	}
	<-detached
	if err := client.Write([]byte("after detach")); !errors.Is(err, ErrTerminalClientClosed) {
		t.Fatalf("Write after detach = %v, want ErrTerminalClientClosed", err)
	}
}

func TestPTYMasterRemainsNonblockingAcrossTerminalIOCTLS(t *testing.T) {
	ptmx, tty := ptyPair(t)
	var err error
	ptmx, err = normalizePTY(ptmx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ptmx.Close() })
	assertPTYNonblocking(t, ptmx, "normalization")
	if err := setPTYSize(ptmx, 100, 40); err != nil {
		t.Fatal(err)
	}
	assertPTYNonblocking(t, ptmx, "resize ioctl")
	_, _ = ptyForegroundPgrp(ptmx)
	assertPTYNonblocking(t, ptmx, "activity ioctl")
	r, err := newPTYReader(ptmx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ptmx.SetReadDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatalf("normalized PTY does not support read deadlines: %v", err)
	}
	_, err = r.readOnce(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("idle RawConn.Read error = %v, want os.ErrDeadlineExceeded (not ErrNoDeadline)", err)
	}
	_ = ptmx.SetReadDeadline(time.Time{})
	_ = tty
}

func TestCreatedPTYRemainsNonblockingAcrossLocalAndBrowserResize(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{Shell: "/bin/cat"})
	local, err := d.AttachTerminal(t.TempDir(), runningTerminalInfo("nonblock-resize"), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.session.close() })
	<-local.Output()
	assertPTYNonblocking(t, local.session.ptmx, "terminal creation and initial local resize")
	if err := local.Resize(100, 40); err != nil {
		t.Fatal(err)
	}
	assertPTYNonblocking(t, local.session.ptmx, "local Resize")

	browser, browserClient := sealedPair(t, "nonblock-resize")
	go local.session.attach(browser)
	if _, err := browserClient.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if _, err := browserClient.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if err := browserClient.WriteFrame(relay.EncodeResize(120, 50)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		rows, cols, err := rawPTYSize(local.session.ptmx)
		if err != nil {
			t.Fatal(err)
		}
		if rows == 50 && cols == 120 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("browser resize left PTY at %dx%d", cols, rows)
		}
		time.Sleep(time.Millisecond)
	}
	assertPTYNonblocking(t, local.session.ptmx, "browser Resize")
	_, _ = ptyForegroundPgrp(local.session.ptmx)
	assertPTYNonblocking(t, local.session.ptmx, "activity ioctl")
	browser.kill()
}

func TestTerminalInputStartsExactlyOneWorkerAndCloseDrainsAccounting(t *testing.T) {
	for _, tc := range []struct {
		name           string
		currentBrowser bool
	}{{name: "current-local-queued-browser"}, {name: "current-browser-queued-local", currentBrowser: true}} {
		t.Run(tc.name, func(t *testing.T) {
			_, inputWrite := fullNonblockingPipe(t)
			s := &terminalSession{ptmx: inputWrite, input: make(chan *terminalInput, terminalInputQueue), done: make(chan struct{})}
			local := newLocalTerminalConn()
			client := &TerminalClient{session: s, conn: local}
			s.startInput()
			<-s.inputStarted
			for range 32 {
				s.startInput()
			}
			select {
			case <-s.inputStopped:
				t.Fatal("the sole input worker exited before close")
			default:
			}
			if tc.currentBrowser {
				if !s.submitInput(&terminalInput{p: []byte("browser-current")}) {
					t.Fatal("admit current browser input")
				}
				waitSessionInput(t, s, 1, 0, time.Second)
				if err := client.Write([]byte("local-queued")); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := client.Write([]byte("local-current")); err != nil {
					t.Fatal(err)
				}
				waitSessionInput(t, s, 1, 0, time.Second)
				if !s.submitInput(&terminalInput{p: []byte("browser-queued")}) {
					t.Fatal("admit queued browser input")
				}
			}
			// Keep a real backlog behind both kinds. This makes shutdown prove
			// draining rather than winning a select between done and one item.
			for i := range 32 {
				if !s.submitInput(&terminalInput{p: []byte{byte(i)}}) {
					t.Fatalf("admit queued browser request %d", i)
				}
			}
			waitSessionInput(t, s, 34, 33, time.Second)
			go s.close()
			select {
			case <-s.inputStopped:
			case <-time.After(time.Second):
				t.Fatal("session close did not stop the input worker")
			}
			waitInputAccounting(t, s, local, 0, 0, time.Second)
			s.close()
			waitInputAccounting(t, s, local, 0, 0, time.Second)
			local.kill()
		})
	}
}

func TestTerminalInputMaximalLocalLeavesBrowserReserve(t *testing.T) {
	_, inputWrite := fullNonblockingPipe(t)
	s := &terminalSession{ptmx: inputWrite, input: make(chan *terminalInput, terminalSchedulerQueue), done: make(chan struct{})}
	local := newLocalTerminalConn()
	client := &TerminalClient{session: s, conn: local}
	if err := client.Write(make([]byte, terminalInputBytes)); err != nil {
		t.Fatalf("admit exact maximal local input: %v", err)
	}
	waitSessionInput(t, s, 1, 0, time.Second)

	dead := make(chan struct{})
	admitted := make(chan bool, 1)
	go func() { admitted <- s.submitBrowserInput([]byte("browser-marker"), dead) }()
	select {
	case ok := <-admitted:
		if !ok {
			t.Fatal("browser marker rejected behind maximal local input")
		}
	case <-time.After(time.Second):
		t.Fatal("browser marker waited for maximal local input to complete")
	}
	waitSessionInput(t, s, 2, 1, time.Second)
	s.inputMu.Lock()
	if s.localInputCalls != 1 || s.localInputBytes != terminalInputBytes || s.browserInputCalls != 1 || s.browserInputBytes != len("browser-marker") {
		t.Fatalf("split accounting local=%d/%d browser=%d/%d", s.localInputCalls, s.localInputBytes, s.browserInputCalls, s.browserInputBytes)
	}
	s.inputMu.Unlock()

	local.kill()
	s.close()
	select {
	case <-s.inputStopped:
	case <-time.After(time.Second):
		t.Fatal("close did not drain maximal local plus browser reserve")
	}
	waitInputAccounting(t, s, local, 0, 0, time.Second)
}

func TestBrowserInputFullCapacityWaitsThenResumes(t *testing.T) {
	inputRead, inputWrite := fullNonblockingPipe(t)
	s := &terminalSession{ptmx: inputWrite, input: make(chan *terminalInput, terminalSchedulerQueue), done: make(chan struct{})}
	s.startInput()
	for i := range terminalBrowserInputQueue {
		if !s.submitInput(&terminalInput{p: bytes.Repeat([]byte{byte(i)}, terminalInputQuantum)}) {
			t.Fatalf("admit browser call %d of exact %d-call reserve", i+1, terminalBrowserInputQueue)
		}
	}
	waitSessionInput(t, s, terminalBrowserInputQueue, terminalBrowserInputQueue-1, time.Second)
	dead := make(chan struct{})
	admitted := make(chan bool, 1)
	go func() { admitted <- s.submitBrowserInput([]byte("next"), dead) }()
	waitInputCapacityWaiter(t, s)
	select {
	case got := <-admitted:
		t.Fatalf("browser submission completed at full capacity: %v", got)
	default:
	}
	select {
	case <-dead:
		t.Fatal("capacity pressure disconnected the browser")
	default:
	}

	buf := make([]byte, terminalInputQuantum)
	if _, err := inputRead.Read(buf); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-admitted:
		if !ok {
			t.Fatal("waiting browser did not resume after accounting release")
		}
	case <-time.After(time.Second):
		t.Fatal("waiting browser did not observe accounting release")
	}
	waitSessionInput(t, s, terminalBrowserInputQueue, terminalBrowserInputQueue-1, time.Second)
	s.close()
}

func TestBrowserInputCapacityWaitCancellation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel func(*terminalSession, chan struct{})
	}{
		{name: "connection", cancel: func(_ *terminalSession, dead chan struct{}) { close(dead) }},
		{name: "session", cancel: func(s *terminalSession, _ chan struct{}) { s.stopInput() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, inputWrite := fullNonblockingPipe(t)
			s := &terminalSession{ptmx: inputWrite, input: make(chan *terminalInput, terminalSchedulerQueue), done: make(chan struct{})}
			s.startInput()
			for i := range terminalBrowserInputQueue {
				if !s.submitInput(&terminalInput{p: bytes.Repeat([]byte{byte(i)}, terminalInputQuantum)}) {
					t.Fatalf("fill browser reserve at call %d", i+1)
				}
			}
			waitSessionInput(t, s, terminalBrowserInputQueue, terminalBrowserInputQueue-1, time.Second)
			dead := make(chan struct{})
			result := make(chan bool, 1)
			go func() { result <- s.submitBrowserInput([]byte("blocked"), dead) }()
			waitInputCapacityWaiter(t, s)
			tc.cancel(s, dead)
			select {
			case ok := <-result:
				if ok {
					t.Fatal("cancelled browser submission was admitted")
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not unblock browser submission")
			}
			s.close()
			select {
			case <-s.inputStopped:
			case <-time.After(time.Second):
				t.Fatal("scheduler did not stop and drain after cancellation")
			}
			s.inputMu.Lock()
			calls, inputBytes := s.inputCalls, s.inputBytes
			s.inputMu.Unlock()
			if calls != 0 || inputBytes != 0 {
				t.Fatalf("accounting after cancellation and close = %d/%d, want 0/0", calls, inputBytes)
			}
		})
	}
}

func TestTerminalInputExactSchedulerBounds(t *testing.T) {
	if terminalSchedulerQueue != 320 || terminalSchedulerBytes != 1280<<10 {
		t.Fatalf("scheduler bounds = %d calls/%d bytes, want 320 calls/1.25 MiB", terminalSchedulerQueue, terminalSchedulerBytes)
	}
	if terminalInputQueue != 256 || terminalInputBytes != 1<<20 || terminalBrowserInputQueue != 64 || terminalBrowserInputBytes != 256<<10 {
		t.Fatalf("component bounds local=%d/%d browser=%d/%d", terminalInputQueue, terminalInputBytes, terminalBrowserInputQueue, terminalBrowserInputBytes)
	}
	_, inputWrite := fullNonblockingPipe(t)
	s := &terminalSession{ptmx: inputWrite, done: make(chan struct{})}
	if !s.submitInput(&terminalInput{p: make([]byte, terminalBrowserInputBytes)}) {
		t.Fatal("exact browser byte reserve was not reachable")
	}
	if got := cap(s.input); got != terminalSchedulerQueue {
		t.Fatalf("initialized scheduler channel capacity = %d, want %d", got, terminalSchedulerQueue)
	}
	waitSessionInput(t, s, 1, 0, time.Second)
	if s.submitInput(&terminalInput{p: []byte{1}}) {
		t.Fatal("browser byte reserve accepted one byte beyond its exact bound")
	}
	s.close()
}

func TestTerminalInputExactCombinedCallBoundAndCloseDrain(t *testing.T) {
	_, inputWrite := fullNonblockingPipe(t)
	s := &terminalSession{ptmx: inputWrite, input: make(chan *terminalInput, terminalSchedulerQueue), done: make(chan struct{})}
	local := newLocalTerminalConn()
	client := &TerminalClient{session: s, conn: local}
	block := make([]byte, terminalInputQuantum)
	for i := range terminalInputQueue {
		if err := client.Write(block); err != nil {
			t.Fatalf("admit local call %d of %d: %v", i+1, terminalInputQueue, err)
		}
	}
	for i := range terminalBrowserInputQueue {
		if !s.submitInput(&terminalInput{p: block}) {
			t.Fatalf("admit browser call %d of %d", i+1, terminalBrowserInputQueue)
		}
	}
	waitSessionInput(t, s, terminalSchedulerQueue, terminalSchedulerQueue-1, time.Second)
	s.inputMu.Lock()
	got := [6]int{s.inputCalls, s.inputBytes, s.localInputCalls, s.localInputBytes, s.browserInputCalls, s.browserInputBytes}
	s.inputMu.Unlock()
	want := [6]int{terminalSchedulerQueue, terminalSchedulerBytes, terminalInputQueue, terminalInputBytes, terminalBrowserInputQueue, terminalBrowserInputBytes}
	if got != want {
		t.Fatalf("combined ledgers = %v, want %v", got, want)
	}
	if err := client.Write([]byte("local-overflow")); !errors.Is(err, ErrTerminalInputBackpressure) {
		t.Fatalf("local call beyond combined bound = %v, want backpressure", err)
	}
	dead := make(chan struct{})
	browserResult := make(chan bool, 1)
	go func() { browserResult <- s.submitBrowserInput([]byte("browser-overflow"), dead) }()
	waitInputCapacityWaiter(t, s)
	select {
	case result := <-browserResult:
		t.Fatalf("browser call did not wait at combined bound: %v", result)
	default:
	}
	s.close()
	select {
	case result := <-browserResult:
		if result {
			t.Fatal("browser call was admitted during close")
		}
	case <-time.After(time.Second):
		t.Fatal("close did not unblock browser at combined bound")
	}
	select {
	case <-s.inputStopped:
	case <-time.After(time.Second):
		t.Fatal("close did not drain combined bound")
	}
	waitInputAccounting(t, s, local, 0, 0, time.Second)
	local.kill()
}

func TestBrowserLargeFrameUsesOrderedBoundedSchedulerChunks(t *testing.T) {
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inputRead.Close(); _ = inputWrite.Close() })
	if err := unix.SetNonblock(int(inputWrite.Fd()), true); err != nil {
		t.Fatal(err)
	}
	s := &terminalSession{ptmx: inputWrite, input: make(chan *terminalInput, terminalSchedulerQueue), done: make(chan struct{})}
	first := bytes.Repeat([]byte{'A'}, terminalBrowserInputBytes)
	tail := []byte("[[ordered-second-browser-chunk]]")
	frame := append(append([]byte(nil), first...), tail...)
	dead := make(chan struct{})
	result := make(chan bool, 1)
	go func() { result <- s.submitBrowserInput(frame, dead) }()
	waitInputCapacityWaiter(t, s)
	s.inputMu.Lock()
	if s.browserInputCalls != 1 || s.browserInputBytes != terminalBrowserInputBytes {
		t.Fatalf("between chunks browser ledger = %d/%d, want 1/%d", s.browserInputCalls, s.browserInputBytes, terminalBrowserInputBytes)
	}
	s.inputMu.Unlock()
	got := make([]byte, len(frame))
	readDone := make(chan error, 1)
	go func() { _, err := io.ReadFull(inputRead, got); readDone <- err }()
	select {
	case ok := <-result:
		if !ok {
			t.Fatal("large browser frame submission failed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("large browser frame did not admit its second chunk")
	}
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("large browser frame did not reach writer")
	}
	if !bytes.Equal(got, frame) {
		t.Fatal("large browser frame chunks were not written in order")
	}
	waitSessionInput(t, s, 0, 0, time.Second)
	s.close()
}

func TestBrowserLargeFrameChunkWaitCancellationDrainsAccounting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel func(*terminalSession, chan struct{})
	}{
		{name: "connection", cancel: func(_ *terminalSession, dead chan struct{}) { close(dead) }},
		{name: "session", cancel: func(s *terminalSession, _ chan struct{}) { s.stopInput() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputRead, inputWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = inputRead.Close(); _ = inputWrite.Close() })
			if err := unix.SetNonblock(int(inputWrite.Fd()), true); err != nil {
				t.Fatal(err)
			}
			s := &terminalSession{ptmx: inputWrite, input: make(chan *terminalInput, terminalSchedulerQueue), done: make(chan struct{})}
			frame := make([]byte, terminalBrowserInputBytes+1)
			dead := make(chan struct{})
			result := make(chan bool, 1)
			go func() { result <- s.submitBrowserInput(frame, dead) }()
			waitInputCapacityWaiter(t, s)
			tc.cancel(s, dead)
			select {
			case ok := <-result:
				if ok {
					t.Fatal("cancelled between-chunk submission succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("between-chunk cancellation did not unblock submission")
			}
			s.close()
			select {
			case <-s.inputStopped:
			case <-time.After(time.Second):
				t.Fatal("scheduler did not stop after chunk cancellation")
			}
			s.inputMu.Lock()
			ledgers := [4]int{s.inputCalls, s.inputBytes, s.browserInputCalls, s.browserInputBytes}
			s.inputMu.Unlock()
			if ledgers != [4]int{} {
				t.Fatalf("chunk cancellation leaked ledgers: %v", ledgers)
			}
		})
	}
}

func TestTerminalInputCallBoundLeavesActiveRequestARequeueSlot(t *testing.T) {
	_, inputWrite := fullNonblockingPipe(t)
	s := &terminalSession{ptmx: inputWrite, input: make(chan *terminalInput, terminalInputQueue), done: make(chan struct{})}
	local := newLocalTerminalConn()
	client := &TerminalClient{session: s, conn: local}
	s.startInput()
	<-s.inputStarted
	for i := range terminalInputQueue {
		if err := client.Write([]byte{byte(i)}); err != nil {
			t.Fatalf("admit call %d of exact %d-call bound: %v", i+1, terminalInputQueue, err)
		}
	}
	waitSessionInput(t, s, terminalInputQueue, terminalInputQueue-1, time.Second)
	if got, want := cap(s.input), terminalInputQueue; got != want {
		t.Fatalf("scheduler channel capacity = %d, want %d so the active request retains one requeue slot", got, want)
	}
	// Let the active request time out and exercise its tail requeue before
	// detach. A channel sized only for the other 255 calls deadlocks here.
	time.Sleep(2 * terminalInputPollWindow)
	local.kill()
	waitInputAccounting(t, s, local, 0, 0, terminalInputPollWindow+100*time.Millisecond)
	s.close()
}

func TestTerminalInputFatalWriteClosesOwnerlessSessionAndReleasesAccounting(t *testing.T) {
	inputRead, inputWrite := fullNonblockingPipe(t)
	s := &terminalSession{ptmx: inputWrite, input: make(chan *terminalInput, terminalInputQueue), done: make(chan struct{})}
	local := newLocalTerminalConn()
	client := &TerminalClient{session: s, conn: local}
	s.startInput()
	if err := client.Write([]byte("current")); err != nil {
		t.Fatal(err)
	}
	waitSessionInput(t, s, 1, 0, time.Second)
	if err := client.Write([]byte("queued")); err != nil {
		t.Fatal(err)
	}
	if err := inputRead.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.inputStopped:
	case <-time.After(time.Second):
		t.Fatal("fatal PTY write/POLLERR did not stop ownerless session scheduler")
	}
	waitInputAccounting(t, s, local, 0, 0, time.Second)
	select {
	case <-s.done:
	default:
		t.Fatal("fatal PTY write/POLLERR did not converge ownerless session close")
	}
	local.kill()
}

func TestTerminalInputHasOneRawPTYWriterAndOneScheduler(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "terminal_sessions.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	rawWrites, schedulerStarts, onceStarts := 0, 0, 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "Write" {
			pkg, unixWrite := sel.X.(*ast.Ident)
			if !unixWrite || pkg.Name != "unix" {
				t.Errorf("direct terminal_sessions.go Write call at %s; PTY input must use ptyWrite", fset.Position(call.Pos()))
			} else {
				rawWrites++
			}
		}
		if sel.Sel.Name == "Fd" {
			t.Errorf("terminal_sessions.go File.Fd call at %s; raw PTY operations must preserve runtime pollability", fset.Position(call.Pos()))
		}
		if sel.Sel.Name == "inputLoop" {
			schedulerStarts++
		}
		if sel.Sel.Name == "Do" {
			if x, ok := sel.X.(*ast.SelectorExpr); ok && x.Sel.Name == "inputOnce" {
				onceStarts++
			}
		}
		return true
	})
	if rawWrites != 1 {
		t.Fatalf("unix.Write PTY helpers = %d, want exactly one", rawWrites)
	}
	if schedulerStarts != 1 || onceStarts != 1 {
		t.Fatalf("scheduler structure: inputLoop calls=%d inputOnce.Do calls=%d, want 1/1", schedulerStarts, onceStarts)
	}
	source, err := os.ReadFile("terminal_sessions.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(source, []byte("s.submitBrowserInput(frame.Data, conn.dead)")) {
		t.Fatal("browser attach does not submit through the terminal input scheduler")
	}
}

func fullNonblockingPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	fd := int(w.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 4<<10)
	for {
		if _, err := unix.Write(fd, block); err == unix.EAGAIN {
			return r, w
		} else if err != nil {
			t.Fatal(err)
		}
	}
}

func waitSessionInput(t *testing.T, s *terminalSession, calls, queued int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		s.inputMu.Lock()
		gotCalls := s.inputCalls
		s.inputMu.Unlock()
		if gotCalls == calls && (queued < 0 || len(s.input) == queued) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session input calls/queued = %d/%d, want %d/%d", gotCalls, len(s.input), calls, queued)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitInputCapacityWaiter(t *testing.T, s *terminalSession) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		s.inputMu.Lock()
		waiting := s.inputCapacity != nil
		s.inputMu.Unlock()
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("browser submission did not enter capacity wait")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitInputAccounting(t *testing.T, s *terminalSession, local *localTerminalConn, calls, inputBytes int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		s.inputMu.Lock()
		sc, sb := s.inputCalls, s.inputBytes
		s.inputMu.Unlock()
		lc, lb := local.inputCalls.Load(), local.inputBytes.Load()
		if sc == calls && sb == inputBytes && lc == int64(calls) && lb == int64(inputBytes) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("input accounting session=%d/%d local=%d/%d, want %d/%d", sc, sb, lc, lb, calls, inputBytes)
		}
		time.Sleep(time.Millisecond)
	}
}

func assertPTYNonblocking(t *testing.T, f *os.File, after string) {
	t.Helper()
	rc, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var opErr error
	if err := rc.Control(func(fd uintptr) { flags, opErr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil {
		t.Fatal(err)
	}
	if opErr != nil {
		t.Fatal(opErr)
	}
	if flags&unix.O_NONBLOCK == 0 {
		t.Fatalf("PTY master lost O_NONBLOCK after %s", after)
	}
}

func rawPTYSize(f *os.File) (rows, cols int, err error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var size *unix.Winsize
	var opErr error
	if err := rc.Control(func(fd uintptr) { size, opErr = unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ) }); err != nil {
		return 0, 0, err
	}
	if opErr != nil {
		return 0, 0, opErr
	}
	return int(size.Row), int(size.Col), nil
}

// The browser sibling above proves the same bounded queue design over a sealed
// transport. This local control proves that an unread local output channel also
// cannot park the PTY read path and is detached at terminalWriteTO.
func TestLocalTerminalOutputNeverBlocksOnStalledClient(t *testing.T) {
	s := &terminalSession{id: "local-stall", done: make(chan struct{})}
	stalled := newLocalTerminalConn()
	if got := cap(stalled.output); got != 1 {
		t.Fatalf("local decoded output queue capacity = %d, want one bounded handoff", got)
	}
	if err := s.attachConn(stalled); err != nil {
		t.Fatal(err)
	}
	go func() { <-stalled.dead; s.detach(stalled) }()
	start := time.Now()
	for range 150 {
		s.output(make([]byte, 8<<10))
	}
	if elapsed := time.Since(start); elapsed > terminalWriteTO/2 {
		t.Fatalf("local stalled output took %s; PTY path waited on local client", elapsed)
	}
	// Fill the local consumer queue one writer hand-off at a time. This avoids
	// treating a particular goroutine scheduling pattern during the output burst
	// as evidence that the write-deadline path is reachable.
	deadline := time.Now().Add(time.Second)
	for len(stalled.output) < cap(stalled.output) {
		if !stalled.enqueue(relay.EncodeData([]byte("x"))) {
			t.Fatal("could not queue local output while filling its consumer buffer")
		}
		for len(stalled.send) != 0 {
			if time.Now().After(deadline) {
				t.Fatal("local writer did not consume queued output")
			}
			time.Sleep(time.Millisecond)
		}
	}
	if !stalled.enqueue(relay.EncodeData([]byte("x"))) {
		t.Fatal("could not queue output that should block the local writer")
	}
	select {
	case <-stalled.dead:
	case <-time.After(terminalWriteTO + time.Second):
		t.Fatal("stalled local client was not detached at the write deadline")
	}
	waitConns(t, s, 0)
	s.mu.Lock()
	if s.expires != nil {
		s.expires.Stop()
	}
	s.mu.Unlock()
}

func TestLocalTerminalClientRechecksPolicyOnReattach(t *testing.T) {
	dir := withTempConfigDir(t)
	allowed := filepath.Join(dir, "allowed")
	if err := os.Mkdir(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), []byte(fmt.Sprintf(`{"allowedRoots":[%q]}`, allowed)), 0o600); err != nil {
		t.Fatal(err)
	}
	d := newSystem(systemConfig{Shell: "/bin/cat"})
	if _, err := d.AttachTerminal(t.TempDir(), runningTerminalInfo("local-policy"), 80, 24); err == nil {
		t.Fatal("local create outside allowedRoots succeeded")
	}
	client, err := d.AttachTerminal(allowed, runningTerminalInfo("local-policy"), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.session.close() })
	<-client.Output()
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), []byte(`{"terminalsDisabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AttachTerminal(allowed, runningTerminalInfo("local-policy"), 80, 24); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("policy-denied reattach error = %v", err)
	}
	if d.terminals["local-policy"] != client.session {
		t.Fatal("policy denial removed the existing terminal")
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
	header := fencedHeader(relay.StreamHeader{Kind: relay.KindTerminal, SessionID: "sess", Cwd: allowed, Cols: 80, Rows: 24})
	if _, err := d.terminal(header, acceptedFenceDeadline(t, header)); err == nil {
		t.Fatal("reattach to a session rooted outside allowedRoots must be refused")
	} else if !strings.Contains(err.Error(), "does not allow terminals") {
		t.Fatalf("reattach error = %v, want a policy refusal", err)
	}
	if s := d.terminals["sess"]; s == nil {
		t.Fatal("a refused reattach must not drop the session")
	}

	// Sanity: a session rooted under the allowed root reattaches fine.
	d.terminals["sess"].cwd = allowed
	s, err := d.terminal(header, acceptedFenceDeadline(t, header))
	if err != nil {
		t.Fatalf("reattach under allowedRoots refused: %v", err)
	}
	if s != d.terminals["sess"] {
		t.Fatal("reattach did not return the pre-existing session")
	}
}

// processDead reports whether pid is dead for the purposes of these tests:
// either reaped, or a zombie (dead, awaiting a reap that may never come under a
// non-reaping init).
//
// This used to read /proc/<pid>/stat, which does not exist on macOS — so every
// read errored, "" was taken to mean "the process is gone", and the
// process-group kill and SIGKILL-escalation tests asserted nothing whatsoever
// on darwin while reporting green. That is a check that survives the deletion
// of the thing it guards, and it would have shipped underneath a CI job added
// specifically to cover those paths.
//
// signal 0 is the portable liveness test: it runs the existence and permission
// checks and delivers nothing. ESRCH means gone; EPERM means the pid exists but
// belongs to somebody else now, which after a pid recycle also means our
// process is gone.
func processDead(t *testing.T, pid int) bool {
	t.Helper()
	if err := unix.Kill(pid, 0); err != nil {
		return true
	}
	// Alive — or a zombie, which signal 0 still finds. ps is the one state
	// query that answers the same way on both supported platforms. Wait4 would
	// not do: the process these tests care about is a grandchild, not ours.
	out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		// NOT "gone". Kill(pid, 0) has already proved the process exists, so a
		// ps that is missing, sandboxed, or spells its state keyword
		// differently would otherwise turn processGone into a no-op on that
		// platform -- the same vacuity the /proc read had. processGone polls,
		// so being conservative costs one more 25ms iteration and the next
		// Kill settles it.
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func processGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if processDead(t, pid) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still alive after terminal close", pid)
		}
		time.Sleep(25 * time.Millisecond)
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
	// The child traps SIGHUP so the KERNEL cannot be what kills it. Closing the
	// PTY master hangs up the session's foreground process group, so with a
	// plain `sleep 600` this test passed no matter what killProcessGroup did --
	// gutting it entirely, or replacing the group signal with a single
	// Process.Kill, both stayed green. What is under test is the agent reaching
	// the whole GROUP, so the only thing left that can kill this child is the
	// explicit unix.Kill(-pgid, ...).
	//
	// The CHILD writes the pidfile, after installing the trap. Writing it from
	// the parent (`... & echo $! > %s`) publishes the pid at fork time, so the
	// test could close the PTY while the inner shell had not yet reached the
	// trap -- and then the kernel's hangup killed it at its default
	// disposition, which is the vacuity this is supposed to have removed. It
	// held only by winning a race; a 50ms sleep before the trap made the test
	// pass again with the group kill gutted. $$ inside sh -c is that shell's
	// own pid and survives the exec, and an ignored disposition survives exec
	// by POSIX, so the pidfile existing now proves the trap is installed.
	//
	// Note what the pair of close tests can and cannot separate: a process in
	// the tty's foreground group cannot tell the kernel's hangup from the
	// agent's SIGHUP, so trapping HUP is the only lever available and what both
	// tests actually pin is that the ESCALATION reaches the group. Replacing
	// the SIGHUP with a single-process kill leaves both green; replacing the
	// group SIGKILL does not.
	s, child := startGroupSession(t, "sh -c 'trap \"\" HUP; echo $$ > %s; exec sleep 600' & wait")
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

	// The child publishes its own pid after the trap; see the note in
	// TestTerminalCloseKillsProcessGroup for why the parent must not.
	s, child := startGroupSession(t, "sh -c 'trap \"\" HUP; echo $$ > %s; exec sleep 600' & wait")
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
