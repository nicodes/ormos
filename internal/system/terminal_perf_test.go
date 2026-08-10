package system

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/nicodes/ormos/relay"
)

// The terminal hot path is measured, not argued about: the numbers behind #206
// are what its issues are judged on, so the harness lives in the suite rather
// than in a scratch program nobody reruns. What is measured here is reported,
// not asserted — the guards that have to fail live with the behaviour they
// guard, which is why this is a benchmark.

// recordCounter is the far end of one terminal connection, counting what
// actually reaches the wire.
//
// Records are recovered from the four-byte big-endian length prefix that
// relay.WriteRecord puts in front of every one, so the record count is
// independent of how many Write calls carry them — one number for "how many
// sealed records did we pay for" and another for "how many yamux frames and
// WebSocket messages did they cost".
type recordCounter struct {
	mu      sync.Mutex
	records int
	bytes   int
	writes  int
	need    int    // payload bytes still owed to the record being parsed
	hdr     []byte // partial length prefix carried across a Write boundary
}

func (c *recordCounter) Write(p []byte) (int, error) {
	total := len(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
	c.bytes += total
	for len(p) > 0 {
		if c.need == 0 {
			take := min(4-len(c.hdr), len(p))
			c.hdr = append(c.hdr, p[:take]...)
			p = p[take:]
			if len(c.hdr) == 4 {
				c.need = int(binary.BigEndian.Uint32(c.hdr))
				c.hdr = c.hdr[:0]
				c.records++
			}
			continue
		}
		take := min(c.need, len(p))
		c.need -= take
		p = p[take:]
	}
	return total, nil
}

func (c *recordCounter) totals() (records, writes, bytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.records, c.writes, c.bytes
}

// discardConn satisfies the net.Conn half of a sealedConn without a socket.
// Every method is implemented rather than promoted from an embedded nil, so a
// call added to the write path later fails somewhere other than a test panic.
type discardConn struct{}

func (discardConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (discardConn) Write(p []byte) (int, error)      { return len(p), nil }
func (discardConn) Close() error                     { return nil }
func (discardConn) LocalAddr() net.Addr              { return pipeAddr{} }
func (discardConn) RemoteAddr() net.Addr             { return pipeAddr{} }
func (discardConn) SetDeadline(time.Time) error      { return nil }
func (discardConn) SetReadDeadline(time.Time) error  { return nil }
func (discardConn) SetWriteDeadline(time.Time) error { return nil }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "discard" }
func (pipeAddr) String() string  { return "discard" }

// blockedReader parks its caller until the test lets go, so a measured
// connection's inbound half stays open for the duration instead of hitting EOF
// and detaching on the first read.
type blockedReader struct{ ch chan struct{} }

func (b blockedReader) Read([]byte) (int, error) { <-b.ch; return 0, io.EOF }

// countingConn builds a connection whose outbound records are counted and whose
// far end drains at memory speed.
func countingConn(t testing.TB) (*sealedConn, *recordCounter) {
	t.Helper()
	key := make([]byte, relay.SealKeySize)
	counter := &recordCounter{}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sealed, err := relay.NewSealedStream(blockedReader{ch: release}, counter, key, key)
	if err != nil {
		t.Fatal(err)
	}
	return newSealedConn(discardConn{}, sealed), counter
}

// ptyPair opens a real PTY. Measuring against one is the point: the chunking
// this path pays for is the kernel tty's, and a bytes.Buffer would not
// reproduce it.
func ptyPair(t testing.TB) (ptmx, tty *os.File) {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no PTY available: %v", err)
	}
	t.Cleanup(func() { _ = tty.Close(); _ = ptmx.Close() })
	return ptmx, tty
}

// ptySession runs a session's read loop over a real PTY master. The returned
// file is the slave: writing to it is what a shell's stdout does, line
// discipline and kernel buffering included.
func ptySession(t testing.TB) (*terminalSession, *os.File) {
	t.Helper()
	ptmx, tty := ptyPair(t)
	owner := &system{terminals: make(map[string]*terminalSession)}
	s := &terminalSession{id: "perf", owner: owner, ptmx: ptmx, done: make(chan struct{})}
	owner.terminals[s.id] = s
	return s, tty
}

// fill writes n bytes of terminal output into a PTY slave, the way a shell's
// stdout does, in blocks of size each.
func fill(t testing.TB, tty *os.File, n, size int) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = 'a' // no newlines: ONLCR would inflate the byte count
	}
	for written := 0; written < n; {
		m, err := tty.Write(data[:min(len(data), n-written)])
		if err != nil {
			t.Errorf("write to pty after %d bytes: %v", written, err)
			return
		}
		written += m
	}
}

// BenchmarkTerminalOutputRecordRate reports what a megabyte of PTY output costs
// on the wire, for the two shapes real output comes in: a `cat` of a large
// file, which writes in big blocks, and a full-screen TUI redraw, which writes
// a couple of kilobytes at a time many times a second.
func BenchmarkTerminalOutputRecordRate(b *testing.B) {
	const payload = 1 << 20
	for _, bc := range []struct {
		name      string
		writeSize int
	}{
		{"cat_of_a_1MiB_file", 64 << 10},
		{"full_screen_TUI_redraws", 2 << 10},
	} {
		b.Run(bc.name, func(b *testing.B) {
			s, tty := ptySession(b)
			conn, counter := countingConn(b)
			go s.attach(conn)
			waitAttached(b, s)
			go s.readPTY()

			baseRecords, baseWrites, baseBytes := counter.totals()
			b.SetBytes(payload)
			b.ResetTimer()
			for b.Loop() {
				fill(b, tty, payload, bc.writeSize)
				waitQuiet(b, counter, payload)
			}
			b.StopTimer()

			records, writes, bytes := counter.totals()
			b.ReportMetric(float64(records-baseRecords)/float64(b.N), "records/MiB")
			b.ReportMetric(float64(writes-baseWrites)/float64(b.N), "writes/MiB")
			b.ReportMetric(float64(bytes-baseBytes)/float64(max(records-baseRecords, 1)), "bytes/record")
		})
	}
}

// waitAttached blocks until the session has registered a connection.
func waitAttached(t testing.TB, s *terminalSession) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.conns)
		s.mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("connection never attached")
		}
		time.Sleep(time.Millisecond)
	}
}

// waitQuiet blocks until the counter has taken at least want more bytes and
// then stopped growing.
func waitQuiet(t testing.TB, c *recordCounter, want int) {
	t.Helper()
	_, _, start := c.totals()
	deadline := time.Now().Add(60 * time.Second)
	last := -1
	for {
		_, _, bytes := c.totals()
		if bytes >= start+want && bytes == last {
			return
		}
		last = bytes
		if time.Now().After(deadline) {
			t.Fatalf("output stalled at %d of %d bytes", bytes-start, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A megabyte of bulk output must not cost a sealed record, a yamux frame, a
// WebSocket message, a fresh ChaCha instance and a term.write per tty chunk.
func TestTerminalReadCoalescesBulkOutput(t *testing.T) {
	const payload = 1 << 20
	ptmx, tty := ptyPair(t)
	r, err := newPTYReader(ptmx)
	if err != nil {
		t.Fatal(err)
	}
	go fill(t, tty, payload, 64<<10)

	type result struct{ total, reads, chunks int }
	done := make(chan result, 1)
	go func() {
		buf := make([]byte, terminalCoalesceMax)
		var got result
		for got.total < payload {
			n, rd, err := r.read(buf)
			got.total += n
			got.reads += rd
			if n > 0 {
				got.chunks++
			}
			if err != nil {
				break
			}
		}
		done <- got
	}()

	// Bounded, so a producer that gave up fails here rather than parking until
	// the package's ten-minute panic buries the reason under a goroutine dump.
	var got result
	select {
	case got = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the PTY read loop never finished; the producer stopped early")
	}
	if got.total < payload {
		t.Fatalf("read %d of %d bytes", got.total, payload)
	}

	perChunk := got.total / max(got.chunks, 1)
	t.Logf("1 MiB of PTY output: %d reads coalesced into %d chunks (%d bytes/chunk)",
		got.reads, got.chunks, perChunk)
	// Two bars, because they fail for different reasons. Bytes per chunk is
	// what actually matters downstream, and it is stable now that the window
	// opens on the size of the first read rather than on whether the writer
	// happened to be scheduled. Reads per chunk is immune to how fast the
	// producer runs at all: uncoalesced it is exactly 1.0 by construction.
	if perChunk < 16<<10 {
		t.Errorf("%d reads produced %d chunks of %d bytes; output is not being coalesced", got.reads, got.chunks, perChunk)
	}
	if got.reads < 2*got.chunks {
		t.Errorf("%d reads produced %d chunks; fewer than two reads per chunk is not coalescing", got.reads, got.chunks)
	}
}

// The other half of the trade: a lone keystroke echo has nothing behind it and
// must leave immediately rather than wait out the coalescing window.
func TestTerminalEchoSkipsTheCoalescingWindow(t *testing.T) {
	ptmx, tty := ptyPair(t)
	r, err := newPTYReader(ptmx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tty.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, terminalCoalesceMax)
	start := time.Now()
	n, reads, err := r.read(buf)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || string(buf[:n]) != "x" {
		t.Fatalf("read %d bytes (%q), want the one echoed byte", n, buf[:n])
	}
	if reads != 1 {
		t.Fatalf("a lone byte cost %d reads; it is waiting when it must not", reads)
	}
	if elapsed >= terminalCoalesceWindow {
		t.Fatalf("a lone byte took %s to leave; the coalescing window is charging interactive latency", elapsed)
	}
}
