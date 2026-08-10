package system

import (
	"strings"
	"testing"
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
