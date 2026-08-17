package relay

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The tunnel handshake header NAMES, pinned to their wire spellings.
//
// Both constants are single-sourced in this package and imported by the agent
// (internal/system/system.go sets both when it dials) and by the hosted relay,
// so a rename cannot make the two halves of one build disagree. What it can do
// is make this build disagree with the OTHER version already deployed: the
// relay in production and the agents on `@latest` are separate binaries built
// from separate commits, and a shared-module bump that carries a renamed header
// compiles cleanly on both sides while silently ending compatibility with
// everything running the previous spelling.
//
// The failure is quiet in each case, which is why the names are worth recording
// even though neither has any behaviour to exercise:
//
//   - PublicKeyHeader — the relay simply never calls SetSystemPubKey, browsers
//     keep sealing to a stale key, and terminals stop opening with no error
//     anywhere.
//   - StreamFenceVersionHeader — absence is the reserved v0.1.5 sentinel, so a
//     renamed header does not fail, it makes every current agent look like a
//     released legacy one and silently gives up the action fences (#400/#401).
//
// This duplicates the production constants, which is the entire mechanism: the
// test's copy is the record of what was deployed, and the constant is what the
// next build will send. A header name derives from nothing — it is not true or
// false, only the same or not the same as what the other side already shipped —
// so there is no independent oracle to exercise instead, and writing the literal
// down is the only check available. Contrast MaxTerminalDim, where
// TestValidTerminalSize can round-trip the bound through the uint16 that
// TIOCSWINSZ takes and let the reason do the asserting; nothing equivalent
// exists for a string that means "what the relay is listening for".
//
// What this pin does NOT cover, so it is not mistaken for more:
//
//   - Whether the agent still SENDS either header. Deleting an entry from
//     connectAndServe's HTTPHeader map is invisible here and produces exactly
//     the two failures listed above. That is covered separately, by
//     TestAgentDialAdvertisesItsKeyAndFenceVersion in internal/system.
//   - A rename on the relay's side. This binds the constant, not the relay's
//     code; if the hosted relay ever hardcoded its own literal instead of
//     importing this one, the pin would be blind to the divergence and could
//     not fail.
//   - Case. HTTP header names are case-insensitive on the wire and a reader
//     using http.Header.Get canonicalises, so a case-only respelling would stay
//     compatible yet fail this test. Stricter than the contract, in the safe
//     direction.
//
// So: changing a spelling here is meant to be a deliberate act that fails this
// test and sends you to the rollout ordering in the README's protocol
// compatibility section, not something a rename tool does on the way past.
func TestTunnelHeaderNamesArePinnedToTheirWireSpellings(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"PublicKeyHeader", PublicKeyHeader, "X-Ormos-Public-Key"},
		{"StreamFenceVersionHeader", StreamFenceVersionHeader, "X-Ormos-Stream-Fence-Version"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want the deployed wire spelling %q. "+
				"Renaming a handshake header breaks compatibility with the released agent "+
				"and the deployed relay; see the rollout ordering under Protocol compatibility in README.md.",
				tc.name, tc.got, tc.want)
		}
	}
}

func TestCurrentAgentAdvertisesOnlyV2(t *testing.T) {
	if StreamFenceVersionLegacyV0 != "" {
		t.Fatalf("legacy v0 sentinel = %q, want header absence", StreamFenceVersionLegacyV0)
	}
	if StreamFenceVersion != StreamFenceVersionV2 {
		t.Fatalf("advertised stream-fence version = %q, want v2 %q", StreamFenceVersion, StreamFenceVersionV2)
	}
}

// The wire values of four groups of protocol string, pinned to their literals:
// the stream-fence versions, the StreamKinds, the ActionAck statuses, and the
// terminal seal's HKDF label. Not every string in this package — see the list of
// what is NOT pinned, below, which is the boundary this test does not cross.
//
// TestCurrentAgentAdvertisesOnlyV2 above pins the fence version's alias
// RELATIONSHIP (StreamFenceVersion == StreamFenceVersionV2) and the empty-string
// legacy sentinel; neither it nor anything else recorded what "2" actually is,
// so respelling it "v2" kept the whole module green (nicodes/ormos-be#421).
//
// The argument is the one TestTunnelHeaderNamesArePinnedToTheirWireSpellings
// makes for the header names, and it applies unchanged: a value here derives
// from nothing, so it is not true or false, only the same or not the same as
// what the other side already shipped. Writing the literal down is the only
// check available.
//
// Three of these fail worse than a plain mismatch:
//
//   - sealInfo (seal.go), and it is the worst of the four groups by some
//     distance. It is not sent anywhere: it is the HKDF label that
//     scheduleInfo feeds into hkdf.Key, so both ends of a terminal seal derive
//     their keys from this exact byte string — the agent here, and the browser's
//     seal.ts named in seal.go. Change it on one side and every
//     SealedStream.ReadFrame fails its AEAD open on records that arrived looking
//     perfectly well-formed: no terminal works at all, on any system, with no
//     error naming the cause. It is the failure #423 was written about, reached
//     from a third direction.
//
//     It is pinned because the value ENDS IN A VERSION and its own comment says
//     to bump that version when the schedule changes, which is exactly right and
//     exactly the invitation. A bump is a coordinated two-sided deployment, not a
//     tidy-up, and nothing said so where the bumper would see it.
//
//   - a fence version. The negotiation treats an UNRECOGNISED value differently
//     from absence, and absence is the reserved legacy sentinel, so a respelt
//     value is neither the current protocol nor the documented fallback.
//
//   - a StreamKind. serveStream switches on it, and for an unrecognised kind no
//     handler runs and NO AUDIT ENTRY IS RECORDED, so the request leaves no trace
//     in the record the agent keeps of what the relay asked it to do. It is not
//     silent at the console — internal/system/streams.go logs `unknown stream
//     kind`, and run.go echoes the log ring to stderr on every non-TTY start — so
//     a headless agent does say something. What it does not do is act, or
//     remember. What the relay makes of the closed stream it gets back is in
//     another repository and is not asserted here.
//
// StreamFenceVersionV1 is here even though this agent only ever advertises v2,
// because a deployed relay still reads it to decide what an older agent can be
// asked to do.
//
// What this does NOT cover:
//
//   - the relay's side. This binds the constants; if the hosted relay hardcoded
//     its own literals rather than importing these, the pin is blind to the
//     divergence and cannot fail.
//   - whether the agent still SENDS any of them. That is a different gap, and
//     for the two handshake headers it is covered by
//     TestAgentDialAdvertisesItsKeyAndFenceVersion in internal/system.
//   - the rest of this package's wire strings, which are a real and open gap
//     rather than a decision that they do not matter: contract.go's
//     DeviceStatusPending/Expired/Approved and its 27 DTO JSON tags, the
//     Resize and activityFrame tags one screen below StreamHeader, and the
//     numeric termTag values. All of them are tracked on
//     nicodes/ormos-be#433. Nothing above guards any of them, and a reader who
//     needs one of them guarded must add it rather than assume this table did.
func TestWireStringValuesArePinnedToTheirLiterals(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"StreamFenceVersionV1", StreamFenceVersionV1, "1"},
		{"StreamFenceVersionV2", StreamFenceVersionV2, "2"},
		{"KindTerminal", string(KindTerminal), "terminal"},
		{"KindProxy", string(KindProxy), "proxy"},
		{"KindListPorts", string(KindListPorts), "listports"},
		{"KindShutdown", string(KindShutdown), "shutdown"},
		{"KindEvent", string(KindEvent), "event"},
		{"ActionAckSuccess", string(ActionAckSuccess), "success"},
		{"ActionAckRefused", string(ActionAckRefused), "refused"},
		{"ActionAckExpired", string(ActionAckExpired), "expired"},
		{"sealInfo", sealInfo, "ormos terminal seal v2"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want the deployed wire value %q. "+
				"Respelling one of these ends compatibility with whatever is already deployed on the "+
				"other side -- the released agent, the hosted relay, or the browser's seal -- and "+
				"nothing else fails when it does; see the rollout ordering under Protocol "+
				"compatibility in README.md.",
				tc.name, tc.got, tc.want)
		}
	}
}

// The StreamHeader and ActionAck JSON tags, pinned against literal wire bytes.
//
// TestHeaderRoundTrip and TestShutdownActionAckRoundTripAndBinding marshal and
// unmarshal through the SAME struct, so a renamed tag round-trips perfectly and
// passes them both — while an agent and a relay built from different module
// versions silently DROP the field. That is the general shape of this defect
// class: the test that looks like it covers the wire format and structurally
// cannot (nicodes/ormos-be#421).
//
// So neither direction is checked against the other. Both are checked against
// one hand-written literal:
//
//   - decode. Every field of the recorded payload must land. A renamed tag
//     leaves its field at the zero value, which the comparison catches.
//   - encode. The exact bytes Marshal produces. This is what catches a
//     CASE-only respelling, which the decode cannot see: encoding/json accepts
//     a case-insensitive key match, so `"session_ID"` would still decode
//     `session_id`. Both ends decode with encoding/json today, so a case change
//     is in fact still compatible — the pin is stricter than the contract, in
//     the safe direction, exactly as the header-name pin is about case.
//
// The literals are written by hand, not pasted from Marshal's output: a literal
// generated by the code it checks records whatever that code currently does.
//
// The encode direction is exact-bytes, so adding a PLAIN field to either struct
// fails it, and so does reordering the existing ones. That much is the intent
// rather than a maintenance cost to design away: a new field on a struct both
// binaries decode is precisely the change that should stop and read the rollout
// ordering under Protocol compatibility in README.md.
//
// But adding a field tagged `,omitempty` does NOT fail it, and that is the way a
// field is most likely to be added, since it is the house style on seven of
// StreamHeader's eight. Marshal omits the new zero field, so the exact-bytes
// comparison never sees it, and the decode comparison sees zero on both sides.
// The reason is the same one the omitempty caveat further down gives; the two
// belong together, because the unqualified version of this sentence is the one a
// reader would have acted on.
//
// Every tag on both structs is covered, which is more than the five the issue
// names — the payload has to be complete for the exact-bytes comparison to mean
// anything, and once complete it costs nothing extra. Two values ride along:
// `"kind":"terminal"` and `"status":"success"` are compared against KindTerminal
// and ActionAckSuccess, so those two constants are pinned here as well as in
// TestWireStringValuesArePinnedToTheirLiterals above.
//
// Cwd is the field whose loss is easiest to misdiagnose, so the reason is
// recorded where the tag is pinned. terminalAllowed (internal/system/policy.go)
// fails CLOSED: with allowedRoots configured an empty cwd is refused outright,
// and with it unset the shell's default was permitted anyway. So a dropped
// `cwd` is a correctness and availability bug — terminals stop opening, or open
// somewhere other than the project the user picked, with no error saying why —
// and NOT a confinement bypass.
//
// What this does NOT cover:
//
//   - the relay's side, for the same reason as every other pin here: it binds
//     these struct tags, not the relay's code.
//   - omitempty. Every field is populated, so what the wire looks like when a
//     field is zero is not asserted, and dropping an `omitempty` would not fail
//     this test.
func TestStreamHeaderAndActionAckTagsArePinnedToALiteralPayload(t *testing.T) {
	fence := strings.Repeat("a", 40)

	t.Run("StreamHeader", func(t *testing.T) {
		const wire = `{"kind":"terminal","port":8080,"cols":120,"rows":40,` +
			`"cwd":"/code/app","session_id":"project:tab",` +
			`"action_fence":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
			`"not_after_milli":1700000000123}`
		want := StreamHeader{
			Kind: KindTerminal, Port: 8080, Cols: 120, Rows: 40,
			Cwd: "/code/app", SessionID: "project:tab",
			ActionFence: fence, NotAfterMilli: 1_700_000_000_123,
		}

		var got StreamHeader
		if err := json.Unmarshal([]byte(wire), &got); err != nil {
			t.Fatalf("decoding the recorded wire header: %v", err)
		}
		if got != want {
			t.Errorf("the recorded wire header decoded to\n  %+v\nwant\n  %+v\n"+
				"a field left at its zero value is a renamed tag: the relay and the agent are "+
				"separate binaries from separate commits, so the field is silently dropped "+
				"rather than refused — a lost cwd opens the terminal somewhere else or not at "+
				"all, a lost session_id defeats the ticket's one-tab binding, and a lost "+
				"action_fence or not_after_milli unbounds the action the stream carries.",
				got, want)
		}

		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != wire {
			t.Errorf("StreamHeader marshalled to\n  %s\nwant the recorded wire payload\n  %s",
				encoded, wire)
		}
	})

	t.Run("ActionAck", func(t *testing.T) {
		const wire = `{"action_fence":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
			`"not_after_milli":1700000000123,"status":"success"}`
		want := ActionAck{ActionFence: fence, NotAfterMilli: 1_700_000_000_123, Status: ActionAckSuccess}

		var got ActionAck
		if err := json.Unmarshal([]byte(wire), &got); err != nil {
			t.Fatalf("decoding the recorded wire ack: %v", err)
		}
		if got != want {
			t.Errorf("the recorded wire ack decoded to\n  %+v\nwant\n  %+v\n"+
				"the ack echoes the fence and deadline to bind the terminal result to the exact "+
				"shutdown that asked for it, so a renamed tag turns that binding into two zero "+
				"values that match nothing.",
				got, want)
		}

		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != wire {
			t.Errorf("ActionAck marshalled to\n  %s\nwant the recorded wire payload\n  %s",
				encoded, wire)
		}
	})
}

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

func TestAcceptedStreamFenceCarriesOneClampedDeadline(t *testing.T) {
	accepted := time.Now()
	header := StreamHeader{
		Kind: KindProxy, ActionFence: strings.Repeat("a", 40),
		NotAfterMilli: accepted.Add(200 * time.Millisecond).UnixMilli(),
	}
	deadline, err := AcceptStreamFence(header, accepted)
	if err != nil {
		t.Fatal(err)
	}
	// A wall rollback makes the original absolute timestamp appear farther in
	// the future; a wall jump forward makes it appear expired. Neither mutation
	// can affect the accepted monotonic deadline carried by the stream.
	header.NotAfterMilli = accepted.Add(time.Hour).UnixMilli()
	if err := ValidateStreamFenceDeadline(deadline, accepted.Add(100*time.Millisecond)); err != nil {
		t.Fatalf("wall rollback shrank a live accepted fence: %v", err)
	}
	header.NotAfterMilli = accepted.Add(-time.Hour).UnixMilli()
	if err := ValidateStreamFenceDeadline(deadline, accepted.Add(250*time.Millisecond)); !IsStreamFenceExpired(err) {
		t.Fatalf("wall jump extended an expired accepted fence: %v", err)
	}

	far := header
	far.NotAfterMilli = accepted.Add(2 * time.Minute).UnixMilli()
	clamped, err := AcceptStreamFence(far, accepted)
	if err == nil || !clamped.Equal(accepted.Add(maxActionFenceFuture)) {
		t.Fatalf("far-future fence = (%s, %v), want clamped %s and refusal", clamped, err, accepted.Add(maxActionFenceFuture))
	}
}

func TestShutdownActionAckRoundTripAndBinding(t *testing.T) {
	header := StreamHeader{
		Kind: KindShutdown, ActionFence: strings.Repeat("a", 40),
		NotAfterMilli: time.Now().Add(time.Second).UnixMilli(),
	}
	want := NewActionAck(header, ActionAckSuccess)
	var buf bytes.Buffer
	if err := WriteActionAck(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadActionAck(&buf)
	if err != nil || got != want {
		t.Fatalf("ack = (%+v, %v), want %+v", got, err, want)
	}
	if err := ValidateActionAck(header, got); err != nil {
		t.Fatalf("valid ack: %v", err)
	}
	got.ActionFence = strings.Repeat("b", 40)
	if err := ValidateActionAck(header, got); err == nil {
		t.Fatal("ack for another durable fence was accepted")
	}
	got = want
	got.Status = "maybe"
	if err := ValidateActionAck(header, got); err == nil {
		t.Fatal("unknown ack status was accepted")
	}
}

// This covers the framing — the newline terminator, and that bytes buffered
// past it survive for the caller — and NOT the wire format. It marshals and
// unmarshals through the same struct, so every tag could be renamed together and
// it would still pass. The tags are pinned by
// TestStreamHeaderAndActionAckTagsArePinnedToALiteralPayload instead.
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
