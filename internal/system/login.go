package system

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nicodes/ormos/relay"
)

// performLogin registers this system under the user's account through the
// relay's device-authorization flow: the relay issues a short code, the user
// approves it in the web app, and the agent polls until the approval (or
// expiry) comes back. Nothing is typed into this terminal — there is no flag
// or environment variable for credentials. The approved pairing token is saved
// to the config file exactly as the old email/password provisioning did.
// Called on demand from `ormos` when there's no valid session. PocketBase is
// never contacted directly — provisioning is relay-mediated.
func performLogin(ctx context.Context, relayWS string) (systemConfig, error) {
	// Pairing is where the token is minted; it must not cross cleartext either.
	// runSystem refuses this earlier and fatally — this guard keeps the flow
	// honest wherever it is entered, and shares the ORMOS_INSECURE=1 hatch.
	if err := checkRelayTransport(relayWS); err != nil {
		return systemConfig{}, err
	}
	fmt.Fprintf(os.Stderr, "relay: %s\n", relayWS)

	hostname, _ := os.Hostname() // display-only; best effort

	// Reuse this config's stable client id, or mint one on first login. Two
	// systems on one host use two config files, hence two ids.
	existing, _ := loadConfigFile()
	clientID := existing.ClientID
	if clientID == "" {
		clientID = generateClientID()
	}
	req := relay.DeviceStartRequest{ClientID: clientID, Hostname: strings.TrimSpace(hostname)}
	httpBase := httpBaseFromWS(relayWS)

	// With a terminal the code is shown on a pairing screen (login_tui.go);
	// headless it is printed plainly to stdout. Either way ctrl-C cancels
	// through ctx.
	var out *relay.ProvisionResponse
	var err error
	if isTTY() {
		out, err = runLoginTUI(ctx, httpBase, req)
	} else {
		out, err = runDeviceLogin(ctx, httpBase, req, headlessCodeDisplay(os.Stdout))
	}
	if errors.Is(err, context.Canceled) {
		err = errLoginCancelled
	}
	if err != nil {
		return systemConfig{}, err
	}

	cfg := systemConfig{
		RelayURL:     relayWS,
		ClientID:     clientID,
		PairingToken: out.Token,
		SystemID:     out.SystemID,
		// No Email: the device flow never sees the account's email address.
	}
	if err := saveConfigFile(cfg); err != nil {
		return systemConfig{}, fmt.Errorf("saving config: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Registered this system as %q.\n", out.Name)
	return cfg, nil
}

// errLoginCancelled reports the user abandoning the pairing (ctrl-C / q).
var errLoginCancelled = errors.New("pairing cancelled")

// minPollInterval floors the relay's poll interval so a broken (or hostile)
// relay can't turn the pairing wait into a request hammer; maxPollInterval
// caps it so an absurd interval can't overflow the duration arithmetic.
const (
	minPollInterval = time.Second
	maxPollInterval = time.Hour
)

// maxDeviceFlowSeconds bounds the relay-supplied code lifetime: real device
// flows live for minutes, and an unbounded ExpiresIn could overflow the
// duration arithmetic into an instant (or never-ending) expiry.
const maxDeviceFlowSeconds = 24 * 3600

// maxRestartBackoff caps the wait between instantly-expired rounds.
const maxRestartBackoff = 30 * time.Second

// runDeviceLogin performs device-authorization rounds until one is approved or
// ctx is cancelled: start a flow, show the code, poll, and — when the code
// expires unapproved — start over with a fresh one. showCode is invoked each
// time a code is issued; restarted reports that it replaces an expired one.
//
// A round that expires instantly is the relay misbehaving, not the user being
// slow, so consecutive instant expiries back off (1s, 3s, 7s, … to
// maxRestartBackoff) instead of hammering /device/start. A round that ran its
// course resets the backoff.
func runDeviceLogin(ctx context.Context, httpBase string, req relay.DeviceStartRequest, showCode func(start relay.DeviceStartResponse, restarted bool)) (*relay.ProvisionResponse, error) {
	restarted := false
	var backoff time.Duration
	for {
		if backoff > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
		began := time.Now()
		start, err := deviceStart(ctx, httpBase, req)
		if err != nil {
			return nil, err
		}
		showCode(start, restarted)
		out, expired, err := devicePoll(ctx, httpBase, start)
		if err != nil {
			return nil, err
		}
		if !expired {
			return out, nil
		}
		restarted = true
		if time.Since(began) < minPollInterval {
			backoff = min(backoff*2+time.Second, maxRestartBackoff)
		} else {
			backoff = 0
		}
	}
}

// deviceStart opens one device-authorization round (POST /device/start).
func deviceStart(ctx context.Context, httpBase string, req relay.DeviceStartRequest) (relay.DeviceStartResponse, error) {
	var out relay.DeviceStartResponse
	if err := postRelayJSON(ctx, httpBase+"/device/start", req, &out); err != nil {
		return out, fmt.Errorf("starting device flow: %w", err)
	}
	if out.UserCode == "" || out.DeviceCode == "" || out.VerificationURL == "" ||
		out.ExpiresIn <= 0 || out.ExpiresIn > maxDeviceFlowSeconds {
		return out, fmt.Errorf("relay returned an implausible device flow (missing code, URL or expiry)")
	}
	return out, nil
}

// devicePoll polls POST /device/poll every start.Interval seconds until the
// relay reports the code approved or expired, or its lifetime (ExpiresIn)
// elapses — a pending code past its deadline is treated as expired. Polls that
// fail at the network level are retried on the same interval until the
// deadline: a flaky connection must not abort a pairing the user may be
// mid-approval on.
func devicePoll(ctx context.Context, httpBase string, start relay.DeviceStartResponse) (out *relay.ProvisionResponse, expired bool, err error) {
	interval := time.Duration(start.Interval) * time.Second
	if interval < minPollInterval {
		interval = minPollInterval
	}
	if interval > maxPollInterval {
		interval = maxPollInterval
	}
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	for {
		var pr relay.DevicePollResponse
		pollErr := postRelayJSON(ctx, httpBase+"/device/poll",
			relay.DevicePollRequest{DeviceCode: start.DeviceCode}, &pr)
		switch {
		case ctx.Err() != nil:
			return nil, false, ctx.Err()
		case pollErr != nil:
			// Transient: fall through and retry until the deadline.
		default:
			switch pr.Status {
			case relay.DeviceStatusApproved:
				return &pr.ProvisionResponse, false, nil
			case relay.DeviceStatusExpired:
				return nil, true, nil
			}
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return nil, true, nil
		}
		if wait > interval {
			wait = interval
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// postRelayJSON POSTs a JSON body to the relay and decodes the JSON response.
// The device-flow endpoints answer 200 for every in-flow state, so there is no
// status switching here; a non-200 is relay trouble, surfaced with the relay's
// own error body (bounded) so a 4xx/5xx is debuggable from the agent's output.
func postRelayJSON(ctx context.Context, url string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		var e struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			if e.Detail != "" {
				return fmt.Errorf("%s: %s", e.Error, e.Detail)
			}
			return fmt.Errorf("%s", e.Error)
		}
		if s := strings.TrimSpace(string(data)); s != "" {
			return fmt.Errorf("relay returned %s: %s", resp.Status, s)
		}
		return fmt.Errorf("relay returned %s", resp.Status)
	}
	return decodeRelayJSON(resp.Body, out)
}

// headlessCodeDisplay prints the pairing code plainly to stdout — the mode
// used under a service manager or without a terminal, where there is no
// pairing screen.
func headlessCodeDisplay(w io.Writer) func(relay.DeviceStartResponse, bool) {
	return func(s relay.DeviceStartResponse, restarted bool) {
		if restarted {
			fmt.Fprintln(w, "\nThat code expired unapproved — here is a fresh one.")
		}
		fmt.Fprintf(w, `
To register this machine, open

  %s

and enter code

  %s

The code expires in %d seconds. Waiting for approval — ctrl-C to cancel.
`, s.VerificationURL, s.UserCode, s.ExpiresIn)
	}
}

// tokenValid checks whether a pairing token is still accepted by the relay
// (a system can be forgotten in the UI, revoking its token). It hits the
// lightweight ports endpoint. If the relay is unreachable it returns true so we
// don't force a re-login while offline — the run loop will keep retrying.
func tokenValid(relayWS, token string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpBaseFromWS(relayWS)+"/system/ports", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return true
	}
	defer resp.Body.Close()
	return resp.StatusCode != http.StatusUnauthorized
}
