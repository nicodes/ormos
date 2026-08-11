//go:build linux || darwin

package system

import (
	"strings"
	"testing"
	"time"
)

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
