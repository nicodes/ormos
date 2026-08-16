package relay

import (
	"context"
	"errors"

	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// The stream window is what a peer may have in flight before it must wait for
// an acknowledgement, so yamux's 256 KiB default capped one stream at 256 KiB
// per tunnel round trip. The port proxy shares it and dev servers routinely
// serve multi-megabyte bundles.
func TestYamuxConfigStreamWindow(t *testing.T) {
	cfg := yamuxConfig()
	if err := yamux.VerifyConfig(cfg); err != nil {
		t.Fatalf("yamuxConfig is not a valid yamux config: %v", err)
	}
	if cfg.MaxStreamWindowSize != MaxStreamWindow {
		t.Fatalf("MaxStreamWindowSize = %d, want %d", cfg.MaxStreamWindowSize, MaxStreamWindow)
	}
	// The budget, not the floor. What this change trades away is the product of
	// the window and the stream cap — the memory an adversarial peer can pin —
	// so raising either factor without deciding to must fail here rather than
	// pass a test that only ever checked the window was big enough.
	if MaxTunnelWindowBytes > 512<<20 {
		t.Fatalf("a peer can pin %d bytes across %d streams; the accepted budget is 512 MiB",
			MaxTunnelWindowBytes, MaxTunnelStreams)
	}
	// Both ends share this function, which is what stops the two drifting.
	if !cfg.EnableKeepAlive {
		t.Fatal("keepalive is off; a dead tunnel would go unnoticed on both ends")
	}
}

// NetConn disables any read limit set on the connection before it: websocket.NetConn
// does so as part of presenting the connection as a net.Conn, since a byte
// stream may span messages. The agent relies on that, and on yamux rather than
// the WebSocket layer for its memory ceiling, so pin the behaviour here.
//
// An assertion rather than a comment: if a future coder/websocket honours the
// limit instead, a caller that set one would silently start truncating a
// stream, and this fails first so that trade can be reconsidered deliberately.
func TestNetConnDiscardsAnyReadLimit(t *testing.T) {
	const payload = 256 << 10

	// Buffered so the handler never blocks on a test that has already failed.
	writeErr := make(chan error, 1)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			writeErr <- err
			return
		}
		defer c.CloseNow()
		// Larger than the limit set below and than one read buffer, so a
		// honoured limit is hit well before the payload is drained.
		writeErr <- c.Write(r.Context(), websocket.MessageBinary, make([]byte, payload))
		// Parked on the test's own channel. websocket.Accept hijacks, so the
		// request context is never cancelled and r.Context().Done() would leak
		// this goroutine for the life of the binary.
		<-done
	}))
	defer srv.Close()
	defer close(done)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	conn.SetReadLimit(1024)
	nc := NetConn(ctx, conn)

	read := 0
	buf := make([]byte, 32<<10)
	for read < payload {
		n, err := nc.Read(buf)
		read += n
		if err == nil {
			continue
		}
		// Only one of these means the pinned behaviour changed. Reporting a
		// slow runner as a protocol change would be a confident wrong answer.
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			t.Fatalf("timed out after %d of %d bytes: %v", read, payload, err)
		case websocket.CloseStatus(err) == websocket.StatusMessageTooBig ||
			strings.Contains(err.Error(), "read limited"):
			t.Fatalf("read limited at %d of %d bytes: NetConn no longer disables "+
				"the read limit, so relying on the yamux window alone should be "+
				"reconsidered", read, payload)
		default:
			t.Fatalf("read %d of %d bytes: %v", read, payload, err)
		}
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("server write: %v", err)
	}
}
