package relay

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// NetConn wraps a coder/websocket connection as a net.Conn suitable for running
// yamux over it. Binary message framing is used so yamux's byte stream passes
// through untouched.
func NetConn(ctx context.Context, c *websocket.Conn) net.Conn {
	return websocket.NetConn(ctx, c, websocket.MessageBinary)
}

// MaxTunnelStreams caps how many logical streams either end will serve on one
// tunnel at a time.
//
// yamux itself has no such limit, so without this a peer could open streams
// until the other side ran out of memory — each one costing a receive window
// plus whatever the stream's handler allocates (a PTY, a TCP connection to a
// local service, a frame buffer). The number is far above real use: a busy
// workspace is a handful of terminals and one proxied port.
const MaxTunnelStreams = 128

// MaxStreamWindow is the per-stream receive window both ends of the tunnel
// advertise, and MaxTunnelWindowBytes is what that costs across every stream a
// peer may open at once. The second is the number worth watching: raising
// either factor raises the memory an adversarial peer can pin.
const (
	MaxStreamWindow      = 4 << 20
	MaxTunnelWindowBytes = MaxStreamWindow * MaxTunnelStreams
)

// yamuxConfig returns a shared yamux config with keepalive enabled so both ends
// detect a dead tunnel, and bounded buffering so neither end can be made to
// hold an unreasonable amount of memory for the other.
func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	// A stream that is opened and then never read from should not sit in the
	// backlog forever; the accept queue is the cheapest thing for a peer to fill.
	cfg.AcceptBacklog = 64
	cfg.StreamOpenTimeout = 30 * time.Second
	cfg.StreamCloseTimeout = 2 * time.Minute
	// yamux defaults MaxStreamWindowSize to 256 KiB, which caps one stream at
	// 256 KiB per tunnel round trip. The port proxy shares this window and dev
	// servers routinely serve multi-megabyte bundles, so a preview reload paid
	// a round trip every quarter megabyte.
	//
	// The window is what a peer may have in flight unacknowledged, so it is
	// also the per-stream memory the other end can be made to hold. Against
	// MaxTunnelStreams that is MaxTunnelWindowBytes — the number the guard in
	// transport_test.go pins, because the trade being made here is the product
	// of the two, not either one alone. yamux allocates the window as data
	// arrives rather than up front, and a peer must already be authenticated to
	// open a stream at all.
	cfg.MaxStreamWindowSize = MaxStreamWindow
	// Silence yamux's internal logging; callers do their own logging.
	cfg.LogOutput = io.Discard
	return cfg
}

// ServerSession creates the system-side yamux session (accepts streams opened
// by the api).
func ServerSession(conn net.Conn) (*yamux.Session, error) {
	return yamux.Server(conn, yamuxConfig())
}

// ClientSession creates the api-side yamux session (opens streams to the
// system).
func ClientSession(conn net.Conn) (*yamux.Session, error) {
	return yamux.Client(conn, yamuxConfig())
}
