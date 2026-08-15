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
	"errors"
	"fmt"
	"io"
	"time"
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
	// StreamFenceVersionHeader is required on the tunnel handshake. A relay that
	// cannot send agent-enforced action fences must not connect to an agent that
	// promises to enforce them (and vice versa).
	StreamFenceVersionHeader = "X-Ormos-Stream-Fence-Version"
	// Version 2 adds a terminal shutdown acknowledgment. Exact-version tunnel
	// negotiation prevents a v2 relay from sending that framing to a connected
	// v1 agent during a backend-first rollout.
	StreamFenceVersion = "2"
)

// ValidTerminalSize reports whether a terminal's dimensions are within bounds.
func ValidTerminalSize(cols, rows int) bool {
	return cols >= MinTerminalDim && cols <= MaxTerminalDim &&
		rows >= MinTerminalDim && rows <= MaxTerminalDim
}

// ValidPort reports whether p is a usable TCP port number. The single
// spelling of the bound: both binaries import this package, so the check
// cannot drift between the agent's policy/TUI guards and the relay's.
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
	Kind          StreamKind `json:"kind"`
	Port          int        `json:"port,omitempty"`            // for KindProxy: local TCP port to dial
	Cols          int        `json:"cols,omitempty"`            // for KindTerminal: initial columns
	Rows          int        `json:"rows,omitempty"`            // for KindTerminal: initial rows
	Cwd           string     `json:"cwd,omitempty"`             // for KindTerminal: working directory
	SessionID     string     `json:"session_id,omitempty"`      // for KindTerminal: stable tab identity
	ActionFence   string     `json:"action_fence,omitempty"`    // opaque durable side-effect capability
	NotAfterMilli int64      `json:"not_after_milli,omitempty"` // agent refuses the action at/after this instant
}

const maxActionFenceFuture = time.Minute

var errStreamFenceExpired = errors.New("stream action fence expired")

// IsStreamFenceExpired distinguishes an expired otherwise-shaped fence from a
// malformed refusal so action protocols can report a stable terminal status.
func IsStreamFenceExpired(err error) bool { return errors.Is(err, errStreamFenceExpired) }

// StreamKindRequiresFence identifies streams that cause an external action.
// Informational list/event streams remain compatible without a fence.
func StreamKindRequiresFence(kind StreamKind) bool {
	return kind == KindTerminal || kind == KindProxy || kind == KindShutdown
}

// ValidateStreamFence lets the agent reject a relay action that was parked past
// its durable authorization. The fence is opaque but strictly shaped so an old
// relay (which sends neither field) fails closed rather than silently regaining
// the pre-fence behavior.
func ValidateStreamFence(h StreamHeader, now time.Time) error {
	if !StreamKindRequiresFence(h.Kind) {
		return nil
	}
	if len(h.ActionFence) < 32 || len(h.ActionFence) > 64 {
		return fmt.Errorf("stream action fence has invalid length")
	}
	for _, c := range h.ActionFence {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return fmt.Errorf("stream action fence has invalid characters")
		}
	}
	notAfter := time.UnixMilli(h.NotAfterMilli)
	if !now.Before(notAfter) {
		return errStreamFenceExpired
	}
	if notAfter.After(now.Add(maxActionFenceFuture)) {
		return fmt.Errorf("stream action fence is too far in the future")
	}
	return nil
}

// ActionAckStatus is the terminal result of a shutdown action. Success means
// the agent committed the shutdown before acknowledging; refused and expired
// mean it performed no shutdown action.
type ActionAckStatus string

const (
	ActionAckSuccess ActionAckStatus = "success"
	ActionAckRefused ActionAckStatus = "refused"
	ActionAckExpired ActionAckStatus = "expired"
)

// ActionAck binds the terminal result to the exact durable capability and
// deadline from the shutdown header. Echoing both prevents a delayed response
// from completing a later stop request on a reused tunnel.
type ActionAck struct {
	ActionFence   string          `json:"action_fence"`
	NotAfterMilli int64           `json:"not_after_milli"`
	Status        ActionAckStatus `json:"status"`
}

// NewActionAck constructs an acknowledgment for h without letting callers
// accidentally omit either replay-binding field.
func NewActionAck(h StreamHeader, status ActionAckStatus) ActionAck {
	return ActionAck{ActionFence: h.ActionFence, NotAfterMilli: h.NotAfterMilli, Status: status}
}

// ValidateActionAck accepts only terminal shutdown results for the exact header
// that opened the stream.
func ValidateActionAck(h StreamHeader, ack ActionAck) error {
	if h.Kind != KindShutdown {
		return fmt.Errorf("action acknowledgment is only valid for shutdown")
	}
	if ack.ActionFence != h.ActionFence || ack.NotAfterMilli != h.NotAfterMilli {
		return fmt.Errorf("action acknowledgment does not match its shutdown fence")
	}
	switch ack.Status {
	case ActionAckSuccess, ActionAckRefused, ActionAckExpired:
		return nil
	default:
		return fmt.Errorf("unknown action acknowledgment status %q", ack.Status)
	}
}

// WriteActionAck writes one newline-delimited terminal shutdown result.
func WriteActionAck(w io.Writer, ack ActionAck) error {
	b, err := json.Marshal(ack)
	if err != nil {
		return err
	}
	return writeAll(w, append(b, '\n'))
}

// ReadActionAck reads one bounded newline-delimited shutdown result.
func ReadActionAck(r io.Reader) (ActionAck, error) {
	line, _, err := readBoundedJSONLine(r)
	if err != nil {
		return ActionAck{}, err
	}
	var ack ActionAck
	if err := json.Unmarshal(line, &ack); err != nil {
		return ActionAck{}, fmt.Errorf("decode action acknowledgment: %w", err)
	}
	return ack, nil
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
	line, br, err := readBoundedJSONLine(r)
	if err != nil {
		return StreamHeader{}, br, err
	}
	var h StreamHeader
	if err := json.Unmarshal(line, &h); err != nil {
		return StreamHeader{}, br, fmt.Errorf("decode stream header: %w", err)
	}
	return h, br, nil
}

func readBoundedJSONLine(r io.Reader) ([]byte, *bufio.Reader, error) {
	br := bufio.NewReader(r)
	// Read up to the newline byte-by-byte so a peer that never sends one can't
	// make us buffer without bound. bufio still batches the underlying reads,
	// and any bytes it buffered past the newline stay available on br for the
	// caller's subsequent reads.
	line := make([]byte, 0, 256)
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, br, err
		}
		if b == '\n' {
			break
		}
		line = append(line, b)
		if len(line) > MaxHeaderSize {
			return nil, br, fmt.Errorf("stream protocol message exceeds %d bytes", MaxHeaderSize)
		}
	}
	return line, br, nil
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
