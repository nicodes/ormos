package relay

import (
	"bytes"
	"encoding/binary"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestSealedResumeReaderRejectsUnsequencedAndTamperedRecords(t *testing.T) {
	for _, test := range []struct {
		name           string
		legacy, tamper bool
	}{
		{name: "resumed"}, {name: "legacy data", legacy: true}, {name: "tampered", tamper: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var wire bytes.Buffer
			key := bytes.Repeat([]byte{1}, SealKeySize)
			writer, err := NewSealedStream(nil, &wire, key, key)
			if err != nil {
				t.Fatal(err)
			}
			frame, err := EncodeSequencedData(7, []byte("hello"))
			if err != nil {
				t.Fatal(err)
			}
			if test.legacy {
				frame = EncodeData([]byte("hello"))
			}
			if err := writer.WriteFrame(frame); err != nil {
				t.Fatal(err)
			}
			if test.tamper {
				wire.Bytes()[wire.Len()-1] ^= 1
			}
			reader, err := NewSealedStream(&wire, nil, key, key)
			if err != nil {
				t.Fatal(err)
			}
			got, err := reader.ReadResumeFrame()
			if test.legacy || test.tamper {
				if err == nil {
					t.Fatal("invalid resumed record accepted")
				}
				return
			}
			if err != nil || got.Data == nil || got.Data.Offset != 7 || string(got.Data.Data) != "hello" {
				t.Fatal("sealed resume round trip failed", err)
			}
		})
	}
}

func TestV5KeepsV4ResourcesButSeparatesConnectionKeys(t *testing.T) {
	v4 := StreamHeader{Kind: KindTerminal, ProtocolVersion: StreamFenceVersionV4, SystemID: "system", TerminalRecordID: "terminal", TerminalGeneration: 1, Cwd: "/work", ActionFence: strings.Repeat("a", 32), NotAfterMilli: 123}
	v5 := v4
	v5.ProtocolVersion = StreamFenceVersionV5
	if err := ValidateDirectStreamHeader(v5, "system"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateV4StreamHeader(v5, "system"); err == nil {
		t.Fatal("v5 was silently accepted as v4")
	}
	oldResource, _ := TerminalResourceIdentity(v4)
	newResource, _ := TerminalResourceIdentity(v5)
	if oldResource != newResource {
		t.Fatal("transport capability changed the durable PTY resource")
	}
	oldBinding, _ := TerminalSessionBinding(v4)
	newBinding, _ := TerminalSessionBinding(v5)
	if oldBinding == newBinding || !strings.HasPrefix(newBinding, "v5\x00") {
		t.Fatal("v5 connection binding was not separated")
	}
	if _, _, err := ReadHeader(strings.NewReader(`{"kind":"terminal","protocol_version":"5","system_id":"system","terminal_record_id":"terminal","terminal_generation":1,"cwd":"/work"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadHeader(strings.NewReader(`{"kind":"terminal","protocol_version":"5","unknown":true}` + "\n")); err == nil {
		t.Fatal("v5 accepted unknown fields")
	}
}

func TestAcknowledgedTerminalFramesRoundTripWithoutChangingLegacyFrames(t *testing.T) {
	writer := [16]byte{1, 2, 3}
	hello := ResumeHello{Writer: writer, OutputOffset: 1 << 54, InputConfirmed: 123, Initial: true}
	frame, err := DecodeResumeFrame(EncodeResumeHello(hello))
	if err != nil || !reflect.DeepEqual(frame.Hello, &hello) {
		t.Fatal("resume hello lost its exact counters", err)
	}
	ready := ResumeReady{InputAccepted: 432, InputWritten: 123, OutputOffset: 1 << 54, Initial: true}
	encoded, err := EncodeResumeReady(ready)
	if err != nil {
		t.Fatal(err)
	}
	frame, err = DecodeResumeFrame(encoded)
	if err != nil || !reflect.DeepEqual(frame.Ready, &ready) {
		t.Fatal("ready counters changed", err)
	}
	encoded, err = EncodeSequencedData(9, []byte("input"))
	if err != nil {
		t.Fatal(err)
	}
	frame, err = DecodeResumeFrame(encoded)
	if err != nil || frame.Data.Offset != 9 || string(frame.Data.Data) != "input" {
		t.Fatal("sequenced data changed", err)
	}
	frame, err = DecodeResumeFrame(EncodeInputAck(123))
	if err != nil || frame.InputAck == nil || *frame.InputAck != 123 {
		t.Fatal("input acknowledgment changed", err)
	}
	ack := ResumeClientAck{OutputOffset: 456, InputConfirmed: 123}
	frame, err = DecodeResumeFrame(EncodeClientAck(ack))
	if err != nil || !reflect.DeepEqual(frame.ClientAck, &ack) {
		t.Fatal("client acknowledgment changed", err)
	}
	encoded, err = EncodeReplayGap(ReplayGap{Reason: ReplayOutputExpired, AvailableFrom: 789})
	if err != nil {
		t.Fatal(err)
	}
	frame, err = DecodeResumeFrame(encoded)
	if err != nil || frame.Gap.AvailableFrom != 789 {
		t.Fatal("gap lost its recovery boundary", err)
	}
	if _, err := DecodeFrame(EncodeInputAck(123)); err == nil {
		t.Fatal("new framing was silently accepted as a legacy frame")
	}
	if _, err := DecodeResumeFrame(EncodeData([]byte("unsequenced"))); err == nil {
		t.Fatal("unsequenced data bypassed resumption")
	}
}

func TestAcknowledgedFramesRejectMalformedBoundsAndImpossibleProgress(t *testing.T) {
	valid := EncodeResumeHello(ResumeHello{Writer: [16]byte{1}})
	for size := 0; size < len(valid); size++ {
		if _, err := DecodeResumeFrame(valid[:size]); err == nil {
			t.Fatal("truncated hello accepted", size)
		}
	}
	if _, err := DecodeResumeFrame(append(valid, 0)); err == nil {
		t.Fatal("trailing frame bytes accepted")
	}
	bad := append([]byte(nil), valid...)
	bad[len(bad)-1] = 2
	if _, err := DecodeResumeFrame(bad); err == nil {
		t.Fatal("non-boolean initial flag accepted")
	}
	if _, err := DecodeResumeFrame(EncodeResumeHello(ResumeHello{})); err == nil {
		t.Fatal("anonymous writer accepted")
	}
	if _, err := EncodeResumeReady(ResumeReady{InputAccepted: 1, InputWritten: 2}); err == nil {
		t.Fatal("impossible written progress accepted")
	}
	if _, err := EncodeSequencedData(math.MaxUint64, []byte{1}); err == nil {
		t.Fatal("offset overflow accepted")
	}
	if _, err := EncodeSequencedData(0, make([]byte, MaxFrameSize-7)); err == nil {
		t.Fatal("oversized frame accepted")
	}
	bad = EncodeInputAck(1)
	binary.BigEndian.PutUint32(bad[1:], MaxFrameSize+1)
	if _, err := DecodeResumeFrame(bad); err == nil {
		t.Fatal("oversized declaration accepted")
	}
}
