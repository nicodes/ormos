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
