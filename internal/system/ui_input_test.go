//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestUIInputQueueBoundsIncludeBlockedWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	input := newUIInputQueue(ctx, func(ctx context.Context, data []byte) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}, func(int) error { t.Error("acknowledged unwritten bytes"); return nil }, func() { t.Error("cancel reported as failure") })
	if !input.push(bytes.Repeat([]byte("x"), uiInputWindow)) {
		t.Fatal("initial window rejected")
	}
	<-entered
	if input.push([]byte("extra")) {
		t.Fatal("in-flight bytes escaped queue accounting")
	}
	if len(input.buffer.buf) != uiInputWindow {
		t.Fatal("input ring exceeded its bound")
	}
	cancel()
	select {
	case <-input.done:
	case <-time.After(time.Second):
		t.Fatal("worker leaked on disconnect")
	}
}

func TestUIInputQueueCoalescesAndPreservesBytes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got []byte
	acks := make(chan int, 64)
	input := newUIInputQueue(ctx, func(_ context.Context, data []byte) error { got = append(got, data...); return nil }, func(n int) error { acks <- n; return nil }, func() { t.Error("unexpected failure") })
	defer func() { cancel(); <-input.done }()
	want := bytes.Repeat([]byte("😀終"), 3000)
	for _, b := range want {
		if !input.push([]byte{b}) {
			t.Fatal("tiny messages exceeded byte bound")
		}
	}
	acked := 0
	for acked < len(want) {
		select {
		case n := <-acks:
			acked += n
		case <-time.After(time.Second):
			t.Fatal("input did not drain")
		}
	}
	cancel()
	<-input.done
	if !bytes.Equal(got, want) {
		t.Fatal("input coalescing changed UTF-8 bytes")
	}
}

func TestUITerminalInputDeadlineLeavesShellAlive(t *testing.T) {
	dir := t.TempDir()
	shell := filepath.Join(dir, "blocked-shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nstty -echo -icanon\nprintf READY\nsleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	term, err := spawnUITerminal(shell, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { term.kill(); <-term.done })
	ready := time.Now().Add(time.Second)
	for !strings.Contains(term.output(), "READY") {
		if time.Now().After(ready) {
			t.Fatal("fixture not ready")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = term.input(ctx, bytes.Repeat([]byte("x"), 1<<20))
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("blocked write deadline=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("input deadline was not bounded")
	}
	term.mu.Lock()
	alive := term.alive
	term.mu.Unlock()
	if !alive {
		t.Fatal("input deadline killed the terminal")
	}
}

func TestUIBlockedInputDoesNotBlockOutputACKOrResize(t *testing.T) {
	fix := newUIFixture(t, nil)
	entered, interrupted := make(chan struct{}), make(chan struct{})
	resized := make(chan struct{}, 1)
	term := &uiTerminal{id: "t_input", alive: true, input: func(ctx context.Context, p []byte) error {
		close(entered)
		<-ctx.Done()
		close(interrupted)
		return ctx.Err()
	}, resize: func(uint16, uint16) error { resized <- struct{}{}; return nil }}
	term.append(bytes.Repeat([]byte("x"), 2*uiOutputWindow))
	fix.srv.terms[term.id] = term
	ts := fix.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/terminal/"+term.id+"/ws?flow=1&input=1", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {ts.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(uiOutputChunk)
	_, metadata, err := conn.Read(ctx)
	var meta struct{ InputWindow int }
	if err != nil || json.Unmarshal(metadata, &meta) != nil || meta.InputWindow != uiInputWindow {
		t.Fatalf("input negotiation=%s %v", metadata, err)
	}
	readWindow := func() {
		t.Helper()
		n := 0
		for n < uiOutputWindow {
			kind, data, err := conn.Read(ctx)
			if err != nil || kind != websocket.MessageBinary {
				t.Fatalf("output blocked: %v", err)
			}
			n += len(data)
		}
	}
	readWindow()
	for _, message := range []string{`{"type":"input","data":"blocked"}`, `{"type":"ack","bytes":262144}`, `{"type":"resize","cols":90,"rows":30}`} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(message)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("input was not started")
	}
	readWindow()
	select {
	case <-resized:
	case <-ctx.Done():
		t.Fatal("resize waited for PTY input")
	}
	conn.CloseNow()
	select {
	case <-interrupted:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel input")
	}
	term.mu.Lock()
	alive := term.alive
	term.mu.Unlock()
	if !alive {
		t.Fatal("browser disconnect killed the terminal")
	}
}

func TestUIInputOverflowClosesOnlyViewer(t *testing.T) {
	fix := newUIFixture(t, nil)
	entered, interrupted := make(chan struct{}), make(chan struct{})
	term := &uiTerminal{id: "t_overflow", alive: true, input: func(ctx context.Context, data []byte) error {
		close(entered)
		<-ctx.Done()
		close(interrupted)
		return ctx.Err()
	}}
	fix.srv.terms[term.id] = term
	ts := fix.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	address := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/terminal/" + term.id + "/ws"
	options := &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {ts.URL}}}
	for _, query := range []string{"?input=2", "?input=1&input=1"} {
		conn, res, err := websocket.Dial(ctx, address+query, options)
		if conn != nil {
			conn.CloseNow()
		}
		if err == nil || res == nil || res.StatusCode != 400 {
			t.Fatalf("malformed input negotiation accepted: %v", err)
		}
	}
	conn, _, err := websocket.Dial(ctx, address+"?input=1", options)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	// A peer that ignores credits cannot flood a program that refuses input.
	for i := 0; i < 2; i++ {
		data, _ := json.Marshal(map[string]string{"type": "input", "data": strings.Repeat("x", uiInputWindow/2)})
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("blocked input not started")
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"input","data":"extra"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("overflow close=%v", err)
	}
	select {
	case <-interrupted:
	case <-time.After(time.Second):
		t.Fatal("overflow leaked blocked writer")
	}
	term.mu.Lock()
	alive := term.alive
	term.mu.Unlock()
	if !alive {
		t.Fatal("overflow killed the PTY")
	}
}

func TestUITerminalInputCancellationDoesNotPoisonNextWriter(t *testing.T) {
	dir := t.TempDir()
	shell := filepath.Join(dir, "blocked-shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nstty -echo -icanon\nprintf READY\nwhile [ ! -f consume ]; do sleep 0.01; done\ncat\n"), 0700); err != nil {
		t.Fatal(err)
	}
	term, err := spawnUITerminal(shell, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { term.kill(); <-term.done })
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(term.output(), "READY") {
		if time.Now().After(deadline) {
			t.Fatal("fixture not ready")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	written := make(chan error, 1)
	go func() { written <- term.input(ctx, bytes.Repeat([]byte("x"), 1<<20)) }()
	select {
	case <-written:
		t.Fatal("fixture did not block")
	case <-time.After(30 * time.Millisecond):
	}
	waiting, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	if err := term.input(waiting, []byte("later")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting writer=%v", err)
	}
	select {
	case <-written:
		t.Fatal("waiting writer changed active deadline")
	default:
	}
	cancel()
	select {
	case err := <-written:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled writer=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("input cancellation did not wake PTY write")
	}
	// Let the owned fixture consume its buffered input, then prove a real
	// subsequent write works rather than merely accepting a zero-byte no-op.
	if err := os.WriteFile(filepath.Join(dir, "consume"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := term.input(context.Background(), []byte("FRESH_INPUT")); err != nil {
		t.Fatalf("next writer inherited deadline: %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for !strings.Contains(term.output(), "FRESH_INPUT") {
		if time.Now().After(deadline) {
			t.Fatal("next input was not consumed")
		}
		time.Sleep(time.Millisecond)
	}
	term.mu.Lock()
	alive := term.alive
	term.mu.Unlock()
	if !alive {
		t.Fatal("canceled write stopped the shell")
	}
}
