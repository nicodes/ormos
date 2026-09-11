//go:build linux && !android

package system

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/nicodes/ormos/relay"
	"golang.org/x/sys/unix"
)

const terminalInputSchedulerAllowance = 100 * time.Millisecond

func TestSealedResumedAttachmentAcknowledgesActualPTYInputWithoutRetryDuplication(t *testing.T) {
	ptmx, tty := rawNonblockingPTYPair(t)
	s := newInputTestSession(ptmx)
	agent, client := sealedPairMode(t, "actual-pty-ack", true)
	done := make(chan struct{})
	go func() { s.attachResume(agent, relay.ResumeHello{Writer: [16]byte{1}, Initial: true}); close(done) }()
	t.Cleanup(func() {
		agent.kill()
		s.close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("resumed attachment reader leaked")
		}
	})
	readResumeInitial(t, client)
	for _, step := range []struct {
		offset uint64
		data   string
		ack    uint64
	}{
		{0, "abc", 3}, {0, "abc", 3}, {3, "def", 6},
	} {
		frame, err := relay.EncodeSequencedData(step.offset, []byte(step.data))
		if err != nil {
			t.Fatal(err)
		}
		if err := client.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
		ack, err := client.ReadResumeFrame()
		if err != nil || ack.InputAck == nil || *ack.InputAck != step.ack {
			t.Fatal("PTY-written acknowledgment incorrect", err)
		}
	}
	result := make(chan error, 1)
	go func() {
		data := make([]byte, 6)
		_, err := io.ReadFull(tty, data)
		if err == nil && string(data) != "abcdef" {
			err = errors.New("sealed retry duplicated PTY input")
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("ACK preceded PTY delivery")
	}
	if err := client.WriteFrame(relay.EncodeClientAck(relay.ResumeClientAck{InputConfirmed: 6})); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		s.inputMu.Lock()
		writer := s.resumeWriters[0]
		confirmed := writer.input.confirmed == 6 && len(writer.input.receipts) == 0
		s.inputMu.Unlock()
		if confirmed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("client confirmation did not release receipts")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestResumedPTYInputAcknowledgesWritesAndDeduplicatesReplacement(t *testing.T) {
	ptmx, tty := rawNonblockingPTYPair(t)
	s := &terminalSession{ptmx: ptmx, done: make(chan struct{}), input: make(chan *terminalInput, terminalSchedulerQueue)}
	// Drive the real writer deterministically rather than racing the scheduler
	// to observe the queued-but-not-written state.
	s.inputOnce.Do(func() {})
	oldConn := &sealedConn{}
	hello := relay.ResumeHello{Writer: [16]byte{1}, Initial: true}
	writer, err := s.resumeWriters.acquire(hello, oldConn)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := s.submitResumeInput(writer, 0, []byte("abcd")); err != nil || duplicate {
		t.Fatal(err)
	}
	if duplicate, err := s.submitResumeInput(writer, 4, []byte("ef")); err != nil || duplicate {
		t.Fatal(err)
	}
	if writer.input.written != 0 || writer.input.accepted != 6 {
		t.Fatal("queued input was acknowledged")
	}
	writer.release(oldConn)
	hello.Initial = false
	if got, err := s.resumeWriters.acquire(hello, &sealedConn{}); err != nil || got != writer {
		t.Fatal("replacement lost accepted bytes", err)
	}
	if duplicate, err := s.submitResumeInput(writer, 0, []byte("abcd")); err != nil || !duplicate {
		t.Fatal("queued retry was not deduplicated", err)
	}
	if s.inputCalls != 2 {
		t.Fatal("retry allocated another scheduler slot")
	}
	first, second := <-s.input, <-s.input
	if complete, err := s.writeInputQuantum(second); complete || err != nil || second.off != 0 {
		t.Fatal("later frame overtook queued predecessor", err)
	}
	// Exercise an actual two-byte PTY write, then the same request's remainder.
	whole := first.p
	first.p = first.p[:2]
	if _, err := s.writeInputQuantum(first); err != nil {
		t.Fatal(err)
	}
	first.p = whole
	if writer.input.written != 2 {
		t.Fatal("partial write progress was not exact")
	}
	if duplicate, err := s.submitResumeInput(writer, 0, []byte("abcd")); err != nil || !duplicate {
		t.Fatal("partial-write retry duplicated bytes", err)
	}
	if complete, err := s.writeInputQuantum(second); complete || err != nil || second.off != 0 {
		t.Fatal("later frame overtook partial predecessor", err)
	}
	if complete, err := s.writeInputQuantum(first); !complete || err != nil {
		t.Fatal(err)
	}
	if complete, err := s.writeInputQuantum(second); !complete || err != nil {
		t.Fatal(err)
	}
	s.releaseInput(first)
	s.releaseInput(second)
	if writer.input.written != 6 || writer.input.confirmed != 0 || writer.reclaimable() {
		t.Fatal("write was mistaken for client confirmation")
	}
	result := make(chan error, 1)
	go func() {
		data := make([]byte, 6)
		_, err := io.ReadFull(tty, data)
		if err == nil && string(data) != "abcdef" {
			err = errors.New("PTY received reordered or duplicate bytes")
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("PTY did not receive admitted input")
	}
	if s.inputCalls != 0 || s.inputBytes != 0 || len(s.input) != 0 {
		t.Fatal("scheduler accounting leaked")
	}
}

func TestResumedInputAdmissionRejectsWithoutAdvancingCursor(t *testing.T) {
	s := &terminalSession{done: make(chan struct{}), input: make(chan *terminalInput, terminalSchedulerQueue)}
	s.inputOnce.Do(func() {})
	writer, err := s.resumeWriters.acquire(relay.ResumeHello{Writer: [16]byte{1}, Initial: true}, &sealedConn{})
	if err != nil {
		t.Fatal(err)
	}
	s.browserInputCalls = terminalBrowserInputQueue
	if _, err := s.submitResumeInput(writer, 0, []byte("x")); !errors.Is(err, ErrTerminalInputBackpressure) {
		t.Fatal(err)
	}
	if writer.input.accepted != 0 || len(writer.input.receipts) != 0 || len(s.input) != 0 {
		t.Fatal("rejected frame mutated acceptance")
	}
	if _, err := s.submitResumeInput(&terminalResumeWriter{}, 0, []byte("x")); !errors.Is(err, errTerminalWriterUnknown) {
		t.Fatal("unretained writer admitted", err)
	}
	s.inputClosed = true
	if _, err := s.submitResumeInput(writer, 0, []byte("x")); !errors.Is(err, ErrTerminalClientClosed) {
		t.Fatal("closed scheduler admitted input", err)
	}
}

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
	localInput := append(append(append([]byte(nil), localBegin...), bytes.Repeat([]byte{'L'}, terminalInputBytes-len(localBegin)-len(localTail))...), localTail...)
	if len(localInput) != terminalInputBytes {
		t.Fatalf("local fixture = %d bytes, want exact %d-byte bound", len(localInput), terminalInputBytes)
	}
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
	s := &terminalSession{ptmx: ptmx, conns: make(map[terminalConn]struct{}), done: make(chan struct{})}
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
