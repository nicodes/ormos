//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	uiMaxTerminalReaders  = 8
	uiMaxWorkspaceReaders = 64
)

// No account system: the private listener is the access boundary. Host and
// Origin checks prevent other browser pages from controlling local terminals.
func (s *uiServer) hostAllowed(authority string) bool {
	host := authority
	if h, _, err := net.SplitHostPort(authority); err == nil {
		host = h
	}
	if host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
		return true
	}
	for _, allowed := range s.hosts {
		if strings.EqualFold(strings.TrimSpace(allowed), authority) {
			return true
		}
	}
	return false
}

func sameUIOrigin(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && (origin.Scheme == "http" || origin.Scheme == "https") && origin.Host == r.Host && origin.Path == "" && origin.User == nil && origin.RawQuery == "" && origin.Fragment == ""
}

func (s *uiServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Ormos-Workspace", "1")
		if !s.hostAllowed(r.Host) {
			uiError(w, http.StatusMisdirectedRequest, "host not allowed")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameUIOrigin(r) {
			uiError(w, http.StatusForbidden, "origin not allowed")
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		apps := "http://" + net.JoinHostPort(host, "*") + " https://" + net.JoinHostPort(host, "*")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' "+apps+"; frame-src "+apps+"; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *uiServer) closeTerminals() {
	s.mu.Lock()
	s.closing = true
	terms := make([]*uiTerminal, 0, len(s.terms))
	for _, term := range s.terms {
		terms = append(terms, term)
	}
	s.mu.Unlock()
	// Signal every terminal first so shutdown takes one grace period, not one
	// per tab. Never wait under the server lock or race a still-opening PTY.
	for _, term := range terms {
		term.kill()
	}
	for _, term := range terms {
		if term.done != nil {
			<-term.done
		}
	}
	s.starts.Wait()
}

func (s *uiServer) terminalWS(w http.ResponseWriter, r *http.Request) {
	if !sameUIOrigin(r) {
		uiError(w, http.StatusForbidden, "origin not allowed")
		return
	}
	pol, err := uiLoadPolicy()
	if (err != nil && !os.IsNotExist(err)) || pol.TerminalsDisabled {
		uiError(w, http.StatusForbidden, "local policy disables terminals")
		return
	}
	s.mu.Lock()
	term := s.terms[r.PathValue("id")]
	s.mu.Unlock()
	if term == nil {
		uiError(w, http.StatusNotFound, "no such terminal")
		return
	}
	var since *uint64
	var flow *uiOutputFlow
	if values, present := r.URL.Query()["flow"]; present {
		if len(values) != 1 || values[0] != "1" {
			uiError(w, http.StatusBadRequest, "invalid terminal flow control")
			return
		}
		flow = newUIOutputFlow()
	}
	if values, present := r.URL.Query()["since"]; present {
		if len(values) != 1 {
			uiError(w, http.StatusBadRequest, "invalid terminal offset")
			return
		}
		offset, err := strconv.ParseUint(values[0], 10, 53)
		if err != nil {
			uiError(w, http.StatusBadRequest, "invalid terminal offset")
			return
		}
		since = &offset
	}
	// A retired terminal can still have a slow socket draining its tail. Count
	// those sockets globally until their handlers finish, even after the PTY
	// leaves the terminal table, so repeated open/kill cycles stay bounded.
	s.mu.Lock()
	if s.closing || s.connections >= uiMaxWorkspaceReaders {
		closing := s.closing
		s.mu.Unlock()
		if closing {
			uiError(w, http.StatusServiceUnavailable, "workspace is stopping")
		} else {
			uiError(w, http.StatusTooManyRequests, "workspace connection limit reached")
		}
		return
	}
	s.connections++
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.connections--; s.mu.Unlock() }()
	// Reserve before upgrading, including connections whose slow output queue
	// was evicted but whose socket is still closing. Failed upgrades release it.
	term.mu.Lock()
	if !term.alive {
		term.mu.Unlock()
		uiError(w, http.StatusGone, "terminal exited")
		return
	}
	if term.attachments >= uiMaxTerminalReaders {
		term.mu.Unlock()
		uiError(w, http.StatusTooManyRequests, "terminal connection limit reached")
		return
	}
	term.attachments++
	term.mu.Unlock()
	var chunks chan []byte
	var updates chan struct{}
	defer func() {
		term.mu.Lock()
		term.attachments--
		if term.readers[chunks] {
			delete(term.readers, chunks)
			close(chunks)
		}
		if term.updates[updates] {
			delete(term.updates, updates)
			close(updates)
		}
		term.mu.Unlock()
	}()
	// The exact same-origin check above is stricter than Accept's host check.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode:      websocket.CompressionNoContextTakeover,
		CompressionThreshold: 1024,
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(32 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	term.mu.Lock()
	if !term.alive {
		term.mu.Unlock()
		return
	}
	// Subscribe and snapshot atomically, only after a successful upgrade.
	var history []byte
	offset, reset := term.replayStart(since)
	if flow != nil {
		if term.updates == nil {
			term.updates = map[chan struct{}]bool{}
		}
		updates = make(chan struct{}, 1)
		term.updates[updates] = true
	} else {
		if term.readers == nil {
			term.readers = map[chan []byte]bool{}
		}
		chunks = make(chan []byte, 64)
		term.readers[chunks] = true
		history, _, _ = term.replay(since)
	}
	term.mu.Unlock()
	// Text metadata precedes binary output. An existing renderer can resume
	// without replaying bytes it already has; a fresh renderer gets history.
	window := 0
	if flow != nil {
		window = uiOutputWindow
	}
	metadata, _ := json.Marshal(struct {
		Type   string `json:"type"`
		Offset uint64 `json:"offset"`
		Reset  bool   `json:"reset"`
		Window int    `json:"window,omitempty"`
	}{"replay", offset, reset, window})
	metaCtx, metaCancel := context.WithTimeout(ctx, 10*time.Second)
	err = conn.Write(metaCtx, websocket.MessageText, metadata)
	metaCancel()
	if err != nil {
		return
	}
	go func() {
		defer cancel()
		if flow != nil {
			term.streamOutput(ctx, conn, flow, updates, offset)
			return
		}
		// Start reading input/resizes immediately while history is streamed.
		// Bounded messages let the browser parse/paint before the whole tail arrives.
		for len(history) > 0 {
			n := min(len(history), 64<<10)
			if err := writeTerminalChunk(ctx, conn, history[:n]); err != nil {
				return
			}
			history = history[n:]
		}
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-chunks:
				if !ok {
					return
				}
				if err := writeTerminalChunk(ctx, conn, chunk); err != nil {
					return
				}
			}
		}
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			Type  string `json:"type"`
			Data  string `json:"data"`
			Cols  uint16 `json:"cols"`
			Rows  uint16 `json:"rows"`
			Bytes int    `json:"bytes"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			conn.Close(websocket.StatusPolicyViolation, "invalid terminal message")
			return
		}
		switch msg.Type {
		case "ack":
			if flow == nil || !flow.ack(msg.Bytes) {
				conn.Close(websocket.StatusPolicyViolation, "invalid terminal acknowledgement")
				return
			}
		case "input":
			if term.input == nil || term.input([]byte(msg.Data)) != nil {
				return
			}
		case "resize":
			if msg.Cols < 2 || msg.Rows < 2 || msg.Cols > 1000 || msg.Rows > 1000 {
				conn.Close(websocket.StatusPolicyViolation, "invalid terminal size")
				return
			}
			if term.resize == nil || term.resize(msg.Cols, msg.Rows) != nil {
				return
			}
		default:
			conn.Close(websocket.StatusPolicyViolation, "unknown terminal message")
			return
		}
	}
}

func writeTerminalChunk(ctx context.Context, conn *websocket.Conn, chunk []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageBinary, chunk)
}

// replay is called under term.mu together with subscribing to live output,
// so bytes cannot be skipped or delivered twice at the replay/live boundary.
func (t *uiTerminal) replay(since *uint64) ([]byte, uint64, bool) {
	start, reset := t.replayStart(since)
	return t.copyOutput(start, int(t.end-start)), start, reset
}

func (t *uiTerminal) replayStart(since *uint64) (uint64, bool) {
	start := t.end - uint64(t.history.size)
	reset := since == nil || *since < start || *since > t.end
	if !reset {
		start = *since
	}
	return start, reset
}

// Called under term.mu. A cursor outside the ring must reconnect/reset; never
// silently combine a missing ANSI/UTF-8 prefix with the current parser state.
func (t *uiTerminal) replayChunk(cursor uint64, limit int) ([]byte, bool) {
	if cursor < t.end-uint64(t.history.size) || cursor > t.end {
		return nil, false
	}
	// Share the most recent immutable delivery between readers at the same
	// position, including partial credits. Lagging readers still use the ring.
	if cursor >= t.chunkStart && cursor-t.chunkStart < uint64(len(t.chunk)) {
		i := int(cursor - t.chunkStart)
		return t.chunk[i : i+min(limit, len(t.chunk)-i)], true
	}
	count := min(int(t.end-cursor), limit, uiOutputChunk)
	if count == 0 {
		return nil, true
	}
	t.chunkStart = cursor
	t.chunk = t.copyOutput(cursor, count)
	return t.chunk, true
}

func (t *uiTerminal) copyOutput(start uint64, count int) []byte {
	if count == 0 {
		return nil
	}
	// Copy only the requested suffix, without copying the whole retained ring.
	i := (t.history.start + t.history.size - int(t.end-start)) % len(t.history.buf)
	out := make([]byte, count)
	n := copy(out, t.history.buf[i:min(i+count, len(t.history.buf))])
	copy(out[n:], t.history.buf[:count-n])
	return out
}
