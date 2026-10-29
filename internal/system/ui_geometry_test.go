//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestUIGeometryReplayAndLiveResize(t *testing.T) {
	fix := newUIFixture(t, nil)
	term := &uiTerminal{id: "t_geometry", alive: true, resize: func(uint16, uint16) error { return nil }}
	term.append([]byte("original"))
	if err := term.setSize(20, 6); err != nil {
		t.Fatal(err)
	}
	term.append([]byte("narrow"))
	if err := term.setSize(90, 30); err != nil {
		t.Fatal(err)
	}
	term.append([]byte("wide"))
	fix.srv.terms[term.id] = term
	ts := httptest.NewServer(fix.srv.routes())
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/terminal/"+term.id+"/ws?flow=1&geometry=1", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{ts.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	read := func(kind websocket.MessageType) []byte {
		t.Helper()
		gotKind, data, err := conn.Read(ctx)
		if err != nil || gotKind != kind {
			t.Fatalf("kind=%v data=%s error=%v", gotKind, data, err)
		}
		return data
	}
	var metadata struct {
		Type string
		Size uiTerminalSize
	}
	if err := json.Unmarshal(read(websocket.MessageText), &metadata); err != nil || metadata.Size.Cols != 80 || metadata.Size.Rows != 24 {
		t.Fatalf("initial grid=%+v error=%v", metadata, err)
	}
	output := func(want string) {
		t.Helper()
		if got := string(read(websocket.MessageBinary)); got != want {
			t.Fatalf("output=%q want=%q", got, want)
		}
	}
	size := func(cols, rows uint16) {
		t.Helper()
		var message struct {
			Type       string
			Cols, Rows uint16
		}
		if err := json.Unmarshal(read(websocket.MessageText), &message); err != nil || message.Type != "size" || message.Cols != cols || message.Rows != rows {
			t.Fatalf("size=%+v error=%v", message, err)
		}
	}
	output("original")
	size(20, 6)
	output("narrow")
	size(90, 30)
	output("wide")
	var end struct{ Type string }
	if err := json.Unmarshal(read(websocket.MessageText), &end); err != nil || end.Type != "replay-end" {
		t.Fatalf("replay boundary=%+v error=%v", end, err)
	}
	// Resizes without output must wake the writer too.
	if err := term.setSize(70, 12); err != nil {
		t.Fatal(err)
	}
	size(70, 12)
	term.append([]byte("live"))
	output("live")
}

func TestUIGeometryRetentionAndCoalescing(t *testing.T) {
	calls := 0
	term := &uiTerminal{resize: func(uint16, uint16) error { calls++; return nil }}
	for i := 0; i < 100; i++ {
		if err := term.setSize(uint16(40+i), 24); err != nil {
			t.Fatal(err)
		}
	}
	if len(term.sizes) != 1 {
		t.Fatal("same-offset resizes accumulated")
	}
	if err := term.setSize(139, 24); err != nil || calls != 100 {
		t.Fatal("unchanged grid resized PTY")
	}
	term.sizes = []uiTerminalSize{{Offset: 0, Cols: 80, Rows: 24}, {Offset: 5, Cols: 20, Rows: 6}, {Offset: 15, Cols: 90, Rows: 30}}
	term.end, term.history.size = 20, 10
	term.trimSizes()
	if len(term.sizes) != 2 || term.sizeAt(10).Cols != 20 || term.geometryLimit(10, 100) != 5 {
		t.Fatal("ring wrap lost active grid or resize boundary")
	}
}
