package system

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/nicodes/ormos/relay"
	"golang.org/x/sys/unix"
)

const (
	terminalReplayBytes = 256 << 10
	terminalDetachTTL   = time.Hour
	terminalWriteTO     = 2 * time.Second
	maxTerminalSessions = 32
)

// terminalKillGrace is how long close waits for the shell's process group to
// die from SIGHUP before escalating to SIGKILL. A var so tests can shrink it.
var terminalKillGrace = 2 * time.Second

// terminalSession owns the PTY independently of any one tunnel stream. A
// browser disconnect only replaces conn; the shell and buffered output remain.
type terminalSession struct {
	id    string
	owner *system
	cwd   string // the directory policy was evaluated against
	ptmx  *os.File
	cmd   *exec.Cmd
	pgid  int // the shell's process group id (it leads it: pty.Start sets Setsid)

	mu        sync.Mutex
	inputMu   sync.Mutex
	conns     map[*sealedConn]struct{}
	buffer    []byte
	active    bool
	closed    bool
	expires   *time.Timer
	done      chan struct{}
	closeOnce sync.Once
}

// sealedConn is one browser connection attached to a terminal session.
//
// Sealing is per connection, not per session: a session the browser reattaches
// to gets a fresh ephemeral key each time, so two live connections to one
// terminal have independent keys and counters and share no crypto state. The
// embedded net.Conn is kept for deadlines and close; everything that carries
// terminal data goes through stream.
type sealedConn struct {
	net.Conn
	stream *relay.SealedStream
}

func (d *system) handleTerminal(stream net.Conn, br *bufio.Reader, h relay.StreamHeader) {
	if h.SessionID == "" {
		d.logf("terminal refused: missing session id")
		return
	}
	// The browser opens with its ephemeral public key. Everything after this is
	// sealed, so a failure here has to end the stream rather than fall back to
	// plaintext — a downgrade that a peer could ask for is not a protection.
	peer, err := relay.ReadClientHello(br)
	if err != nil {
		d.logf("terminal refused: %v", err)
		return
	}
	// The session id is bound into the key schedule, so a record captured from
	// one tab cannot be replayed into another even between the same two ends.
	keys, err := relay.DeriveSessionKeys(d.key, peer, h.SessionID)
	if err != nil {
		d.logf("terminal refused: key agreement failed: %v", err)
		return
	}
	sealed, err := relay.NewSealedStream(br, stream, keys.AgentToClient, keys.ClientToAgent)
	if err != nil {
		d.logf("terminal refused: %v", err)
		return
	}

	s, err := d.terminal(h)
	if err != nil {
		d.logf("terminal session: %v", err)
		return
	}
	s.attach(&sealedConn{Conn: stream, stream: sealed})
}

func (d *system) terminal(h relay.StreamHeader) (*terminalSession, error) {
	if h.Cols < 1 || h.Cols > 1000 || h.Rows < 1 || h.Rows > 1000 {
		return nil, fmt.Errorf("invalid terminal size")
	}

	// Resolve the directory first: local policy is decided on the path this
	// machine would actually use, not the string the relay sent. Relay input
	// gets ~ expansion only — never environment expansion (expandRelayCwd).
	cwd := ""
	if h.Cwd != "" {
		expanded, err := expandRelayCwd(h.Cwd)
		if err != nil {
			d.audit.record(auditEntry{Event: "terminal", Detail: h.Cwd, Allowed: false})
			d.logf("terminal refused: %v", err)
			return nil, err
		}
		if fi, err := os.Stat(expanded); err == nil && fi.IsDir() {
			cwd = expanded
		} else {
			d.logf("terminal cwd %q unusable, using default", h.Cwd)
		}
	}
	// Checked before the session lookup, and on every attach rather than only on
	// creation. Reattaching to a session opened under a laxer policy — or under a
	// project whose root directory has since moved — has to be a fresh decision,
	// or a session outlives the permission that created it for as long as it is
	// kept alive.
	p, policyOK := d.livePolicy()
	if !policyOK {
		d.audit.record(auditEntry{Event: "terminal", Detail: "policy unreadable", Allowed: false})
		return nil, fmt.Errorf("local policy could not be read")
	}
	if ok, reason := p.terminalAllowed(cwd); !ok {
		d.audit.record(auditEntry{Event: "terminal", Detail: cwd, Allowed: false})
		d.logf("terminal refused: %s", reason)
		return nil, fmt.Errorf("%s", reason)
	}

	d.terminalMu.Lock()
	defer d.terminalMu.Unlock()
	if s := d.terminals[h.SessionID]; s != nil {
		// The check above covered the REQUESTED directory; the session being
		// reattached to may be rooted somewhere else entirely, and a relay that
		// learns its id inherits whatever it was opened on. Re-decide against
		// the session's own cwd, or the reattach path is a way around a policy
		// that tightened since the shell was spawned.
		if ok, reason := p.terminalAllowed(s.cwd); !ok {
			d.audit.record(auditEntry{Event: "terminal-reattach", Detail: s.cwd, Allowed: false})
			d.logf("terminal reattach refused: %s", reason)
			return nil, fmt.Errorf("%s", reason)
		}
		d.audit.record(auditEntry{Event: "terminal-reattach", Detail: s.cwd, Allowed: true})
		return s, nil
	}
	if len(d.terminals) >= maxTerminalSessions {
		return nil, fmt.Errorf("terminal session limit reached (%d)", maxTerminalSessions)
	}
	d.audit.record(auditEntry{Event: "terminal", Detail: cwd, Allowed: true})

	cmd := exec.Command(d.cfg.Shell)
	cmd.Env = append(scrubbedEnv(), "TERM=xterm-256color")
	if cwd != "" {
		cmd.Dir = cwd
	}
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}
	// pty.Start runs the shell with Setsid, so it leads a new session and its
	// process group id equals its pid. Capture the pgid now — close needs it
	// to signal the whole group, and by the time close runs the shell may
	// already be gone.
	pgid, err := unix.Getpgid(cmd.Process.Pid)
	if err != nil {
		_ = ptmx.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return nil, fmt.Errorf("read shell process group: %w", err)
	}
	if h.Cols > 0 && h.Rows > 0 {
		_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(h.Cols), Rows: uint16(h.Rows)})
	}
	s := &terminalSession{id: h.SessionID, owner: d, cwd: cwd, ptmx: ptmx, cmd: cmd, pgid: pgid, conns: make(map[*sealedConn]struct{}), done: make(chan struct{})}
	d.terminals[h.SessionID] = s
	d.addSession(1)
	d.logf("terminal session started (%s)", d.cfg.Shell)
	go s.readPTY()
	go s.pollActivity()
	return s, nil
}

// enforcePolicy ends any live terminal the current policy would no longer
// permit. Re-checking on attach is not enough on its own: a session with a
// client already attached would otherwise keep its shell for as long as that
// client holds on, so tightening policy would appear to do nothing to the
// session someone is most likely worried about.
func (d *system) enforcePolicy() {
	p, ok := d.livePolicy()

	d.terminalMu.Lock()
	doomed := make([]*terminalSession, 0)
	for _, s := range d.terminals {
		if !ok {
			doomed = append(doomed, s)
			continue
		}
		if allowed, _ := p.terminalAllowed(s.cwd); !allowed {
			doomed = append(doomed, s)
		}
	}
	d.terminalMu.Unlock()

	for _, s := range doomed {
		d.logf("ending terminal in %q: local policy no longer allows it", s.cwd)
		d.audit.record(auditEntry{Event: "terminal-revoked", Detail: s.cwd, Allowed: false})
		s.close()
	}
}

func (s *terminalSession) attach(conn *sealedConn) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.conns == nil {
		s.conns = make(map[*sealedConn]struct{})
	}
	s.conns[conn] = struct{}{}
	if s.expires != nil {
		s.expires.Stop()
		s.expires = nil
	}
	// Replaying the bounded terminal history lets a fresh xterm reconstruct the
	// current display after mobile sleep or a WebView remount.
	_ = conn.SetWriteDeadline(time.Now().Add(terminalWriteTO))
	err := conn.stream.WriteFrame(relay.EncodeData(s.buffer))
	if err == nil {
		err = conn.stream.WriteFrame(relay.EncodeActivity(s.active))
	}
	_ = conn.SetWriteDeadline(time.Time{})
	if err != nil {
		delete(s.conns, conn)
	}
	s.mu.Unlock()
	if err != nil {
		return
	}

	for {
		frame, err := conn.stream.ReadFrame()
		if err != nil {
			break
		}
		switch {
		case frame.Data != nil:
			s.inputMu.Lock()
			if _, err := s.ptmx.Write(frame.Data); err != nil {
				s.inputMu.Unlock()
				s.detach(conn)
				return
			}
			s.inputMu.Unlock()
		case frame.Resize != nil:
			if frame.Resize.Cols < 1 || frame.Resize.Cols > 1000 || frame.Resize.Rows < 1 || frame.Resize.Rows > 1000 {
				continue
			}
			_ = pty.Setsize(s.ptmx, &pty.Winsize{Cols: uint16(frame.Resize.Cols), Rows: uint16(frame.Resize.Rows)})
		}
	}
	s.detach(conn)
}

func (s *terminalSession) detach(conn *sealedConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	delete(s.conns, conn)
	if len(s.conns) == 0 {
		s.expires = time.AfterFunc(terminalDetachTTL, s.close)
	}
}

func (s *terminalSession) readPTY() {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.output(buf[:n])
		}
		if err != nil {
			s.close()
			return
		}
	}
}

func (s *terminalSession) output(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.buffer = append(s.buffer, data...)
	if len(s.buffer) > terminalReplayBytes {
		copy(s.buffer, s.buffer[len(s.buffer)-terminalReplayBytes:])
		s.buffer = s.buffer[:terminalReplayBytes]
	}
	for conn := range s.conns {
		_ = conn.SetWriteDeadline(time.Now().Add(terminalWriteTO))
		if err := conn.stream.WriteFrame(relay.EncodeData(data)); err != nil {
			_ = conn.Close()
			delete(s.conns, conn)
			continue
		}
		_ = conn.SetWriteDeadline(time.Time{})
	}
	if len(s.conns) == 0 && s.expires == nil {
		s.expires = time.AfterFunc(terminalDetachTTL, s.close)
	}
}

func (s *terminalSession) pollActivity() {
	ticker := time.NewTicker(700 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			fg, err := unix.IoctlGetInt(int(s.ptmx.Fd()), unix.TIOCGPGRP)
			if err != nil {
				return
			}
			s.setActivity(fg > 0 && fg != s.cmd.Process.Pid)
		}
	}
}

func (s *terminalSession) setActivity(active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || active == s.active {
		return
	}
	s.active = active
	for conn := range s.conns {
		_ = conn.SetWriteDeadline(time.Now().Add(terminalWriteTO))
		if err := conn.stream.WriteFrame(relay.EncodeActivity(active)); err != nil {
			_ = conn.Close()
			delete(s.conns, conn)
			continue
		}
		_ = conn.SetWriteDeadline(time.Time{})
	}
	if len(s.conns) == 0 && s.expires == nil {
		s.expires = time.AfterFunc(terminalDetachTTL, s.close)
	}
}

// killProcessGroup ends the shell and everything it spawned. Signalling only
// the shell's pid leaves children behind — detached from the dead terminal but
// still running — which is exactly what a policy revocation (enforcePolicy) is
// meant to stop. The shell leads its own process group (pty.Start sets
// Setsid), so signalling -pgid reaches the group.
//
// SIGHUP first: a well-behaved process group exits on it, and the PTY master
// is closed right after so anything blocked on terminal I/O wakes with EIO.
// Whatever of the group is still there once the shell has been reaped — a
// child that ignores SIGHUP, a stopped job — gets the rest of a short grace
// and then SIGKILL, because a close that returns with the group alive has not
// closed anything that matters.
func (s *terminalSession) killProcessGroup() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	if s.pgid > 0 {
		_ = unix.Kill(-s.pgid, unix.SIGHUP)
	} else {
		_ = s.cmd.Process.Kill()
	}
	_ = s.ptmx.Close()
	reaped := make(chan struct{})
	go func() {
		_, _ = s.cmd.Process.Wait()
		close(reaped)
	}()
	grace := time.NewTimer(terminalKillGrace)
	defer grace.Stop()
	graced := false
	select {
	case <-reaped:
	case <-grace.C:
		graced = true
	}
	if s.pgid > 0 && unix.Kill(-s.pgid, 0) == nil {
		// The group outlived the shell; let the grace run out before the
		// SIGKILL, unless it already has.
		if !graced {
			<-grace.C
		}
		_ = unix.Kill(-s.pgid, unix.SIGKILL)
	}
	<-reaped
}

func (s *terminalSession) close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		conns := s.conns
		s.conns = nil
		s.mu.Unlock()
		for conn := range conns {
			_ = conn.Close()
		}
		s.killProcessGroup()
		close(s.done)

		s.owner.terminalMu.Lock()
		if s.owner.terminals[s.id] == s {
			delete(s.owner.terminals, s.id)
		}
		s.owner.terminalMu.Unlock()
		s.owner.addSession(-1)
		s.owner.logf("terminal session ended")
	})
}
