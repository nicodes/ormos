package relay

import (
	"bytes"
	"encoding/binary"
	"testing"
)

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

// A header line with no newline must not buffer without bound.
func TestReadHeaderRejectsOversized(t *testing.T) {
	big := bytes.Repeat([]byte("a"), MaxHeaderSize+10) // no newline
	if _, _, err := ReadHeader(bytes.NewReader(big)); err == nil {
		t.Fatal("expected error for oversized header, got nil")
	}
}
