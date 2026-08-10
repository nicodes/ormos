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

// testSalt is a fixed per-connection salt so derivations in tests are
// reproducible (and matchable against the app's cross-language vectors).
var testSalt = bytes.Repeat([]byte{0x5a}, ServerSaltSize)

// derive calls DeriveSessionKeys from either side of the fixed test agreement.
func derive(t *testing.T, priv *ecdh.PrivateKey, agentPub, clientPub []byte, sessionID string, salt []byte) *SessionKeys {
	t.Helper()
	keys, err := DeriveSessionKeys(priv, agentPub, clientPub, sessionID, salt)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	return keys
}

// Both ends must derive the same two keys from opposite sides of the agreement.
func TestDeriveSessionKeysAgree(t *testing.T) {
	agent := fixedKey(t, agentSeed)
	client := fixedKey(t, clientSeed)
	agentPub, clientPub := agent.PublicKey().Bytes(), client.PublicKey().Bytes()

	a := derive(t, agent, agentPub, clientPub, "tab-1", testSalt)
	c := derive(t, client, agentPub, clientPub, "tab-1", testSalt)
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
	agentPub, clientPub := agent.PublicKey().Bytes(), client.PublicKey().Bytes()

	one := derive(t, agent, agentPub, clientPub, "tab-1", testSalt)
	two := derive(t, agent, agentPub, clientPub, "tab-2", testSalt)
	if bytes.Equal(one.ClientToAgent, two.ClientToAgent) {
		t.Fatal("different sessions derived the same key")
	}
}

// The per-connection salt is the HKDF salt, so two connections between the same
// peers in the same session never share a key — and with it a nonce counter —
// even if the client reused its ephemeral key.
func TestServerSaltSeparatesConnections(t *testing.T) {
	agent := fixedKey(t, agentSeed)
	client := fixedKey(t, clientSeed)
	agentPub, clientPub := agent.PublicKey().Bytes(), client.PublicKey().Bytes()

	other := bytes.Repeat([]byte{0x41}, ServerSaltSize)
	one := derive(t, agent, agentPub, clientPub, "tab-1", testSalt)
	two := derive(t, agent, agentPub, clientPub, "tab-1", other)
	if bytes.Equal(one.ClientToAgent, two.ClientToAgent) || bytes.Equal(one.AgentToClient, two.AgentToClient) {
		t.Fatal("different server salts derived the same key")
	}
}

// Both public keys are bound into the schedule: a relay that substitutes the
// agent's published key with its own derives — with the browser — keys the real
// agent does not hold, so the first record fails authentication instead of
// opening a session the middle can read.
func TestSubstitutedAgentKeyDerivesDifferently(t *testing.T) {
	agent := fixedKey(t, agentSeed)
	impostor := fixedKey(t, clientSeed) // any key that is not the agent's
	client := fixedKey(t, "f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff000102030405060708090a0b0c0d0e0f")
	clientPub := client.PublicKey().Bytes()

	honest := derive(t, client, agent.PublicKey().Bytes(), clientPub, "tab-1", testSalt)
	pinned := derive(t, client, impostor.PublicKey().Bytes(), clientPub, "tab-1", testSalt)
	if bytes.Equal(honest.ClientToAgent, pinned.ClientToAgent) {
		t.Fatal("a substituted agent key derived the same client-to-agent key")
	}

	// And the agent's own derivation under its real key matches only the honest
	// client's, never the substituted transcript.
	agentKeys := derive(t, agent, agent.PublicKey().Bytes(), clientPub, "tab-1", testSalt)
	if !bytes.Equal(agentKeys.ClientToAgent, honest.ClientToAgent) {
		t.Fatal("agent and honest client disagree")
	}
}

// The private key must be the half of one of the two public keys; otherwise the
// caller has swapped or mismatched the transcript and would derive a key the
// peer does not hold.
func TestDeriveRejectsMismatchedOwnKey(t *testing.T) {
	agent := fixedKey(t, agentSeed)
	client := fixedKey(t, clientSeed)
	stranger := fixedKey(t, "f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff000102030405060708090a0b0c0d0e0f")

	if _, err := DeriveSessionKeys(stranger, agent.PublicKey().Bytes(), client.PublicKey().Bytes(), "tab", testSalt); err == nil {
		t.Fatal("a private key matching neither public key was accepted")
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
	keys := derive(t, agent, agent.PublicKey().Bytes(), client.PublicKey().Bytes(), "tab-1", testSalt)

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
	client := fixedKey(t, clientSeed)
	good := agent.PublicKey().Bytes()
	for _, bad := range [][]byte{nil, {}, bytes.Repeat([]byte{1}, 31), bytes.Repeat([]byte{1}, 33)} {
		if _, err := DeriveSessionKeys(agent, good, bad, "tab", testSalt); err == nil {
			t.Errorf("client key of %d bytes was accepted", len(bad))
		}
		if _, err := DeriveSessionKeys(agent, bad, client.PublicKey().Bytes(), "tab", testSalt); err == nil {
			t.Errorf("agent key of %d bytes was accepted", len(bad))
		}
	}
	for _, bad := range [][]byte{nil, {}, bytes.Repeat([]byte{1}, 31)} {
		if _, err := DeriveSessionKeys(agent, good, client.PublicKey().Bytes(), "tab", bad); err == nil {
			t.Errorf("salt of %d bytes was accepted", len(bad))
		}
	}
}

// MaxSealedRecord must fit exactly one maximal sealed record: a frame with a
// MaxFrameSize payload, its header, and the authentication tag. The relay's
// WebSocket read limit is derived from this constant, so if it drifted below
// the real size a maximal legitimate paste would be killed mid-stream.
func TestMaxSealedRecordComposition(t *testing.T) {
	if want := MaxFrameSize + frameHeaderSize + sealOverhead; MaxSealedRecord != want {
		t.Fatalf("MaxSealedRecord = %d, want MaxFrameSize + frameHeaderSize + sealOverhead = %d", MaxSealedRecord, want)
	}
	s, err := NewSealer(bytes.Repeat([]byte{7}, SealKeySize))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s.Seal(EncodeData(make([]byte, MaxFrameSize)))); got != MaxSealedRecord {
		t.Fatalf("a maximal sealed record is %d bytes, MaxSealedRecord = %d", got, MaxSealedRecord)
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
	agentPub, clientPub := agent.PublicKey().Bytes(), client.PublicKey().Bytes()

	agentKeys := derive(t, agent, agentPub, clientPub, "tab-1", testSalt)
	clientKeys := derive(t, client, agentPub, clientPub, "tab-1", testSalt)

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

// The server hello carries the per-connection salt and answers to the same
// discipline as the client hello: exactly one salt-sized record, or refusal.
func TestServerHelloRoundTrip(t *testing.T) {
	salt, err := GenerateServerSalt()
	if err != nil {
		t.Fatal(err)
	}
	if len(salt) != ServerSaltSize {
		t.Fatalf("salt is %d bytes, want %d", len(salt), ServerSaltSize)
	}
	var buf bytes.Buffer
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
	buf.Reset()
	if err := WriteRecord(&buf, bytes.Repeat([]byte{1}, ServerSaltSize+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadServerHello(&buf); err == nil {
		t.Fatal("a long server hello must be refused")
	}
}
