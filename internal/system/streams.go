package system

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nicodes/ormos/relay"
)

// expandHome resolves a leading ~ (or ~/) to the agent's home directory and
// expands $VARS, so a project root_dir like "~/code/app" works — the daemon
// sets cmd.Dir directly, so no shell is around to expand it.
func expandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	} else if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	return os.ExpandEnv(p)
}

// serveStream reads a stream's header and dispatches to the right handler.
func (d *system) serveStream(stream net.Conn) {
	defer stream.Close()
	header, br, err := relay.ReadHeader(stream)
	if err != nil {
		d.logf("stream header error: %v", err)
		return
	}
	switch header.Kind {
	case relay.KindPing:
		io.Copy(stream, br) // echo until closed
	case relay.KindTerminal:
		d.handleTerminal(stream, br, header)
	case relay.KindProxy:
		d.handleProxy(stream, br, header.Port)
	case relay.KindListPorts:
		d.handleListPorts(stream)
	case relay.KindShutdown:
		d.audit.record(auditEntry{Event: "shutdown", Allowed: true})
		d.handleShutdown()
	case relay.KindEvent:
		d.notifyEvent() // upstream data changed (web UI); TUI refetches
	default:
		d.logf("unknown stream kind %q", header.Kind)
	}
}

// scrubbedEnv returns the process environment with every ORMOS_* variable
// removed, so nothing the system was configured with can be read from inside an
// interactive shell it spawns.
func scrubbedEnv() []string {
	all := os.Environ()
	out := make([]string, 0, len(all))
	for _, kv := range all {
		if strings.HasPrefix(kv, "ORMOS_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// handleProxy dials a local TCP port and pipes raw bytes both ways.
func (d *system) handleProxy(stream io.ReadWriteCloser, br io.Reader, port int) {
	d.addSession(1)
	defer d.addSession(-1)

	// Two independent checks, both of which must pass. Local policy is decided
	// here and comes from this machine's own config file; the exposed-port list
	// comes from the relay and only catches a relay that is buggy or out of date,
	// not one that has been taken over.
	pol, policyOK := d.livePolicy()
	if !policyOK {
		d.audit.record(auditEntry{Event: "proxy", Port: port, Detail: "policy unreadable", Allowed: false})
		body := "Local policy on this system could not be read; nothing is being served.\n"
		fmt.Fprintf(stream, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain; charset=utf-8\r\n"+
			"Content-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		return
	}
	if ok, reason := pol.proxyAllowed(port); !ok {
		d.audit.record(auditEntry{Event: "proxy", Port: port, Detail: reason, Allowed: false})
		d.logf("proxy refused by local policy: %s", reason)
		body := fmt.Sprintf("Local policy on this system refuses port %d.\n", port)
		fmt.Fprintf(stream, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain; charset=utf-8\r\n"+
			"Content-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		return
	}
	if !d.proxyPortAllowed(port) {
		d.audit.record(auditEntry{Event: "proxy", Port: port, Allowed: false})
		d.logf("proxy refused: port %d is not exposed", port)
		body := fmt.Sprintf("Port %d is not exposed on this system.\n", port)
		fmt.Fprintf(stream, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain; charset=utf-8\r\n"+
			"Content-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		return
	}

	p := strconv.Itoa(port)
	local, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", p))
	if err != nil {
		// Dev servers (Vite, Node, …) often bind IPv6 loopback only; try ::1 too.
		local, err = net.Dial("tcp", net.JoinHostPort("::1", p))
	}
	if err != nil {
		d.logf("proxy dial :%d: %v", port, err)
		// Send a clear 502 so the iframe shows a message instead of a bare EOF.
		body := fmt.Sprintf(
			"Nothing is listening on port %d on this system.\nStart your app on that port and reload.\n", port)
		fmt.Fprintf(stream, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain; charset=utf-8\r\n"+
			"Content-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		return
	}
	defer local.Close()
	d.audit.record(auditEntry{Event: "proxy", Port: port, Allowed: true})
	d.logf("proxy session -> :%d", port)
	defer d.logf("proxy session closed (:%d)", port)

	done := make(chan struct{}, 2)
	// stream -> local (use buffered reader from header parse).
	go func() { io.Copy(local, br); done <- struct{}{} }()
	// local -> stream.
	go func() { io.Copy(stream, local); done <- struct{}{} }()
	<-done
}
