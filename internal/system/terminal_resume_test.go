//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/nicodes/ormos/relay"
)

func readResumeInitial(t *testing.T, client *relay.SealedStream) (relay.ResumeReady, relay.SequencedData) {
	t.Helper()
	ready, err := client.ReadResumeFrame()
	if err != nil || ready.Ready == nil {
		t.Fatal("missing ready", err)
	}
	data, err := client.ReadResumeFrame()
	if err != nil || data.Data == nil {
		t.Fatal("missing sequenced replay", err)
	}
	activity, err := client.ReadResumeFrame()
	if err != nil || activity.Active == nil {
		t.Fatal("missing initial activity", err)
	}
	return *ready.Ready, *data.Data
}

func TestResumeAttachmentReplaysExactSuffixWithoutResetOrReplacingOldConnection(t *testing.T) {
	s := &terminalSession{done: make(chan struct{})}
	s.replay.append([]byte("before"))
	old, oldClient := sealedPairMode(t, "old", true)
	defer old.kill()
	hello := relay.ResumeHello{Writer: [16]byte{1}, Initial: true}
	writer, err := s.prepareResume(old, hello)
	if err != nil {
		t.Fatal(err)
	}
	ready, replay := readResumeInitial(t, oldClient)
	if !ready.Initial || ready.OutputOffset != 0 || replay.Offset != 0 || string(replay.Data) != "before" {
		t.Fatal("initial replay changed")
	}
	s.output([]byte("after"))
	live, err := oldClient.ReadResumeFrame()
	if err != nil || live.Data == nil || live.Data.Offset != 6 || string(live.Data.Data) != "after" {
		t.Fatal("live output lost sequencing", err)
	}
	replacement, replacementClient := sealedPairMode(t, "replacement", true)
	defer replacement.kill()
	hello.Initial, hello.OutputOffset = false, 3
	got, err := s.prepareResume(replacement, hello)
	if err != nil || got != writer {
		t.Fatal("replacement lost writer", err)
	}
	ready, replay = readResumeInitial(t, replacementClient)
	if ready.Initial || ready.OutputOffset != 3 || replay.Offset != 3 || string(replay.Data) != "oreafter" {
		t.Fatal("resume reset or skipped retained output")
	}
	if len(s.conns) != 2 {
		t.Fatal("new connection replaced the old before handoff proof")
	}
	select {
	case <-old.dead:
		t.Fatal("old connection died")
	default:
	}
	if !s.confirmResume(writer, replacement, relay.ResumeClientAck{OutputOffset: 11}) {
		t.Fatal("valid rendered cursor refused")
	}
	if s.confirmResume(writer, replacement, relay.ResumeClientAck{OutputOffset: 12}) {
		t.Fatal("future rendered cursor accepted")
	}
}

func TestExpiredReplayGapIsDeliveredWithoutMutatingOldWriterOrPTY(t *testing.T) {
	s := &terminalSession{done: make(chan struct{})}
	old, oldClient := sealedPairMode(t, "retained", true)
	defer old.kill()
	hello := relay.ResumeHello{Writer: [16]byte{1}, Initial: true}
	writer, err := s.prepareResume(old, hello)
	if err != nil {
		t.Fatal(err)
	}
	readResumeInitial(t, oldClient)
	s.mu.Lock()
	s.replay.append(bytes.Repeat([]byte{'x'}, terminalReplayBytes+1))
	s.mu.Unlock()
	candidate, client := sealedPairMode(t, "gap", true)
	defer candidate.kill()
	hello.Initial = false
	finished := make(chan struct{})
	go func() { s.attachResume(candidate, hello); close(finished) }()
	gap, err := client.ReadResumeFrame()
	if err != nil || gap.Gap == nil || gap.Gap.Reason != relay.ReplayOutputExpired || gap.Gap.AvailableFrom != 1 {
		t.Fatal("gap was not explicit", err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("failed attach did not terminate")
	}
	if len(s.conns) != 1 || s.resumeWriters[0] != writer {
		t.Fatal("failed handoff changed existing admission")
	}
	select {
	case <-old.dead:
		t.Fatal("old connection was killed")
	default:
	}
	select {
	case <-s.done:
		t.Fatal("PTY was closed")
	default:
	}
}

func TestResumeAcknowledgmentsAreCoalescedAndFollowReady(t *testing.T) {
	agent, client := sealedPairMode(t, "resume-ack", true)
	defer agent.kill()
	// Queue progress before ready to exercise the unscheduled-writer case.
	for i := uint64(1); i <= 10000; i++ {
		agent.acknowledgeInput(i)
	}
	agent.acknowledgeInput(1)
	if len(agent.ackWake) != 1 || agent.inputAck.Load() != 10000 {
		t.Fatal("ACK state grew or regressed")
	}
	ready, err := relay.EncodeResumeReady(relay.ResumeReady{})
	if err != nil || !agent.enqueue(ready) {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		frame, err := client.ReadResumeFrame()
		if err == nil && frame.Ready == nil {
			err = errors.New("input ACK overtook ready")
		}
		if err == nil {
			frame, err = client.ReadResumeFrame()
			if err == nil && (frame.InputAck == nil || *frame.InputAck != 10000) {
				err = errors.New("coalesced ACK lost progress")
			}
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("sealed ready/ACK exchange stalled")
	}
}

func TestResumeWriterRetentionPreservesUncertainDelivery(t *testing.T) {
	var writers terminalResumeWriters
	var retained [maxTerminalResumeWriters]*terminalResumeWriter
	for i := range retained {
		conn := &sealedConn{dead: make(chan struct{})}
		writer, err := writers.acquire(relay.ResumeHello{Writer: [16]byte{byte(i + 1)}, Initial: true}, conn)
		if err != nil {
			t.Fatal(err)
		}
		retained[i] = writer
		if err := writer.input.accept(0, []byte("x")); err != nil {
			t.Fatal(err)
		}
		writer.release(conn)
	}
	newHello := relay.ResumeHello{Writer: [16]byte{99}, Initial: true}
	newConn := &sealedConn{dead: make(chan struct{})}
	if _, err := writers.acquire(newHello, newConn); !errors.Is(err, errTerminalWriterCapacity) {
		t.Fatal("queued input was evicted", err)
	}
	for _, writer := range retained {
		if err := writer.input.commit(0, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writers.acquire(newHello, newConn); !errors.Is(err, errTerminalWriterCapacity) {
		t.Fatal("unconfirmed written input was evicted", err)
	}
	if err := retained[0].input.confirm(1); err != nil {
		t.Fatal(err)
	}
	if _, err := writers.acquire(newHello, newConn); err != nil {
		t.Fatal("safe disconnected slot was not reclaimed", err)
	}
	if _, err := writers.acquire(relay.ResumeHello{Writer: retained[0].id}, &sealedConn{}); !errors.Is(err, errTerminalWriterUnknown) {
		t.Fatal("reclaimed identity was silently recreated", err)
	}
	for i := 1; i < len(retained); i++ {
		if writers[i] != retained[i] {
			t.Fatal("unrelated uncertain writer changed")
		}
	}
}

func TestResumeWriterReplacementSharesSlotWithoutReclaimingLiveWriter(t *testing.T) {
	var writers terminalResumeWriters
	hello := relay.ResumeHello{Writer: [16]byte{1}, Initial: true}
	oldConn := &sealedConn{dead: make(chan struct{})}
	writer, err := writers.acquire(hello, oldConn)
	if err != nil {
		t.Fatal(err)
	}
	hello.Initial = false
	var replacements []*sealedConn
	for range maxTerminalConns - 1 {
		conn := &sealedConn{dead: make(chan struct{})}
		replacements = append(replacements, conn)
		got, err := writers.acquire(hello, conn)
		if err != nil || got != writer {
			t.Fatal("replacement lost its writer", err)
		}
	}
	if _, err := writers.acquire(hello, &sealedConn{}); !errors.Is(err, errTerminalWriterCapacity) {
		t.Fatal("overlapping connections were unbounded", err)
	}
	writer.release(oldConn)
	writer.release(oldConn) // Late/duplicate detach must not detach its replacement.
	if writer.reclaimable() {
		t.Fatal("live replacement was reclaimable")
	}
	for _, conn := range replacements {
		writer.release(conn)
	}
	if !writer.reclaimable() {
		t.Fatal("fully confirmed disconnected writer was retained unnecessarily")
	}
	for i := 1; i < len(writers); i++ {
		if writers[i] != nil {
			t.Fatal("replacement consumed another writer slot")
		}
	}
}

func TestResumeWriterAdmissionFailureDoesNotMutateState(t *testing.T) {
	var writers terminalResumeWriters
	conn := &sealedConn{}
	if _, err := writers.acquire(relay.ResumeHello{Initial: true}, conn); !errors.Is(err, errTerminalInputConflict) {
		t.Fatal(err)
	}
	hello := relay.ResumeHello{Writer: [16]byte{1}, Initial: true}
	writer, err := writers.acquire(hello, conn)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []relay.ResumeHello{
		hello,
		{Writer: hello.Writer, InputConfirmed: 1},
		{Writer: [16]byte{2}, Initial: true, OutputOffset: 1},
		{Writer: [16]byte{2}, Initial: true, InputConfirmed: 1},
	} {
		if _, err := writers.acquire(invalid, &sealedConn{}); !errors.Is(err, errTerminalInputConflict) {
			t.Fatal("invalid hello admitted", err)
		}
	}
	if writer.connections[0] != conn || writer.input.confirmed != 0 {
		t.Fatal("failed admission changed original writer")
	}
	for _, other := range writer.connections[1:] {
		if other != nil {
			t.Fatal("failed admission leaked connection")
		}
	}
	for _, other := range writers[1:] {
		if other != nil {
			t.Fatal("failed admission leaked writer")
		}
	}
}

func TestReplayRingUsesAbsoluteOffsetsAndFailsClosedAfterHistoryLoss(t *testing.T) {
	ring := replayRing{buf: make([]byte, 8)}
	ring.append([]byte("abcdef"))
	ring.append([]byte("ghijkl"))
	if got := string(ring.snapshot()); got != "efghijkl" {
		t.Fatal("legacy snapshot changed", got)
	}
	data, err := ring.from(7)
	if err != nil || string(data) != "hijkl" {
		t.Fatal("resume duplicated or skipped output", string(data), err)
	}
	if data, err := ring.from(12); err != nil || len(data) != 0 {
		t.Fatal("exact end cursor failed", err)
	}
	for _, offset := range []uint64{0, 3, 13} {
		if _, err := ring.from(offset); !errors.Is(err, errTerminalReplayGap) {
			t.Fatal("missing or future history became a successful resume", offset, err)
		}
	}
	ring.total = ^uint64(0) - 1
	ring.append([]byte("xx"))
	if _, err := ring.from(ring.total); !errors.Is(err, errTerminalReplayGap) {
		t.Fatal("output position wrapped into a valid cursor")
	}
}

func TestResumeInputDistinguishesQueuedAndWrittenBytesAcrossPartialDelivery(t *testing.T) {
	var state terminalResumeInput
	if err := state.accept(0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if state.ready(0, false).InputWritten != 0 {
		t.Fatal("queued bytes were acknowledged as written")
	}
	if err := state.commit(0, 2); err != nil {
		t.Fatal(err)
	}
	if state.written != 2 {
		t.Fatal("partial PTY write did not advance exact progress")
	}
	if err := state.confirm(2); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := state.check(0, []byte("abcd")); err != nil || !duplicate {
		t.Fatal("whole-frame retry after partial ACK was not deduplicated", err)
	}
	if _, err := state.check(0, []byte("abXY")); !errors.Is(err, errTerminalInputConflict) {
		t.Fatal("changed retry payload was accepted", err)
	}
	if err := state.accept(4, []byte("ef")); err != nil {
		t.Fatal(err)
	}
	if err := state.commit(4, 2); !errors.Is(err, errTerminalInputConflict) {
		t.Fatal("later input overtook unfinished bytes")
	}
	if err := state.commit(2, 2); err != nil {
		t.Fatal(err)
	}
	if err := state.commit(4, 2); err != nil {
		t.Fatal(err)
	}
	if err := state.confirm(6); err != nil || len(state.receipts) != 0 {
		t.Fatal("confirmed receipts were retained", err)
	}
	if _, err := state.check(0, []byte("abcd")); !errors.Is(err, errTerminalInputConflict) {
		t.Fatal("forgotten old input was silently reaccepted")
	}
	if err := state.confirm(7); !errors.Is(err, errTerminalInputConflict) {
		t.Fatal("client acknowledged unwritten bytes")
	}
}

func TestResumeInputReceiptCountIsBounded(t *testing.T) {
	var state terminalResumeInput
	for index := range terminalBrowserInputQueue {
		if err := state.accept(uint64(index), []byte{'x'}); err != nil {
			t.Fatal(err)
		}
		if err := state.commit(uint64(index), 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.accept(state.accepted, []byte{'x'}); err == nil {
		t.Fatal("unconfirmed writer metadata grew beyond its bound")
	}
	if err := state.confirm(state.written); err != nil {
		t.Fatal(err)
	}
	if err := state.accept(state.accepted, []byte{'x'}); err != nil {
		t.Fatal("confirmed capacity did not become reusable", err)
	}
}
