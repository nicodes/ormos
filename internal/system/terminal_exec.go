//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	term "github.com/charmbracelet/x/term"
	"github.com/nicodes/ormos/relay"
	"golang.org/x/sys/unix"
)

const (
	terminalInputPollInterval = 10 * time.Millisecond
	terminalInputRetryWait    = 10 * time.Millisecond
	terminalInputRetryLimit   = 2 * time.Second
	terminalDetachByte        = byte(0x07) // Ctrl-G; local detach, never PTY input.
)

var errTerminalInputIdle = errors.New("terminal input is not ready")

type terminalAttachment interface {
	Output() <-chan []byte
	Done() <-chan struct{}
	Write([]byte) error
	Resize(int, int) error
	Detach()
}

type contextReader interface {
	Read(context.Context, []byte) (int, error)
	Close() error
}

type terminalFD interface{ Fd() uintptr }

var (
	makeTerminalRaw = func(fd uintptr) (func() error, error) {
		state, err := term.MakeRaw(fd)
		if err != nil {
			return nil, err
		}
		return func() error { return term.Restore(fd, state) }, nil
	}
	terminalIsTTY    = term.IsTerminal
	terminalGetSize  = term.GetSize
	newContextReader = func(r io.Reader) (contextReader, error) {
		if f, ok := r.(*os.File); ok {
			// The duplicate exists only to make this Run's reads cancellable. It
			// must not cross the pty.Start exec performed by attach (or any other
			// concurrent terminal spawn), where a shell could retain access to the
			// dashboard's TTY after this attachment returns.
			fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
			if err != nil {
				return nil, err
			}
			return &pollContextReader{file: os.NewFile(uintptr(fd), "ormos-terminal-input")}, nil
		}
		return &directContextReader{reader: r}, nil
	}
	terminalResizeEvents = func() (<-chan os.Signal, func()) {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGWINCH)
		return ch, func() { signal.Stop(ch) }
	}
)

// terminalExecCommand is a Bubble Tea ExecCommand. Bubble Tea calls Run only
// after releasing its input reader and restoring cooked mode; Run then owns raw
// mode until its local defers restore it. Ctrl-G is the documented local detach
// sequence and is not forwarded to the shared PTY.
type terminalExecCommand struct {
	ctx                      context.Context
	attach                   func() (terminalAttachment, error)
	projectRoot, terminalKey string
	initialCols, initialRows int
	stdin                    io.Reader
	stdout, stderr           io.Writer
}

func newTerminalExecCommand(ctx context.Context, d *system, projectRoot, terminalKey string, cols, rows int) *terminalExecCommand {
	return &terminalExecCommand{
		ctx: ctx, projectRoot: projectRoot, terminalKey: terminalKey,
		initialCols: cols, initialRows: rows,
		attach: func() (terminalAttachment, error) {
			return d.AttachTerminal(projectRoot, terminalKey, cols, rows)
		},
	}
}

func (c *terminalExecCommand) SetStdin(r io.Reader)  { c.stdin = r }
func (c *terminalExecCommand) SetStdout(w io.Writer) { c.stdout = w }
func (c *terminalExecCommand) SetStderr(w io.Writer) { c.stderr = w }

func (c *terminalExecCommand) Run() (runErr error) {
	if c.ctx == nil {
		c.ctx = context.Background()
	}
	if c.stdin == nil || c.stdout == nil || c.stderr == nil {
		return fmt.Errorf("terminal streams are not configured")
	}

	reader, err := newContextReader(c.stdin)
	if err != nil {
		return fmt.Errorf("prepare terminal input: %w", err)
	}
	defer func() {
		if err := reader.Close(); runErr == nil && err != nil {
			runErr = fmt.Errorf("close terminal input: %w", err)
		}
	}()

	var inputFD uintptr
	hasInputFD := false
	if fd, ok := c.stdin.(terminalFD); ok && terminalIsTTY(fd.Fd()) {
		inputFD = fd.Fd()
		hasInputFD = true
		restore, err := makeTerminalRaw(inputFD)
		if err != nil {
			return fmt.Errorf("enter terminal raw mode: %w", err)
		}
		// Installed immediately after MakeRaw so it runs on every return and panic,
		// before Bubble Tea gets control back and restores its own terminal state.
		defer func() {
			if err := restore(); runErr == nil && err != nil {
				runErr = fmt.Errorf("restore terminal mode: %w", err)
			}
		}()
	}

	client, err := c.attach()
	if err != nil {
		return fmt.Errorf("attach terminal: %w", err)
	}
	defer client.Detach()

	cols, rows := c.initialCols, c.initialRows
	if hasInputFD {
		if w, h, sizeErr := terminalGetSize(inputFD); sizeErr == nil && relay.ValidTerminalSize(w, h) {
			cols, rows = w, h
		}
	}
	if !relay.ValidTerminalSize(cols, rows) {
		return fmt.Errorf("invalid terminal size")
	}
	if err := client.Resize(cols, rows); err != nil {
		return fmt.Errorf("resize terminal: %w", err)
	}

	if err := writeTerminalOutput(c.stdout, []byte("\r\n[ormos terminal; Ctrl-G detaches]\r\n")); err != nil {
		return fmt.Errorf("write terminal output: %w", err)
	}

	resizeCh, stopResize := terminalResizeEvents()
	defer stopResize()
	input := make([]byte, 32<<10)
	for {
		select {
		case <-c.ctx.Done():
			return nil
		case <-client.Done():
			return nil
		case out := <-client.Output():
			if err := writeTerminalOutput(c.stdout, out); err != nil {
				return fmt.Errorf("write terminal output: %w", err)
			}
		case <-resizeCh:
			if !hasInputFD {
				continue
			}
			w, h, err := terminalGetSize(inputFD)
			if err != nil {
				return fmt.Errorf("read terminal size: %w", err)
			}
			if !relay.ValidTerminalSize(w, h) {
				continue
			}
			if err := client.Resize(w, h); err != nil {
				return fmt.Errorf("resize terminal: %w", err)
			}
		default:
			n, readErr := reader.Read(c.ctx, input)
			if n > 0 {
				chunk := input[:n]
				if detachAt := bytesIndexByte(chunk, terminalDetachByte); detachAt >= 0 {
					if detachAt > 0 {
						if err := writeTerminalInput(c.ctx, client, c.stdout, chunk[:detachAt]); err != nil {
							return err
						}
					}
					return nil
				}
				if err := writeTerminalInput(c.ctx, client, c.stdout, chunk); err != nil {
					return err
				}
			}
			switch {
			case readErr == nil, errors.Is(readErr, errTerminalInputIdle):
			case errors.Is(readErr, io.EOF):
				return nil
			case c.ctx.Err() != nil && errors.Is(readErr, c.ctx.Err()):
				return nil
			default:
				return fmt.Errorf("read terminal input: %w", readErr)
			}
		}
	}
}

func writeTerminalOutput(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err == nil && n != len(p) {
		return io.ErrShortWrite
	}
	return err
}

func writeTerminalInput(ctx context.Context, client terminalAttachment, stdout io.Writer, p []byte) error {
	deadline := time.NewTimer(terminalInputRetryLimit)
	defer deadline.Stop()
	for {
		err := client.Write(p)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrTerminalInputBackpressure) {
			return fmt.Errorf("write terminal input: %w", err)
		}
		wait := time.NewTimer(terminalInputRetryWait)
		select {
		case <-ctx.Done():
			stopTimer(wait)
			return nil
		case <-client.Done():
			stopTimer(wait)
			return nil
		case <-deadline.C:
			stopTimer(wait)
			return fmt.Errorf("terminal input remained busy for %s", terminalInputRetryLimit)
		case out := <-client.Output():
			stopTimer(wait)
			if err := writeTerminalOutput(stdout, out); err != nil {
				return fmt.Errorf("write terminal output: %w", err)
			}
		case <-wait.C:
		}
	}
}

func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

func bytesIndexByte(p []byte, target byte) int {
	for i, b := range p {
		if b == target {
			return i
		}
	}
	return -1
}

type pollContextReader struct{ file *os.File }

func (r *pollContextReader) Read(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	fds := []unix.PollFd{{Fd: int32(r.file.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, int(terminalInputPollInterval/time.Millisecond))
	if err != nil {
		if errors.Is(err, unix.EINTR) {
			return 0, errTerminalInputIdle
		}
		return 0, err
	}
	if n == 0 {
		return 0, errTerminalInputIdle
	}
	if fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
		return 0, fmt.Errorf("terminal input poll failed")
	}
	return r.file.Read(p)
}

func (r *pollContextReader) Close() error { return r.file.Close() }

type directContextReader struct{ reader io.Reader }

func (r *directContextReader) Read(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
func (*directContextReader) Close() error { return nil }
