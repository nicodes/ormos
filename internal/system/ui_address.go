//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
)

var (
	uiTailnetDNS  = readTailnetDNS
	uiServeStatus = readServeStatus
	uiStartShare  = startServeCommand
)

func readTailnetDNS(ctx context.Context) (string, error) {
	var state struct {
		BackendState   string
		Self           struct{ DNSName string }
		CurrentTailnet struct{ MagicDNSEnabled bool }
	}
	if err := readTailscaleJSON(ctx, &state, "status", "--json"); err != nil {
		return "", err
	}
	dns := strings.TrimSuffix(state.Self.DNSName, ".")
	if state.BackendState != "Running" || !state.CurrentTailnet.MagicDNSEnabled || !validTailnetDNS(dns) {
		return "", errors.New("Tailscale is not connected with MagicDNS")
	}
	return dns, nil
}

func validTailnetDNS(dns string) bool {
	if len(dns) > 253 || !strings.HasSuffix(dns, ".ts.net") {
		return false
	}
	for _, label := range strings.Split(dns, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// Port 80 is the UI's browser origin. Refuse other apps on that origin, even
// at different paths: sharing an origin would bypass the terminal Origin guard.
func (c *serveConfig) uiRoute(target string) (exists, compatible bool) {
	if c == nil {
		return false, false
	}
	return c.routeTarget("80", "http", "/ormos", target)
}

func shareUI(parent context.Context, address *net.TCPAddr, server *uiServer) (string, *serveSession, error) {
	if !address.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		return "", nil, errors.New("automatic sharing requires a 127.0.0.1 listener (use --share=false for a custom bind)")
	}
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	dns, err := uiTailnetDNS(ctx)
	if err != nil {
		return "", nil, errors.New("connect Tailscale with MagicDNS and allow this user to manage Serve")
	}
	cfg, err := uiServeStatus(ctx)
	if err != nil {
		return "", nil, errors.New("allow this user to manage Tailscale Serve")
	}
	target := "http://127.0.0.1:" + strconv.Itoa(address.Port) + "/ormos"
	used, compatible := cfg.uiRoute(target)
	if used && !compatible {
		return "", nil, errors.New("Tailscale port 80 is already used; existing routes were preserved")
	}
	var session *serveSession
	if !used {
		session, err = uiStartShare(parent, "http", "--http=80", "--set-path=/ormos", target)
		if err != nil {
			return "", nil, errors.New("Tailscale could not register /ormos/")
		}
		select {
		case <-session.ready:
			select {
			case <-session.done:
				stopServeSession(session)
				return "", nil, errors.New("Tailscale could not register /ormos/")
			default:
			}
		case <-session.done:
			stopServeSession(session)
			return "", nil, errors.New("Tailscale could not register /ormos/")
		case <-ctx.Done():
			stopServeSession(session)
			return "", nil, errors.New("Tailscale setup timed out")
		}
	}
	short := strings.Split(dns, ".")[0]
	server.hosts = append(server.hosts, short, dns, short+":80", dns+":80")
	return "http://" + short + "/ormos/", session, nil
}

func stopServeSession(session *serveSession) {
	if session == nil {
		return
	}
	session.stop()
	select {
	case <-session.done:
	case <-time.After(3 * time.Second):
	}
}
