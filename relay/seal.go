package relay

import (
	"bytes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// End-to-end sealing for terminal streams.
//
// The server carries terminal traffic but must not be able to read it. It holds
// no key material for a session and never parses a sealed record: on the
// terminal path it is a byte pipe between the browser's WebSocket and the
// agent's yamux stream.
//
// Key agreement is one-pass. The agent has a long-lived X25519 keypair whose
// public half is published on its `systems` record, so the browser can fetch it
// over the ordinary authenticated record API. The browser generates an
// ephemeral keypair per stream, sends the public half as the stream's first
// record, and both sides derive the same secret.
//
// # What this protects against, and what it does not
//
// It protects against a server that reads, logs or stores what passes through
// it, and against anyone who later obtains those logs or that disk. Compromise
// of the agent's static private key exposes past sessions — the browser's key
// is ephemeral but the agent's is not, so this is not full forward secrecy.
// Getting that would need a round trip before the first keystroke; it is a
// deliberate trade, not an oversight.
//
// It does not protect against a server that serves modified client code. On the
// web build the app bundle comes from the same host, so a hostile server could
// ship a bundle that leaks the key. The installed iOS and Android builds are
// not fetched per session, so sealing does hold against a hostile server there.
// That asymmetry is real and is documented in docs/TARGET-ARCHITECTURE.md.

const (
	// SealKeySize is the length of an X25519 public key, and of each derived
	// direction key.
	SealKeySize = 32
	// SealSaltSize is the length of the per-connection server salt (see
	// GenerateServerSalt).
	SealSaltSize = 32
	// sealOverhead is the ChaCha20-Poly1305 authentication tag.
	sealOverhead = chacha20poly1305.Overhead
	// MaxSealedRecord bounds a single sealed record on the wire. A record holds
	// one terminal frame plus the tag, so this tracks MaxFrameSize. frameHeaderSize
	// is the single spelling of the frame's tag+length prefix (see protocol.go).
	MaxSealedRecord = MaxFrameSize + frameHeaderSize + sealOverhead
	// sealInfo domain-separates this key schedule from any other use of the same
	// agreement. Bump the version whenever the schedule changes: v2 binds both
	// public keys and a per-connection server salt (scheduleInfo), so a client
	// derives a key against the exact agent it thinks it is talking to.
	sealInfo = "ormos terminal seal v2"
)

// ErrSealFailed is returned when a record does not authenticate. It is
// deliberately opaque: distinguishing "wrong key" from "tampered" tells an
// attacker which of the two they achieved.
var ErrSealFailed = errors.New("sealed record failed authentication")

// GenerateAgentKey returns a new X25519 private key for an agent.
func GenerateAgentKey() (*ecdh.PrivateKey, error) {
	return ecdh.X25519().GenerateKey(rand.Reader)
}

// SessionKeys are the two directional keys derived from one agreement. Each
// direction has its own key and its own counter, so a record cannot be replayed
// back at its sender.
type SessionKeys struct {
	ClientToAgent []byte
	AgentToClient []byte
}

// DeriveSessionKeys performs the X25519 agreement and expands it into the two
// directional keys.
//
// Three things are bound into the schedule beyond the shared secret itself:
//
//   - Both public keys (scheduleInfo). A relay distributes the agent's public
//     key to the client; if it substitutes one of its own, the client derives
//     against that substituted key while the real agent derives against its own,
//     so the two never agree and the stream fails closed. The seal alone already
//     stops the relay reading traffic; binding the key is what stops it standing
//     in as the endpoint against a client that pins the fingerprint.
//   - The per-connection server salt, as the HKDF salt. A fresh salt each
//     connection guarantees a distinct key/nonce space even if a client reuses
//     its ephemeral key across reattaches — the counter-nonce scheme is only
//     safe while no key is ever reused.
//   - sessionID, so two tabs between the same two parties derive different keys
//     and a record from one cannot be replayed into another.
func DeriveSessionKeys(priv *ecdh.PrivateKey, peerPub, salt []byte, sessionID string) (*SessionKeys, error) {
	if len(peerPub) != SealKeySize {
		return nil, fmt.Errorf("peer public key must be %d bytes, got %d", SealKeySize, len(peerPub))
	}
	if len(salt) != SealSaltSize {
		return nil, fmt.Errorf("server salt must be %d bytes, got %d", SealSaltSize, len(salt))
	}
	pub, err := ecdh.X25519().NewPublicKey(peerPub)
	if err != nil {
		return nil, fmt.Errorf("invalid peer public key: %w", err)
	}
	shared, err := priv.ECDH(pub)
	if err != nil {
		return nil, fmt.Errorf("key agreement failed: %w", err)
	}
	out, err := hkdf.Key(sha256.New, shared, salt, scheduleInfo(priv.PublicKey().Bytes(), peerPub, sessionID), 2*SealKeySize)
	if err != nil {
		return nil, fmt.Errorf("key derivation failed: %w", err)
	}
	return &SessionKeys{
		ClientToAgent: out[:SealKeySize],
		AgentToClient: out[SealKeySize:],
	}, nil
}

// scheduleInfo builds the HKDF info that binds the protocol version, both
// public keys, and the session id into the key schedule.
//
// The two keys are ordered by value rather than by role, so both ends build the
// identical info without the function needing to know which of the pair is the
// agent and which is the client — the agent calls it with (its own pub, the
// client's pub) and the client with (its own pub, the agent's pub), and the
// sorted result is the same. sessionID is last and variable-length, so the
// framing is unambiguous.
func scheduleInfo(a, b []byte, sessionID string) string {
	lo, hi := a, b
	if bytes.Compare(lo, hi) > 0 {
		lo, hi = hi, lo
	}
	var buf []byte
	buf = append(buf, sealInfo...)
	buf = append(buf, 0)
	buf = append(buf, lo...)
	buf = append(buf, hi...)
	buf = append(buf, sessionID...)
	return string(buf)
}

// Fingerprint renders a short, human-comparable base32 fingerprint of an agent
// public key for out-of-band verification: the agent prints it on startup and
// the app pins it, so a user can read one against the other and catch a relay
// that swapped the key. It is the RFC 4648 base32 of the first 10 bytes of
// SHA-256(pub) — 80 bits — grouped in fours. seal.ts's fingerprint() must
// produce the identical string.
func Fingerprint(pub []byte) string {
	sum := sha256.Sum256(pub)
	raw := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:10])
	var b strings.Builder
	for i := 0; i < len(raw); i += 4 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(raw[i:min(i+4, len(raw))])
	}
	return b.String()
}

// Sealer seals and opens records for one direction of one stream.
//
// Nonces are a counter, never random: a 96-bit random nonce has a birthday
// bound that a long-lived terminal could plausibly approach, whereas a counter
// cannot repeat before it overflows. Each direction has its own Sealer, so the
// two never share a counter under the same key.
type Sealer struct {
	aead    cipher.AEAD
	counter uint64
}

// NewSealer builds a Sealer for one direction from a 32-byte key.
func NewSealer(key []byte) (*Sealer, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// nonce renders the current counter as a 12-byte nonce and advances it.
//
// The counter is explicit rather than sent on the wire: both ends start at zero
// and step in lockstep, so a dropped, reordered or replayed record breaks the
// stream instead of being silently accepted. On a reliable ordered transport —
// which yamux over a WebSocket is — that is the behaviour we want.
func (s *Sealer) nonce() [chacha20poly1305.NonceSize]byte {
	var n [chacha20poly1305.NonceSize]byte
	binary.BigEndian.PutUint64(n[4:], s.counter)
	s.counter++
	return n
}

// Seal encrypts one plaintext record.
func (s *Sealer) Seal(plaintext []byte) []byte {
	return s.sealAppend(nil, plaintext)
}

// sealAppend encrypts one plaintext record onto the end of dst, so a caller
// that has already reserved room for a length prefix pays neither a second
// allocation nor a copy. It is the single place the nonce counter is consumed.
func (s *Sealer) sealAppend(dst, plaintext []byte) []byte {
	n := s.nonce()
	return s.aead.Seal(dst, n[:], plaintext, nil)
}

// Open decrypts one record, or returns ErrSealFailed.
func (s *Sealer) Open(record []byte) ([]byte, error) {
	n := s.nonce()
	out, err := s.aead.Open(nil, n[:], record, nil)
	if err != nil {
		return nil, ErrSealFailed
	}
	return out, nil
}

// recordPrefixSize is the fixed big-endian length prefix on every sealed
// record. The single spelling of that 4: WriteRecord writes it and ReadRecord
// reads it.
const recordPrefixSize = 4

// WriteRecord writes a length-prefixed record in a single Write.
//
// The length prefix exists for the yamux side, which is a byte stream. Over the
// browser WebSocket each record is already one binary message, so the server
// adds this prefix in one direction and strips it in the other — which is the
// whole of its involvement in terminal traffic.
//
// One Write, not two, because of what sits underneath. yamux emits a data frame
// per Stream.Write (up to its send window), and its send loop writes that
// frame's 12-byte header and its body as two separate writes on the tunnel
// conn, which websocket.NetConn turns into one WebSocket message each. So a
// record split across two writes cost four WebSocket messages and two yamux
// frames — one of the frames carrying nothing but a 4-byte length — and every
// keystroke paid it. It now costs two messages and one frame. The bytes on the
// wire are otherwise unchanged.
//
// The whole record is copied into one buffer to do that. Inside this repo that
// only ever carries the two 32-byte hellos, but relay/ is shared: the server's
// browser-to-agent pump calls this per inbound record, so it trades a copy
// there for a frame and a message on every one.
func WriteRecord(w io.Writer, record []byte) error {
	buf := make([]byte, recordPrefixSize+len(record))
	binary.BigEndian.PutUint32(buf, uint32(len(record)))
	copy(buf[recordPrefixSize:], record)
	return writeAll(w, buf)
}

// ReadRecord reads a single length-prefixed record.
func ReadRecord(r io.Reader) ([]byte, error) {
	var hdr [recordPrefixSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxSealedRecord {
		return nil, fmt.Errorf("sealed record length %d exceeds max %d", n, MaxSealedRecord)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// ClientHelloSize is the length of the browser's opening record: its ephemeral
// public key, sent in the clear because it is public by construction.
const ClientHelloSize = SealKeySize

// WriteClientHello sends the browser's ephemeral public key as the stream's
// first record. It is unsealed — there is no shared key yet, and the value is
// public anyway.
func WriteClientHello(w io.Writer, pub []byte) error {
	if len(pub) != ClientHelloSize {
		return fmt.Errorf("client hello must be %d bytes, got %d", ClientHelloSize, len(pub))
	}
	return WriteRecord(w, pub)
}

// ReadClientHello reads the browser's ephemeral public key.
//
// A hello of the wrong length is refused rather than padded or truncated: the
// only thing that produces one is a peer that does not speak this protocol, and
// deriving a key from a malformed input would turn a version mismatch into a
// silent failure much later.
func ReadClientHello(r io.Reader) ([]byte, error) {
	rec, err := ReadRecord(r)
	if err != nil {
		return nil, err
	}
	if len(rec) != ClientHelloSize {
		return nil, fmt.Errorf("client hello must be %d bytes, got %d", ClientHelloSize, len(rec))
	}
	return rec, nil
}

// ServerHelloSize is the length of the agent's reply to the client hello: the
// per-connection server salt, sent in the clear because it is a public random
// value that reveals nothing on its own.
const ServerHelloSize = SealSaltSize

// GenerateServerSalt returns a fresh per-connection salt. A new one every
// connection is what makes the counter-nonce scheme safe across reattaches: the
// salt goes into the key schedule (DeriveSessionKeys), so even a client that
// reuses its ephemeral key lands in a distinct key/nonce space each time.
func GenerateServerSalt() ([]byte, error) {
	salt := make([]byte, SealSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating server salt: %w", err)
	}
	return salt, nil
}

// WriteServerHello sends the agent's per-connection salt as its reply to the
// client hello. It is unsealed — there is no shared key yet, and the salt is
// public by construction.
func WriteServerHello(w io.Writer, salt []byte) error {
	if len(salt) != ServerHelloSize {
		return fmt.Errorf("server hello must be %d bytes, got %d", ServerHelloSize, len(salt))
	}
	return WriteRecord(w, salt)
}

// ReadServerHello reads the agent's per-connection salt. A reply of the wrong
// length is refused rather than used, for the same reason ReadClientHello
// refuses a malformed hello: it only comes from a peer that does not speak this
// protocol, and mixing nonsense into the schedule turns a version mismatch into
// a confusing failure much later.
func ReadServerHello(r io.Reader) ([]byte, error) {
	rec, err := ReadRecord(r)
	if err != nil {
		return nil, err
	}
	if len(rec) != ServerHelloSize {
		return nil, fmt.Errorf("server hello must be %d bytes, got %d", ServerHelloSize, len(rec))
	}
	return rec, nil
}

// SealedStream carries terminal frames over an ordered byte stream, sealed.
//
// Each direction has its own key and its own counter, so the two never share a
// nonce and a record cannot be replayed back at its sender. One of these
// belongs to one connection: a terminal session the browser reattaches to gets
// a fresh ephemeral key each time, so a session with two live connections has
// two independent SealedStreams and no shared crypto state between them.
type SealedStream struct {
	r    io.Reader
	w    io.Writer
	send *Sealer
	recv *Sealer
}

// NewSealedStream builds the transport for one connection. sendKey seals what
// this side writes; recvKey opens what it reads.
func NewSealedStream(r io.Reader, w io.Writer, sendKey, recvKey []byte) (*SealedStream, error) {
	send, err := NewSealer(sendKey)
	if err != nil {
		return nil, err
	}
	recv, err := NewSealer(recvKey)
	if err != nil {
		return nil, err
	}
	return &SealedStream{r: r, w: w, send: send, recv: recv}, nil
}

// WriteFrame seals an encoded frame and writes it as one record.
//
// The record is sealed straight into a buffer that already carries room for the
// length prefix, so one allocation and one Write carry the whole thing — see
// WriteRecord for what the second Write cost.
func (s *SealedStream) WriteFrame(frame []byte) error {
	buf := make([]byte, recordPrefixSize, recordPrefixSize+len(frame)+sealOverhead)
	buf = s.send.sealAppend(buf, frame)
	binary.BigEndian.PutUint32(buf, uint32(len(buf)-recordPrefixSize))
	return writeAll(s.w, buf)
}

// ReadFrame reads one record, opens it, and decodes the frame inside.
func (s *SealedStream) ReadFrame() (TermFrame, error) {
	rec, err := ReadRecord(s.r)
	if err != nil {
		return TermFrame{}, err
	}
	plain, err := s.recv.Open(rec)
	if err != nil {
		return TermFrame{}, err
	}
	return DecodeFrame(plain)
}
