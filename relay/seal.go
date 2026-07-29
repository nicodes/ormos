package relay

import (
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

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
	// sealOverhead is the ChaCha20-Poly1305 authentication tag.
	sealOverhead = chacha20poly1305.Overhead
	// MaxSealedRecord bounds a single sealed record on the wire. A record holds
	// one terminal frame plus the tag, so this tracks MaxFrameSize.
	MaxSealedRecord = MaxFrameSize + 5 + sealOverhead
	// sealInfo domain-separates this key schedule from any other use of the same
	// agreement. Bump the version if the schedule changes.
	sealInfo = "ormos terminal seal v1"
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
// sessionID is bound in as the HKDF salt so two streams that somehow agreed on
// the same secret still derive different keys, and a record captured from one
// stream cannot be replayed into another.
func DeriveSessionKeys(priv *ecdh.PrivateKey, peerPub []byte, sessionID string) (*SessionKeys, error) {
	if len(peerPub) != SealKeySize {
		return nil, fmt.Errorf("peer public key must be %d bytes, got %d", SealKeySize, len(peerPub))
	}
	pub, err := ecdh.X25519().NewPublicKey(peerPub)
	if err != nil {
		return nil, fmt.Errorf("invalid peer public key: %w", err)
	}
	shared, err := priv.ECDH(pub)
	if err != nil {
		return nil, fmt.Errorf("key agreement failed: %w", err)
	}
	out, err := hkdf.Key(sha256.New, shared, []byte(sessionID), sealInfo, 2*SealKeySize)
	if err != nil {
		return nil, fmt.Errorf("key derivation failed: %w", err)
	}
	return &SessionKeys{
		ClientToAgent: out[:SealKeySize],
		AgentToClient: out[SealKeySize:],
	}, nil
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
	n := s.nonce()
	return s.aead.Seal(nil, n[:], plaintext, nil)
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

// WriteRecord writes a length-prefixed record.
//
// The length prefix exists for the yamux side, which is a byte stream. Over the
// browser WebSocket each record is already one binary message, so the server
// adds this prefix in one direction and strips it in the other — which is the
// whole of its involvement in terminal traffic.
func WriteRecord(w io.Writer, record []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(record)))
	if err := writeAll(w, hdr[:]); err != nil {
		return err
	}
	return writeAll(w, record)
}

// ReadRecord reads a single length-prefixed record.
func ReadRecord(r io.Reader) ([]byte, error) {
	var hdr [4]byte
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
func (s *SealedStream) WriteFrame(frame []byte) error {
	return WriteRecord(s.w, s.send.Seal(frame))
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
