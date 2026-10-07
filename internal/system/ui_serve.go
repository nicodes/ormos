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
	"net/url"
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
	sessions  map[string]*previewRoute
	status    func(context.Context) (*serveConfig, error)
	start     func(context.Context, string, int, int) (*serveSession, error)
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
	return &previewServe{ctx: ctx, cancel: cancel, gate: make(chan struct{}, 1), sessions: map[string]*previewRoute{}, status: readServeStatus, start: startServeSession, listening: loopbackAppListening}
}

func loopbackAppListening(ctx context.Context, port int) error {
	conn, err := dialLoopback(ctx, port)
	if err != nil {
		return err
	}
	address := conn.RemoteAddr().String()
	conn.Close()
	// Updated Ormos instances identify their control listener. Never translate
	// a preview's Origin into another workspace's trusted localhost origin.
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "http://"+address+"/", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err == nil {
		res.Body.Close()
		if res.Header.Get("X-Ormos-Workspace") == "1" {
			return &previewSetupError{http.StatusForbidden, "Choose an app port, not an Ormos workspace port."}
		}
	}
	return nil
}

// Host-level listeners share a port namespace; service VIPs do not.
func (c *serveConfig) usesPort(port int) bool {
	if c == nil {
		return false
	}
	if _, exists := c.TCP[strconv.Itoa(port)]; exists {
		return true
	}
	for authority := range c.Web {
		if _, p, err := net.SplitHostPort(authority); err == nil && p == strconv.Itoa(port) {
			return true
		}
	}
	for authority, public := range c.AllowFunnel {
		if _, p, err := net.SplitHostPort(authority); public && err == nil && p == strconv.Itoa(port) {
			return true
		}
	}
	for _, foreground := range c.Foreground {
		if foreground.usesPort(port) {
			return true
		}
	}
	return false
}

// open serializes route creation and returns the browser-facing port. Every
// preview has its own listener/origin; no terminal routes live on that listener.
func (s *previewServe) open(ctx context.Context, port int, scheme, host string, controlPort int) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Prevent routing back into Ormos or one of its forwarding listeners.
	for key, route := range s.sessions {
		if route.session != nil {
			select {
			case <-route.session.done:
				route.close()
				delete(s.sessions, key)
			default:
			}
		}
	}
	for _, route := range s.sessions {
		if port == route.localPort {
			return 0, &previewSetupError{http.StatusForbidden, "Choose an app port, not an Ormos preview port."}
		}
	}
	if err := s.listening(ctx, port); err != nil {
		var setup *previewSetupError
		if errors.As(err, &setup) {
			return 0, setup
		}
		return 0, &previewSetupError{http.StatusConflict, "Start your app in the terminal, then refresh."}
	}
	key := scheme + ":" + net.JoinHostPort(host, strconv.Itoa(port))
	if route := s.sessions[key]; route != nil {
		return route.publicPort, nil
	}
	if len(s.sessions) >= maxPreviewRoutes {
		return 0, &previewSetupError{http.StatusTooManyRequests, "Preview port limit reached. Restart Ormos to release its preview ports."}
	}
	local := host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
	var cfg *serveConfig
	if !local {
		var err error
		cfg, err = s.status(ctx)
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				return 0, &previewSetupError{http.StatusServiceUnavailable, "Install and connect Tailscale on this machine, then refresh."}
			}
			return 0, &previewSetupError{http.StatusServiceUnavailable, "Allow Ormos's user to manage Tailscale Serve, then refresh."}
		}
	}
	bind := "127.0.0.1:0"
	if ip := net.ParseIP(host); local && ip != nil && ip.To4() == nil {
		bind = "[::1]:0"
	}
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return 0, &previewSetupError{http.StatusServiceUnavailable, "Could not open a preview listener. Refresh to try again."}
	}
	route := &previewRoute{localPort: ln.Addr().(*net.TCPAddr).Port}
	route.publicPort = route.localPort
	if !local {
		// Never reuse or replace existing routes: even an existing direct route
		// would bypass the localhost Host rewrite required by development servers.
		available := func(candidate int) bool {
			if candidate == controlPort {
				return false
			}
			if cfg.usesPort(candidate) {
				return false
			}
			for _, other := range s.sessions {
				if candidate == other.publicPort || candidate == other.localPort {
					return false
				}
			}
			return true
		}
		// Keep the app port free of tailnet listeners. Some dev servers probe
		// wildcard addresses when restarting, even when bound to localhost.
		route.publicPort = route.localPort
		if !available(route.publicPort) {
			route.publicPort = 0
			for candidate := 20000; candidate <= 65535; candidate++ {
				if available(candidate) {
					route.publicPort = candidate
					break
				}
			}
			if route.publicPort == 0 {
				ln.Close()
				return 0, &previewSetupError{http.StatusServiceUnavailable, "No preview port is available."}
			}
		}
	}
	routeCtx, routeCancel := context.WithCancel(s.ctx)
	route.cancel = routeCancel
	authority := net.JoinHostPort(host, strconv.Itoa(route.publicPort))
	route.server = &http.Server{Handler: s.proxyHandler(routeCtx, port, scheme, authority), ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return routeCtx }}
	go func() { _ = route.server.Serve(ln) }()
	if !local {
		session, err := s.start(s.ctx, scheme, route.publicPort, route.localPort)
		if err != nil {
			route.close()
			return 0, &previewSetupError{http.StatusServiceUnavailable, "Tailscale could not open this preview. Check its setup, then refresh."}
		}
		route.session = session
		select {
		case <-session.ready:
			select {
			case <-session.done:
				route.close()
				return 0, &previewSetupError{http.StatusServiceUnavailable, "Tailscale could not open this preview. Refresh to try again."}
			default:
			}
		case <-session.done:
			route.close()
			return 0, &previewSetupError{http.StatusServiceUnavailable, "Tailscale could not open this preview. Refresh to try again."}
		case <-ctx.Done():
			route.close()
			return 0, &previewSetupError{http.StatusServiceUnavailable, "Tailscale setup did not finish. Check its connection and permissions, then refresh."}
		}
	}
	s.sessions[key] = route
	return route.publicPort, nil
}

func (s *previewServe) close() {
	s.cancel()
	s.gate <- struct{}{}
	defer func() { <-s.gate }()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for _, route := range s.sessions {
		route.close()
	}
	for _, route := range s.sessions {
		if route.session == nil {
			continue
		}
		select {
		case <-route.session.done:
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

func startServeSession(parent context.Context, scheme string, port, localPort int) (*serveSession, error) {
	ctx, cancel := context.WithCancel(parent)
	session := &serveSession{scheme: scheme, ready: make(chan struct{}), done: make(chan struct{}), stop: cancel}
	// No shell, background mode, --yes, Funnel or destructive config commands.
	cmd := exec.CommandContext(ctx, "tailscale", "serve", "--"+scheme+"="+strconv.Itoa(port), "http://127.0.0.1:"+strconv.Itoa(localPort))
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
	origin, _ := url.Parse(r.Header.Get("Origin"))
	if origin == nil || origin.Scheme != body.Scheme {
		uiError(w, http.StatusBadRequest, "Use the workspace connection scheme.")
		return
	}
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
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	exposed, err := s.previewServe.open(r.Context(), body.Port, body.Scheme, host, s.controlPort)
	if err != nil {
		var setup *previewSetupError
		if errors.As(err, &setup) {
			uiError(w, setup.status, setup.message)
		} else {
			uiError(w, http.StatusServiceUnavailable, "Could not open this port. Refresh to try again.")
		}
		return
	}
	uiJSON(w, map[string]any{"port": exposed})
}
