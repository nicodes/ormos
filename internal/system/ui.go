//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/nicodes/ormos/internal/ui"
)

// `ormos ui` -- the local web app, served by this machine for whoever can
// reach it. The CLI is the whole product and this is its screen: a SolidJS +
// Tailwind app built to a static export, embedded, and served here. It does
// not touch the relay: the agent dials out and this process only listens, so
// a dead or revoked pairing changes nothing below.
//
// ZERO AUTH, BY DESIGN: the listener is the boundary. It binds loopback by
// default and may be widened to the tailnet interface with an explicit
// --bind -- Tailscale is the identity. It is NEVER wildcard by accident:
// 0.0.0.0 arrives only when somebody typed it.
//
// The verbs are the lifecycle pair, enforced HERE, server-side: a request for
// anything else is refused whatever the page sent. `open` spawns a PTY on this
// machine with no relay involved, bounded, and `kill` ends one this UI opened.
// Input (typing) is deliberately not here in v1 -- read-mostly, plus this pair.
var uiActions = map[string]bool{"open": true, "kill": true}

// Fixture seams: tests point these at stubs. Production defaults read this
// machine only.
var (
	uiLoadPolicy      = loadPolicy
	uiListeningPorts  = listeningPorts
	uiSpawnTerminal   = spawnUITerminal
	uiMaxTerminals    = 8
	uiTerminalBufMax  = 64 << 10
	uiTerminalKillGap = 2 * time.Second
)

const (
	uiDefaultBind = "127.0.0.1"
	uiDefaultPort = 8481
	uiAuditLines  = 50
	uiAuditBytes  = 256 << 10
)

var uiShellAllowlist = []string{"/bin/sh", "/bin/bash", "/bin/zsh", "/usr/bin/zsh", "/usr/bin/bash"}

type uiTerminal struct {
	id      string
	shell   string
	cwd     string
	started time.Time
	alive   bool
	mu      sync.Mutex
	buf     []byte
	kill    func()
}

func (t *uiTerminal) output() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func (t *uiTerminal) append(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > uiTerminalBufMax {
		t.buf = t.buf[len(t.buf)-uiTerminalBufMax:]
	}
}

type uiServer struct {
	version  string
	static   fs.FS
	hostname string
	mu       sync.Mutex
	terms    map[string]*uiTerminal
}

func RunUI(args []string, version string) error {
	fsflags := flag.NewFlagSet("ui", flag.ContinueOnError)
	bind := fsflags.String("bind", uiDefaultBind, "address to listen on (default loopback; set the tailnet interface address to reach it from your tailnet)")
	port := fsflags.Int("port", uiDefaultPort, "port to listen on")
	if err := fsflags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if fsflags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q -- every input is a flag", fsflags.Arg(0))
	}
	if err := validateUIBind(*bind); err != nil {
		return err
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("--port must be 1-65535, got %d", *port)
	}
	static, err := ui.Dist()
	if err != nil {
		return err
	}
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(*bind, strconv.Itoa(*port)))
	if err != nil {
		return err
	}
	if ip := net.ParseIP(*bind); ip != nil && ip.IsUnspecified() {
		fmt.Fprintf(os.Stderr, "ormos ui: --bind %s listens on every interface. There is no auth: anyone who can reach it can read this machine and open terminals on it.\n", *bind)
	} else if *bind != "127.0.0.1" && *bind != "localhost" && *bind != "::1" {
		fmt.Fprintf(os.Stderr, "ormos ui: --bind %s: no auth beyond reachability -- that address is the boundary.\n", *bind)
	}
	fmt.Printf("ormos ui on http://%s\n", ln.Addr())
	srv := &http.Server{
		Handler:           (&uiServer{version: version, static: static, hostname: host, terms: map[string]*uiTerminal{}}).routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.Serve(ln)
}

// validateUIBind keeps the listen address an address. The wildcard is not
// refused -- refusing an explicit flag is paternalism -- but the default is
// loopback and ui_test.go pins that it stays loopback.
func validateUIBind(bind string) error {
	if bind == "localhost" {
		return nil
	}
	if net.ParseIP(bind) == nil {
		return fmt.Errorf("--bind must be an IP address or localhost, got %q", bind)
	}
	return nil
}

func (s *uiServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/system", s.apiSystem)
	mux.HandleFunc("GET /api/ports", s.apiPorts)
	mux.HandleFunc("GET /api/terminals", s.apiTerminals)
	mux.HandleFunc("GET /api/audit", s.apiAudit)
	mux.HandleFunc("GET /api/terminal/{id}/output", s.apiTerminalOutput)
	mux.HandleFunc("/api/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			uiError(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		s.apiAction(w, r)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		uiError(w, http.StatusNotFound, "no such read")
	})
	mux.HandleFunc("/", s.spa)
	return mux
}

func uiJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

func uiError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (s *uiServer) apiSystem(w http.ResponseWriter, r *http.Request) {
	dir, err := ormosDir()
	if err != nil {
		uiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pol, polErr := uiLoadPolicy()
	cfg, _, cfgErr := loadConfigFileChecked()
	shell := loadSystemConfig().Shell
	roots := []string{}
	if polErr == nil {
		roots = pol.AllowedRoots
	}
	s.mu.Lock()
	terminals := len(s.terms)
	s.mu.Unlock()
	uiJSON(w, map[string]any{
		"v":            1,
		"version":      s.version,
		"hostname":     s.hostname,
		"os":           runtime.GOOS,
		"arch":         runtime.GOARCH,
		"shell":        shell,
		"configDir":    dir,
		"hasConfig":    cfgErr == nil && cfg.SystemID != "",
		"hasPolicy":    polErr == nil,
		"allowedRoots": roots,
		"terminals":    terminals,
	})
}

func (s *uiServer) apiPorts(w http.ResponseWriter, r *http.Request) {
	ports, err := uiListeningPorts()
	if err != nil {
		uiError(w, http.StatusServiceUnavailable, "could not read listening ports")
		return
	}
	pol, polErr := uiLoadPolicy()
	rows := make([]map[string]any, 0, len(ports))
	for _, port := range ports {
		allowed := false
		if polErr == nil {
			allowed, _ = pol.proxyAllowed(port)
		}
		rows = append(rows, map[string]any{"port": port, "allowed": allowed})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["port"].(int) < rows[j]["port"].(int) })
	uiJSON(w, map[string]any{"v": 1, "policyError": polErr != nil, "ports": rows})
}

func (s *uiServer) apiTerminals(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	rows := make([]map[string]any, 0, len(s.terms))
	for _, t := range s.terms {
		t.mu.Lock()
		rows = append(rows, map[string]any{
			"id": t.id, "shell": t.shell, "cwd": t.cwd,
			"started": t.started.Format(time.RFC3339), "alive": t.alive,
		})
		t.mu.Unlock()
	}
	s.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i]["started"].(string) < rows[j]["started"].(string) })
	uiJSON(w, map[string]any{"v": 1, "terminals": rows})
}

func (s *uiServer) apiTerminalOutput(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	t, ok := s.terms[r.PathValue("id")]
	s.mu.Unlock()
	if !ok {
		uiError(w, http.StatusNotFound, "no such terminal")
		return
	}
	uiJSON(w, map[string]any{"v": 1, "output": t.output()})
}

func (s *uiServer) apiAudit(w http.ResponseWriter, r *http.Request) {
	dir, err := ormosDir()
	if err != nil {
		uiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	path := filepath.Join(dir, auditFileName)
	info, err := os.Stat(path)
	if err != nil {
		uiJSON(w, map[string]any{"v": 1, "entries": []any{}})
		return
	}
	offset := int64(0)
	if info.Size() > uiAuditBytes {
		offset = info.Size() - uiAuditBytes
	}
	f, err := os.Open(path)
	if err != nil {
		uiError(w, http.StatusServiceUnavailable, "audit log unreadable")
		return
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		uiError(w, http.StatusServiceUnavailable, "audit log unreadable")
		return
	}
	raw := make([]byte, uiAuditBytes)
	n, _ := f.Read(raw)
	lines := strings.Split(string(raw[:n]), "\n")
	if offset > 0 && len(lines) > 0 {
		lines = lines[1:] // discard the partial first line
	}
	entries := make([]map[string]any, 0, uiAuditLines)
	for i := len(lines) - 1; i >= 0 && len(entries) < uiAuditLines; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	uiJSON(w, map[string]any{"v": 1, "entries": entries})
}

type uiActionBody struct {
	Action string `json:"action"`
	Shell  string `json:"shell,omitempty"`
	Cwd    string `json:"cwd,omitempty"`
	ID     string `json:"id,omitempty"`
}

func (s *uiServer) apiAction(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body uiActionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		uiError(w, http.StatusBadRequest, "body must be one JSON object")
		return
	}
	if !uiActions[body.Action] {
		uiError(w, http.StatusForbidden, "action not allowed")
		return
	}
	switch body.Action {
	case "open":
		s.apiActionOpen(w, body)
	case "kill":
		s.apiActionKill(w, body)
	}
}

func (s *uiServer) apiActionOpen(w http.ResponseWriter, body uiActionBody) {
	pol, polErr := uiLoadPolicy()
	if polErr == nil && pol.TerminalsDisabled {
		uiError(w, http.StatusForbidden, "local policy disables terminals")
		return
	}
	shell := body.Shell
	if shell == "" {
		shell = loadSystemConfig().Shell
	}
	if err := validateUIShell(shell); err != nil {
		uiError(w, http.StatusBadRequest, err.Error())
		return
	}
	cwd := body.Cwd
	if cwd == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cwd = home
		} else {
			cwd = "/"
		}
	}
	resolved, err := validateUICwd(cwd, polErr == nil, pol.AllowedRoots)
	if err != nil {
		uiError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	if len(s.terms) >= uiMaxTerminals {
		s.mu.Unlock()
		uiError(w, http.StatusTooManyRequests, "terminal limit reached")
		return
	}
	s.mu.Unlock()
	term, err := uiSpawnTerminal(shell, resolved)
	if err != nil {
		uiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.Lock()
	s.terms[term.id] = term
	s.mu.Unlock()
	uiJSON(w, map[string]any{"v": 1, "id": term.id})
}

func (s *uiServer) apiActionKill(w http.ResponseWriter, body uiActionBody) {
	if body.ID == "" {
		uiError(w, http.StatusBadRequest, "kill needs an id")
		return
	}
	s.mu.Lock()
	t, ok := s.terms[body.ID]
	s.mu.Unlock()
	if !ok {
		uiError(w, http.StatusNotFound, "no such terminal")
		return
	}
	t.kill()
	uiJSON(w, map[string]any{"v": 1, "id": t.id, "alive": false})
}

// validateUIShell refuses anything that is not an absolute allowlisted shell
// that exists, is a regular file, and is executable. Nothing the page sends is
// ever passed through a shell -- the path is handed to exec.Command directly.
func validateUIShell(shell string) error {
	allowed := false
	for _, candidate := range uiShellAllowlist {
		if shell == candidate {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("shell %q is not on the allowlist", shell)
	}
	info, err := os.Stat(shell)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("shell %q is not an executable regular file", shell)
	}
	return nil
}

// validateUICwd resolves the directory for real -- Clean, then EvalSymlinks,
// then existence -- and, when the machine's policy names allowed roots,
// confines the terminal to one of them. A traversal that lands outside every
// root is refused, never cleaned into place.
func validateUICwd(cwd string, hasPolicy bool, roots []string) (string, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Clean(cwd))
	if err != nil {
		return "", fmt.Errorf("cwd %q does not resolve: %v", cwd, err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("cwd %q is not a directory", cwd)
	}
	if !hasPolicy || len(roots) == 0 {
		return resolved, nil
	}
	for _, root := range roots {
		expanded, err := expandUIRoot(root)
		if err != nil {
			continue
		}
		if resolved == expanded || strings.HasPrefix(resolved, expanded+string(filepath.Separator)) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("cwd %q is outside the policy's allowed roots", cwd)
}

func expandUIRoot(root string) (string, error) {
	if strings.HasPrefix(root, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, strings.TrimPrefix(root, "~"))
	}
	return filepath.EvalSymlinks(filepath.Clean(root))
}

// spawnUITerminal opens one PTY on this machine. There is no relay in this
// path: no admission, no fence, no handshake -- the local UI is the only
// client, and the session dies when it does.
func spawnUITerminal(shell, cwd string) (*uiTerminal, error) {
	id := newUIID()
	cmd := exec.Command(shell)
	cmd.Dir = cwd
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	t := &uiTerminal{
		id:      id,
		shell:   shell,
		cwd:     cwd,
		started: time.Now(),
		alive:   true,
	}
	t.kill = func() {
		t.mu.Lock()
		t.alive = false
		t.mu.Unlock()
		_ = cmd.Process.Signal(syscall.SIGHUP)
		time.AfterFunc(uiTerminalKillGap, func() {
			_ = cmd.Process.Kill()
			_ = ptmx.Close()
		})
	}
	go func() {
		chunk := make([]byte, 8192)
		for {
			n, err := ptmx.Read(chunk)
			if n > 0 {
				t.append(chunk[:n])
			}
			if err != nil {
				break
			}
		}
	}()
	go func() {
		_ = cmd.Wait()
		t.mu.Lock()
		t.alive = false
		t.mu.Unlock()
		_ = ptmx.Close()
	}()
	return t, nil
}

func newUIID() string {
	b := make([]byte, 8)
	f, err := os.Open("/dev/urandom")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if _, err := f.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("t_%x", b)
}

func (s *uiServer) spa(w http.ResponseWriter, r *http.Request) {
	name := filepath.ToSlash(filepath.Clean("/" + r.URL.Path))
	if name == "/" {
		name = "/index.html"
	}
	serveUIFile(w, s.static, strings.TrimPrefix(name, "/"))
}

func serveUIFile(w http.ResponseWriter, static fs.FS, name string) {
	if strings.Contains(name, "..") {
		http.NotFound(w, nil)
		return
	}
	data, err := fs.ReadFile(static, name)
	if err != nil {
		// SPA fallback: client-side routes resolve to index.html.
		data, err = fs.ReadFile(static, "index.html")
		if err != nil {
			http.NotFound(w, nil)
			return
		}
		name = "index.html"
	}
	switch {
	case strings.HasSuffix(name, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	_, _ = w.Write(data)
}
