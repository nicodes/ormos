//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"errors"

	"github.com/nicodes/ormos/relay"
)

// prepareResume publishes replay, writer membership and connection admission
// atomically against PTY output and input progress. The only nested order is
// session mu -> inputMu. Neither lock performs network I/O.
func (s *terminalSession) prepareResume(conn *sealedConn, hello relay.ResumeHello) (*terminalResumeWriter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrTerminalClientClosed
	}
	if !conn.resume {
		return nil, errTerminalInputConflict
	}
	if len(s.conns) >= maxTerminalConns {
		return nil, errTerminalWriterCapacity
	}
	offset := hello.OutputOffset
	if s.replay.positionLost {
		return nil, errTerminalReplayGap
	}
	if hello.Initial {
		offset = s.replay.total - uint64(s.replay.size)
	}
	replay, err := s.replay.from(offset)
	if err != nil {
		return nil, err
	}
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	if s.inputClosed {
		return nil, ErrTerminalClientClosed
	}
	writer, err := s.resumeWriters.acquire(hello, conn)
	if err != nil {
		return nil, err
	}
	ready, err := relay.EncodeResumeReady(writer.input.ready(offset, hello.Initial))
	if err != nil {
		writer.release(conn)
		return nil, err
	}
	data, err := relay.EncodeSequencedData(offset, replay)
	if err != nil {
		writer.release(conn)
		return nil, err
	}
	if !conn.enqueue(ready) || !conn.enqueue(data) || !conn.enqueue(relay.EncodeActivity(s.active)) {
		writer.release(conn)
		return nil, ErrTerminalClientClosed
	}
	conn.outputConfirmed, conn.outputQueued = offset, s.replay.total
	// The next progress notification must not emit an ACK older than ready.
	conn.inputAck.Store(writer.input.written)
	if s.conns == nil {
		s.conns = make(map[terminalConn]struct{})
	}
	s.conns[conn] = struct{}{}
	if s.expires != nil {
		s.expires.Stop()
		s.expires = nil
	}
	return writer, nil
}

func (s *terminalSession) attachResume(conn *sealedConn, hello relay.ResumeHello) {
	writer, err := s.prepareResume(conn, hello)
	if err != nil {
		s.rejectResume(conn, err)
		return
	}
	defer func() {
		s.inputMu.Lock()
		writer.release(conn)
		s.inputMu.Unlock()
		s.detach(conn)
	}()
	for {
		frame, err := conn.stream.ReadResumeFrame()
		if err != nil {
			return
		}
		switch {
		case frame.Data != nil:
			if !s.waitResumeInput(writer, conn, *frame.Data) {
				return
			}
		case frame.ClientAck != nil:
			if !s.confirmResume(writer, conn, *frame.ClientAck) {
				return
			}
		case frame.Resize != nil:
			if !relay.ValidTerminalSize(frame.Resize.Cols, frame.Resize.Rows) {
				return
			}
			if err := setPTYSize(s.ptmx, frame.Resize.Cols, frame.Resize.Rows); err != nil {
				return
			}
		default:
			// Hello/ready/gap/ACK/activity are not client input messages here.
			return
		}
	}
}

func (s *terminalSession) rejectResume(conn *sealedConn, err error) {
	reason := relay.ReplayInputConflict
	switch {
	case errors.Is(err, errTerminalReplayGap):
		reason = relay.ReplayOutputExpired
	case errors.Is(err, errTerminalWriterUnknown):
		reason = relay.ReplayWriterUnknown
	case errors.Is(err, errTerminalWriterCapacity):
		reason = relay.ReplayCapacity
	case errors.Is(err, ErrTerminalClientClosed):
		conn.kill()
		return
	}
	s.mu.Lock()
	available := s.replay.total - uint64(s.replay.size)
	s.mu.Unlock()
	frame, encodeErr := relay.EncodeReplayGap(relay.ReplayGap{Reason: reason, AvailableFrom: available})
	if encodeErr != nil || !conn.enqueue(frame) {
		conn.kill()
	}
	// writeLoop delivers the first gap under its write deadline, then closes.
	// Wait before the stream handler's deferred Close can truncate the reply.
	<-conn.dead
}

func (s *terminalSession) waitResumeInput(writer *terminalResumeWriter, conn *sealedConn, data relay.SequencedData) bool {
	s.startInput()
	for {
		select {
		case <-conn.dead:
			return false
		case <-s.done:
			return false
		default:
		}
		s.inputMu.Lock()
		duplicate, err := s.submitResumeInputLocked(writer, data.Offset, data.Data)
		if err == nil {
			if duplicate {
				conn.acknowledgeInput(writer.input.written)
			}
			s.inputMu.Unlock()
			return true
		}
		if !errors.Is(err, ErrTerminalInputBackpressure) {
			s.inputMu.Unlock()
			return false
		}
		if s.inputCapacity == nil {
			s.inputCapacity = make(chan struct{})
		}
		capacity := s.inputCapacity
		s.inputMu.Unlock()
		select {
		case <-capacity:
		case <-conn.dead:
			return false
		case <-s.done:
			return false
		}
	}
}

func (s *terminalSession) confirmResume(writer *terminalResumeWriter, conn *sealedConn, ack relay.ResumeClientAck) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ack.OutputOffset < conn.outputConfirmed || ack.OutputOffset > conn.outputQueued {
		return false
	}
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	if ack.InputConfirmed > writer.input.written {
		return false
	}
	if ack.InputConfirmed > writer.input.confirmed {
		if err := writer.input.confirm(ack.InputConfirmed); err != nil {
			return false
		}
	}
	conn.outputConfirmed = ack.OutputOffset
	return true
}
