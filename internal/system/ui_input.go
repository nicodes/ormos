//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"os"
	"sync"
	"time"
)

const (
	uiInputWindow  = 32 << 10
	uiInputTimeout = 5 * time.Second
)

// One cancellable writer owns the PTY deadline at a time, including viewers
// waiting for that writer. Disconnecting a browser never closes its PTY.
func newUITerminalInput(file *os.File) func(context.Context, []byte) error {
	gate := make(chan struct{}, 1)
	return func(parent context.Context, data []byte) error {
		ctx, cancel := context.WithTimeout(parent, uiInputTimeout)
		defer cancel()
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		deadline, _ := ctx.Deadline()
		if err := file.SetWriteDeadline(deadline); err != nil {
			return err
		}
		interrupted := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { _ = file.SetWriteDeadline(time.Now()); close(interrupted) })
		_, err := file.Write(data)
		if !stop() {
			<-interrupted
		}
		_ = file.SetWriteDeadline(time.Time{})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
}

// Byte accounting includes the in-flight write. ACKs and resizes continue to
// be read while a foreground program is not consuming its input. The message
// count is coalesced into a fixed byte ring rather than a queue of allocations.
type uiInputQueue struct {
	mu      sync.Mutex
	pending int
	buffer  replayRing
	wake    chan struct{}
	done    chan struct{}
}

func newUIInputQueue(ctx context.Context, write func(context.Context, []byte) error, ack func(int) error, failed func()) *uiInputQueue {
	input := &uiInputQueue{wake: make(chan struct{}, 1), done: make(chan struct{})}
	go func() {
		defer close(input.done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-input.wake:
				data := input.take()
				if len(data) == 0 {
					continue
				}
				if ctx.Err() != nil {
					return
				}
				if write == nil || write(ctx, data) != nil {
					if ctx.Err() == nil {
						failed()
					}
					return
				}
				input.mu.Lock()
				input.pending -= len(data)
				input.mu.Unlock()
				if ack(len(data)) != nil {
					if ctx.Err() == nil {
						failed()
					}
					return
				}
			}
		}
	}()
	return input
}

func (input *uiInputQueue) push(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	input.mu.Lock()
	defer input.mu.Unlock()
	if len(data) > uiInputWindow-input.pending {
		return false
	}
	if input.buffer.buf == nil {
		input.buffer.buf = make([]byte, uiInputWindow)
	}
	input.buffer.append(data)
	input.pending += len(data)
	select {
	case input.wake <- struct{}{}:
	default:
	}
	return true
}

func (input *uiInputQueue) take() []byte {
	input.mu.Lock()
	defer input.mu.Unlock()
	n := min(input.buffer.size, 8192)
	if n == 0 {
		return nil
	}
	data := make([]byte, n)
	copied := copy(data, input.buffer.buf[input.buffer.start:min(input.buffer.start+n, len(input.buffer.buf))])
	copy(data[copied:], input.buffer.buf[:n-copied])
	input.buffer.start = (input.buffer.start + n) % len(input.buffer.buf)
	input.buffer.size -= n
	if input.buffer.size > 0 {
		select {
		case input.wake <- struct{}{}:
		default:
		}
	}
	return data
}
