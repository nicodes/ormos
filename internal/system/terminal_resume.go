//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"crypto/sha256"
	"errors"

	"github.com/nicodes/ormos/relay"
)

var errTerminalReplayGap = errors.New("terminal output is outside the retained replay window")
var errTerminalInputConflict = errors.New("terminal input position or contents conflict with accepted input")
var errTerminalWriterUnknown = errors.New("terminal input writer is no longer retained")
var errTerminalWriterCapacity = errors.New("terminal resumable writer capacity exhausted")

const maxTerminalResumeWriters = 4

// The session inputMu protects the registry, including connections and cursors.
// Fixed arrays bound both retained identities and overlapping transports. A
// disconnected writer is not disposable merely because its PTY write finished:
// the client must also have confirmed that progress.
type terminalResumeWriters [maxTerminalResumeWriters]*terminalResumeWriter

type terminalResumeWriter struct {
	id          [16]byte
	input       terminalResumeInput
	connections [maxTerminalConns]*sealedConn
}

func (w *terminalResumeWriter) reclaimable() bool {
	for _, conn := range w.connections {
		if conn != nil {
			return false
		}
	}
	return w.input.accepted == w.input.written && w.input.written == w.input.confirmed
}

// acquire must run only after output replay/admission validation succeeds. A
// failed admission leaves both existing connections and retained state intact.
func (writers *terminalResumeWriters) acquire(hello relay.ResumeHello, conn *sealedConn) (*terminalResumeWriter, error) {
	if conn == nil || hello.Writer == ([16]byte{}) {
		return nil, errTerminalInputConflict
	}
	for _, writer := range writers {
		if writer == nil || writer.id != hello.Writer {
			continue
		}
		if hello.Initial || hello.InputConfirmed > writer.input.written {
			return nil, errTerminalInputConflict
		}
		slot := -1
		for i, attached := range writer.connections {
			if attached == conn {
				return nil, errTerminalInputConflict
			}
			if attached == nil && slot == -1 {
				slot = i
			}
		}
		if slot == -1 {
			return nil, errTerminalWriterCapacity
		}
		// An overlapping older transport may have observed an earlier ACK.
		// Never let its confirmation regress the retained monotonic cursor.
		if hello.InputConfirmed > writer.input.confirmed {
			if err := writer.input.confirm(hello.InputConfirmed); err != nil {
				return nil, err
			}
		}
		writer.connections[slot] = conn
		return writer, nil
	}
	if !hello.Initial {
		return nil, errTerminalWriterUnknown
	}
	if hello.InputConfirmed != 0 || hello.OutputOffset != 0 {
		return nil, errTerminalInputConflict
	}
	slot := -1
	for i, writer := range writers {
		if writer == nil {
			slot = i
			break
		}
		if slot == -1 && writer.reclaimable() {
			slot = i
		}
	}
	if slot == -1 {
		return nil, errTerminalWriterCapacity
	}
	writer := &terminalResumeWriter{id: hello.Writer}
	writer.connections[0] = conn
	writers[slot] = writer
	return writer, nil
}

func (w *terminalResumeWriter) release(conn *sealedConn) {
	for i, attached := range w.connections {
		if attached == conn {
			w.connections[i] = nil
		}
	}
}

// submitResumeInput admits at most one bounded frame without waiting. A caller
// may wait on inputCapacity after backpressure, but must keep consuming client
// confirmations when the receipt budget is full. Transport loss does not cancel
// accepted bytes: the retained writer, not that transport, owns their delivery.
func (s *terminalSession) submitResumeInput(writer *terminalResumeWriter, offset uint64, data []byte) (duplicate bool, err error) {
	s.startInput()
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	return s.submitResumeInputLocked(writer, offset, data)
}

func (s *terminalSession) submitResumeInputLocked(writer *terminalResumeWriter, offset uint64, data []byte) (duplicate bool, err error) {
	if s.inputClosed {
		return false, ErrTerminalClientClosed
	}
	retained := false
	for _, candidate := range s.resumeWriters {
		if candidate != nil && candidate == writer {
			retained = true
			break
		}
	}
	if !retained {
		return false, errTerminalWriterUnknown
	}
	duplicate, err = writer.input.check(offset, data)
	if err != nil || duplicate {
		return duplicate, err
	}
	in := &terminalInput{p: append([]byte(nil), data...), resume: writer, resumeOffset: offset}
	if !s.submitInputLocked(in) {
		return false, ErrTerminalInputBackpressure
	}
	// The scheduler checks this same inputMu before writing, so acceptance and
	// queue publication form one transaction from its perspective.
	if err := writer.input.accept(offset, data); err != nil {
		return false, err
	}
	return false, nil
}

// All ring access remains protected by terminalSession.mu. A resume never
// manufactures missing history or silently resets an already-running renderer.
func (r *replayRing) from(offset uint64) ([]byte, error) {
	if r.positionLost || offset > r.total || r.total < uint64(r.size) || offset < r.total-uint64(r.size) {
		return nil, errTerminalReplayGap
	}
	count := int(r.total - offset)
	if count == 0 {
		return []byte{}, nil
	}
	start := (r.start + r.size - count) % len(r.buf)
	result := make([]byte, count)
	n := copy(result, r.buf[start:])
	copy(result[n:], r.buf[:count-n])
	return result, nil
}

type inputReceipt struct {
	end    uint64
	digest [32]byte
}

// One logical browser writer survives transport replacement. Accepted tracks
// bytes reserved in the existing bounded scheduler; written advances only after
// successful PTY writes. The client retains whole frames until fully ACKed.
// terminalSession.inputMu protects this state alongside scheduler admission.
type terminalResumeInput struct {
	accepted  uint64
	written   uint64
	confirmed uint64
	receipts  map[uint64]inputReceipt
}

func (s *terminalResumeInput) check(offset uint64, data []byte) (duplicate bool, err error) {
	if len(data) == 0 || len(data) > terminalInputQuantum || uint64(len(data)) > ^uint64(0)-offset {
		return false, errTerminalInputConflict
	}
	if offset < s.accepted {
		receipt, ok := s.receipts[offset]
		if !ok || receipt.end != offset+uint64(len(data)) || receipt.digest != sha256.Sum256(data) {
			return false, errTerminalInputConflict
		}
		return true, nil
	}
	if offset != s.accepted || len(s.receipts) >= terminalBrowserInputQueue {
		return false, errTerminalInputConflict
	}
	return false, nil
}

// accept is called only after the scheduler successfully reserves this frame.
func (s *terminalResumeInput) accept(offset uint64, data []byte) error {
	duplicate, err := s.check(offset, data)
	if err != nil {
		return err
	}
	if duplicate {
		return nil
	}
	if s.receipts == nil {
		s.receipts = make(map[uint64]inputReceipt)
	}
	s.accepted += uint64(len(data))
	s.receipts[offset] = inputReceipt{end: s.accepted, digest: sha256.Sum256(data)}
	return nil
}

func (s *terminalResumeInput) commit(offset uint64, written int) error {
	if written < 0 || offset != s.written || uint64(written) > s.accepted-s.written {
		return errTerminalInputConflict
	}
	s.written += uint64(written)
	return nil
}

func (s *terminalResumeInput) confirm(offset uint64) error {
	if offset < s.confirmed || offset > s.written {
		return errTerminalInputConflict
	}
	s.confirmed = offset
	for start, receipt := range s.receipts {
		if receipt.end <= offset {
			delete(s.receipts, start)
		}
	}
	return nil
}

func (s *terminalResumeInput) ready(output uint64, initial bool) relay.ResumeReady {
	return relay.ResumeReady{InputAccepted: s.accepted, InputWritten: s.written, OutputOffset: output, Initial: initial}
}
