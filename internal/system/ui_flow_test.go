//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestUIOutputCredits(t *testing.T) {
	flow := newUIOutputFlow()
	if flow.ack(1) || flow.ack(0) || flow.ack(-1) {
		t.Fatal("accepted unsolicited ACK")
	}
	flow.sent(uiOutputWindow)
	if flow.ack(uiOutputWindow + 1) {
		t.Fatal("accepted oversized ACK")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := flow.capacity(ctx); err == nil {
		t.Fatal("full window did not wait for parser progress")
	}
	if !flow.ack(17) {
		t.Fatal("valid ACK rejected")
	}
	if available, err := flow.capacity(context.Background()); err != nil || available != 17 {
		t.Fatalf("capacity=%d error=%v", available, err)
	}
}

func TestUIOutputRingChunks(t *testing.T) {
	term := &uiTerminal{}
	data := bytes.Repeat([]byte("0123456789"), uiTerminalBufMax/5)
	term.append(data)
	start, reset := term.replayStart(nil)
	if !reset || start != uint64(len(data)-uiTerminalBufMax) {
		t.Fatal("wrong replay boundary")
	}
	var got []byte
	for cursor := start; cursor < term.end; {
		chunk, valid := term.replayChunk(cursor, uiOutputChunk)
		if !valid || len(chunk) == 0 || len(chunk) > uiOutputChunk {
			t.Fatal("invalid ring chunk")
		}
		got = append(got, chunk...)
		cursor += uint64(len(chunk))
	}
	if !bytes.Equal(got, data[len(data)-uiTerminalBufMax:]) {
		t.Fatal("chunking changed retained bytes")
	}
	if _, valid := term.replayChunk(start-1, 1); valid {
		t.Fatal("expired cursor silently skipped output")
	}
	if _, valid := term.replayChunk(term.end+1, 1); valid {
		t.Fatal("future cursor accepted")
	}
}

func TestUIOutputDeliveryIsImmutableAndShared(t *testing.T) {
	term := &uiTerminal{}
	term.append([]byte("original"))
	a, _ := term.replayChunk(0, 8)
	b, _ := term.replayChunk(0, 8)
	c, _ := term.replayChunk(2, 3)
	if &a[0] != &b[0] || string(c) != "igi" {
		t.Fatal("readers did not reuse immutable output")
	}
	term.append(bytes.Repeat([]byte("x"), uiTerminalBufMax))
	if string(a) != "original" || string(b) != "original" {
		t.Fatal("ring wrap mutated a delivery in flight")
	}
	if _, valid := term.replayChunk(0, 8); valid {
		t.Fatal("delivery cache bypassed retained-history limit")
	}
}

func TestUIFlowReplayAndInput(t *testing.T) {
	for _, mode := range []websocket.CompressionMode{websocket.CompressionDisabled, websocket.CompressionNoContextTakeover} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) { testUIFlowReplayAndInput(t, mode) })
	}
}

func testUIFlowReplayAndInput(t *testing.T, mode websocket.CompressionMode) {
	fix := newUIFixture(t, nil)
	input := make(chan string, 1)
	term := &uiTerminal{id: "t_flow", alive: true, input: func(p []byte) error { input <- string(p); return nil }}
	history := bytes.Repeat([]byte("history\r\n"), (3<<20)/9)
	term.append(history)
	fix.srv.terms[term.id] = term
	ts := httptest.NewUnstartedServer(fix.srv.routes())
	counter := &replayCountingListener{Listener: ts.Listener}
	ts.Listener = counter
	ts.Start()
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/terminal/"+term.id+"/ws?flow=1", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{ts.URL}}, CompressionMode: mode})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(uiOutputChunk)
	_, metadata, err := conn.Read(ctx)
	var meta struct {
		Window int
		Offset uint64
		Reset  bool
	}
	if err != nil || json.Unmarshal(metadata, &meta) != nil || meta.Window != uiOutputWindow || !meta.Reset || meta.Offset != 0 {
		t.Fatalf("metadata=%s error=%v", metadata, err)
	}
	var replay []byte
	for len(replay) < uiOutputWindow {
		_, chunk, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		replay = append(replay, chunk...)
	}
	// The window is full. Input must still arrive before we acknowledge output.
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"input","data":"s"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-input:
		if got != "s" {
			t.Fatal(got)
		}
	case <-ctx.Done():
		t.Fatal("input waited on parser credits")
	}
	term.mu.Lock()
	readers, updates := len(term.readers), len(term.updates)
	term.mu.Unlock()
	if readers != 0 || updates != 1 {
		t.Fatal("flow client retained a private output queue")
	}
	if wire := counter.written.Load(); wire > uiOutputWindow+4096 {
		t.Fatalf("server exceeded the unparsed-output window before ACK: %d socket bytes", wire)
	}
	// More than 64 PTY-sized chunks accumulate while the browser is paused.
	// A flow client must retain them through the shared ring, not be evicted.
	var live []byte
	for i := 0; i < 100; i++ {
		chunk := bytes.Repeat([]byte("L"), 8192)
		live = append(live, chunk...)
		term.append(chunk)
	}
	ack := func(count int) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"type": "ack", "bytes": count})
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
	}
	ack(len(replay))
	for len(replay) < len(history)+len(live) {
		kind, chunk, err := conn.Read(ctx)
		if err != nil || kind != websocket.MessageBinary || len(chunk) > uiOutputChunk {
			t.Fatalf("chunk=%d kind=%d error=%v", len(chunk), kind, err)
		}
		replay = append(replay, chunk...)
		ack(len(chunk))
	}
	if !bytes.Equal(replay, append(history, live...)) {
		t.Fatal("history/live transition changed bytes")
	}
}

func BenchmarkUIFlowRingFanout(b *testing.B) {
	term := &uiTerminal{}
	term.history.buf = make([]byte, uiTerminalBufMax)
	data := bytes.Repeat([]byte("x"), 8192)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		term.append(data)
		for reader := 0; reader < 8; reader++ {
			term.mu.Lock()
			chunk, valid := term.replayChunk(term.end-uint64(len(data)), uiOutputChunk)
			term.mu.Unlock()
			if !valid || len(chunk) != len(data) {
				b.Fatal("lost output")
			}
		}
	}
}

func TestUIFlowProtocolFailures(t *testing.T) {
	for _, kind := range []string{"expired history", "unsolicited ACK", "legacy ACK"} {
		t.Run(kind, func(t *testing.T) {
			fix := newUIFixture(t, nil)
			term := &uiTerminal{id: "t_protocol", alive: true}
			if kind == "expired history" {
				term.append(bytes.Repeat([]byte("x"), 2*uiOutputWindow))
			}
			fix.srv.terms[term.id] = term
			ts := fix.start(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			query := "?flow=1"
			if kind == "legacy ACK" {
				query = ""
			}
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/terminal/"+term.id+"/ws"+query, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{ts.URL}}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			conn.SetReadLimit(uiOutputChunk)
			if _, _, err := conn.Read(ctx); err != nil {
				t.Fatal(err)
			}
			count := 1
			want := websocket.StatusPolicyViolation
			if kind == "expired history" {
				count = 0
				for count < uiOutputWindow {
					_, chunk, err := conn.Read(ctx)
					if err != nil {
						t.Fatal(err)
					}
					count += len(chunk)
				}
				term.append(bytes.Repeat([]byte("y"), uiTerminalBufMax+1))
				want = websocket.StatusTryAgainLater
			}
			data, _ := json.Marshal(map[string]any{"type": "ack", "bytes": count})
			if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
				t.Fatal(err)
			}
			_, _, err = conn.Read(ctx)
			if websocket.CloseStatus(err) != want {
				t.Fatalf("close=%v error=%v", websocket.CloseStatus(err), err)
			}
		})
	}
}

func TestUIFlowResumeAndInvalidNegotiation(t *testing.T) {
	fix := newUIFixture(t, nil)
	term := &uiTerminal{id: "t_resume_flow", alive: true}
	term.append([]byte("already rendered while disconnected"))
	fix.srv.terms[term.id] = term
	ts := fix.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/terminal/" + term.id + "/ws"
	opts := &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{ts.URL}}}
	conn, _, err := websocket.Dial(ctx, endpoint+"?flow=1&since=16", opts)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	_, data, err := conn.Read(ctx)
	var meta struct {
		Offset uint64
		Reset  bool
		Window int
	}
	if err != nil || json.Unmarshal(data, &meta) != nil || meta.Offset != 16 || meta.Reset || meta.Window != uiOutputWindow {
		t.Fatalf("resume metadata: %s %v", data, err)
	}
	_, data, err = conn.Read(ctx)
	if err != nil || string(data) != " while disconnected" {
		t.Fatalf("resume output: %s %v", data, err)
	}
	conn.CloseNow()
	for _, query := range []string{"flow=", "flow=0", "flow=1&flow=1", "flow=garbage"} {
		conn, res, err := websocket.Dial(ctx, endpoint+"?"+query, opts)
		if conn != nil {
			conn.CloseNow()
		}
		if err == nil || res == nil || res.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid negotiation %s: %v %v", query, res, err)
		}
	}
}

func TestUIFlowSlowViewerDoesNotBlockOtherViewers(t *testing.T) {
	fix := newUIFixture(t, nil)
	term := &uiTerminal{id: "t_viewers", alive: true}
	history := bytes.Repeat([]byte("x"), uiOutputWindow+8192)
	term.append(history)
	fix.srv.terms[term.id] = term
	ts := fix.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dial := func(mode websocket.CompressionMode) *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/terminal/"+term.id+"/ws?flow=1", &websocket.DialOptions{CompressionMode: mode, HTTPHeader: http.Header{"Origin": []string{ts.URL}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.CloseNow() })
		conn.SetReadLimit(uiOutputChunk)
		if _, _, err := conn.Read(ctx); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	slow := dial(websocket.CompressionDisabled)
	fast := dial(websocket.CompressionNoContextTakeover)
	for count := 0; count < uiOutputWindow; {
		_, chunk, err := slow.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		count += len(chunk) // Deliberately do not acknowledge the slow viewer.
	}
	read := func(expected []byte) {
		t.Helper()
		var got []byte
		for len(got) < len(expected) {
			_, chunk, err := fast.Read(ctx)
			if err != nil {
				t.Fatal("other viewer was blocked:", err)
			}
			got = append(got, chunk...)
			ack, _ := json.Marshal(map[string]any{"type": "ack", "bytes": len(chunk)})
			if err := fast.Write(ctx, websocket.MessageText, ack); err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(got, expected) {
			t.Fatal("concurrent compressed/uncompressed viewers changed shared output")
		}
	}
	read(history)
	live := bytes.Repeat([]byte("live"), 8192)
	term.append(live)
	read(live)
}
