package relay

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func TestValidateStreamFence(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	valid := StreamHeader{
		Kind: KindShutdown, ActionFence: strings.Repeat("a", 40),
		NotAfterMilli: now.Add(5 * time.Second).UnixMilli(),
	}
	if err := ValidateStreamFence(valid, now); err != nil {
		t.Fatalf("valid fence: %v", err)
	}
	for name, mutate := range map[string]func(*StreamHeader){
		"missing capability": func(h *StreamHeader) { h.ActionFence = "" },
		"invalid capability": func(h *StreamHeader) { h.ActionFence = strings.Repeat("!", 40) },
		"expired":            func(h *StreamHeader) { h.NotAfterMilli = now.UnixMilli() },
		"far future":         func(h *StreamHeader) { h.NotAfterMilli = now.Add(2 * time.Minute).UnixMilli() },
	} {
		t.Run(name, func(t *testing.T) {
			h := valid
			mutate(&h)
			if err := ValidateStreamFence(h, now); err == nil {
				t.Fatal("invalid fence was accepted")
			}
		})
	}
	if err := ValidateStreamFence(StreamHeader{Kind: KindEvent}, now); err != nil {
		t.Fatalf("informational stream unexpectedly required a fence: %v", err)
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := StreamHeader{Kind: KindTerminal, Cols: 120, Rows: 40, Cwd: "/code/app", SessionID: "project:tab"}
	if err := WriteHeader(&buf, want); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	// extra bytes after the header line must survive for the caller.
	buf.WriteString("payload-bytes")

	got, br, err := ReadHeader(&buf)
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if got != want {
		t.Fatalf("header = %+v, want %+v", got, want)
	}
	rest, _ := br.ReadString(0)
	if rest != "payload-bytes" {
		t.Fatalf("leftover = %q, want %q", rest, "payload-bytes")
	}
}

func TestTerminalFrameRoundTrip(t *testing.T) {
	f1, err := DecodeFrame(EncodeData([]byte("hello")))
	if err != nil {
		t.Fatal(err)
	}
	if string(f1.Data) != "hello" || f1.Resize != nil {
		t.Fatalf("frame1 = %+v", f1)
	}

	f2, err := DecodeFrame(EncodeResize(120, 40))
	if err != nil {
		t.Fatal(err)
	}
	if f2.Resize == nil || f2.Resize.Cols != 120 || f2.Resize.Rows != 40 {
		t.Fatalf("frame2 = %+v", f2)
	}

	f3, err := DecodeFrame(EncodeActivity(true))
	if err != nil {
		t.Fatal(err)
	}
	if f3.Active == nil || !*f3.Active {
		t.Fatalf("frame3 = %+v", f3)
	}
}

// The wire format must match the TypeScript encoder byte for byte, so pin it:
// one tag byte, four big-endian length bytes, then the payload.
func TestTerminalFrameLayout(t *testing.T) {
	got := EncodeData(bytes.Repeat([]byte{7}, 258))
	if got[0] != 0 {
		t.Fatalf("data tag = %d, want 0", got[0])
	}
	if !bytes.Equal(got[1:5], []byte{0, 0, 1, 2}) {
		t.Fatalf("length bytes = %v, want big-endian 258", got[1:5])
	}
}

// A frame whose declared length exceeds MaxFrameSize must be rejected before any
// payload allocation, so a compromised peer can't drive an OOM.
func TestDecodeFrameRejectsOversized(t *testing.T) {
	var hdr [5]byte
	hdr[0] = 0 // tagData
	binary.BigEndian.PutUint32(hdr[1:], uint32(MaxFrameSize)+1)
	if _, err := DecodeFrame(hdr[:]); err == nil {
		t.Fatal("expected error for oversized frame length, got nil")
	}
}

// The plaintext inside a record is authenticated but still authored by the
// peer, so a length that overruns the buffer must be an error rather than a
// slice past the end.
func TestDecodeFrameRejectsOverrunAndTruncation(t *testing.T) {
	if _, err := DecodeFrame([]byte{0, 0, 0, 0, 200, 1, 2}); err == nil {
		t.Fatal("a length overrunning the buffer must be refused")
	}
	if _, err := DecodeFrame([]byte{0, 0, 0}); err == nil {
		t.Fatal("a frame shorter than its header must be refused")
	}
	if _, err := DecodeFrame([]byte{9, 0, 0, 0, 0}); err == nil {
		t.Fatal("an unknown tag must be refused")
	}
}

// MaxSealedRecord must budget for exactly a maximal terminal frame plus the
// frame header plus the AEAD tag. This is the single relationship the relay's
// read limit depends on: a maximal legitimate paste seals to MaxSealedRecord
// bytes, which is larger than MaxFrameSize, so a read limit of MaxFrameSize
// (the old api/terminal.go bug) would reject it.
func TestMaxSealedRecordBudget(t *testing.T) {
	if MaxSealedRecord != MaxFrameSize+frameHeaderSize+sealOverhead {
		t.Fatalf("MaxSealedRecord = %d, want MaxFrameSize+frameHeaderSize+sealOverhead = %d",
			MaxSealedRecord, MaxFrameSize+frameHeaderSize+sealOverhead)
	}
	// Construct the real thing: a frame at the payload ceiling, sealed.
	sealer, err := NewSealer(bytes.Repeat([]byte{1}, SealKeySize))
	if err != nil {
		t.Fatal(err)
	}
	rec := sealer.Seal(EncodeData(bytes.Repeat([]byte{2}, MaxFrameSize)))
	if len(rec) != MaxSealedRecord {
		t.Fatalf("maximal sealed record = %d bytes, want MaxSealedRecord = %d", len(rec), MaxSealedRecord)
	}
	if len(rec) <= MaxFrameSize {
		t.Fatalf("maximal sealed record %d should exceed MaxFrameSize %d", len(rec), MaxFrameSize)
	}
}

// A header line with no newline must not buffer without bound.
func TestReadHeaderRejectsOversized(t *testing.T) {
	big := bytes.Repeat([]byte("a"), MaxHeaderSize+10) // no newline
	if _, _, err := ReadHeader(bytes.NewReader(big)); err == nil {
		t.Fatal("expected error for oversized header, got nil")
	}
}

// The bounds live here rather than as a literal per call site, because a
// literal per call site is a bound per call site. The cases are written as
// literals on purpose: they pin the values themselves, which the browser and
// the relay also encode, rather than restating the constants back at
// themselves.
func TestValidTerminalSize(t *testing.T) {
	for _, tc := range []struct {
		cols, rows int
		want       bool
	}{
		{80, 24, true},
		{1, 1, true},
		{1000, 1000, true},
		{0, 24, false},
		{80, 0, false},
		{1001, 24, false},
		{80, 1001, false},
		{-1, -1, false},
	} {
		if got := ValidTerminalSize(tc.cols, tc.rows); got != tc.want {
			t.Errorf("ValidTerminalSize(%d, %d) = %v, want %v", tc.cols, tc.rows, got, tc.want)
		}
	}
	// The reason for the upper bound, exercised rather than asserted about: the
	// agent casts to the uint16 TIOCSWINSZ takes, so a bound past 65535 would
	// wrap silently. Round-tripping the constant through that cast is a real
	// check; `if MaxTerminalDim > 65535` would be a constant expression the
	// compiler folds, with a t.Fatalf that can never run.
	if got := int(uint16(MaxTerminalDim)); got != MaxTerminalDim {
		t.Fatalf("MaxTerminalDim %d survives the uint16 cast pty.Setsize takes as %d", MaxTerminalDim, got)
	}
}

func TestValidPort(t *testing.T) {
	for port, want := range map[int]bool{
		-1: false, 0: false, 1: true, 3000: true, 65535: true, 65536: false,
	} {
		if got := ValidPort(port); got != want {
			t.Errorf("ValidPort(%d) = %v, want %v", port, got, want)
		}
	}
}
