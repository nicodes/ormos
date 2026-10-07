//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
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
	defer s.mu.Unlock()
	for _, term := range s.terms {
		term.kill()
	}
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
	term.mu.Lock()
	alive := term.alive
	term.mu.Unlock()
	if !alive {
		uiError(w, http.StatusGone, "terminal exited")
		return
	}
	// The exact same-origin check above is stricter than Accept's host check.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(32 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	term.mu.Lock()
	if term.readers == nil {
		term.readers = map[chan []byte]bool{}
	}
	chunks := make(chan []byte, 64)
	term.readers[chunks] = true
	history := append([]byte(nil), term.buf...)
	term.mu.Unlock()
	defer func() {
		term.mu.Lock()
		if term.readers[chunks] {
			delete(term.readers, chunks)
			close(chunks)
		}
		term.mu.Unlock()
	}()
	if len(history) > 0 {
		if err := writeTerminalChunk(ctx, conn, history); err != nil {
			return
		}
	}
	go func() {
		defer cancel()
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
			Type string `json:"type"`
			Data string `json:"data"`
			Cols uint16 `json:"cols"`
			Rows uint16 `json:"rows"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			conn.Close(websocket.StatusPolicyViolation, "invalid terminal message")
			return
		}
		switch msg.Type {
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
