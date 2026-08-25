//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type fakeTerminalAttachment struct {
	output     chan []byte
	done       chan struct{}
	writeErrs  []error
	writes     [][]byte
	resizes    [][2]int
	resizeErr  error
	detached   int
	writeGate  *atomic.Bool
	firstWrite chan struct{}
	writeOnce  sync.Once
	mu         sync.Mutex
}

func newFakeTerminalAttachment() *fakeTerminalAttachment {
	return &fakeTerminalAttachment{output: make(chan []byte, 8), done: make(chan struct{})}
}
func (f *fakeTerminalAttachment) Output() <-chan []byte { return f.output }
func (f *fakeTerminalAttachment) Done() <-chan struct{} { return f.done }
func (f *fakeTerminalAttachment) Write(p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, append([]byte(nil), p...))
	if f.firstWrite != nil {
		f.writeOnce.Do(func() { close(f.firstWrite) })
	}
	if f.writeGate != nil && !f.writeGate.Load() {
		return ErrTerminalInputBackpressure
	}
	if len(f.writeErrs) > 0 {
		err := f.writeErrs[0]
		f.writeErrs = f.writeErrs[1:]
		return err
	}
	return nil
}
func (f *fakeTerminalAttachment) Resize(cols, rows int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]int{cols, rows})
	return f.resizeErr
}
func (f *fakeTerminalAttachment) Detach() { f.mu.Lock(); f.detached++; f.mu.Unlock() }
func (f *fakeTerminalAttachment) snapshot() ([][]byte, [][2]int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writes := append([][]byte(nil), f.writes...)
	resizes := append([][2]int(nil), f.resizes...)
	return writes, resizes, f.detached
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

type scriptedContextReader struct {
	data     []byte
	err      error
	closeErr error
	closed   bool
}

func (r *scriptedContextReader) Read(context.Context, []byte) (int, error) {
	if len(r.data) == 0 {
		if r.err != nil {
			err := r.err
			r.err = nil
			return 0, err
		}
		return 0, errTerminalInputIdle
	}
	return 0, errors.New("scripted reader must be installed with dataReader")
}
func (r *scriptedContextReader) Close() error { r.closed = true; return r.closeErr }

type dataReader struct {
	chunks   [][]byte
	err      error
	closeErr error
}

func (r *dataReader) Read(_ context.Context, p []byte) (int, error) {
	if len(r.chunks) > 0 {
		chunk := r.chunks[0]
		r.chunks = r.chunks[1:]
		return copy(p, chunk), nil
	}
	if r.err != nil {
		err := r.err
		r.err = nil
		return 0, err
	}
	return 0, errTerminalInputIdle
}
func (r *dataReader) Close() error { return r.closeErr }

func execTestCommand(client terminalAttachment, reader contextReader, stdout io.Writer) *terminalExecCommand {
	c := &terminalExecCommand{
		ctx: context.Background(), initialCols: 80, initialRows: 24,
		attach: func() (terminalAttachment, error) { return client, nil },
	}
	c.SetStdin(strings.NewReader("unused"))
	c.SetStdout(stdout)
	c.SetStderr(io.Discard)
	newContextReader = func(io.Reader) (contextReader, error) { return reader, nil }
	terminalResizeEvents = func() (<-chan os.Signal, func()) { return nil, func() {} }
	return c
}

func saveTerminalExecGlobals(t *testing.T) {
	t.Helper()
	oldReader, oldTTY, oldRaw := newContextReader, terminalIsTTY, makeTerminalRaw
	oldSize, oldResize := terminalGetSize, terminalResizeEvents
	t.Cleanup(func() {
		newContextReader, terminalIsTTY, makeTerminalRaw = oldReader, oldTTY, oldRaw
		terminalGetSize, terminalResizeEvents = oldSize, oldResize
	})
}

func TestTerminalExecForwardsOutputInputResizeAndDetach(t *testing.T) {
	saveTerminalExecGlobals(t)
	client := newFakeTerminalAttachment()
	reader := &dataReader{chunks: [][]byte{[]byte("hello")}}
	var stdout lockedBuffer
	c := execTestCommand(client, reader, &stdout)
	client.output <- []byte("replay")

	result := make(chan error, 1)
	go func() { result <- c.Run() }()
	eventually(t, func() bool {
		writes, _, _ := client.snapshot()
		return strings.Contains(stdout.String(), "replay") && len(writes) == 1
	})
	close(client.done)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	writes, resizes, detached := client.snapshot()
	if got := string(writes[0]); got != "hello" {
		t.Fatalf("input = %q", got)
	}
	if len(resizes) != 1 || resizes[0] != [2]int{80, 24} {
		t.Fatalf("initial resizes = %#v", resizes)
	}
	if detached != 1 {
		t.Fatalf("detach count = %d", detached)
	}
}

func TestTerminalExecCtrlGIsLocalDetach(t *testing.T) {
	saveTerminalExecGlobals(t)
	client := newFakeTerminalAttachment()
	c := execTestCommand(client, &dataReader{chunks: [][]byte{{'a', 'b', terminalDetachByte, 'c'}}}, io.Discard)
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	writes, _, detached := client.snapshot()
	if len(writes) != 1 || string(writes[0]) != "ab" {
		t.Fatalf("writes = %q; Ctrl-G or trailing bytes were forwarded", writes)
	}
	if detached != 1 {
		t.Fatalf("detach count = %d", detached)
	}
}

func TestTerminalExecRetriesBackpressureWithoutChangingData(t *testing.T) {
	saveTerminalExecGlobals(t)
	client := newFakeTerminalAttachment()
	client.writeErrs = []error{ErrTerminalInputBackpressure, ErrTerminalInputBackpressure, nil}
	c := execTestCommand(client, &dataReader{chunks: [][]byte{[]byte("same")}, err: io.EOF}, io.Discard)
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	writes, _, _ := client.snapshot()
	if len(writes) != 3 {
		t.Fatalf("write calls = %d", len(writes))
	}
	for i, got := range writes {
		if string(got) != "same" {
			t.Fatalf("retry %d = %q", i, got)
		}
	}
}

func TestTerminalExecDrainsOutputWhileInputIsBackpressured(t *testing.T) {
	saveTerminalExecGlobals(t)
	client := newFakeTerminalAttachment()
	var outputWritten atomic.Bool
	client.writeGate = &outputWritten
	client.firstWrite = make(chan struct{})
	writer := &gateWriter{gate: &outputWritten}
	c := execTestCommand(client, &dataReader{chunks: [][]byte{[]byte("input")}, err: io.EOF}, writer)
	result := make(chan error, 1)
	go func() { result <- c.Run() }()
	<-client.firstWrite
	client.output <- []byte("live while busy")
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	writes, _, _ := client.snapshot()
	if !outputWritten.Load() || len(writes) < 2 || string(writes[len(writes)-1]) != "input" {
		t.Fatalf("output drained=%v writes=%q", outputWritten.Load(), writes)
	}
}

func TestTerminalExecStopsOnContextAndClientCompletion(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(context.CancelFunc, *fakeTerminalAttachment)
	}{
		{"context cancellation", func(cancel context.CancelFunc, _ *fakeTerminalAttachment) { cancel() }},
		{"client completion", func(_ context.CancelFunc, client *fakeTerminalAttachment) { close(client.done) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saveTerminalExecGlobals(t)
			client := newFakeTerminalAttachment()
			ctx, cancel := context.WithCancel(context.Background())
			c := execTestCommand(client, &scriptedContextReader{}, io.Discard)
			c.ctx = ctx
			result := make(chan error, 1)
			go func() { result <- c.Run() }()
			tc.stop(cancel, client)
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(250 * time.Millisecond):
				t.Fatal("Run did not stop promptly")
			}
			_, _, detached := client.snapshot()
			if detached != 1 {
				t.Fatalf("detach count = %d", detached)
			}
		})
	}
}

func TestTerminalExecPollCancellationLeavesProcessStdinOpen(t *testing.T) {
	saveTerminalExecGlobals(t)
	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer inputWriter.Close()
	client := newFakeTerminalAttachment()
	ctx, cancel := context.WithCancel(context.Background())
	c := &terminalExecCommand{
		ctx: ctx, initialCols: 80, initialRows: 24,
		attach: func() (terminalAttachment, error) { return client, nil },
	}
	c.SetStdin(input)
	c.SetStdout(io.Discard)
	c.SetStderr(io.Discard)
	terminalResizeEvents = func() (<-chan os.Signal, func()) { return nil, func() {} }
	result := make(chan error, 1)
	go func() { result <- c.Run() }()
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("polling stdin did not observe cancellation")
	}
	if _, err := inputWriter.Write([]byte("x")); err != nil {
		t.Fatalf("process stdin was closed: %v", err)
	}
	one := make([]byte, 1)
	if _, err := input.Read(one); err != nil || string(one) != "x" {
		t.Fatalf("original stdin after Run = %q, %v", one, err)
	}
}

func TestTerminalExecPollingDuplicateIsCloseOnExec(t *testing.T) {
	saveTerminalExecGlobals(t)
	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer inputWriter.Close()
	reader, err := newContextReader(input)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	pollReader := reader.(*pollContextReader)
	flags, err := unix.FcntlInt(pollReader.file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("polling stdin duplicate can leak into a spawned PTY shell")
	}
}

func TestTerminalExecResizeSignalAndFailures(t *testing.T) {
	saveTerminalExecGlobals(t)
	client := newFakeTerminalAttachment()
	resize := make(chan os.Signal, 1)
	terminalIsTTY = func(uintptr) bool { return true }
	makeTerminalRaw = func(uintptr) (func() error, error) { return func() error { return nil }, nil }
	sizes := [][2]int{{80, 24}, {120, 40}}
	terminalGetSize = func(uintptr) (int, int, error) {
		s := sizes[0]
		sizes = sizes[1:]
		return s[0], s[1], nil
	}
	reader := &scriptedContextReader{}
	c := execTestCommand(client, reader, io.Discard)
	terminalResizeEvents = func() (<-chan os.Signal, func()) { return resize, func() {} }
	c.stdin = fakeFDReader{}
	result := make(chan error, 1)
	go func() { result <- c.Run() }()
	resize <- os.Interrupt
	eventually(t, func() bool { _, resizes, _ := client.snapshot(); return len(resizes) == 2 })
	close(client.done)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	_, resizes, _ := client.snapshot()
	if resizes[1] != [2]int{120, 40} {
		t.Fatalf("signal resize = %#v", resizes)
	}

	client = newFakeTerminalAttachment()
	client.resizeErr = errors.New("resize failed")
	c = execTestCommand(client, &scriptedContextReader{}, io.Discard)
	err := c.Run()
	_, _, detached := client.snapshot()
	if err == nil || !strings.Contains(err.Error(), "resize failed") || detached != 1 {
		t.Fatalf("resize failure = %v detach=%d", err, detached)
	}
}

func TestTerminalExecIOFailuresDetach(t *testing.T) {
	readFailure := errors.New("read failed")
	writeFailure := errors.New("write failed")
	for _, tc := range []struct {
		name   string
		reader contextReader
		out    io.Writer
		setup  func(*fakeTerminalAttachment)
		want   string
	}{
		{"input read", &dataReader{err: readFailure}, io.Discard, nil, "read failed"},
		{"input write", &dataReader{chunks: [][]byte{[]byte("x")}}, io.Discard, func(c *fakeTerminalAttachment) { c.writeErrs = []error{writeFailure} }, "write failed"},
		{"initial output", &scriptedContextReader{}, errorWriter{writeFailure}, nil, "write failed"},
		{"short output", &dataReader{err: io.EOF}, shortWriter{}, nil, "short write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saveTerminalExecGlobals(t)
			client := newFakeTerminalAttachment()
			if tc.setup != nil {
				tc.setup(client)
			}
			c := execTestCommand(client, tc.reader, tc.out)
			err := c.Run()
			_, _, detached := client.snapshot()
			if err == nil || !strings.Contains(err.Error(), tc.want) || detached != 1 {
				t.Fatalf("Run = %v detach=%d", err, detached)
			}
		})
	}
}

func TestTerminalExecSetupAndCleanupFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*terminalExecCommand, *fakeTerminalAttachment)
		want      string
		detach    int
	}{
		{"input setup", func(*terminalExecCommand, *fakeTerminalAttachment) {
			newContextReader = func(io.Reader) (contextReader, error) { return nil, errors.New("dup failed") }
		}, "dup failed", 0},
		{"attach", func(c *terminalExecCommand, _ *fakeTerminalAttachment) {
			c.attach = func() (terminalAttachment, error) { return nil, errors.New("attach failed") }
		}, "attach failed", 0},
		{"input close", func(_ *terminalExecCommand, _ *fakeTerminalAttachment) {}, "close failed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saveTerminalExecGlobals(t)
			client := newFakeTerminalAttachment()
			reader := &dataReader{chunks: [][]byte{{terminalDetachByte}}}
			if tc.name == "input close" {
				reader.closeErr = errors.New("close failed")
			}
			c := execTestCommand(client, reader, io.Discard)
			tc.configure(c, client)
			err := c.Run()
			_, _, detached := client.snapshot()
			if err == nil || !strings.Contains(err.Error(), tc.want) || detached != tc.detach {
				t.Fatalf("Run = %v detach=%d", err, detached)
			}
		})
	}
}

func TestTerminalExecRawAndResizeReadFailures(t *testing.T) {
	t.Run("enter raw", func(t *testing.T) {
		saveTerminalExecGlobals(t)
		client := newFakeTerminalAttachment()
		c := execTestCommand(client, &scriptedContextReader{}, io.Discard)
		c.stdin = fakeFDReader{}
		terminalIsTTY = func(uintptr) bool { return true }
		makeTerminalRaw = func(uintptr) (func() error, error) { return nil, errors.New("raw failed") }
		err := c.Run()
		_, _, detached := client.snapshot()
		if err == nil || !strings.Contains(err.Error(), "raw failed") || detached != 0 {
			t.Fatalf("Run = %v detach=%d", err, detached)
		}
	})

	t.Run("restore raw", func(t *testing.T) {
		saveTerminalExecGlobals(t)
		client := newFakeTerminalAttachment()
		c := execTestCommand(client, &dataReader{chunks: [][]byte{{terminalDetachByte}}}, io.Discard)
		c.stdin = fakeFDReader{}
		terminalIsTTY = func(uintptr) bool { return true }
		makeTerminalRaw = func(uintptr) (func() error, error) {
			return func() error { return errors.New("restore failed") }, nil
		}
		terminalGetSize = func(uintptr) (int, int, error) { return 80, 24, nil }
		err := c.Run()
		_, _, detached := client.snapshot()
		if err == nil || !strings.Contains(err.Error(), "restore failed") || detached != 1 {
			t.Fatalf("Run = %v detach=%d", err, detached)
		}
	})

	t.Run("SIGWINCH size read", func(t *testing.T) {
		saveTerminalExecGlobals(t)
		client := newFakeTerminalAttachment()
		resize := make(chan os.Signal, 1)
		c := execTestCommand(client, &scriptedContextReader{}, io.Discard)
		c.stdin = fakeFDReader{}
		terminalIsTTY = func(uintptr) bool { return true }
		makeTerminalRaw = func(uintptr) (func() error, error) { return func() error { return nil }, nil }
		calls := 0
		terminalGetSize = func(uintptr) (int, int, error) {
			calls++
			if calls == 1 {
				return 80, 24, nil
			}
			return 0, 0, errors.New("size failed")
		}
		terminalResizeEvents = func() (<-chan os.Signal, func()) { return resize, func() {} }
		result := make(chan error, 1)
		go func() { result <- c.Run() }()
		resize <- os.Interrupt
		err := <-result
		_, _, detached := client.snapshot()
		if err == nil || !strings.Contains(err.Error(), "size failed") || detached != 1 {
			t.Fatalf("Run = %v detach=%d", err, detached)
		}
	})
}

func TestTerminalExecLiveOutputFailureDetaches(t *testing.T) {
	saveTerminalExecGlobals(t)
	client := newFakeTerminalAttachment()
	writer := &failAfterWriter{remaining: 1, err: errors.New("live output failed")}
	c := execTestCommand(client, &scriptedContextReader{}, writer)
	client.output <- []byte("live")
	err := c.Run()
	_, _, detached := client.snapshot()
	if err == nil || !strings.Contains(err.Error(), "live output failed") || detached != 1 {
		t.Fatalf("Run = %v detach=%d", err, detached)
	}
}

func TestTerminalExecRestoresRawModeAndDetachesOnPanic(t *testing.T) {
	saveTerminalExecGlobals(t)
	client := newFakeTerminalAttachment()
	restored := false
	terminalIsTTY = func(uintptr) bool { return true }
	makeTerminalRaw = func(uintptr) (func() error, error) {
		return func() error { restored = true; return nil }, nil
	}
	terminalGetSize = func(uintptr) (int, int, error) { return 80, 24, nil }
	c := execTestCommand(client, &scriptedContextReader{}, panicWriter{})
	c.stdin = fakeFDReader{}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Run did not propagate output panic")
			}
		}()
		_ = c.Run()
	}()
	_, _, detached := client.snapshot()
	if !restored || detached != 1 {
		t.Fatalf("panic cleanup restored=%v detach=%d", restored, detached)
	}
}

func TestTerminalExecRestoresRawModeAcrossExitPaths(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reader     contextReader
		stdout     io.Writer
		configure  func(*terminalExecCommand, *fakeTerminalAttachment)
		stop       func(context.CancelFunc, *fakeTerminalAttachment)
		wantErr    string
		wantDetach int
	}{
		{
			name: "context cancellation", reader: &scriptedContextReader{},
			stop: func(cancel context.CancelFunc, _ *fakeTerminalAttachment) { cancel() }, wantDetach: 1,
		},
		{
			name: "client completion", reader: &scriptedContextReader{},
			stop: func(_ context.CancelFunc, client *fakeTerminalAttachment) { close(client.done) }, wantDetach: 1,
		},
		{name: "input failure", reader: &dataReader{err: errors.New("read failed")}, wantErr: "read failed", wantDetach: 1},
		{name: "output failure", reader: &scriptedContextReader{}, stdout: errorWriter{errors.New("write failed")}, wantErr: "write failed", wantDetach: 1},
		{
			name: "resize failure", reader: &scriptedContextReader{}, wantErr: "resize failed", wantDetach: 1,
			configure: func(_ *terminalExecCommand, client *fakeTerminalAttachment) {
				client.resizeErr = errors.New("resize failed")
			},
		},
		{
			name: "attach failure", reader: &scriptedContextReader{}, wantErr: "attach failed",
			configure: func(command *terminalExecCommand, _ *fakeTerminalAttachment) {
				command.attach = func() (terminalAttachment, error) { return nil, errors.New("attach failed") }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saveTerminalExecGlobals(t)
			client := newFakeTerminalAttachment()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stdout := tc.stdout
			if stdout == nil {
				stdout = io.Discard
			}
			command := execTestCommand(client, tc.reader, stdout)
			command.ctx = ctx
			command.stdin = fakeFDReader{}
			terminalIsTTY = func(uintptr) bool { return true }
			var restores atomic.Int32
			makeTerminalRaw = func(uintptr) (func() error, error) {
				return func() error { restores.Add(1); return nil }, nil
			}
			terminalGetSize = func(uintptr) (int, int, error) { return 80, 24, nil }
			if tc.configure != nil {
				tc.configure(command, client)
			}

			var err error
			if tc.stop == nil {
				err = command.Run()
			} else {
				result := make(chan error, 1)
				go func() { result <- command.Run() }()
				eventually(t, func() bool {
					_, resizes, _ := client.snapshot()
					return len(resizes) == 1
				})
				tc.stop(cancel, client)
				err = <-result
			}
			_, _, detached := client.snapshot()
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("Run = %v, want error containing %q", err, tc.wantErr)
			}
			if restores.Load() != 1 || detached != tc.wantDetach {
				t.Fatalf("cleanup restores=%d detach=%d, want 1/%d", restores.Load(), detached, tc.wantDetach)
			}
		})
	}
}

type fakeFDReader struct{}

func (fakeFDReader) Read([]byte) (int, error) { return 0, errTerminalInputIdle }
func (fakeFDReader) Fd() uintptr              { return 9 }

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) { panic("output panic") }

type failAfterWriter struct {
	remaining int
	err       error
}

type gateWriter struct{ gate *atomic.Bool }

func (w *gateWriter) Write(p []byte) (int, error) {
	// The first write is the local Ctrl-G banner. Only terminal output opens the
	// input gate, proving backpressure handling drains Output before retrying.
	if string(p) == "live while busy" {
		w.gate.Store(true)
	}
	return len(p), nil
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.remaining == 0 {
		return 0, w.err
	}
	w.remaining--
	return len(p), nil
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
