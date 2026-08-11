package relay

import (
	"testing"

	"github.com/hashicorp/yamux"
)

// The stream window is what a peer may have in flight before it must wait for
// an acknowledgement, so yamux's 256 KiB default capped one stream at 256 KiB
// per tunnel round trip. The port proxy shares it and dev servers routinely
// serve multi-megabyte bundles.
func TestYamuxConfigStreamWindow(t *testing.T) {
	cfg := yamuxConfig()
	if err := yamux.VerifyConfig(cfg); err != nil {
		t.Fatalf("yamuxConfig is not a valid yamux config: %v", err)
	}
	if cfg.MaxStreamWindowSize != MaxStreamWindow {
		t.Fatalf("MaxStreamWindowSize = %d, want %d", cfg.MaxStreamWindowSize, MaxStreamWindow)
	}
	// The budget, not the floor. What this change trades away is the product of
	// the window and the stream cap — the memory an adversarial peer can pin —
	// so raising either factor without deciding to must fail here rather than
	// pass a test that only ever checked the window was big enough.
	if MaxTunnelWindowBytes > 512<<20 {
		t.Fatalf("a peer can pin %d bytes across %d streams; the accepted budget is 512 MiB",
			MaxTunnelWindowBytes, MaxTunnelStreams)
	}
	// Both ends share this function, which is what stops the two drifting.
	if !cfg.EnableKeepAlive {
		t.Fatal("keepalive is off; a dead tunnel would go unnoticed on both ends")
	}
}
