//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestUIRejectsForeignBrowserRequests(t *testing.T) {
	fix := newUIFixture(t, nil)
	ts := fix.start(t)
	for _, origin := range []string{"", "https://evil.example", ts.URL + "/path"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/action", strings.NewReader(`{"action":"open"}`))
		req.Header.Set("Origin", origin)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %q: %d", origin, res.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.Host = "evil.example"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("foreign host: %d", res.StatusCode)
	}
	if fix.spawnLog.Len() != 0 {
		t.Fatal("foreign page spawned a shell")
	}
}

func TestUIInteractiveTerminalAndResize(t *testing.T) {
	fix := newUIFixture(t, nil)
	term, err := spawnUITerminal("/bin/sh", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer term.kill()
	fix.srv.terms[term.id] = term
	ts := fix.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/terminal/"+term.id+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{ts.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	for _, msg := range []any{
		map[string]any{"type": "resize", "cols": 91, "rows": 27},
		map[string]any{"type": "input", "data": "printf 'ORMOS_%s\\n' LIVE; stty size\n"},
	} {
		data, _ := json.Marshal(msg)
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
	}
	var output strings.Builder
	for !strings.Contains(output.String(), "ORMOS_LIVE") || !strings.Contains(output.String(), "27 91") {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("output=%q: %v", output.String(), err)
		}
		output.Write(data)
	}
	conn.CloseNow()
	// A connection ending leaves the PTY alive for a phone reconnect.
	term.mu.Lock()
	alive := term.alive
	term.mu.Unlock()
	if !alive {
		t.Fatal("disconnect killed terminal")
	}
}

func TestUIWebSocketRejectsForeignOrigin(t *testing.T) {
	fix := newUIFixture(t, nil)
	ts := fix.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/terminal/t_unknown/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}}})
	if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign websocket: %v %v", res, err)
	}
}

func TestUIDirectAppsCannotControlTerminals(t *testing.T) {
	fix := newUIFixture(t, nil)
	fix.srv.hosts = []string{"devbox:8481"}
	for _, origin := range []string{"http://devbox:3000", "http://devbox:39123"} {
		req := httptest.NewRequest(http.MethodPost, "http://devbox:8481/api/action", strings.NewReader(`{"action":"open"}`))
		req.Header.Set("Origin", origin)
		res := httptest.NewRecorder()
		fix.srv.routes().ServeHTTP(res, req)
		if res.Code != http.StatusForbidden {
			t.Fatalf("app origin %q could control terminals: %d", origin, res.Code)
		}
	}
	if fix.spawnLog.Len() != 0 {
		t.Fatal("preview app spawned a shell")
	}
}

func TestUIDirectPreviewContentPolicy(t *testing.T) {
	fix := newUIFixture(t, nil)
	fix.srv.hosts = []string{"devbox:8481"}
	res := httptest.NewRecorder()
	fix.srv.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "http://devbox:8481/", nil))
	csp := res.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"script-src 'self'", "frame-src http://devbox:* https://devbox:*", "connect-src 'self' http://devbox:* https://devbox:*", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Fatalf("missing %q in CSP %q", directive, csp)
		}
	}
	if strings.Contains(csp, "http://*:*") || strings.Contains(csp, "https://*:*") {
		t.Fatal("content policy permits unrelated preview hosts")
	}
}

func TestUIExitedTerminalDoesNotConsumeCapacity(t *testing.T) {
	fix := newUIFixture(t, nil)
	ts := fix.start(t)
	oldMax := uiMaxTerminals
	uiMaxTerminals = 1
	t.Cleanup(func() { uiMaxTerminals = oldMax })
	if code, _ := postJSON(t, ts.URL+"/api/action", `{"action":"open","shell":"/bin/sh"}`); code != 200 {
		t.Fatal(code)
	}
	if code, _ := postJSON(t, ts.URL+"/api/action", `{"action":"kill","id":"t_stub"}`); code != 200 {
		t.Fatal(code)
	}
	if code, _ := postJSON(t, ts.URL+"/api/action", `{"action":"open","shell":"/bin/sh"}`); code != 200 {
		t.Fatalf("replacement: %d", code)
	}
}

func TestUINewTerminalUsesHomeByDefault(t *testing.T) {
	fix := newUIFixture(t, nil)
	server := fix.start(t)
	if code, _ := postJSON(t, server.URL+"/api/action", `{"action":"open","shell":"/bin/sh"}`); code != 200 {
		t.Fatal(code)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fix.spawnLog.String(), "/bin/sh\x00"+home+"\n") {
		t.Fatalf("default terminal cwd: %q", fix.spawnLog.String())
	}
}
