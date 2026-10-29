//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	uiOutputWindow = 256 << 10
	uiOutputChunk  = 64 << 10
	uiParseTimeout = 30 * time.Second
)

// One writer consumes credits; the input reader returns them only after the
// browser parses output. Never hold the terminal lock while waiting on a client.
type uiOutputFlow struct {
	mu      sync.Mutex
	pending int
	wake    chan struct{}
}

func newUIOutputFlow() *uiOutputFlow { return &uiOutputFlow{wake: make(chan struct{}, 1)} }

func (f *uiOutputFlow) capacity(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	available := uiOutputWindow - f.pending
	f.mu.Unlock()
	if available > 0 {
		return available, nil
	}
	ctx, cancel := context.WithTimeout(ctx, uiParseTimeout)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-f.wake:
		}
		f.mu.Lock()
		available = uiOutputWindow - f.pending
		f.mu.Unlock()
		if available > 0 {
			return available, nil
		}
	}
}

func (f *uiOutputFlow) sent(count int) {
	f.mu.Lock()
	f.pending += count
	f.mu.Unlock()
}

func (f *uiOutputFlow) ack(count int) bool {
	f.mu.Lock()
	if count <= 0 || count > f.pending {
		f.mu.Unlock()
		return false
	}
	f.pending -= count
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
	return true
}

// A slow browser consumes the shared bounded ring rather than a private queue
// of output copies. PTY capture continues even if every browser is suspended.
func (t *uiTerminal) streamOutput(ctx context.Context, conn *websocket.Conn, flow *uiOutputFlow, updates <-chan struct{}, cursor, replayEnd uint64, replaySize *uiTerminalSize) {
	geometry := replaySize != nil
	replaying := geometry
	control := func(message any) error {
		data, err := json.Marshal(message)
		if err != nil {
			return err
		}
		writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return conn.Write(writeCtx, websocket.MessageText, data)
	}
	for {
		available, err := flow.capacity(ctx)
		if err != nil {
			return
		}
		t.mu.Lock()
		limit := min(available, uiOutputChunk)
		var size uiTerminalSize
		if geometry {
			size = t.sizeAt(cursor)
			limit = t.geometryLimit(cursor, limit)
			if replaying && cursor < replayEnd {
				limit = min(limit, int(replayEnd-cursor))
			}
		}
		chunk, valid := t.replayChunk(cursor, limit)
		alive := t.alive
		t.mu.Unlock()
		if !valid {
			conn.Close(websocket.StatusTryAgainLater, "terminal history expired; reconnect")
			return
		}
		if geometry && (size.Cols != replaySize.Cols || size.Rows != replaySize.Rows) {
			if err := control(struct {
				Type string `json:"type"`
				uiTerminalSize
			}{"size", size}); err != nil {
				return
			}
			*replaySize = size
		}
		if replaying && cursor >= replayEnd {
			if err := control(struct {
				Type string `json:"type"`
			}{"replay-end"}); err != nil {
				return
			}
			replaying = false
		}
		if len(chunk) == 0 {
			if !alive {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-updates:
			}
			continue
		}
		// Reserve before Write: a fast browser can acknowledge before it returns.
		flow.sent(len(chunk))
		if err := writeTerminalChunk(ctx, conn, chunk); err != nil {
			return
		}
		cursor += uint64(len(chunk))
	}
}
