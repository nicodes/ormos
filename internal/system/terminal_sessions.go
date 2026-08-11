package system

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
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
	// maxTerminalConns caps live connections to ONE session. maxTerminalSessions
	// bounds sessions, but a session is looked up by the id on the header, so
	// without this a peer can attach to one session as many times as it has
	// stream slots. Each connection now carries a terminalSendBytes queue, so
	// that is relay-controlled agent memory measured in hundreds of megabytes.
	// A terminal is one view, occasionally two while a reattach overlaps the
	// connection it replaces; four is slack, not a budget.
	maxTerminalConns = 4
	// terminalSendQueue and terminalSendBytes bound how far one connection may
	// fall behind the PTY before its backlog is collapsed into a single
	// catch-up frame.
	//
	// Both bounds exist because either can bind first: a client taking bulk
	// output hits the byte budget after a few chunks, while one taking a stream
	// of keystroke echoes would sit at a few bytes a frame forever.
	//
	// Overrunning them is deliberately NOT grounds for disconnection. A
	// connection falls behind either because its socket is stuck or because its
	// writer goroutine has not been scheduled, and from here those are
	// indistinguishable — on a single-core host under load the second is
	// ordinary. Dropping that kind costs a healthy session a fresh X25519
	// handshake and a 256 KiB replay at exactly the moment the link is busiest,
	// which is self-reinforcing. What ends a connection instead is its own
	// terminalWriteTO deadline in writeLoop, which measures lateness rather
	// than backlog.
	terminalSendQueue = 256
	terminalSendBytes = 1 << 20
	// terminalResyncPrefix precedes a catch-up replay: RIS, a full terminal
	// reset. A connection that missed frames has to redraw from a KNOWN state
	// rather than append the replay to what it has already rendered, and the
	// terminal's own reset is how to say that without inventing a frame type
	// the browser would first have to learn.
	//
	// RIS rather than a clear-screen sequence, because clearing is not
	// resetting. A session resynchronised while a full-screen program is
	// running is in the alternate buffer with a scroll region, SGR attributes
	// and a charset set by that program; erasing the display leaves every one
	// of those in place and paints main-screen history into the alternate
	// buffer. Erase-scrollback would also throw away the user's real scrollback
	// on each resynchronisation, which a reattach has never done.
	terminalResyncPrefix = "\x1bc"
	// terminalCoalesceMax is the largest chunk readPTY builds out of
	// consecutive PTY reads before sealing and sending it, and so also the size
	// of its read buffer. Well under relay.MaxFrameSize, which bounds what the
	// far end will accept.
	terminalCoalesceMax = 64 << 10
	// terminalCoalesceMinChunk is the read size above which output is treated
	// as bulk and worth waiting on. A keystroke echo is a handful of bytes; a
	// tty hands back kilobytes at a time. Anything smaller leaves immediately,
	// so interactive latency is untouched.
	terminalCoalesceMinChunk = 1 << 10
	// terminalCoalesceWindow bounds how long a bulk chunk may wait for more
	// output to batch with. It is the entire latency cost of coalescing, and it
	// is paid only by output that already arrived in tty-chunk quantities.
	terminalCoalesceWindow = 2 * time.Millisecond
)

// terminalKillGrace is how long close waits for the shell's process group to
// die from SIGHUP before escalating to SIGKILL. A var so tests can shrink it.
var terminalKillGrace = 2 * time.Second

// terminalHandshakeTO bounds the whole sealed handshake — client hello, server
// hello, key agreement — the way headerReadTO bounds the stream header before
// it. serveStream clears its deadline once a stream has announced itself, and
// the very next read on a terminal stream is the client hello; without this the
// threat model's compromised relay could open a terminal stream, send a
// well-formed header, and never say another word, pinning a goroutine and one
// of the relay.MaxTunnelStreams slots for as long as the agent runs.
//
// Deliberately separate from headerReadTO despite sharing its value: they bound
// different phases against different peers' behaviour, and collapsing them into
// one knob would mean neither can be tuned without moving the other. A var so
// tests can shrink it.
var terminalHandshakeTO = 10 * time.Second

// replayRing is the bounded terminal history a reattaching or resynchronising
// xterm redraws from.
//
// A fixed ring rather than a slice that is appended to and trimmed, because the
// trim was a memmove of the whole bound on every single PTY read once the
// buffer was full. A megabyte of `cat` against a full 256 KiB buffer shifted
// roughly 64 MB for 1 MB of real output — under the session lock, compounding
// the head-of-line problem, and running even with no client attached at all, so
// a detached build spamming logs burned CPU indefinitely. Steady-state append
// is now O(chunk) and independent of the bound.
//
// Not safe for concurrent use: terminalSession.mu covers every call, in the
// same way it covers broadcast.
//
// A zero value is usable and sizes itself to terminalReplayBytes on first
// append. Capacity is len(buf) rather than a field of its own, so there is no
// second copy of it to disagree; tests and the benchmark shrink it by
// constructing the ring with a buffer of the size they want.
type replayRing struct {
	buf   []byte
	start int // index of the oldest byte held
	size  int // bytes held
}

func (r *replayRing) append(p []byte) {
	if r.buf == nil {
		r.buf = make([]byte, terminalReplayBytes)
	}
	capacity := len(r.buf)
	// A chunk at least as large as the whole ring replaces it outright; only
	// its tail could survive anyway.
	if len(p) >= capacity {
		copy(r.buf, p[len(p)-capacity:])
		r.start, r.size = 0, capacity
		return
	}
	end := (r.start + r.size) % capacity
	n := copy(r.buf[end:], p)
	copy(r.buf, p[n:])
	if r.size += len(p); r.size > capacity {
		r.start = (r.start + r.size - capacity) % capacity
		r.size = capacity
	}
}

// snapshotAfter returns prefix followed by the history, oldest-first, in a
// single allocation and a single pass.
//
// One copy rather than two, because this runs under the session lock on the PTY
// read path: snapshot-then-concatenate moved the whole bound twice, which is
// most of what the ring exists to stop doing.
func (r *replayRing) snapshotAfter(prefix string) []byte {
	out := make([]byte, 0, len(prefix)+r.size)
	out = append(out, prefix...)
	out = append(out, r.buf[r.start:min(r.start+r.size, len(r.buf))]...)
	if tail := r.size - (len(out) - len(prefix)); tail > 0 {
		out = append(out, r.buf[:tail]...)
	}
	return out
}

// snapshot returns the history oldest-first as one contiguous slice.
//
// It copies, which is both what lets the ring keep wrapping underneath and what
// makes the result safe to hand to a connection's writer goroutine. That copy
// is paid per attach or per resynchronisation — both rare — rather than once
// per PTY read.
func (r *replayRing) snapshot() []byte {
	out := make([]byte, r.size)
	n := copy(out, r.buf[r.start:min(r.start+r.size, len(r.buf))])
	copy(out[n:], r.buf)
	return out
}

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
	replay    replayRing
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
//
// Every connection owns a send queue and a writer goroutine, and nothing on the
// PTY read path ever touches the socket. That separation is the whole point:
// writing to a browser that has stopped draining — a backgrounded tab, a slow
// relay leg, a full yamux window — used to block the session for the whole
// terminalWriteTO, during which the PTY master went undrained, the kernel's tty
// buffer filled, and the foreground process itself blocked in write().
//
// Sealing happens in the writer goroutine too. That keeps the direction's nonce
// counter single-threaded without the session lock standing in for it, and
// exactly one writer per connection is guaranteed by construction rather than
// by convention: newSealedConn starts the only writeLoop there will ever be. A
// second one would reuse a ChaCha20-Poly1305 nonce, which fails silently and
// catastrophically, so it must not be reachable by calling something twice.
type sealedConn struct {
	net.Conn
	stream *relay.SealedStream

	// send carries encoded (not yet sealed) frames to the writer goroutine.
	// Frames are immutable once encoded, so one buffer is safely shared by
	// every connection a broadcast reaches.
	send chan []byte
	// queued approximates the byte weight sitting in send. It is approximate on
	// purpose and in both directions: the writer decrements before writing, so
	// a frame in flight is uncounted while still referenced, and enqueue adds
	// after the channel send, so a reader can transiently see it negative. The
	// atomics commute, producers are serialised by the session lock, and the
	// real ceiling is terminalSendBytes plus one in-flight frame — which is all
	// this needs to be for its purpose, which is deciding when to collapse a
	// backlog.
	queued atomic.Int64
	// dead is closed once, by kill, and is what stops the writer goroutine and
	// tells enqueue to stop accepting.
	dead    chan struct{}
	killOne sync.Once
}

func newSealedConn(conn net.Conn, stream *relay.SealedStream) *sealedConn {
	c := &sealedConn{
		Conn:   conn,
		stream: stream,
		send:   make(chan []byte, terminalSendQueue),
		dead:   make(chan struct{}),
	}
	go c.writeLoop()
	return c
}

// enqueue hands one encoded frame to the connection's writer, reporting false
// if the connection is gone or has fallen too far behind to take it. It never
// blocks — that is the property the PTY read loop depends on.
func (c *sealedConn) enqueue(frame []byte) bool {
	select {
	case <-c.dead:
		return false
	default:
	}
	if c.queued.Load()+int64(len(frame)) > terminalSendBytes {
		return false
	}
	select {
	case c.send <- frame:
		c.queued.Add(int64(len(frame)))
		return true
	default:
		return false
	}
}

// writeLoop seals and writes queued frames until the connection dies. The
// per-write deadline is still terminalWriteTO, but it now bounds only this
// goroutine: a socket that stalls costs its own connection and nothing else.
func (c *sealedConn) writeLoop() {
	for {
		select {
		case <-c.dead:
			return
		case frame := <-c.send:
			c.queued.Add(-int64(len(frame)))
			_ = c.SetWriteDeadline(time.Now().Add(terminalWriteTO))
			err := c.stream.WriteFrame(frame)
			_ = c.SetWriteDeadline(time.Time{})
			if err != nil {
				c.kill()
				return
			}
		}
	}
}

// resync replaces everything queued on the connection with one catch-up frame,
// reporting false only if the connection is already dead.
//
// Discarding queued frames is safe for the seal: sealing happens in writeLoop
// at write time, so a frame that never reaches it never consumed a nonce, and
// both ends stay in lockstep. It is safe for the screen because the catch-up
// frame carries a reset (terminalResyncPrefix) and the whole replay history, so
// the browser redraws rather than resuming mid-stream.
func (c *sealedConn) resync(frame []byte) bool {
drain:
	for {
		select {
		case stale := <-c.send:
			c.queued.Add(-int64(len(stale)))
		default:
			break drain
		}
	}
	return c.enqueue(frame)
}

// kill ends the connection and wakes whoever is blocked on it.
//
// The read deadline is not belt-and-braces: this net.Conn is a *yamux.Stream,
// and Close on an established stream only moves it to streamLocalClose, which
// yamux documents as prohibiting further local writes while reads carry on
// normally. A parked Read is woken, finds an empty receive buffer and parks
// again — so without the deadline a killed connection's attach goroutine, its
// relay.MaxTunnelStreams slot and everything still queued on it survive until
// the peer sends a FIN or StreamCloseTimeout (two minutes) fires. Against the
// silent peer this path exists to defend against, that is the whole two
// minutes. net.Pipe, which the tests use, does wake on Close, which is exactly
// why a test cannot be what establishes this.
func (c *sealedConn) kill() {
	c.killOne.Do(func() {
		close(c.dead)
		_ = c.SetReadDeadline(time.Now())
		_ = c.Close()
	})
}

func (d *system) handleTerminal(stream net.Conn, br *bufio.Reader, h relay.StreamHeader) {
	if h.SessionID == "" {
		d.logf("terminal refused: missing session id")
		return
	}
	// The handshake gets its own deadline, covering the read of the hello, the
	// write of the reply and the agreement between them — a peer that never
	// READS stalls the reply just as effectively as one that never writes. It
	// is cleared only once there is a sealed stream to hand to attach, past
	// which point there must be no read deadline at all: an idle terminal is
	// not a broken one, and only the writer goroutine is paced.
	_ = stream.SetDeadline(time.Now().Add(terminalHandshakeTO))
	// The browser opens with its ephemeral public key. Everything after this is
	// sealed, so a failure here has to end the stream rather than fall back to
	// plaintext — a downgrade that a peer could ask for is not a protection.
	peer, err := relay.ReadClientHello(br)
	if err != nil {
		d.logf("terminal refused: %v", err)
		return
	}
	// Reply with a fresh per-connection salt before deriving. It goes into the
	// key schedule on both ends, so every connection — including a reattach that
	// reuses a client ephemeral — lands in its own key/nonce space.
	salt, err := relay.GenerateServerSalt()
	if err != nil {
		d.logf("terminal refused: %v", err)
		return
	}
	if err := relay.WriteServerHello(stream, salt); err != nil {
		d.logf("terminal refused: %v", err)
		return
	}
	// Both public keys, the salt and the session id are bound into the schedule,
	// so a relay that swapped the agent's key yields a derivation the client
	// rejects, and a record captured from one tab cannot be replayed into another.
	keys, err := relay.DeriveSessionKeys(d.key, peer, salt, h.SessionID)
	if err != nil {
		d.logf("terminal refused: key agreement failed: %v", err)
		return
	}
	sealed, err := relay.NewSealedStream(br, stream, keys.AgentToClient, keys.ClientToAgent)
	if err != nil {
		d.logf("terminal refused: %v", err)
		return
	}
	_ = stream.SetDeadline(time.Time{})

	s, err := d.terminal(h)
	if err != nil {
		d.logf("terminal session: %v", err)
		return
	}
	s.attach(newSealedConn(stream, sealed))
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
	// Unconditional: the header's dimensions were validated at the top of this
	// function, so there is no reachable case where they are zero.
	_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(h.Cols), Rows: uint16(h.Rows)})
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
		conn.kill()
		return
	}
	if s.conns == nil {
		s.conns = make(map[*sealedConn]struct{})
	}
	if len(s.conns) >= maxTerminalConns {
		s.mu.Unlock()
		s.owner.logf("terminal refused: session already has %d connections", maxTerminalConns)
		conn.kill()
		return
	}
	s.conns[conn] = struct{}{}
	if s.expires != nil {
		s.expires.Stop()
		s.expires = nil
	}
	// Replaying the bounded terminal history lets an xterm reconstruct the
	// current display after mobile sleep or a WebView remount. It is queued like
	// everything else, so it cannot block the lock, and a browser that never
	// reads it costs this connection alone.
	//
	// Neither enqueue can fail here — the connection is new, so its queue is
	// empty, and the catch-up frame is bounded by terminalReplayBytes, a quarter
	// of the byte budget. It is still checked, because the failure mode if that
	// ever stops being true is a browser painting live output onto a screen it
	// never received, with nothing anywhere saying so.
	if !conn.enqueue(relay.EncodeData(s.catchUp())) || !conn.enqueue(relay.EncodeActivity(s.active)) {
		delete(s.conns, conn)
		// Back to no connections, and attach stopped the timer above — without
		// re-arming it the session would keep its shell until the process died.
		if len(s.conns) == 0 && s.expires == nil {
			s.expires = time.AfterFunc(terminalDetachTTL, s.close)
		}
		s.mu.Unlock()
		conn.kill()
		return
	}
	s.mu.Unlock()

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
	conn.kill()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	delete(s.conns, conn)
	if len(s.conns) == 0 && s.expires == nil {
		s.expires = time.AfterFunc(terminalDetachTTL, s.close)
	}
}

func (s *terminalSession) readPTY() {
	r, err := newPTYReader(s.ptmx)
	if err != nil {
		s.owner.logf("terminal: cannot read the pty: %v", err)
		s.close()
		return
	}
	buf := make([]byte, terminalCoalesceMax)
	for {
		n, _, err := r.read(buf)
		if n > 0 {
			s.output(buf[:n])
		}
		if err != nil {
			s.close()
			return
		}
	}
}

// ptyReader reads coalesced chunks from a PTY master.
//
// A Linux PTY master hands back at most one tty chunk per read, and one read
// used to become exactly one sealed record — with everything that costs
// downstream, all the way to a fresh AEAD instance and a term.write in the
// browser. The relay deliberately cannot parse a record, so it cannot merge
// two; the batching has to happen here. ssh and mosh both coalesce for the same
// reason.
type ptyReader struct {
	f  *os.File
	rc syscall.RawConn
	// fds is reused across probes rather than built per call: it escapes into
	// the poll syscall, and read runs on exactly one goroutine per session.
	fds [1]unix.PollFd
	// blind latches a probe that failed for a reason the blocking read did not
	// also surface. Retrying it per chunk would buy a syscall pair for ever and
	// coalesce nothing.
	blind bool
}

func newPTYReader(f *os.File) (*ptyReader, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	return &ptyReader{f: f, rc: rc}, nil
}

// read performs one blocking read and then absorbs whatever more the tty has,
// up to len(buf). It reports how many underlying reads produced the chunk,
// which is what the coalescing test measures.
//
// The window opens on the SIZE of the blocking read, not on a zero-wait probe.
// A probe is a bet on the writer having been scheduled in the gap between the
// read returning and poll(2) running: on an idle machine that bet wins, and
// under load it loses, so coalescing switched itself off precisely when the
// output storm made it worth having — measured degrading from 65 KiB a chunk to
// 7 KiB under CPU contention. The size of the first read carries the same
// information with no timing in it at all, because a keystroke echo is a
// handful of bytes while a tty chunk is kilobytes. A lone echo therefore still
// leaves without waiting a nanosecond, and bulk output gets the full window
// whatever the host is doing.
func (p *ptyReader) read(buf []byte) (n, reads int, err error) {
	n, err = p.f.Read(buf)
	reads = 1
	if err != nil || n < terminalCoalesceMinChunk || p.blind {
		return n, reads, err
	}
	deadline := time.Now().Add(terminalCoalesceWindow)
	for n < len(buf) {
		if !p.readable(deadline) {
			return n, reads, nil
		}
		m, rerr := p.f.Read(buf[n:])
		reads++
		n += m
		if rerr != nil {
			return n, reads, rerr
		}
	}
	return n, reads, nil
}

// readable reports whether the master has bytes ready, waiting no later than
// deadline.
//
// It reports readiness only. A failed probe is deliberately indistinguishable
// from "nothing more is waiting": the genuine error is surfaced by the next
// blocking read, and there is nothing useful to do with a transient poll
// failure in the middle of a chunk. One that would repeat, though, latches, so
// a permanently failing probe stops costing a syscall pair per chunk.
//
// poll on the raw descriptor rather than a read with a deadline, because
// creack/pty does not build the master the same way on every supported
// platform — Linux opens it, macOS wraps an already-open blocking descriptor —
// so whether the runtime poller has it, and therefore whether SetReadDeadline
// does anything at all rather than returning ErrNoDeadline, is not something
// this code can rely on. It also keeps a runtime timer off a path that runs per
// chunk.
//
// The wait rounds UP to whole milliseconds, which is all poll(2) takes: a
// sub-millisecond remainder truncated toward zero would quietly turn the tail
// of the window into non-blocking probes and halve it.
func (p *ptyReader) readable(deadline time.Time) bool {
	var ready bool
	err := p.rc.Control(func(fd uintptr) {
		p.fds[0] = unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}
		for {
			wait := time.Until(deadline)
			if wait <= 0 {
				return
			}
			ms := int((wait + time.Millisecond - 1) / time.Millisecond)
			n, err := unix.Poll(p.fds[:], ms)
			if err == unix.EINTR {
				continue // with what is LEFT of the budget, not a fresh one
			}
			if err != nil {
				p.blind = true
				return
			}
			ready = n > 0
			return
		}
	})
	if err != nil {
		p.blind = true
	}
	return ready
}

func (s *terminalSession) output(data []byte) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.replay.append(data)
	var dropped []*sealedConn
	if len(s.conns) > 0 {
		// Encoded here, under the lock, rather than before it. encodeFrame
		// copies, and that copy is both what makes readPTY's reusable buffer
		// safe to hand to another goroutine and what lets every connection
		// share one frame — but it is the same memcpy of the same chunk that
		// the replay append above already pays here, so it does not change the
		// order of this critical section. Doing it inside is what lets a
		// detached session — up to terminalDetachTTL, an hour, a build spamming
		// logs into nothing — pay none of it.
		dropped = s.broadcast(relay.EncodeData(data), false)
	} else if s.expires == nil {
		s.expires = time.AfterFunc(terminalDetachTTL, s.close)
	}
	s.mu.Unlock()
	// Off this goroutine entirely: this is the PTY read loop, and killing a
	// yamux stream writes a close frame, which parks for up to yamux's
	// ConnectionWriteTimeout when the tunnel's writer is wedged — the very
	// condition that causes the drop. Nothing on the PTY read path may wait on
	// the network, including on the way out.
	if len(dropped) > 0 {
		go killAll(dropped)
	}
}

// catchUp is the replay history behind a screen reset: what a connection needs
// in order to redraw, whether it is new or has just missed frames.
func (s *terminalSession) catchUp() []byte {
	return s.replay.snapshotAfter(terminalResyncPrefix)
}

// broadcast queues one encoded frame on every attached connection, collapsing
// the backlog of any that has fallen a whole budget behind, and returns those
// that turned out to be dead. The caller holds s.mu.
//
// Falling behind is not itself fatal — see terminalSendBytes for why a starved
// writer goroutine and a stuck socket cannot be told apart from here. A
// connection only appears in the returned slice once its own write deadline has
// already killed it. Deleting it here but killing outside the lock is
// deliberate: closing a stream is network work, and this lock sits directly on
// the PTY read path.
//
// It also arms the detach TTL when the last connection goes, which is how a
// session whose every client died eventually reaps itself.
func (s *terminalSession) broadcast(frame []byte, resendOnResync bool) []*sealedConn {
	var dropped []*sealedConn
	var catchUp []byte
	for conn := range s.conns {
		if conn.enqueue(frame) {
			continue
		}
		if catchUp == nil {
			catchUp = relay.EncodeData(s.catchUp()) // at most once per broadcast
		}
		// The catch-up frame carries the terminal's screen, so a data frame it
		// replaced is already in it. An activity frame is not — it is a state
		// transition the replay says nothing about, and swallowing one leaves
		// the browser's running/idle indicator wrong until the next transition,
		// which may be an hour away. Those are re-sent behind the catch-up.
		if !conn.resync(catchUp) || (resendOnResync && !conn.enqueue(frame)) {
			dropped = append(dropped, conn)
			delete(s.conns, conn)
		}
	}
	if len(s.conns) == 0 && s.expires == nil {
		s.expires = time.AfterFunc(terminalDetachTTL, s.close)
	}
	return dropped
}

func killAll(conns []*sealedConn) {
	for _, conn := range conns {
		conn.kill()
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
	// Before the encode, not after: pollActivity ticks every 700 ms for the
	// life of the session and almost always finds nothing changed, and
	// EncodeActivity marshals JSON.
	if s.closed || active == s.active {
		s.mu.Unlock()
		return
	}
	s.active = active
	dropped := s.broadcast(relay.EncodeActivity(active), true)
	s.mu.Unlock()
	if len(dropped) > 0 {
		go killAll(dropped)
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
		// Stopped, not left to fire: close is idempotent so a late tick is
		// harmless, but an armed timer holds this session — and the PTY and
		// buffers it closes over — reachable for up to terminalDetachTTL.
		if s.expires != nil {
			s.expires.Stop()
			s.expires = nil
		}
		s.mu.Unlock()
		for conn := range conns {
			conn.kill()
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
