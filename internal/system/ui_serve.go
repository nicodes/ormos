//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxPreviewRoutes = 32

// Foreground Serve sessions are owned by their CLI watcher. Stopping that
// watcher removes only its session; no reset, off or background edits are used.
type previewServe struct {
	ctx       context.Context
	cancel    context.CancelFunc
	gate      chan struct{}
	sessions  map[int]*serveSession
	status    func(context.Context) (*serveConfig, error)
	start     func(context.Context, string, int) (*serveSession, error)
	listening func(context.Context, int) error
}

type serveSession struct {
	scheme string
	ready  chan struct{}
	done   chan struct{}
	stop   context.CancelFunc
}

type serveConfig struct {
	TCP map[string]json.RawMessage
	Web map[string]struct {
		Handlers map[string]struct{ Proxy, Path, Text string }
	}
	AllowFunnel map[string]bool
	Foreground  map[string]*serveConfig
}

type previewSetupError struct {
	status  int
	message string
}

func (e *previewSetupError) Error() string { return e.message }

func newPreviewServe(parent context.Context) *previewServe {
	ctx, cancel := context.WithCancel(parent)
	return &previewServe{ctx: ctx, cancel: cancel, gate: make(chan struct{}, 1), sessions: map[int]*serveSession{}, status: readServeStatus, start: startServeSession, listening: loopbackAppListening}
}

func loopbackAppListening(ctx context.Context, port int) error {
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err == nil {
		conn.Close()
	}
	return err
}

// Inspect all host-level sessions; service VIPs have their own port namespace.
func (c *serveConfig) route(port int, scheme string) (exists, compatible bool) {
	if c == nil {
		return false, false
	}
	number := strconv.Itoa(port)
	raw, hasTCP := c.TCP[number]
	exists = hasTCP
	var listener struct {
		HTTP, HTTPS              bool
		TCPForward, TerminateTLS string
	}
	matching := hasTCP && json.Unmarshal(raw, &listener) == nil && listener.TCPForward == "" && listener.TerminateTLS == "" &&
		((scheme == "http" && listener.HTTP && !listener.HTTPS) || (scheme == "https" && listener.HTTPS && !listener.HTTP))
	roots := 0
	for authority, web := range c.Web {
		_, p, err := net.SplitHostPort(authority)
		if err != nil || p != number {
			continue
		}
		exists = true
		handler, ok := web.Handlers["/"]
		if !ok || len(web.Handlers) != 1 || handler.Proxy != "http://127.0.0.1:"+number || handler.Path != "" || handler.Text != "" {
			matching = false
		}
		roots++
	}
	matching = matching && roots == 1
	for authority, public := range c.AllowFunnel {
		_, p, err := net.SplitHostPort(authority)
		if err == nil && p == number && public {
			exists = true
			matching = false
		}
	}
	compatible = matching
	for _, foreground := range c.Foreground {
		used, match := foreground.route(port, scheme)
		if used {
			compatible = match && !exists
			exists = true
		}
	}
	return
}

func (s *previewServe) ensure(ctx context.Context, port int, scheme string) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.listening(ctx, port); err != nil {
		return &previewSetupError{http.StatusConflict, "Start your app in the terminal, then refresh."}
	}
	for p, session := range s.sessions {
		select {
		case <-session.done:
			delete(s.sessions, p)
		default:
		}
	}
	if session := s.sessions[port]; session != nil && session.scheme == scheme {
		return nil
	}
	cfg, err := s.status(ctx)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return &previewSetupError{http.StatusServiceUnavailable, "Install and connect Tailscale on this machine, then refresh."}
		}
		return &previewSetupError{http.StatusServiceUnavailable, "Allow Ormos's user to manage Tailscale Serve, then refresh."}
	}
	if exists, compatible := cfg.route(port, scheme); exists {
		if compatible {
			return nil
		}
		return &previewSetupError{http.StatusConflict, "This port is used by another Tailscale service. Choose another port."}
	}
	if len(s.sessions) >= maxPreviewRoutes {
		return &previewSetupError{http.StatusTooManyRequests, "Preview port limit reached. Restart Ormos to release its preview ports."}
	}
	session, err := s.start(s.ctx, scheme, port)
	if err != nil {
		return &previewSetupError{http.StatusServiceUnavailable, "Tailscale could not open this port. Check its setup, then refresh."}
	}
	select {
	case <-session.ready:
		select {
		case <-session.done:
			session.stop()
			return &previewSetupError{http.StatusServiceUnavailable, "Tailscale could not open this port. Check its setup, then refresh."}
		default:
		}
		s.sessions[port] = session
		return nil
	case <-session.done:
		session.stop()
		return &previewSetupError{http.StatusServiceUnavailable, "Tailscale could not open this port. Check its setup, then refresh."}
	case <-ctx.Done():
		session.stop()
		return &previewSetupError{http.StatusServiceUnavailable, "Tailscale setup did not finish. Check its connection and permissions, then refresh."}
	}
}

func (s *previewServe) close() {
	s.cancel()
	s.gate <- struct{}{}
	defer func() { <-s.gate }()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for _, session := range s.sessions {
		select {
		case <-session.done:
		case <-timeout.C:
			return
		}
	}
}

// Bound status output and discard CLI diagnostics: raw output can contain
// account/setup details and is never returned to the browser.
func readServeStatus(ctx context.Context) (*serveConfig, error) {
	cmd := exec.CommandContext(ctx, "tailscale", "serve", "status", "--json")
	output := &boundedServeOutput{}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	var cfg serveConfig
	if err := json.Unmarshal(output.Bytes(), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

type boundedServeOutput struct{ bytes.Buffer }

func (b *boundedServeOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1<<20 {
		return 0, errors.New("serve status too large")
	}
	return b.Buffer.Write(p)
}

type serveReadyOutput struct {
	tail  string
	once  sync.Once
	ready chan struct{}
}

func (b *serveReadyOutput) Write(p []byte) (int, error) {
	b.tail += string(p)
	if strings.Contains(b.tail, "Available within your tailnet:") {
		b.once.Do(func() { close(b.ready) })
	}
	if len(b.tail) > 256 {
		b.tail = b.tail[len(b.tail)-256:]
	}
	return len(p), nil
}

func startServeSession(parent context.Context, scheme string, port int) (*serveSession, error) {
	ctx, cancel := context.WithCancel(parent)
	session := &serveSession{scheme: scheme, ready: make(chan struct{}), done: make(chan struct{}), stop: cancel}
	// No shell, background mode, --yes, Funnel or destructive config commands.
	cmd := exec.CommandContext(ctx, "tailscale", "serve", "--"+scheme+"="+strconv.Itoa(port), "http://127.0.0.1:"+strconv.Itoa(port))
	cmd.Stdout = &serveReadyOutput{ready: session.ready}
	cmd.Stderr = io.Discard
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	go func() { _ = cmd.Wait(); close(session.done); cancel() }()
	return session, nil
}

func (s *uiServer) apiPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		uiError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256)
	var body struct {
		Port   int    `json:"port"`
		Scheme string `json:"scheme"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(&struct{}{}) != io.EOF || body.Port < 1 || body.Port > 65535 || (body.Scheme != "http" && body.Scheme != "https") {
		uiError(w, http.StatusBadRequest, "Enter a valid app port.")
		return
	}
	_, control, _ := net.SplitHostPort(r.Host)
	if body.Port == s.controlPort || strconv.Itoa(body.Port) == control {
		uiError(w, http.StatusForbidden, "Choose an app port, not the Ormos port.")
		return
	}
	pol, err := uiLoadPolicy()
	if err != nil && !os.IsNotExist(err) {
		uiError(w, http.StatusForbidden, "Local policy does not allow opening this port.")
		return
	}
	if allowed, _ := pol.proxyAllowed(body.Port); !allowed {
		uiError(w, http.StatusForbidden, "Local policy does not allow opening this port.")
		return
	}
	if s.previewServe == nil {
		uiError(w, http.StatusServiceUnavailable, "Tailscale setup is unavailable.")
		return
	}
	if err := s.previewServe.ensure(r.Context(), body.Port, body.Scheme); err != nil {
		var setup *previewSetupError
		if errors.As(err, &setup) {
			uiError(w, setup.status, setup.message)
		} else {
			uiError(w, http.StatusServiceUnavailable, "Could not open this port. Refresh to try again.")
		}
		return
	}
	uiJSON(w, map[string]any{"port": body.Port})
}
