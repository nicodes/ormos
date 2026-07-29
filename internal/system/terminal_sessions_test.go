package system

import (
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/nicodes/ormos/relay"
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
