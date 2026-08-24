//go:build linux && !android

package system

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/nicodes/ormos/relay"
	"golang.org/x/sys/unix"
)

const terminalInputSchedulerAllowance = 100 * time.Millisecond

// This is the kernel-level shutdown/progress case: the slave is raw and does
// not echo, the nonblocking master is filled to EAGAIN, and no shell drains it.
// Detach gets one 25ms POLLOUT window plus 100ms of explicit scheduler allowance
// to release the active 1MiB local request; the session itself must stay alive.
func TestRealPTYDetachReleasesMaximalLocalInputAndBrowserProgresses(t *testing.T) {
	ptmx, tty := rawNonblockingPTYPair(t)
	fillPTYToEAGAIN(t, ptmx)
	s := newInputTestSession(ptmx)
	localConn := newLocalTerminalConn()
	if err := s.attachConn(localConn); err != nil {
		t.Fatal(err)
	}
	local := &TerminalClient{session: s, conn: localConn}
	admitted := make(chan error, 1)
	go func() { admitted <- local.Write(make([]byte, terminalInputBytes)) }()
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatalf("admit maximal local request: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("maximal local admission blocked on a full PTY")
	}
	waitSessionInput(t, s, 1, 0, time.Second)

	local.Detach()
	waitInputAccounting(t, s, localConn, 0, 0, terminalInputPollWindow+terminalInputSchedulerAllowance)
	select {
	case <-s.done:
		t.Fatal("local detach closed the shared terminal session")
	default:
	}

	browser, browserClient := sealedPair(t, "real-pty-detach")
	go s.attach(browser)
	if _, err := browserClient.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if _, err := browserClient.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	marker := []byte("[[browser-after-detach-7d3f]]")
	if err := browserClient.WriteFrame(relay.EncodeData(marker)); err != nil {
		t.Fatal(err)
	}
	readPTYUntil(t, tty, marker, 2*time.Second)
	waitSessionInput(t, s, 0, 0, time.Second)
	browser.kill()
	s.close()
}

// A request is returned to the queue tail after no more than 4096 bytes. The
// permit-driven 512-byte slave reads provide capacity without sleep timing: the
// browser marker admitted behind the active local request must therefore occur
// after its prefix but no more than one exact fairness quantum into it, and
// before its unique tail.
func TestRealPTYInputFairnessAt4096ByteQuantum(t *testing.T) {
	ptmx, tty := rawNonblockingPTYPair(t)
	fillPTYToEAGAIN(t, ptmx)
	s := newInputTestSession(ptmx)
	localConn := newLocalTerminalConn()
	local := &TerminalClient{session: s, conn: localConn}
	localBegin := []byte("[[local-begin-29ac]]")
	localTail := []byte("[[local-tail-e841]]")
	localInput := append(append(append([]byte(nil), localBegin...), bytes.Repeat([]byte{'L'}, 64<<10)...), localTail...)
	if err := local.Write(localInput); err != nil {
		t.Fatal(err)
	}
	waitSessionInput(t, s, 1, 0, time.Second)

	browser, browserClient := sealedPair(t, "real-pty-fairness")
	go s.attach(browser)
	if _, err := browserClient.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if _, err := browserClient.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	browserMarker := []byte("[[browser-fair-54b2]]")
	if err := browserClient.WriteFrame(relay.EncodeData(browserMarker)); err != nil {
		t.Fatal(err)
	}
	waitSessionInput(t, s, 2, 1, time.Second)

	permits := make(chan struct{})
	chunks := make(chan []byte)
	go func() {
		buf := make([]byte, 512)
		for range permits {
			n, err := tty.Read(buf)
			if err != nil {
				close(chunks)
				return
			}
			chunks <- append([]byte(nil), buf[:n]...)
		}
	}()
	defer close(permits)
	deadline := time.Now().Add(3 * time.Second)
	var seen []byte
	for !bytes.Contains(seen, localTail) {
		select {
		case permits <- struct{}{}:
		case <-time.After(time.Until(deadline)):
			t.Fatal("deadline granting controlled PTY drain permit")
		}
		select {
		case chunk, ok := <-chunks:
			if !ok {
				t.Fatal("PTY slave read ended before local tail")
			}
			seen = append(seen, chunk...)
		case <-time.After(time.Until(deadline)):
			t.Fatal("deadline draining PTY input")
		}
	}
	beginAt := bytes.Index(seen, localBegin)
	browserAt := bytes.Index(seen, browserMarker)
	tailAt := bytes.Index(seen, localTail)
	if beginAt < 0 || browserAt < 0 || tailAt < 0 {
		t.Fatalf("missing delimiters: begin=%d browser=%d tail=%d", beginAt, browserAt, tailAt)
	}
	if browserAt <= beginAt || browserAt >= tailAt {
		t.Fatalf("browser marker order begin/browser/tail = %d/%d/%d", beginAt, browserAt, tailAt)
	}
	if beforeBrowser := browserAt - beginAt; beforeBrowser > 4<<10 {
		t.Fatalf("browser marker followed %d local bytes, exceeds exact 4096-byte quantum", beforeBrowser)
	}
	waitInputAccounting(t, s, localConn, 0, 0, time.Second)
	browser.kill()
	localConn.kill()
	s.close()
}

func rawNonblockingPTYPair(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no PTY available: %v", err)
	}
	termios, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	raw := *termios
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(int(tty.Fd()), unix.TCSETS, &raw); err != nil {
		t.Fatal(err)
	}
	ptmx, err = normalizePTY(ptmx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tty.Close(); _ = ptmx.Close() })
	return ptmx, tty
}

func fillPTYToEAGAIN(t *testing.T, ptmx *os.File) int {
	t.Helper()
	total := 0
	block := bytes.Repeat([]byte{'F'}, 4<<10)
	for {
		n, err := ptyWrite(ptmx, block)
		total += n
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			if total == 0 {
				t.Fatal("PTY reached EAGAIN before accepting fill data")
			}
			return total
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func newInputTestSession(ptmx *os.File) *terminalSession {
	s := &terminalSession{ptmx: ptmx, conns: make(map[terminalConn]struct{}), input: make(chan *terminalInput, terminalInputQueue), done: make(chan struct{})}
	s.startInput()
	return s
}

func readPTYUntil(t *testing.T, tty *os.File, marker []byte, timeout time.Duration) {
	t.Helper()
	chunks := make(chan []byte)
	go func() {
		buf := make([]byte, 4<<10)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				chunks <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				close(chunks)
				return
			}
		}
	}()
	deadline := time.After(timeout)
	var seen []byte
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				t.Fatal("PTY slave closed before browser marker")
			}
			seen = append(seen, chunk...)
			if bytes.Contains(seen, marker) {
				return
			}
		case <-deadline:
			t.Fatalf("browser marker not observed within %s", timeout)
		}
	}
}
