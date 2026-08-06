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

// The pairing screen shows the code and URL prominently once a code arrives,
// notes restarts, and quits with a cancellation on ctrl-C.
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

	upd, _ = m.Update(loginCodeMsg{restarted: true, start: relay.DeviceStartResponse{
		UserCode: "NEXT-0001", VerificationURL: "https://app.example.test/pair", ExpiresIn: 600,
	}})
	m = upd.(loginModel)
	if v := m.View(); !strings.Contains(v, "NEXT-0001") || !strings.Contains(v, "expired") {
		t.Fatalf("restarted screen missing new code or expiry note:\n%s", v)
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
