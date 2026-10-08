//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type replayCountingListener struct {
	net.Listener
	written atomic.Int64
	hold    <-chan struct{}
}

func (l *replayCountingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &replayCountingConn{Conn: conn, owner: l}, nil
}

type replayCountingConn struct {
	net.Conn
	owner *replayCountingListener
}

func (c *replayCountingConn) Write(data []byte) (int, error) {
	if len(data) > 1024 && c.owner.hold != nil {
		<-c.owner.hold
	}
	n, err := c.Conn.Write(data)
	c.owner.written.Add(int64(n))
	return n, err
}

func TestUITerminalCompressedReplay(t *testing.T) {
	for _, mode := range []websocket.CompressionMode{websocket.CompressionDisabled, websocket.CompressionNoContextTakeover} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			fix := newUIFixture(t, nil)
			term := &uiTerminal{id: "t_bulk", alive: true}
			redraw := []byte("\x1b[H\x1b[2Krendered terminal status and unchanged application content\r\n")
			history := bytes.Repeat(redraw, uiTerminalBufMax/len(redraw))
			term.append(history)
			fix.srv.terms[term.id] = term
			ts := httptest.NewUnstartedServer(fix.srv.routes())
			counter := &replayCountingListener{Listener: ts.Listener}
			ts.Listener = counter
			ts.Start()
			defer ts.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started := time.Now()
			conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/terminal/"+term.id+"/ws", &websocket.DialOptions{CompressionMode: mode, HTTPHeader: http.Header{"Origin": []string{ts.URL}}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			conn.SetReadLimit(64 << 10)
			if _, _, err := conn.Read(ctx); err != nil {
				t.Fatal(err)
			}
			var replay []byte
			for len(replay) < len(history) {
				kind, chunk, err := conn.Read(ctx)
				if err != nil || kind != websocket.MessageBinary || len(chunk) > 64<<10 {
					t.Fatalf("replay chunk: %d %v", len(chunk), err)
				}
				replay = append(replay, chunk...)
			}
			if !bytes.Equal(replay, history) {
				t.Fatal("terminal output changed during replay")
			}
			wire := counter.written.Load()
			if mode != websocket.CompressionDisabled {
				if !strings.Contains(response.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate") || wire >= int64(len(history))/10 {
					t.Fatalf("compression not effective: %d bytes for %d raw", wire, len(history))
				}
			}
			t.Logf("raw=%d wire=%d elapsed=%s", len(history), wire, time.Since(started))
		})
	}
}

func TestUITerminalInputWhileHistoryBlocked(t *testing.T) {
	fix := newUIFixture(t, nil)
	input := make(chan string, 1)
	term := &uiTerminal{id: "t_slow", alive: true, input: func(p []byte) error { input <- string(p); return nil }}
	term.append(bytes.Repeat([]byte("x"), 128<<10))
	fix.srv.terms[term.id] = term
	ts := httptest.NewUnstartedServer(fix.srv.routes())
	release := make(chan struct{})
	ts.Listener = &replayCountingListener{Listener: ts.Listener, hold: release}
	ts.Start()
	defer ts.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/terminal/"+term.id+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{ts.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"input","data":"s"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-input:
		if got != "s" {
			t.Fatal(got)
		}
	case <-ctx.Done():
		t.Fatal("input waited for blocked history transfer")
	}
}

func TestUIRejectsForeignBrowserRequests(t *testing.T) {
	fix := newUIFixture(t, nil)
	ts := fix.start(t)
	for _, origin := range []string{"", "https://evil.example", ts.URL + "/path"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/action", strings.NewReader(`{"action":"open"}`))
		req.Header.Set("Origin", origin)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %q: %d", origin, res.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.Host = "evil.example"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("foreign host: %d", res.StatusCode)
	}
	if fix.spawnLog.Len() != 0 {
		t.Fatal("foreign page spawned a shell")
	}
}

func TestUIInteractiveTerminalAndResize(t *testing.T) {
	fix := newUIFixture(t, nil)
	term, err := spawnUITerminal("/bin/sh", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer term.kill()
	fix.srv.terms[term.id] = term
	ts := fix.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/terminal/"+term.id+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{ts.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	for _, msg := range []any{
		map[string]any{"type": "resize", "cols": 91, "rows": 27},
		map[string]any{"type": "input", "data": "printf 'ORMOS_%s\\n' LIVE; stty size\n"},
	} {
		data, _ := json.Marshal(msg)
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
	}
	var output strings.Builder
	for !strings.Contains(output.String(), "ORMOS_LIVE") || !strings.Contains(output.String(), "27 91") {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("output=%q: %v", output.String(), err)
		}
		output.Write(data)
	}
	conn.CloseNow()
	// A connection ending leaves the PTY alive for a phone reconnect.
	term.mu.Lock()
	alive := term.alive
	term.mu.Unlock()
	if !alive {
		t.Fatal("disconnect killed terminal")
	}
}

func TestUIWebSocketRejectsForeignOrigin(t *testing.T) {
	fix := newUIFixture(t, nil)
	ts := fix.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/terminal/t_unknown/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}}})
	if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign websocket: %v %v", res, err)
	}
}

func TestUITerminalAttachmentLimitAndCleanup(t *testing.T) {
	fix := newUIFixture(t, nil)
	term := &uiTerminal{id: "t_limit", alive: true}
	fix.srv.terms[term.id] = term
	ts := fix.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint := ts.URL + "/api/terminal/" + term.id + "/ws"
	readers := func() int { term.mu.Lock(); defer term.mu.Unlock(); return len(term.readers) }
	attachments := func() int { term.mu.Lock(); defer term.mu.Unlock(); return term.attachments }
	// Failed upgrades must release their reservations, including repeated ones.
	for i := 0; i < uiMaxTerminalReaders+1; i++ {
		req, _ := http.NewRequest(http.MethodGet, endpoint, nil)
		req.Header.Set("Origin", ts.URL)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode == http.StatusSwitchingProtocols || readers() != 0 || attachments() != 0 {
			t.Fatal("failed upgrade retained an attachment")
		}
	}
	dial := func() (*websocket.Conn, *http.Response, error) {
		return websocket.Dial(ctx, "ws"+strings.TrimPrefix(endpoint, "http"), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{ts.URL}}})
	}
	var connections []*websocket.Conn
	defer func() {
		for _, conn := range connections {
			conn.CloseNow()
		}
	}()
	for i := 0; i < uiMaxTerminalReaders; i++ {
		conn, _, err := dial()
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
	}
	conn, res, err := dial()
	if conn != nil {
		conn.CloseNow()
	}
	if err == nil || res == nil || res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("excess attachment: response=%v error=%v", res, err)
	}
	connections[0].CloseNow()
	for readers() == uiMaxTerminalReaders {
		select {
		case <-ctx.Done():
			t.Fatal("disconnect leaked attachment")
		case <-time.After(time.Millisecond):
		}
	}
	conn, _, err = dial()
	if err != nil {
		t.Fatal("released slot could not reconnect:", err)
	}
	connections = append(connections, conn)
	if readers() != uiMaxTerminalReaders || attachments() != uiMaxTerminalReaders {
		t.Fatal("unexpected attachment count")
	}
}

func TestUITerminalDrainingAttachmentsRemainBounded(t *testing.T) {
	fix := newUIFixture(t, nil)
	// A slow writer may still be exiting after its full reader queue is evicted.
	// Admission must count its attachment until cleanup, not just live queues.
	term := &uiTerminal{id: "t_draining", alive: true, attachments: uiMaxTerminalReaders}
	fix.srv.terms[term.id] = term
	req := httptest.NewRequest(http.MethodGet, "http://localhost/api/terminal/"+term.id+"/ws", nil)
	req.Header.Set("Origin", "http://localhost")
	res := httptest.NewRecorder()
	fix.srv.routes().ServeHTTP(res, req)
	if res.Code != http.StatusTooManyRequests || term.attachments != uiMaxTerminalReaders || term.history.buf != nil {
		t.Fatal("draining connections bypassed the limit or allocated replay history")
	}
}

func TestUIDirectAppsCannotControlTerminals(t *testing.T) {
	fix := newUIFixture(t, nil)
	fix.srv.hosts = []string{"devbox:8481"}
	for _, origin := range []string{"http://devbox:3000", "http://devbox:39123"} {
		req := httptest.NewRequest(http.MethodPost, "http://devbox:8481/api/action", strings.NewReader(`{"action":"open"}`))
		req.Header.Set("Origin", origin)
		res := httptest.NewRecorder()
		fix.srv.routes().ServeHTTP(res, req)
		if res.Code != http.StatusForbidden {
			t.Fatalf("app origin %q could control terminals: %d", origin, res.Code)
		}
	}
	if fix.spawnLog.Len() != 0 {
		t.Fatal("preview app spawned a shell")
	}
}

func TestUIDirectPreviewContentPolicy(t *testing.T) {
	fix := newUIFixture(t, nil)
	fix.srv.hosts = []string{"devbox:8481"}
	res := httptest.NewRecorder()
	fix.srv.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "http://devbox:8481/", nil))
	csp := res.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"script-src 'self'", "frame-src http://devbox:* https://devbox:*", "connect-src 'self' http://devbox:* https://devbox:*", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Fatalf("missing %q in CSP %q", directive, csp)
		}
	}
	if strings.Contains(csp, "http://*:*") || strings.Contains(csp, "https://*:*") {
		t.Fatal("content policy permits unrelated preview hosts")
	}
}

func TestUIExitedTerminalDoesNotConsumeCapacity(t *testing.T) {
	fix := newUIFixture(t, nil)
	ts := fix.start(t)
	oldMax := uiMaxTerminals
	uiMaxTerminals = 1
	t.Cleanup(func() { uiMaxTerminals = oldMax })
	if code, _ := postJSON(t, ts.URL+"/api/action", `{"action":"open","shell":"/bin/sh"}`); code != 200 {
		t.Fatal(code)
	}
	if code, _ := postJSON(t, ts.URL+"/api/action", `{"action":"kill","id":"t_stub"}`); code != 200 {
		t.Fatal(code)
	}
	if code, _ := postJSON(t, ts.URL+"/api/action", `{"action":"open","shell":"/bin/sh"}`); code != 200 {
		t.Fatalf("replacement: %d", code)
	}
}

func TestUINewTerminalUsesHomeByDefault(t *testing.T) {
	fix := newUIFixture(t, nil)
	server := fix.start(t)
	if code, _ := postJSON(t, server.URL+"/api/action", `{"action":"open","shell":"/bin/sh"}`); code != 200 {
		t.Fatal(code)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fix.spawnLog.String(), "/bin/sh\x00"+home+"\n") {
		t.Fatalf("default terminal cwd: %q", fix.spawnLog.String())
	}
}

func TestUITerminalReplayRetainsHistoryBeyondOldLimit(t *testing.T) {
	term := &uiTerminal{}
	payload := []byte("EARLY_HISTORY\r\n" + strings.Repeat("history line\r\n", 10000))
	term.append(payload)
	got, offset, reset := term.replay(nil)
	if string(got) != string(payload) || offset != 0 || !reset {
		t.Fatalf("fresh replay lost history: length=%d offset=%d reset=%v", len(got), offset, reset)
	}
	// The larger history remains bounded, including at ring wrap boundaries.
	term.append([]byte(strings.Repeat("x", uiTerminalBufMax)))
	term.append([]byte("tail"))
	got, offset, reset = term.replay(nil)
	if len(got) != uiTerminalBufMax || !strings.HasSuffix(string(got), "tail") || offset != uint64(len(payload)+4) || !reset {
		t.Fatalf("bounded replay: length=%d offset=%d reset=%v", len(got), offset, reset)
	}
}

func TestUITerminalResumeAfterDisconnect(t *testing.T) {
	fix := newUIFixture(t, nil)
	term := &uiTerminal{id: "t_resume", alive: true}
	term.append([]byte("already rendered"))
	fix.srv.terms[term.id] = term
	ts := fix.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	readReplay := func(suffix string, wantOffset uint64, wantReset bool, want string) *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/terminal/"+term.id+"/ws"+suffix, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{ts.URL}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.CloseNow() })
		kind, data, err := conn.Read(ctx)
		var meta struct {
			Type   string
			Offset uint64
			Reset  bool
		}
		if err != nil || kind != websocket.MessageText || json.Unmarshal(data, &meta) != nil || meta.Type != "replay" || meta.Offset != wantOffset || meta.Reset != wantReset {
			t.Fatalf("replay metadata: %s %v", data, err)
		}
		if want != "" {
			kind, data, err = conn.Read(ctx)
			if err != nil || kind != websocket.MessageBinary || string(data) != want {
				t.Fatalf("replay output: %q %v", data, err)
			}
		}
		return conn
	}
	conn := readReplay("", 0, true, "already rendered")
	conn.CloseNow()
	term.append([]byte(" while disconnected"))
	conn = readReplay("?since=16", 16, false, " while disconnected")
	term.append([]byte(" live"))
	_, data, err := conn.Read(ctx)
	if err != nil || string(data) != " live" {
		t.Fatalf("live output: %q %v", data, err)
	}
	conn.CloseNow()
	// No missed output means metadata only, followed directly by new live bytes.
	conn = readReplay("?since=40", 40, false, "")
	term.append([]byte(" next"))
	_, data, err = conn.Read(ctx)
	if err != nil || string(data) != " next" {
		t.Fatalf("caught-up output: %q %v", data, err)
	}
	conn.CloseNow()
	readReplay("?since=999", 0, true, "already rendered while disconnected live next").CloseNow()
	for _, query := range []string{"-1", "oops", "9007199254740992", "1&since=2"} {
		conn, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/terminal/"+term.id+"/ws?since="+query, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{ts.URL}}})
		if conn != nil {
			conn.CloseNow()
		}
		if err == nil || res == nil || res.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid offset %q accepted: %v", query, err)
		}
	}
}

func TestUITerminalExpiredResumeResetsToRetainedTail(t *testing.T) {
	term := &uiTerminal{}
	term.append([]byte(strings.Repeat("x", uiTerminalBufMax+10)))
	since := uint64(9)
	got, offset, reset := term.replay(&since)
	if !reset || offset != 10 || len(got) != uiTerminalBufMax {
		t.Fatalf("expired replay: %d %d %v", len(got), offset, reset)
	}
	since = uint64(10)
	_, offset, reset = term.replay(&since)
	if reset || offset != 10 {
		t.Fatal("oldest retained offset should still resume")
	}
}

func TestUIWorkspaceConnectionLimitIncludesRetiredPTYs(t *testing.T) {
	fixture := newUIFixture(t, nil)
	for i := 0; i < uiMaxWorkspaceReaders/uiMaxTerminalReaders; i++ {
		id := fmt.Sprintf("t_group_%d", i)
		term := &uiTerminal{id: id, alive: true}
		if i == 0 {
			term.append(bytes.Repeat([]byte("x"), uiOutputWindow))
		}
		fixture.srv.terms[id] = term
	}
	server := fixture.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var clients []*websocket.Conn
	t.Cleanup(func() {
		for _, client := range clients {
			_ = client.CloseNow()
		}
	})
	dial := func(id string) (*websocket.Conn, *http.Response, error) {
		return websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/terminal/"+id+"/ws?flow=1", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{server.URL}}})
	}
	for i := 0; i < uiMaxWorkspaceReaders; i++ {
		group := i / uiMaxTerminalReaders
		client, _, err := dial(fmt.Sprintf("t_group_%d", group))
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		client.SetReadLimit(uiOutputChunk + 1024)
		if _, _, err := client.Read(ctx); err != nil {
			t.Fatal(err)
		} // replay boundary
		if group == 0 {
			for received := 0; received < uiOutputWindow; {
				kind, data, err := client.Read(ctx)
				if err != nil || kind != websocket.MessageBinary {
					t.Fatalf("output: %v", err)
				}
				received += len(data)
			}
			// No ACK: all eight retiring viewers remain at a full parser window.
		}
	}
	fixture.srv.mu.Lock()
	retired := fixture.srv.terms["t_group_0"]
	delete(fixture.srv.terms, "t_group_0")
	fixture.srv.terms["t_replacement"] = &uiTerminal{id: "t_replacement", alive: true}
	fixture.srv.mu.Unlock()
	retired.mu.Lock()
	retired.alive = false
	for update := range retired.updates {
		close(update)
		delete(retired.updates, update)
	}
	retired.mu.Unlock()
	client, response, err := dial("t_replacement")
	if client != nil {
		_ = client.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != 429 {
		t.Fatal("retired PTY sockets escaped the workspace connection bound")
	}
	_ = clients[0].CloseNow()
	deadline := time.Now().Add(2 * time.Second)
	for {
		fixture.srv.mu.Lock()
		connections := fixture.srv.connections
		fixture.srv.mu.Unlock()
		if connections < uiMaxWorkspaceReaders {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disconnect did not release the global reservation")
		}
		time.Sleep(time.Millisecond)
	}
	client, _, err = dial("t_replacement")
	if err != nil {
		t.Fatal(err)
	}
	clients = append(clients, client)
}
