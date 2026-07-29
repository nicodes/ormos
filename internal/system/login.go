package system

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nicodes/ormos/relay"
	"golang.org/x/term"
)

// performLogin authenticates the user against the relay and registers this
// system under their account, saving the returned pairing credential to the
// config file. Called on demand from `ormos` when there's no valid session.
// Credentials are always typed in at the prompt — there is no flag or
// environment variable for them. PocketBase is never contacted directly —
// provisioning is relay-mediated.
func performLogin(relayWS string) (systemConfig, error) {
	if insecureRemoteRelay(relayWS) {
		return systemConfig{}, fmt.Errorf(
			"refusing to send credentials in cleartext to remote relay %s — use a wss:// URL", relayWS)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return systemConfig{}, fmt.Errorf(
			"this machine is not registered and stdin is not a terminal — run ormos interactively once to register it")
	}
	fmt.Fprintf(os.Stderr, "relay: %s\n", relayWS)

	fmt.Fprint(os.Stderr, "email: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	em := strings.TrimSpace(line)

	fmt.Fprint(os.Stderr, "password: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return systemConfig{}, fmt.Errorf("reading password: %w", err)
	}
	pw := string(b)

	if em == "" || pw == "" {
		return systemConfig{}, fmt.Errorf("email and password are required")
	}

	hostname, _ := os.Hostname() // display-only; best effort

	// Reuse this config's stable client id, or mint one on first login. Two
	// systems on one host use two config files, hence two ids.
	existing, _ := loadConfigFile()
	clientID := existing.ClientID
	if clientID == "" {
		clientID = generateClientID()
	}

	httpBase := httpBaseFromWS(relayWS)
	body, _ := json.Marshal(relay.ProvisionRequest{
		Email:    em,
		Password: pw,
		ClientID: clientID,
		Hostname: strings.TrimSpace(hostname),
		// No Name: the relay assigns "System N" and it is renamed in the app.
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, httpBase+"/system/provision", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return systemConfig{}, fmt.Errorf("could not reach relay at %s: %w", httpBase, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusUnauthorized:
		return systemConfig{}, fmt.Errorf("invalid email or password")
	default:
		var e struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		msg := e.Error
		if msg == "" {
			msg = resp.Status
		}
		// The relay sends `detail` when it knows why the write failed; without it
		// the user only ever saw "registration failed".
		if e.Detail != "" {
			msg += ": " + e.Detail
		}
		return systemConfig{}, fmt.Errorf("%s", msg)
	}

	var out relay.ProvisionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return systemConfig{}, fmt.Errorf("decoding relay response: %w", err)
	}

	cfg := systemConfig{
		RelayURL:     relayWS,
		ClientID:     clientID,
		PairingToken: out.Token,
		SystemID:     out.SystemID,
		Email:        em,
	}
	if err := saveConfigFile(cfg); err != nil {
		return systemConfig{}, fmt.Errorf("saving config: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Logged in as %s — registered this system as %q.\n", em, out.Name)
	return cfg, nil
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
