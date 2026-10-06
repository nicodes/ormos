//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
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

func TestUIPreviewRootAssetsAndPolicy(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmtBody := r.URL.Path + "?" + r.URL.RawQuery
		io.WriteString(w, fmtBody)
	}))
	defer app.Close()
	u, _ := url.Parse(app.URL)
	port, _ := strconv.Atoi(u.Port())
	fix := newUIFixture(t, nil)
	uiLoadPolicy = func() (policy, error) { return policy{AllowedPorts: []int{port}}, nil }
	preview := httptest.NewServer(fix.srv.previewRoutes())
	defer preview.Close()
	req, _ := http.NewRequest(http.MethodGet, preview.URL+"/assets/app.js?v=2", nil)
	req.AddCookie(&http.Cookie{Name: "ormos_preview_port", Value: strconv.Itoa(port)})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(data) != "/assets/app.js?v=2" {
		t.Fatalf("asset proxy: %d %q", res.StatusCode, data)
	}
	req, _ = http.NewRequest(http.MethodGet, preview.URL+"/__ormos_preview/5432/", nil)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("database proxy: %d", res.StatusCode)
	}
	fix.srv.blockedPorts = []int{port}
	res, err = http.Get(preview.URL + "/__ormos_preview/" + strconv.Itoa(port) + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("self proxy: %d", res.StatusCode)
	}
	uiLoadPolicy = func() (policy, error) { return policy{}, os.ErrPermission }
	res, err = http.Get(preview.URL + "/__ormos_preview/3000/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("unreadable policy: %d", res.StatusCode)
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

func TestUIPreviewWebSocketUpgrade(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("ormos_preview_port"); err == nil {
			t.Error("selector cookie leaked upstream")
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		kind, data, err := conn.Read(ctx)
		if err == nil {
			conn.Write(ctx, kind, data)
		}
	}))
	defer app.Close()
	u, _ := url.Parse(app.URL)
	port, _ := strconv.Atoi(u.Port())
	fix := newUIFixture(t, nil)
	uiLoadPolicy = func() (policy, error) { return policy{AllowedPorts: []int{port}}, nil }
	preview := httptest.NewServer(fix.srv.previewRoutes())
	defer preview.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(preview.URL, "http")+"/hmr", &websocket.DialOptions{HTTPHeader: http.Header{
		"Origin": []string{preview.URL}, "Cookie": []string{"ormos_preview_port=" + strconv.Itoa(port)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageText, []byte("hot reload")); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil || string(data) != "hot reload" {
		t.Fatalf("upgrade: %q %v", data, err)
	}
}

func TestUIPreviewUnavailablePage(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	fix := newUIFixture(t, nil)
	uiLoadPolicy = func() (policy, error) { return policy{AllowedPorts: []int{port}}, nil }
	preview := httptest.NewServer(fix.srv.previewRoutes())
	defer preview.Close()
	req, _ := http.NewRequest(http.MethodGet, preview.URL+"/", nil)
	req.AddCookie(&http.Cookie{Name: "ormos_preview_port", Value: strconv.Itoa(port)})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: %d", res.StatusCode)
	}
	if res.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("content type: %q", res.Header.Get("Content-Type"))
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("unavailable preview could be cached after the app starts")
	}
	for _, text := range []string{"<h1>No app listening</h1>", "refresh the preview."} {
		if !strings.Contains(string(data), text) {
			t.Fatalf("missing %q in unavailable page", text)
		}
	}
	if strings.Contains(string(data), "connect: connection refused") {
		t.Fatal("raw proxy error leaked into the page")
	}
}
