//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nicodes/ormos/relay"
)

// fakeDeviceRelay scripts the two device-flow endpoints. startFn and pollFn
// receive the 1-based number of times their endpoint has been hit, so a test
// can script "pending, pending, approved" without extra state.
type fakeDeviceRelay struct {
	srv    *httptest.Server
	starts atomic.Int32
	polls  atomic.Int32
	start  func(n int32) relay.DeviceStartResponse
	poll   func(deviceCode string, n int32, w http.ResponseWriter, r *http.Request) relay.DevicePollResponse
}

func newFakeDeviceRelay(t *testing.T, f *fakeDeviceRelay) {
	t.Helper()
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/device/start":
			var req relay.DeviceStartRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decoding start request: %v", err)
			}
			if req.ClientID == "" || req.Hostname == "" {
				t.Errorf("start request missing client_id or hostname: %+v", req)
			}
			_ = json.NewEncoder(w).Encode(f.start(f.starts.Add(1)))
		case "/device/poll":
			var req relay.DevicePollRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decoding poll request: %v", err)
			}
			resp := f.poll(req.DeviceCode, f.polls.Add(1), w, r)
			if resp.Status != "" {
				_ = json.NewEncoder(w).Encode(resp)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
}

func startResponse(userCode, deviceCode string) relay.DeviceStartResponse {
	return relay.DeviceStartResponse{
		UserCode:        userCode,
		DeviceCode:      deviceCode,
		VerificationURL: "https://app.example.test/pair",
		ExpiresIn:       60,
		Interval:        1,
	}
}

func TestTokenValidClassifiesStartupResponses(t *testing.T) {
	oldClient := httpClient
	t.Cleanup(func() { httpClient = oldClient })

	tests := []struct {
		name      string
		status    int
		wantValid bool
		wantErr   string
	}{
		{"accepted", http.StatusOK, true, ""},
		{"revoked", http.StatusUnauthorized, false, ""},
		{"throttled", http.StatusTooManyRequests, false, "throttled"},
		{"unavailable", http.StatusServiceUnavailable, false, "unavailable"},
		{"unexpected client response", http.StatusForbidden, false, "failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var auth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth = r.Header.Get("Authorization")
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			httpClient = srv.Client()
			valid, err := tokenValid("ws"+strings.TrimPrefix(srv.URL, "http"), "startup-token")
			if valid != tc.wantValid {
				t.Fatalf("valid = %v, want %v", valid, tc.wantValid)
			}
			if tc.wantErr == "" && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want text %q", err, tc.wantErr)
			}
			if auth != "Bearer startup-token" {
				t.Fatalf("Authorization = %q", auth)
			}
		})
	}
}

func TestTokenValidTreatsTransportFailureAsUnavailable(t *testing.T) {
	oldClient := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})}
	t.Cleanup(func() { httpClient = oldClient })
	valid, err := tokenValid("wss://relay.example.test", "saved-token")
	if valid || err == nil || !strings.Contains(err.Error(), "network down") {
		t.Fatalf("tokenValid transport failure = (%v, %v), want false and preserved error", valid, err)
	}
}

func TestStartupRevocationClearsOnlySavedAuthentication(t *testing.T) {
	withTempConfigDir(t)
	oldClient := httpClient
	t.Cleanup(func() { httpClient = oldClient })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	httpClient = srv.Client()

	cfg := systemConfig{
		RelayURL:     "ws" + strings.TrimPrefix(srv.URL, "http"),
		ClientID:     "stable-client",
		SystemID:     "revoked-system",
		Email:        "owner@example.test",
		PairingToken: "revoked-token",
	}
	if err := saveConfigFile(cfg); err != nil {
		t.Fatal(err)
	}
	needsLogin, err := loginRequired(&cfg)
	if err != nil || !needsLogin {
		t.Fatalf("loginRequired = (%v, %v), want (true, nil)", needsLogin, err)
	}
	saved, err := loadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PairingToken != "" || saved.PairingToken != "" {
		t.Fatal("authoritative startup revocation left the pairing token in memory or on disk")
	}
	if cfg.SystemID != "" || cfg.Email != "" || saved.SystemID != "" || saved.Email != "" {
		t.Fatal("authoritative startup revocation left saved account authentication metadata")
	}
	if cfg.ClientID != "stable-client" || saved.ClientID != "stable-client" ||
		cfg.RelayURL != "ws"+strings.TrimPrefix(srv.URL, "http") || saved.RelayURL != cfg.RelayURL {
		t.Fatal("clearing startup authentication changed the stable client identity or relay URL")
	}
}

func TestStartupInconclusiveCheckPreservesSavedAuthentication(t *testing.T) {
	withTempConfigDir(t)
	oldClient := httpClient
	t.Cleanup(func() { httpClient = oldClient })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	httpClient = srv.Client()

	cfg := systemConfig{
		RelayURL:     "ws" + strings.TrimPrefix(srv.URL, "http"),
		ClientID:     "stable-client",
		SystemID:     "current-system",
		Email:        "owner@example.test",
		PairingToken: "current-token",
	}
	if err := saveConfigFile(cfg); err != nil {
		t.Fatal(err)
	}
	needsLogin, err := loginRequired(&cfg)
	if err == nil || needsLogin {
		t.Fatalf("loginRequired = (%v, %v), want (false, throttled error)", needsLogin, err)
	}
	saved, err := loadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PairingToken != "current-token" || saved.PairingToken != "current-token" ||
		cfg.SystemID != "current-system" || saved.SystemID != "current-system" ||
		cfg.Email != "owner@example.test" || saved.Email != "owner@example.test" {
		t.Fatal("inconclusive startup check destroyed saved authentication")
	}
}

// A code that stays pending for one poll and is then approved must yield the
// provisioning payload, with the code shown exactly once.
func TestDeviceLoginPendingThenApproved(t *testing.T) {
	f := &fakeDeviceRelay{
		start: func(n int32) relay.DeviceStartResponse { return startResponse("ABCD-1234", "dev-1") },
		poll: func(deviceCode string, n int32, w http.ResponseWriter, r *http.Request) relay.DevicePollResponse {
			if deviceCode != "dev-1" {
				t.Errorf("poll device_code = %q, want dev-1", deviceCode)
			}
			if n < 2 {
				return relay.DevicePollResponse{Status: relay.DeviceStatusPending}
			}
			return relay.DevicePollResponse{
				Status:            relay.DeviceStatusApproved,
				ProvisionResponse: relay.ProvisionResponse{SystemID: "sys1", Token: "tok-1", Name: "System 1"},
			}
		},
	}
	newFakeDeviceRelay(t, f)

	var shown []string
	out, err := runDeviceLogin(context.Background(), f.srv.URL,
		relay.DeviceStartRequest{ClientID: "c", Hostname: "h"},
		func(s relay.DeviceStartResponse, restarted bool) { shown = append(shown, s.UserCode) })
	if err != nil {
		t.Fatal(err)
	}
	if out.Token != "tok-1" || out.SystemID != "sys1" || out.Name != "System 1" {
		t.Fatalf("approved payload = %+v", out)
	}
	if len(shown) != 1 || shown[0] != "ABCD-1234" {
		t.Fatalf("shown codes = %v, want [ABCD-1234]", shown)
	}
}

// An expired code must start a fresh round: the new code is shown flagged as a
// restart, and approval on the second round completes the login.
func TestDeviceLoginExpiredRestarts(t *testing.T) {
	f := &fakeDeviceRelay{
		start: func(n int32) relay.DeviceStartResponse {
			if n == 1 {
				return startResponse("OLD-CODE", "dev-old")
			}
			return startResponse("NEW-CODE", "dev-new")
		},
		poll: func(deviceCode string, n int32, w http.ResponseWriter, r *http.Request) relay.DevicePollResponse {
			if deviceCode == "dev-old" {
				return relay.DevicePollResponse{Status: relay.DeviceStatusExpired}
			}
			return relay.DevicePollResponse{
				Status:            relay.DeviceStatusApproved,
				ProvisionResponse: relay.ProvisionResponse{SystemID: "sys2", Token: "tok-2", Name: "System 2"},
			}
		},
	}
	newFakeDeviceRelay(t, f)

	type show struct {
		code      string
		restarted bool
	}
	var shows []show
	out, err := runDeviceLogin(context.Background(), f.srv.URL,
		relay.DeviceStartRequest{ClientID: "c", Hostname: "h"},
		func(s relay.DeviceStartResponse, restarted bool) {
			shows = append(shows, show{code: s.UserCode, restarted: restarted})
		})
	if err != nil {
		t.Fatal(err)
	}
	if out.Token != "tok-2" {
		t.Fatalf("token = %q, want tok-2", out.Token)
	}
	if len(shows) != 2 || shows[0] != (show{"OLD-CODE", false}) || shows[1] != (show{"NEW-CODE", true}) {
		t.Fatalf("shows = %+v, want old then restarted new", shows)
	}
	if f.starts.Load() != 2 {
		t.Fatalf("starts = %d, want 2", f.starts.Load())
	}
}

// A code the relay never expires but lets run past its expires_in lifetime is
// treated as expired: the flow restarts with a fresh code.
func TestDeviceLoginDeadlineExpiryRestarts(t *testing.T) {
	f := &fakeDeviceRelay{
		start: func(n int32) relay.DeviceStartResponse {
			if n == 1 {
				s := startResponse("SHORT-LIVED", "dev-short")
				s.ExpiresIn = 1 // deadline passes while the relay says pending
				return s
			}
			return startResponse("FRESH-CODE", "dev-fresh")
		},
		poll: func(deviceCode string, n int32, w http.ResponseWriter, r *http.Request) relay.DevicePollResponse {
			if deviceCode == "dev-short" {
				return relay.DevicePollResponse{Status: relay.DeviceStatusPending}
			}
			return relay.DevicePollResponse{
				Status:            relay.DeviceStatusApproved,
				ProvisionResponse: relay.ProvisionResponse{SystemID: "sys4", Token: "tok-4", Name: "System 4"},
			}
		},
	}
	newFakeDeviceRelay(t, f)

	var restarts atomic.Int32
	out, err := runDeviceLogin(context.Background(), f.srv.URL,
		relay.DeviceStartRequest{ClientID: "c", Hostname: "h"},
		func(s relay.DeviceStartResponse, restarted bool) {
			if restarted {
				restarts.Add(1)
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	if out.Token != "tok-4" {
		t.Fatalf("token = %q, want tok-4", out.Token)
	}
	if restarts.Load() != 1 {
		t.Fatalf("restarts = %d, want 1 (deadline expiry)", restarts.Load())
	}
}

// A relay that expires every code instantly must be met with backoff, not a
// hot loop: the second round starts at least a second after the first.
func TestDeviceLoginInstantExpiryBacksOff(t *testing.T) {
	f := &fakeDeviceRelay{
		start: func(n int32) relay.DeviceStartResponse { return startResponse("ABCD-1234", "dev-1") },
		poll: func(deviceCode string, n int32, w http.ResponseWriter, r *http.Request) relay.DevicePollResponse {
			if n == 1 {
				return relay.DevicePollResponse{Status: relay.DeviceStatusExpired}
			}
			return relay.DevicePollResponse{
				Status:            relay.DeviceStatusApproved,
				ProvisionResponse: relay.ProvisionResponse{SystemID: "sys5", Token: "tok-5", Name: "System 5"},
			}
		},
	}
	newFakeDeviceRelay(t, f)

	began := time.Now()
	out, err := runDeviceLogin(context.Background(), f.srv.URL,
		relay.DeviceStartRequest{ClientID: "c", Hostname: "h"},
		func(relay.DeviceStartResponse, bool) {})
	if err != nil {
		t.Fatal(err)
	}
	if out.Token != "tok-5" {
		t.Fatalf("token = %q, want tok-5", out.Token)
	}
	if elapsed := time.Since(began); elapsed < time.Second {
		t.Fatalf("instant-expiry restart happened after %s, want >= 1s of backoff", elapsed)
	}
}

// A relay advertising a zero poll interval is clamped to minPollInterval, not
// polled in a busy loop.
func TestDeviceLoginClampsZeroInterval(t *testing.T) {
	f := &fakeDeviceRelay{
		start: func(n int32) relay.DeviceStartResponse {
			s := startResponse("ABCD-1234", "dev-1")
			s.Interval = 0
			return s
		},
		poll: func(deviceCode string, n int32, w http.ResponseWriter, r *http.Request) relay.DevicePollResponse {
			if n < 3 {
				return relay.DevicePollResponse{Status: relay.DeviceStatusPending}
			}
			return relay.DevicePollResponse{
				Status:            relay.DeviceStatusApproved,
				ProvisionResponse: relay.ProvisionResponse{SystemID: "sys6", Token: "tok-6", Name: "System 6"},
			}
		},
	}
	newFakeDeviceRelay(t, f)

	began := time.Now()
	out, err := runDeviceLogin(context.Background(), f.srv.URL,
		relay.DeviceStartRequest{ClientID: "c", Hostname: "h"},
		func(relay.DeviceStartResponse, bool) {})
	if err != nil {
		t.Fatal(err)
	}
	if out.Token != "tok-6" {
		t.Fatalf("token = %q, want tok-6", out.Token)
	}
	// Three polls with two clamped waits between them: anything meaningfully
	// under 2s means the interval was not floored at a second.
	if elapsed := time.Since(began); elapsed < 2*time.Second {
		t.Fatalf("3 polls took %s, want >= 2s with the 1s clamp", elapsed)
	}
}

// A poll that dies on the wire (connection closed mid-request) is retried on
// the same code instead of aborting the pairing.
func TestDeviceLoginNetworkErrorRetries(t *testing.T) {
	f := &fakeDeviceRelay{
		start: func(n int32) relay.DeviceStartResponse { return startResponse("ABCD-1234", "dev-1") },
		poll: func(deviceCode string, n int32, w http.ResponseWriter, r *http.Request) relay.DevicePollResponse {
			if n == 1 {
				// Kill the connection without a response: the client sees EOF.
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Error("test server does not support hijacking")
					return relay.DevicePollResponse{}
				}
				conn, _, err := hj.Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return relay.DevicePollResponse{}
				}
				_ = conn.Close()
				return relay.DevicePollResponse{}
			}
			return relay.DevicePollResponse{
				Status:            relay.DeviceStatusApproved,
				ProvisionResponse: relay.ProvisionResponse{SystemID: "sys3", Token: "tok-3", Name: "System 3"},
			}
		},
	}
	newFakeDeviceRelay(t, f)

	out, err := runDeviceLogin(context.Background(), f.srv.URL,
		relay.DeviceStartRequest{ClientID: "c", Hostname: "h"},
		func(relay.DeviceStartResponse, bool) {})
	if err != nil {
		t.Fatal(err)
	}
	if out.Token != "tok-3" {
		t.Fatalf("token = %q, want tok-3", out.Token)
	}
}

// Cancelling the context (ctrl-C) stops the wait promptly.
func TestDeviceLoginCancelStopsPolling(t *testing.T) {
	f := &fakeDeviceRelay{
		start: func(n int32) relay.DeviceStartResponse { return startResponse("ABCD-1234", "dev-1") },
		poll: func(deviceCode string, n int32, w http.ResponseWriter, r *http.Request) relay.DevicePollResponse {
			return relay.DevicePollResponse{Status: relay.DeviceStatusPending}
		},
	}
	newFakeDeviceRelay(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runDeviceLogin(ctx, f.srv.URL,
			relay.DeviceStartRequest{ClientID: "c", Hostname: "h"},
			func(relay.DeviceStartResponse, bool) {})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDeviceLogin did not stop after cancel")
	}
}

// A cleartext remote relay is refused before any code is requested.
func TestPerformLoginRefusesCleartextRemoteRelay(t *testing.T) {
	_, err := performLogin(context.Background(), "ws://relay.example.test")
	if err == nil || !strings.Contains(err.Error(), "cleartext") {
		t.Fatalf("err = %v, want a cleartext refusal", err)
	}
}

// End to end, headless: the flow saves the pairing token, system id and stable
// client id to the config file, exactly as the old provisioning did.
func TestPerformLoginHeadlessSavesConfig(t *testing.T) {
	useTempConfig(t)

	f := &fakeDeviceRelay{
		start: func(n int32) relay.DeviceStartResponse { return startResponse("ABCD-1234", "dev-1") },
		poll: func(deviceCode string, n int32, w http.ResponseWriter, r *http.Request) relay.DevicePollResponse {
			return relay.DevicePollResponse{
				Status:            relay.DeviceStatusApproved,
				ProvisionResponse: relay.ProvisionResponse{SystemID: "sys9", Token: "tok-9", Name: "System 9"},
			}
		},
	}
	newFakeDeviceRelay(t, f)
	// performLogin takes the ws URL and derives the http base itself.
	wsURL := "ws" + strings.TrimPrefix(f.srv.URL, "http")

	cfg, err := performLogin(context.Background(), wsURL)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PairingToken != "tok-9" || cfg.SystemID != "sys9" || cfg.ClientID == "" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.Email != "" {
		t.Fatalf("device flow must not record an email, got %q", cfg.Email)
	}

	saved, err := loadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if saved.PairingToken != "tok-9" || saved.SystemID != "sys9" || saved.ClientID != cfg.ClientID {
		t.Fatalf("saved config = %+v", saved)
	}

	// A second login reuses the stable client id from the saved config.
	if _, err := performLogin(context.Background(), wsURL); err != nil {
		t.Fatal(err)
	}
	again, err := loadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if again.ClientID != cfg.ClientID {
		t.Fatalf("client id changed across logins: %q → %q", cfg.ClientID, again.ClientID)
	}
}

// Headless display prints the code and verification URL plainly.
func TestHeadlessCodeDisplayShowsCodeAndURL(t *testing.T) {
	var b strings.Builder
	headlessCodeDisplay(&b)(relay.DeviceStartResponse{
		UserCode: "WXYZ-9876", VerificationURL: "https://app.example.test/pair", ExpiresIn: 600,
	}, false)
	out := b.String()
	if !strings.Contains(out, "WXYZ-9876") || !strings.Contains(out, "https://app.example.test/pair") {
		t.Fatalf("display missing code or URL:\n%s", out)
	}
	if strings.Contains(out, "expired") {
		t.Fatalf("first code must not be framed as a restart:\n%s", out)
	}

	b.Reset()
	headlessCodeDisplay(&b)(relay.DeviceStartResponse{
		UserCode: "NEXT-0001", VerificationURL: "https://app.example.test/pair", ExpiresIn: 600,
	}, true)
	if !strings.Contains(b.String(), "expired") || !strings.Contains(b.String(), "NEXT-0001") {
		t.Fatalf("restart display missing notice or code:\n%s", b.String())
	}
}

// The error returned by the real device-start path is printed only after the
// pairing TUI has restored the live terminal. Exercise that producer and the
// exact writer used by runSystem, rather than testing sanitize in isolation.
func TestLoginErrorOutputSanitisesRelayText(t *testing.T) {
	for name, tc := range map[string]struct {
		errorText string
		detail    string
		want      string
	}{
		"ESC and C0": {
			errorText: "denied\x1b]0;OWNED\a",
			detail:    "try again\x1b[2J",
			want:      "denied]0;OWNED: try again[2J",
		},
		"C1 and bidi format": {
			errorText: "denied\u009b",
			detail:    "try\u202e again",
			want:      "denied: try again",
		},
		"ordinary printable text": {
			errorText: "access denied",
			detail:    "pairing is disabled",
			want:      "access denied: pairing is disabled",
		},
		"empty after sanitizing": {
			errorText: "\x1b\u009b\u202e",
			want:      "[relay supplied no printable text]",
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": tc.errorText, "detail": tc.detail})
			}))
			defer srv.Close()

			_, err := deviceStart(context.Background(), srv.URL, relay.DeviceStartRequest{})
			if err == nil {
				t.Fatal("deviceStart accepted the relay error response")
			}
			var out strings.Builder
			writeLoginError(&out, err)
			got := out.String()
			if !strings.Contains(got, tc.want) {
				t.Errorf("printable text did not survive in the login error (want %q): %q", tc.want, got)
			}
			for _, forbidden := range []string{"\x1b", "\u009b", "\u202e", "\a"} {
				if strings.Contains(got, forbidden) {
					t.Errorf("relay-supplied %q reached the login error output: %q", forbidden, got)
				}
			}
		})
	}

	// Known-good sibling: a local error contains no relay data and must retain
	// its useful wording through the same output boundary.
	var local strings.Builder
	writeLoginError(&local, errors.New("local configuration failed"))
	if got, want := local.String(), "error: local configuration failed\n"; got != want {
		t.Errorf("local error changed: got %q, want %q", got, want)
	}
}

// Validation must use the sanitized values that the operator will see. Raw
// non-empty checks let all-control codes render as blank and leave the TUI on
// its pre-code screen forever.
func TestDeviceStartRejectsValuesEmptyAfterSanitising(t *testing.T) {
	for name, response := range map[string]relay.DeviceStartResponse{
		"code": {
			UserCode: "\x1b\u009b\u202e", DeviceCode: "opaque", VerificationURL: "https://app.example.test/pair", ExpiresIn: 600, Interval: 1,
		},
		"URL": {
			UserCode: "ABCD-1234", DeviceCode: "opaque", VerificationURL: "\x1b\u009b\u202e", ExpiresIn: 600, Interval: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer srv.Close()

			if _, err := deviceStart(context.Background(), srv.URL, relay.DeviceStartRequest{}); err == nil ||
				!strings.Contains(err.Error(), "implausible device flow") {
				t.Fatalf("deviceStart error = %v, want fixed unusable-flow error", err)
			}
		})
	}

	// Control comparison through the same endpoint: printable values survive
	// unchanged, proving the rejection above is the post-sanitize branch rather
	// than a server or decoding failure.
	want := relay.DeviceStartResponse{
		UserCode: "ABCD-1234", DeviceCode: "opaque", VerificationURL: "https://app.example.test/pair", ExpiresIn: 600, Interval: 1,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()
	got, err := deviceStart(context.Background(), srv.URL, relay.DeviceStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.UserCode != want.UserCode || got.VerificationURL != want.VerificationURL {
		t.Fatalf("printable display values changed: got %+v, want %+v", got, want)
	}
}

// The headless path was the one place the PAIRING CODE reached an output
// unsanitised. Inert where it is printed today — no TTY, so a pipe or a journal
// — but the agent's rule is that sanitising happens at the boundary, not
// wherever the bytes are currently believed to end up. Read back with
// `journalctl` on a terminal, an escape acts.
//
// It is not the last unsanitised relay string in the agent. Tracked in
// nicodes/ormos-be#420 — named here so this test is not read as closing more
// than it does, without restating there what that issue exists to hold.
//
// The payloads are the pairing screen's, from
// TestPairingScreenStripsEscapesFromTheRelay and
// TestPairingScreenStripsC1AndFormatCharacters, so the two outputs are held to
// one standard rather than each to its own.
func TestHeadlessCodeDisplaySanitisesTheRelaysStrings(t *testing.T) {
	// The line headlessCodeDisplay prints between the URL and the code, so it
	// is the boundary between the two slots. Local, because this is the only
	// place it is used.
	const codeInstruction = "and enter code"

	for name, tc := range map[string]struct {
		code, url string
		forbidden []string
		// Both fields are checked: an implementation that sanitised the code
		// and dropped the URL entirely would otherwise pass, and the operator
		// needs both to pair.
		codeSurvives, urlSurvives string
	}{
		// C0: a title-set, an erase-display and a BEL. Unlike the pairing
		// screen this path writes no OSC 8 hyperlink of its own, so a bare ESC
		// has no legitimate reason to appear either.
		"C0 escapes": {
			code:         "AB\x1b]0;OWNED\a\x1b[2JCD",
			url:          "https://app.example.test/pair\x1b]0;PWN\a",
			forbidden:    []string{"\x1b", "\a"},
			codeSurvives: "AB]0;OWNED[2JCD",
			urlSurvives:  "https://app.example.test/pair]0;PWN",
		},
		// What stripCtl would have left behind: C1 controls (U+009B is CSI) and
		// Cf format characters (U+202E rewrites reading order).
		"C1 and format characters": {
			code:         "AB\u202eCD\u009bEF",
			url:          "https://app.example.test/pair\u009bEND\u202e",
			forbidden:    []string{"\u202e", "\u009b"},
			codeSurvives: "ABCDEF",
			urlSurvives:  "https://app.example.test/pairEND",
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Both framings, because each writes the relay's strings: a first
			// code and a re-issued one.
			for _, restarted := range []bool{false, true} {
				var b strings.Builder
				headlessCodeDisplay(&b)(relay.DeviceStartResponse{
					UserCode: tc.code, VerificationURL: tc.url, ExpiresIn: 600,
				}, restarted)
				out := b.String()
				for _, seq := range tc.forbidden {
					if strings.Contains(out, seq) {
						t.Errorf("restarted=%v: a relay-supplied %q reached the headless output:\n%q", restarted, seq, out)
					}
				}
				// Sanitising must not have eaten what the operator needs —
				// otherwise this passes by printing nothing. Checked per SLOT,
				// not merely present somewhere in the output: the two %s verbs
				// are adjacent in one format string, so transposing them is a
				// live edit hazard, and a Contains over the whole output cannot
				// tell the operator's "open this" from their "type this".
				// Shown as a URL to type and a code to open, pairing simply
				// cannot complete.
				//
				// Each is required on its OWN indented line, not merely
				// somewhere in its half. The code's half runs to the end of the
				// output, so "in its half" would also be satisfied by a payload
				// buried in the trailing "expires in" sentence, under a code
				// line reading something else entirely.
				open, code, found := strings.Cut(out, codeInstruction)
				if !found {
					t.Fatalf("restarted=%v: the output no longer contains %q, so the slots cannot be told apart:\n%s", restarted, codeInstruction, out)
				}
				for _, slot := range []struct{ what, in, want string }{
					{"URL", open, tc.urlSurvives},
					{"code", code, tc.codeSurvives},
				} {
					// An empty expectation would make the check below vacuous:
					// a blank indented line is exactly what the format string
					// prints when sanitize eats a payload whole, so "" would
					// assert nothing while reading as a pass. Since sanitize
					// DELETES rather than escapes, a payload that vanishes is a
					// real degradation someone may want to document here, and
					// they must not document it with a dead assertion.
					if slot.want == "" {
						t.Fatalf("%s case has an empty want: this assertion cannot tell survival from deletion, so it would pass vacuously", slot.what)
					}
					// Compared per line with the indent trimmed, rather than
					// against a literal "\n  " prefix. The two-space indent is
					// login.go's formatting, not a property this test is about,
					// and hardcoding it here made a purely cosmetic indent
					// change fail eight assertions with a message blaming
					// sanitising.
					onOwnLine := false
					for _, line := range strings.Split(slot.in, "\n") {
						if strings.TrimSpace(line) == slot.want {
							onOwnLine = true
							break
						}
					}
					if !onOwnLine {
						t.Errorf("restarted=%v: the %s did not survive as inert text on its own line in its own slot (want %q):\n%s", restarted, slot.what, slot.want, out)
					}
				}
			}
		})
	}
}

// The pairing screen shows the code and URL prominently once a code arrives,
// swaps in a re-issued code, and quits with a cancellation on ctrl-C.
func TestLoginModelRendersCode(t *testing.T) {
	m := loginModel{}
	if !strings.Contains(m.View(), "requesting") {
		t.Fatalf("pre-code view should say a code is being requested:\n%s", m.View())
	}

	upd, _ := m.Update(loginCodeMsg{start: relay.DeviceStartResponse{
		UserCode: "WXYZ-9876", VerificationURL: "https://app.example.test/pair", ExpiresIn: 600,
	}})
	m = upd.(loginModel)
	v := m.View()
	if !strings.Contains(v, "WXYZ-9876") || !strings.Contains(v, "https://app.example.test/pair") {
		t.Fatalf("pairing screen missing code or URL:\n%s", v)
	}

	upd, _ = m.Update(loginCodeMsg{start: relay.DeviceStartResponse{
		UserCode: "NEXT-0001", VerificationURL: "https://app.example.test/pair", ExpiresIn: 600,
	}})
	m = upd.(loginModel)
	if v := m.View(); !strings.Contains(v, "NEXT-0001") || !strings.Contains(v, "expires in") {
		t.Fatalf("restarted screen missing new code or countdown:\n%s", v)
	}

	upd, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = upd.(loginModel)
	if cmd == nil || !errors.Is(m.err, errLoginCancelled) {
		t.Fatalf("ctrl-C should quit with errLoginCancelled, err = %v", m.err)
	}

	upd, cmd = m.Update(loginFinishedMsg{out: &relay.ProvisionResponse{Token: "tok"}})
	m = upd.(loginModel)
	if cmd == nil || m.out == nil || m.out.Token != "tok" {
		t.Fatalf("finish should quit with the approved payload, out = %+v", m.out)
	}
}

// The pairing screen owns the whole terminal, runs before any token exists,
// and its entire job is telling the operator which code to trust. A relay that
// could paint over it would be forging exactly the thing being verified.
func TestPairingScreenStripsEscapesFromTheRelay(t *testing.T) {
	var m loginModel
	next, _ := m.Update(loginCodeMsg{start: relay.DeviceStartResponse{
		UserCode:        "AB\x1b]0;OWNED\a\x1b[2JCD",
		VerificationURL: "https://app.example.test/pair\x1b]0;PWN\a",
		ExpiresIn:       60,
	}})
	shown, _ := next.(loginModel).Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	view := shown.(loginModel).View()

	// Named sequences rather than a bare ESC: this screen emits its own OSC 8
	// hyperlink for the URL, so "\x1b]" is legitimately present. What must not
	// be is a title-set, an erase-display, or a BEL — none of which this screen
	// has any reason to write.
	for _, seq := range []string{"\x1b]0;", "\x1b[2J", "\a"} {
		if strings.Contains(view, seq) {
			t.Errorf("a relay-supplied %q reached the pairing screen", seq)
		}
	}
	// The payload survives as inert text, which is the point — the code stays
	// readable, it just cannot act.
	if !strings.Contains(view, "AB]0;OWNED[2JCD") {
		t.Errorf("the pairing code did not survive sanitising:\n%s", view)
	}
}

// stripCtl alone would leave these behind: the C1 controls and the Cf format
// characters. U+202E (right-to-left override) is the one that can rewrite what
// the operator reads on the screen whose whole job is which code to trust, so
// the pairing screen sanitises, not just strips.
func TestPairingScreenStripsC1AndFormatCharacters(t *testing.T) {
	var m loginModel
	next, _ := m.Update(loginCodeMsg{start: relay.DeviceStartResponse{
		UserCode:        "AB\u202eCD\u009bEF",
		VerificationURL: "https://app.example.test/pair\u009bEND\u202e",
		ExpiresIn:       60,
	}})
	shown, _ := next.(loginModel).Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	for name, view := range map[string]string{
		"sized":      shown.(loginModel).View(),
		"pre-resize": next.(loginModel).View(),
	} {
		for _, seq := range []string{"\u202e", "\u009b"} {
			if strings.Contains(view, seq) {
				t.Errorf("%s view: a relay-supplied %q reached the pairing screen", name, seq)
			}
		}
	}
	// The payload survives as inert text.
	if view := shown.(loginModel).View(); !strings.Contains(view, "ABCD") {
		t.Errorf("the pairing code did not survive sanitising:\n%s", view)
	}
}

// The relay picks the code's length as well as its content, so the code is
// clipped to the terminal width: an over-wide code must not paint past the
// screen's edge.
func TestPairingScreenClipsTheCodeToTheWidth(t *testing.T) {
	var m loginModel
	next, _ := m.Update(loginCodeMsg{start: relay.DeviceStartResponse{
		UserCode:        strings.Repeat("A", 200),
		VerificationURL: "https://app.example.test/pair",
		ExpiresIn:       60,
	}})
	shown, _ := next.(loginModel).Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	view := shown.(loginModel).View()

	for _, row := range strings.Split(view, "\n") {
		if w := lipgloss.Width(row); w > 60 {
			t.Errorf("a %d-column row on a 60-column screen: %q", w, row)
		}
	}
}
