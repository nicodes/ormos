// Package relay defines the multiplexed tunnel protocol shared by the api
// (relay server) and the cli (system). The system holds a single outbound
// WebSocket to the api; hashicorp/yamux multiplexes many logical streams over
// it. Each stream begins with a JSON StreamHeader announcing its purpose, after
// which the stream carries kind-specific bytes.
package relay

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Size bounds on the two attacker-influenced read paths, so a compromised or
// buggy peer can't drive an unbounded allocation. Both ends of the tunnel read
// these, so the limits must be generous enough for real traffic (a large paste
// into a terminal, a long project path in a header) yet finite.
const (
	// MaxFrameSize caps a single terminal frame payload (see ReadFrame). PTY
	// output is chunked at 32 KiB; browser input/paste is the larger case.
	//
	// ReadFrame allocates the declared length before reading a byte of payload,
	// and neither end limits how many streams the other may open, so this number
	// is the per-stream cost an adversarial peer can impose at will. 16 MiB made
	// a few hundred streams enough to exhaust an agent. 1 MiB is still orders of
	// magnitude above any real paste.
	MaxFrameSize = 1 << 20 // 1 MiB
	// MaxHeaderSize caps the newline-terminated JSON StreamHeader (see
	// ReadHeader). Headers are tiny; the only variable field is Cwd (a path).
	MaxHeaderSize = 64 << 10 // 64 KiB
)

// frameHeaderSize is the fixed prefix on every terminal frame: a one-byte tag
// and a four-byte big-endian payload length. It is the single spelling of that
// 5 — encodeFrame writes it, DecodeFrame reads it, and MaxSealedRecord budgets
// for it (seal.go).
const frameHeaderSize = 1 + 4

// Protocol invariants both binaries must agree on. They live here, in the one
// package the agent and the relay both import, because that is the only place
// they cannot drift: a bound spelled as a literal on each side is two bounds,
// and nothing fails loudly when they stop matching.
const (
	// MinTerminalDim and MaxTerminalDim bound a terminal's columns and rows, on
	// the opening StreamHeader and on every resize frame after it.
	//
	// The upper bound is not about what a display could plausibly be. It is
	// what keeps the dimensions inside the uint16 that TIOCSWINSZ takes: the
	// agent casts to uint16 when it calls pty.Setsize, so a bound above 65535
	// would wrap silently and set a window nothing asked for.
	MinTerminalDim = 1
	MaxTerminalDim = 1000
	// MinPort and MaxPort bound a TCP port number.
	MinPort = 1
	MaxPort = 65535
	// PublicKeyHeader carries the agent's sealing public key on the tunnel
	// handshake — every connect, rather than once at pairing, so an agent whose
	// key was regenerated starts working again by reconnecting instead of being
	// re-paired. It is the invariant with the quietest failure of the three:
	// change it on one side and the relay simply never calls SetSystemPubKey,
	// browsers keep sealing against a stale key, and terminals stop opening
	// with no error anywhere.
	PublicKeyHeader = "X-Ormos-Public-Key"
)

// ValidTerminalSize reports whether a terminal's dimensions are within bounds.
func ValidTerminalSize(cols, rows int) bool {
	return cols >= MinTerminalDim && cols <= MaxTerminalDim &&
		rows >= MinTerminalDim && rows <= MaxTerminalDim
}

// ValidPort reports whether p is a usable TCP port number.
//
// It has no caller in this repository yet. The remaining agent-side literals
// live in files a concurrent workstream is editing, and the relay's live in the
// other repository, which re-pins to this one; both conversions are tracked
// rather than taken here so that two workstreams never edit one file. Until
// they land, the port bound is the one invariant here with two spellings and no
// check tying them together — unlike PublicKeyHeader, which the agent names and
// can therefore be pinned to.
func ValidPort(p int) bool { return p >= MinPort && p <= MaxPort }

// StreamKind identifies what a newly opened yamux stream is for.
type StreamKind string

const (
	// KindTerminal carries interactive PTY traffic using terminal frames.
	KindTerminal StreamKind = "terminal"
	// KindProxy carries a raw TCP proxy to a local port on the system host.
	KindProxy StreamKind = "proxy"
	// KindListPorts asks the system to report its currently-listening loopback
	// TCP ports as a JSON array of ints, then the stream is closed.
	KindListPorts StreamKind = "listports"
	// KindPing is a debug stream that echoes bytes back (used in phase 3).
	KindPing StreamKind = "ping"
	// KindShutdown asks the system agent to shut down gracefully (stop the CLI).
	KindShutdown StreamKind = "shutdown"
	// KindEvent is a relay→system nudge that upstream data (its projects, ports,
	// or its own name) changed — e.g. edited in the web UI — so the system should
	// refetch. Carries no payload; the stream is opened and immediately closed.
	KindEvent StreamKind = "event"
)

// StreamHeader is the first message written on every yamux stream. The api
// (yamux client) opens a stream and writes this; the system (yamux server)
// reads it to decide how to handle the stream.
type StreamHeader struct {
	Kind      StreamKind `json:"kind"`
	Port      int        `json:"port,omitempty"`       // for KindProxy: local TCP port to dial
	Cols      int        `json:"cols,omitempty"`       // for KindTerminal: initial columns
	Rows      int        `json:"rows,omitempty"`       // for KindTerminal: initial rows
	Cwd       string     `json:"cwd,omitempty"`        // for KindTerminal: working directory
	SessionID string     `json:"session_id,omitempty"` // for KindTerminal: stable tab identity
}

// WriteHeader encodes h as a single newline-terminated JSON line on w.
func WriteHeader(w io.Writer, h StreamHeader) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return writeAll(w, b)
}

// ReadHeader reads a single newline-terminated JSON header from r. The returned
// bufio.Reader MUST be used for any subsequent reads on the stream, since it may
// have buffered bytes past the header's newline.
func ReadHeader(r io.Reader) (StreamHeader, *bufio.Reader, error) {
	br := bufio.NewReader(r)
	// Read up to the newline byte-by-byte so a peer that never sends one can't
	// make us buffer without bound. bufio still batches the underlying reads,
	// and any bytes it buffered past the newline stay available on br for the
	// caller's subsequent reads.
	line := make([]byte, 0, 256)
	for {
		b, err := br.ReadByte()
		if err != nil {
			return StreamHeader{}, br, err
		}
		if b == '\n' {
			break
		}
		line = append(line, b)
		if len(line) > MaxHeaderSize {
			return StreamHeader{}, br, fmt.Errorf("stream header exceeds %d bytes", MaxHeaderSize)
		}
	}
	var h StreamHeader
	if err := json.Unmarshal(line, &h); err != nil {
		return StreamHeader{}, br, fmt.Errorf("decode stream header: %w", err)
	}
	return h, br, nil
}

// Terminal frame protocol. Terminal streams carry length-prefixed, tagged
// frames so that data and out-of-band resize events share one stream.
//
//	[1 byte tag][4 bytes big-endian length][payload]
type termTag byte

const (
	tagData     termTag = 0 // payload is raw terminal bytes
	tagResize   termTag = 1 // payload is JSON {"cols":N,"rows":N}
	tagActivity termTag = 2 // payload is JSON {"active":bool} (system -> browser)
)

// Resize is the payload of a resize frame.
type Resize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// activityFrame is the payload of an activity frame: whether a foreground
// process (other than the shell itself) is currently running on the PTY.
type activityFrame struct {
	Active bool `json:"active"`
}

// TermFrame is a decoded terminal frame.
type TermFrame struct {
	Resize *Resize // non-nil for resize frames
	Data   []byte  // non-nil for data frames
	Active *bool   // non-nil for activity frames (system -> browser)
}

// Frames are encoded to bytes rather than written straight to a stream, because
// the stream no longer carries them in the clear: a frame is the plaintext that
// goes inside a sealed record (see seal.go). The server never encodes or
// decodes one — on the terminal path it moves opaque records between the
// browser's WebSocket and the agent's stream, and holds no key to read them.

// EncodeData encodes a terminal data frame.
func EncodeData(p []byte) []byte { return encodeFrame(tagData, p) }

// EncodeResize encodes a terminal resize frame.
func EncodeResize(cols, rows int) []byte {
	b, _ := json.Marshal(Resize{Cols: cols, Rows: rows})
	return encodeFrame(tagResize, b)
}

// EncodeActivity encodes a frame reporting whether a foreground process is
// running on the PTY (agent -> browser only).
func EncodeActivity(active bool) []byte {
	b, _ := json.Marshal(activityFrame{Active: active})
	return encodeFrame(tagActivity, b)
}

func encodeFrame(tag termTag, payload []byte) []byte {
	out := make([]byte, frameHeaderSize+len(payload))
	out[0] = byte(tag)
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)))
	copy(out[frameHeaderSize:], payload)
	return out
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

// DecodeFrame decodes a single terminal frame from the plaintext of a record.
//
// The length is validated against the buffer rather than trusted, because this
// runs on plaintext that has already been authenticated but was still authored
// by the peer: a frame claiming to be longer than it is must be an error, not a
// slice past the end.
func DecodeFrame(b []byte) (TermFrame, error) {
	if len(b) < frameHeaderSize {
		return TermFrame{}, fmt.Errorf("terminal frame shorter than its header")
	}
	tag := termTag(b[0])
	n := binary.BigEndian.Uint32(b[1:frameHeaderSize])
	if n > MaxFrameSize {
		return TermFrame{}, fmt.Errorf("terminal frame length %d exceeds max %d", n, MaxFrameSize)
	}
	if int(n)+frameHeaderSize > len(b) {
		return TermFrame{}, fmt.Errorf("terminal frame length %d overruns the %d bytes present", n, len(b)-frameHeaderSize)
	}
	payload := b[frameHeaderSize : frameHeaderSize+n]
	switch tag {
	case tagData:
		return TermFrame{Data: payload}, nil
	case tagResize:
		var rs Resize
		if err := json.Unmarshal(payload, &rs); err != nil {
			return TermFrame{}, fmt.Errorf("decode resize frame: %w", err)
		}
		return TermFrame{Resize: &rs}, nil
	case tagActivity:
		var a activityFrame
		if err := json.Unmarshal(payload, &a); err != nil {
			return TermFrame{}, fmt.Errorf("decode activity frame: %w", err)
		}
		return TermFrame{Active: &a.Active}, nil
	default:
		return TermFrame{}, fmt.Errorf("unknown terminal frame tag %d", tag)
	}
}
