package relay

import (
	"bytes"
	"crypto/ecdh"
	"encoding/hex"
	"errors"
	"net"
	"testing"
)

// fixedKey builds a deterministic X25519 private key so tests (and the
// cross-language vectors in app/src/lib/seal.test.ts) are reproducible.
func fixedKey(t *testing.T, hexSeed string) *ecdh.PrivateKey {
	t.Helper()
	raw, err := hex.DecodeString(hexSeed)
	if err != nil {
		t.Fatalf("bad seed: %v", err)
	}
	k, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		t.Fatalf("NewPrivateKey: %v", err)
	}
	return k
}

const (
	agentSeed  = "a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf"
	clientSeed = "6162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f80"
)

// testSalt is a deterministic stand-in for the per-connection server salt so
// derivations in tests are reproducible.
var testSalt = bytes.Repeat([]byte{0x5a}, SealSaltSize)

// Both ends must derive the same two keys from opposite sides of the agreement,
// given the same server salt.
func TestDeriveSessionKeysAgree(t *testing.T) {
	agent := fixedKey(t, agentSeed)
	client := fixedKey(t, clientSeed)

	a, err := DeriveSessionKeys(agent, client.PublicKey().Bytes(), testSalt, "tab-1")
	if err != nil {
		t.Fatalf("agent derive: %v", err)
	}
	c, err := DeriveSessionKeys(client, agent.PublicKey().Bytes(), testSalt, "tab-1")
	if err != nil {
		t.Fatalf("client derive: %v", err)
	}
	if !bytes.Equal(a.ClientToAgent, c.ClientToAgent) || !bytes.Equal(a.AgentToClient, c.AgentToClient) {
		t.Fatal("the two ends derived different keys")
	}
	if bytes.Equal(a.ClientToAgent, a.AgentToClient) {
		t.Fatal("the two directions must not share a key")
	}
}

// The session id is bound into the schedule, so one tab's records cannot be
// replayed into another even with the same peers.
func TestSessionIDSeparatesStreams(t *testing.T) {
	agent := fixedKey(t, agentSeed)
	client := fixedKey(t, clientSeed)

	one, _ := DeriveSessionKeys(agent, client.PublicKey().Bytes(), testSalt, "tab-1")
	two, _ := DeriveSessionKeys(agent, client.PublicKey().Bytes(), testSalt, "tab-2")
	if bytes.Equal(one.ClientToAgent, two.ClientToAgent) {
		t.Fatal("different sessions derived the same key")
	}
}

// A fresh server salt separates connections even when everything else — both
// keypairs and the session id — is identical, which is what keeps the counter
// nonces safe across a reattach that reuses a client ephemeral.
func TestServerSaltSeparatesConnections(t *testing.T) {
	agent := fixedKey(t, agentSeed)
	client := fixedKey(t, clientSeed)

	saltA := bytes.Repeat([]byte{0x01}, SealSaltSize)
	saltB := bytes.Repeat([]byte{0x02}, SealSaltSize)
	one, _ := DeriveSessionKeys(agent, client.PublicKey().Bytes(), saltA, "tab-1")
	two, _ := DeriveSessionKeys(agent, client.PublicKey().Bytes(), saltB, "tab-1")
	if bytes.Equal(one.ClientToAgent, two.ClientToAgent) || bytes.Equal(one.AgentToClient, two.AgentToClient) {
		t.Fatal("different server salts derived the same key")
	}
}

// Both ends must use the same salt to agree: a mismatched salt breaks the
// derivation, which is what makes the server hello a required handshake step
// rather than advisory.
func TestSaltMismatchBreaksAgreement(t *testing.T) {
	agent := fixedKey(t, agentSeed)
	client := fixedKey(t, clientSeed)

	saltA := bytes.Repeat([]byte{0x01}, SealSaltSize)
	saltB := bytes.Repeat([]byte{0x02}, SealSaltSize)
	a, _ := DeriveSessionKeys(agent, client.PublicKey().Bytes(), saltA, "tab-1")
	c, _ := DeriveSessionKeys(client, agent.PublicKey().Bytes(), saltB, "tab-1")
	if bytes.Equal(a.ClientToAgent, c.ClientToAgent) {
		t.Fatal("the two ends agreed on a key despite different salts")
	}
}

// The client's view of the agent's public key is bound into the schedule, so a
// relay that hands the client a substituted key makes the client derive a key
// the real agent never shares — the stream fails closed rather than being read.
func TestAgentKeyIsBoundIntoSchedule(t *testing.T) {
	agent := fixedKey(t, agentSeed)
	client := fixedKey(t, clientSeed)
	imposter := fixedKey(t, "030a11181f262d343b424950575e656c737a81888f969da4abb2b9c0c7ced5dc")

	real, _ := DeriveSessionKeys(agent, client.PublicKey().Bytes(), testSalt, "tab-1")
	// The client, fed the imposter's key by a hostile relay, derives against it.
	swapped, _ := DeriveSessionKeys(client, imposter.PublicKey().Bytes(), testSalt, "tab-1")
	if bytes.Equal(real.ClientToAgent, swapped.ClientToAgent) {
		t.Fatal("a substituted agent key still derived the agent's own key")
	}
}

func TestSealRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, SealKeySize)
	sender, err := NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}

	// A terminal is a long sequence of small records; the counter must keep the
	// two ends in step across all of them.
	for i := range 500 {
		msg := []byte{byte(i), byte(i >> 8), 'x'}
		out, err := receiver.Open(sender.Seal(msg))
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if !bytes.Equal(out, msg) {
			t.Fatalf("record %d round-tripped wrong", i)
		}
	}
}

// The same plaintext must not produce the same ciphertext twice, or an observer
// learns when a keystroke repeats.
func TestSealIsNotDeterministic(t *testing.T) {
	s, _ := NewSealer(bytes.Repeat([]byte{7}, SealKeySize))
	first := s.Seal([]byte("ls\n"))
	second := s.Seal([]byte("ls\n"))
	if bytes.Equal(first, second) {
		t.Fatal("identical plaintexts produced identical records")
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	key := bytes.Repeat([]byte{7}, SealKeySize)
	sender, _ := NewSealer(key)
	receiver, _ := NewSealer(key)

	record := sender.Seal([]byte("whoami\n"))
	tampered := bytes.Clone(record)
	tampered[0] ^= 0x01

	if _, err := receiver.Open(tampered); !errors.Is(err, ErrSealFailed) {
		t.Fatalf("tampered record should fail with ErrSealFailed, got %v", err)
	}
}

// A record replayed at the sender must not open: the two directions use
// different keys, so this is structurally impossible rather than checked.
func TestRecordCannotBeReplayedBackAtSender(t *testing.T) {
	agent := fixedKey(t, agentSeed)
	client := fixedKey(t, clientSeed)
	keys, _ := DeriveSessionKeys(agent, client.PublicKey().Bytes(), testSalt, "tab-1")

	clientSide, _ := NewSealer(keys.ClientToAgent)
	agentReading, _ := NewSealer(keys.ClientToAgent)
	agentWriting, _ := NewSealer(keys.AgentToClient)

	record := clientSide.Seal([]byte("rm -rf /\n"))
	if _, err := agentWriting.Open(record); err == nil {
		t.Fatal("a client record opened under the agent's own send key")
	}
	if _, err := agentReading.Open(record); err != nil {
		t.Fatalf("legitimate record failed: %v", err)
	}
}

// Out-of-order delivery must break the stream rather than be accepted, since a
// silently-tolerated gap is how a dropped record becomes a security question.
func TestOutOfOrderRecordsFail(t *testing.T) {
	key := bytes.Repeat([]byte{7}, SealKeySize)
	sender, _ := NewSealer(key)
	receiver, _ := NewSealer(key)

	first := sender.Seal([]byte("one"))
	second := sender.Seal([]byte("two"))

	if _, err := receiver.Open(second); err == nil {
		t.Fatal("a record delivered out of order was accepted")
	}
	_ = first
}

func TestDeriveRejectsBadPeerKey(t *testing.T) {
	agent := fixedKey(t, agentSeed)
	for _, bad := range [][]byte{nil, {}, bytes.Repeat([]byte{1}, 31), bytes.Repeat([]byte{1}, 33)} {
		if _, err := DeriveSessionKeys(agent, bad, testSalt, "tab"); err == nil {
			t.Errorf("peer key of %d bytes was accepted", len(bad))
		}
	}
}

// A salt of the wrong length is refused rather than stretched or truncated: it
// feeds the key schedule, and silently accepting a malformed one would let a
// broken peer derive an agreed-but-unintended key.
func TestDeriveRejectsBadSalt(t *testing.T) {
	agent := fixedKey(t, agentSeed)
	client := fixedKey(t, clientSeed)
	for _, bad := range [][]byte{nil, {}, bytes.Repeat([]byte{1}, SealSaltSize-1), bytes.Repeat([]byte{1}, SealSaltSize+1)} {
		if _, err := DeriveSessionKeys(agent, client.PublicKey().Bytes(), bad, "tab"); err == nil {
			t.Errorf("salt of %d bytes was accepted", len(bad))
		}
	}
}

// The server hello round-trips as a length-prefixed record, and a reply that is
// not exactly a salt is refused at the handshake.
func TestServerHelloRoundTripAndLength(t *testing.T) {
	var buf bytes.Buffer
	salt := bytes.Repeat([]byte{0x33}, ServerHelloSize)
	if err := WriteServerHello(&buf, salt); err != nil {
		t.Fatal(err)
	}
	got, err := ReadServerHello(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, salt) {
		t.Fatal("server hello did not round-trip")
	}
	if err := WriteServerHello(&buf, []byte{1, 2, 3}); err == nil {
		t.Fatal("writing a short server hello must be refused")
	}
	var short bytes.Buffer
	if err := WriteRecord(&short, bytes.Repeat([]byte{1}, ServerHelloSize-1)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadServerHello(&short); err == nil {
		t.Fatal("a short server hello must be refused")
	}
}

// The fingerprint is deterministic, formatted as four groups of four base32
// characters, and distinguishes different keys — the whole point of showing it
// for out-of-band verification.
func TestFingerprint(t *testing.T) {
	agent := fixedKey(t, agentSeed)
	client := fixedKey(t, clientSeed)
	fp := Fingerprint(agent.PublicKey().Bytes())
	if fp != Fingerprint(agent.PublicKey().Bytes()) {
		t.Fatal("fingerprint is not deterministic")
	}
	if want := "XXXX-XXXX-XXXX-XXXX"; len(fp) != len(want) {
		t.Fatalf("fingerprint %q is not the %d-char grouped form", fp, len(want))
	}
	if Fingerprint(agent.PublicKey().Bytes()) == Fingerprint(client.PublicKey().Bytes()) {
		t.Fatal("different keys produced the same fingerprint")
	}
}

func TestRecordFraming(t *testing.T) {
	var buf bytes.Buffer
	payloads := [][]byte{[]byte("a"), {}, bytes.Repeat([]byte{9}, 4096)}
	for _, p := range payloads {
		if err := WriteRecord(&buf, p); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range payloads {
		got, err := ReadRecord(&buf)
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("record %d mismatched", i)
		}
	}
}

// An oversized declared length must be refused before allocating, or a peer can
// drive the other end's memory with a five-byte header.
func TestReadRecordRefusesOversizedLength(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 0xff, 0xff, 0xff})
	if _, err := ReadRecord(&buf); err == nil {
		t.Fatal("an oversized record length was accepted")
	}
}

// The two ends must agree end to end: derive from opposite sides of the
// agreement, then round-trip real frames through the sealed transport.
func TestSealedStreamRoundTrip(t *testing.T) {
	agent := fixedKey(t, agentSeed)
	client := fixedKey(t, clientSeed)

	agentKeys, err := DeriveSessionKeys(agent, client.PublicKey().Bytes(), testSalt, "tab-1")
	if err != nil {
		t.Fatal(err)
	}
	clientKeys, err := DeriveSessionKeys(client, agent.PublicKey().Bytes(), testSalt, "tab-1")
	if err != nil {
		t.Fatal(err)
	}

	// One pipe per direction, so neither end reads its own writes.
	toAgent, fromClient := net.Pipe()
	defer toAgent.Close()
	defer fromClient.Close()

	clientSide, err := NewSealedStream(fromClient, fromClient,
		clientKeys.ClientToAgent, clientKeys.AgentToClient)
	if err != nil {
		t.Fatal(err)
	}
	agentSide, err := NewSealedStream(toAgent, toAgent,
		agentKeys.AgentToClient, agentKeys.ClientToAgent)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		_ = clientSide.WriteFrame(EncodeData([]byte("whoami\n")))
		_ = clientSide.WriteFrame(EncodeResize(100, 30))
	}()

	got, err := agentSide.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data) != "whoami\n" {
		t.Fatalf("data frame = %q", got.Data)
	}
	got, err = agentSide.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if got.Resize == nil || got.Resize.Cols != 100 || got.Resize.Rows != 30 {
		t.Fatalf("resize frame = %+v", got.Resize)
	}
}

// A hello that is not exactly a public key is refused, so a peer that does not
// speak this protocol fails at the handshake rather than deriving a key from
// nonsense and failing confusingly later.
func TestClientHelloRejectsWrongLength(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRecord(&buf, bytes.Repeat([]byte{1}, SealKeySize-1)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadClientHello(&buf); err == nil {
		t.Fatal("a short client hello must be refused")
	}
	if err := WriteClientHello(&buf, []byte{1, 2, 3}); err == nil {
		t.Fatal("writing a short client hello must be refused")
	}
}

// writeCounter records how many Write calls a record cost, and what they
// carried.
type writeCounter struct {
	bytes.Buffer
	writes int
}

func (w *writeCounter) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

// One record must be one Write. yamux emits a data frame per Stream.Write, and
// its send loop writes that frame's header and body as separate writes on the
// tunnel conn, each of which websocket.NetConn turns into a WebSocket message.
// A record split across two writes therefore cost four messages and two frames,
// one of the frames carrying nothing but a 4-byte length. Every keystroke paid
// it.
func TestWriteRecordIssuesOneWrite(t *testing.T) {
	var w writeCounter
	record := []byte("a terminal record")
	if err := WriteRecord(&w, record); err != nil {
		t.Fatal(err)
	}
	if w.writes != 1 {
		t.Fatalf("WriteRecord made %d writes, want 1", w.writes)
	}
	// And the bytes on the wire are unchanged.
	got, err := ReadRecord(&w.Buffer)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, record) {
		t.Fatalf("round-tripped record = %q, want %q", got, record)
	}
}

func TestWriteFrameIssuesOneWrite(t *testing.T) {
	agent := fixedKey(t, agentSeed)
	client := fixedKey(t, clientSeed)
	keys, err := DeriveSessionKeys(agent, client.PublicKey().Bytes(), testSalt, "one-write")
	if err != nil {
		t.Fatal(err)
	}
	var w writeCounter
	send, err := NewSealedStream(&bytes.Buffer{}, &w, keys.AgentToClient, keys.ClientToAgent)
	if err != nil {
		t.Fatal(err)
	}
	if err := send.WriteFrame(EncodeData([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	if w.writes != 1 {
		t.Fatalf("WriteFrame made %d writes, want 1", w.writes)
	}

	// And the receiving end, with the directions the other way round, still
	// reads exactly what was sent.
	recv, err := NewSealedStream(&w.Buffer, &bytes.Buffer{}, keys.ClientToAgent, keys.AgentToClient)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := recv.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if string(frame.Data) != "x" {
		t.Fatalf("frame data = %q, want %q", frame.Data, "x")
	}
}
