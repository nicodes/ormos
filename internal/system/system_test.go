//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func TestReconnectRunsTerminalReconciliation(t *testing.T) {
	withTempConfigDir(t)
	hold := make(chan struct{})
	terminalLists := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/system/connect" {
			ws, err := websocket.Accept(w, r, nil)
			if err == nil {
				<-hold
				_ = ws.Close(websocket.StatusNormalClosure, "")
			}
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/system/terminal-sessions/reset":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/system/terminal-sessions":
			terminalLists++
			if terminalLists == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, `{"sessions":[{"id":"keep","state":"running","generation":4}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	d := newSystem(systemConfig{RelayURL: "ws" + strings.TrimPrefix(server.URL, "http"), Shell: "/bin/sh"})
	d.pollPortsFn = func(context.Context) {}
	d.terminals["keep"] = &terminalSession{id: "keep", recordID: "keep", generation: 4, owner: d, done: make(chan struct{})}
	d.terminals["stale"] = &terminalSession{id: "stale", recordID: "stale", generation: 2, owner: d, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for {
		d.terminalMu.Lock()
		_, keep := d.terminals["keep"]
		_, stale := d.terminals["stale"]
		d.terminalMu.Unlock()
		if keep && !stale {
			close(hold)
			cancel()
			<-done
			return
		}
		select {
		case <-deadline:
			t.Fatalf("reconnect reconciliation state keep=%v stale=%v", keep, stale)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// Relay responses decode through a bounded reader: a relay that answers with
// an endless body must not make the agent buffer without limit.
func TestDecodeRelayJSONBoundsTheBody(t *testing.T) {
	var small struct {
		Name string `json:"name"`
	}
	if err := decodeRelayJSON(strings.NewReader(`{"name":"ok"}`), &small); err != nil || small.Name != "ok" {
		t.Fatalf("small body: %v %q", err, small.Name)
	}

	huge := `{"name":"` + strings.Repeat("x", maxRelayResponse) + `"}`
	var big struct {
		Name string `json:"name"`
	}
	if err := decodeRelayJSON(strings.NewReader(huge), &big); err == nil {
		t.Fatal("a body past the cap must fail to decode, not buffer forever")
	}
}

// pollPorts is the relay-status producer and logf's echo is the actual headless
// stderr path. A custom transport is necessary because net/http's test server
// chooses a canonical printable reason phrase; Response.Status itself is the
// relay-controlled value the production client receives.
func TestHeadlessPortsPollEchoSanitisesRelayStatus(t *testing.T) {
	for name, status := range map[string]string{
		"ESC and C0":           "599 relay\x1b]0;OWNED\a\x1b[2J",
		"C1 and bidi format":   "599 relay\u009b\u202e failed",
		"ordinary printable":   "599 relay maintenance",
		"empty after sanitize": "\x1b\u009b\u202e",
	} {
		t.Run(name, func(t *testing.T) {
			oldClient := httpClient
			t.Cleanup(func() { httpClient = oldClient })
			httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: 599,
					Status:     status,
					Body:       io.NopCloser(strings.NewReader("")),
					Header:     make(http.Header),
				}, nil
			})}

			var echoed bytes.Buffer
			d := &system{
				cfg:        systemConfig{RelayURL: "ws://relay.example", PairingToken: "local-test-token"},
				echoStderr: true,
				echoWriter: &echoed,
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // pollPorts still performs its immediate tick, then exits.
			d.pollPorts(ctx)

			got := echoed.String()
			if !strings.Contains(got, "ports poll failed: relay returned") {
				t.Fatalf("the reachable headless poll error was not echoed: %q", got)
			}
			if sanitize(status) == "" && !strings.Contains(got, "[relay supplied no printable text]") {
				t.Errorf("empty sanitized status has no fixed fallback: %q", got)
			}
			if clean := sanitize(status); clean != "" && !strings.Contains(got, clean) {
				t.Errorf("printable status text did not survive (want %q): %q", clean, got)
			}
			for _, forbidden := range []string{"\x1b", "\u009b", "\u202e", "\a"} {
				if strings.Contains(got, forbidden) {
					t.Errorf("relay-supplied %q reached the headless echo: %q", forbidden, got)
				}
			}
		})
	}

	// Known-good sibling: local log text must remain useful on the same echo
	// path; sanitization is scoped to the relay-derived poll argument.
	var local bytes.Buffer
	d := &system{echoStderr: true, echoWriter: &local}
	d.logf("tunnel error: connection refused")
	if !strings.HasSuffix(local.String(), " tunnel error: connection refused\n") {
		t.Errorf("local log text changed: %q", local.String())
	}
}

// Only a tunnel that lived past the floor resets the reconnect backoff; a
// clean close of a young tunnel is churn and must back off like a failure,
// or a relay that accepts and drops immediately gets a zero-delay TLS loop.
func TestTunnelHealthyFloor(t *testing.T) {
	for _, tc := range []struct {
		connected bool
		lifetime  time.Duration
		want      bool
	}{
		{true, tunnelHealthyFloor + time.Second, true},
		{true, tunnelHealthyFloor, true},
		{true, tunnelHealthyFloor - time.Second, false},
		{true, 100 * time.Millisecond, false},
		{false, tunnelHealthyFloor + time.Second, false},
	} {
		if got := tunnelHealthy(tc.connected, tc.lifetime); got != tc.want {
			t.Fatalf("tunnelHealthy(%v, %s) = %v, want %v", tc.connected, tc.lifetime, got, tc.want)
		}
	}
}
