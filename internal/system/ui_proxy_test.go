//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func BenchmarkPreviewProxyAsset(b *testing.B) {
	payload := bytes.Repeat([]byte("x"), 1<<20)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) }))
	defer app.Close()
	target, _ := url.Parse(app.URL)
	port, _ := strconv.Atoi(target.Port())
	oldPolicy := uiLoadPolicy
	uiLoadPolicy = func() (policy, error) { return policy{AllowedPorts: []int{port}}, nil }
	defer func() { uiLoadPolicy = oldPolicy }()
	for _, pooled := range []bool{false, true} {
		b.Run(strconv.FormatBool(pooled), func(b *testing.B) {
			s := newPreviewServe(context.Background())
			defer s.cancel()
			if !pooled {
				s.buffers = nil
			}
			proxy := s.proxyHandler(s.ctx, port, "http", "localhost:3000")
			r := httptest.NewRequest("GET", "http://localhost:3000/asset.wasm", nil)
			w := &uiDiscardResponse{header: make(http.Header)}
			proxy.ServeHTTP(w, r) // warm transport and buffer pool
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				w.size = 0
				clear(w.header)
				proxy.ServeHTTP(w, r)
				if w.size != len(payload) {
					b.Fatal("proxy changed body length")
				}
			}
		})
	}
}

func TestPreviewBufferPoolRetentionBound(t *testing.T) {
	pool := make(previewBufferPool, 16)
	for range 32 {
		pool.Put(make([]byte, 32<<10))
	}
	if len(pool) != 16 {
		t.Fatal("idle buffer retention exceeded its bound")
	}
	pool.Put(make([]byte, 1))
	buffer := pool.Get()
	if len(buffer) != 32<<10 || len(pool) != 15 {
		t.Fatal("pooled transfer buffer not reused")
	}
}

func TestPreviewRouteCloseCancelsStreamingHTTPAndWebSocket(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(strconv.FormatBool(upgrade), func(t *testing.T) {
			newUIFixture(t, nil)
			started, ended := make(chan struct{}), make(chan struct{})
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(ended)
				if upgrade {
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.CloseNow()
					close(started)
					_, _, _ = conn.Read(r.Context())
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: ready\n\n")
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			}))
			defer app.Close()
			target, _ := url.Parse(app.URL)
			port, _ := strconv.Atoi(target.Port())
			uiLoadPolicy = func() (policy, error) { return policy{AllowedPorts: []int{port}}, nil }
			s := testPreviewServe(t)
			exposed, err := s.open(context.Background(), port, "http", "127.0.0.1", 4242)
			if err != nil {
				t.Fatal(err)
			}
			address := "127.0.0.1:" + strconv.Itoa(exposed)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var socket *websocket.Conn
			if upgrade {
				socket, _, err = websocket.Dial(ctx, "ws://"+address+"/hmr", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"http://" + address}}})
				if err != nil {
					t.Fatal(err)
				}
				defer socket.CloseNow()
			} else {
				r, _ := http.NewRequestWithContext(ctx, "GET", "http://"+address+"/events", nil)
				res, err := http.DefaultClient.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				prefix := make([]byte, len("data: ready\n\n"))
				if _, err := io.ReadFull(res.Body, prefix); err != nil || string(prefix) != "data: ready\n\n" {
					t.Fatalf("SSE buffered or changed: %q %v", prefix, err)
				}
			}
			<-started
			s.close()
			select {
			case <-ended:
			case <-ctx.Done():
				t.Fatal("route shutdown left the app connection running")
			}
			if socket != nil {
				if _, _, err := socket.Read(ctx); err == nil {
					t.Fatal("hijacked preview survived shutdown")
				}
			}
		})
	}
}

func TestPreviewProxyHostOriginPathsRedirectsAndPolicy(t *testing.T) {
	newUIFixture(t, nil)
	var port int
	var requests int
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Host != "localhost:"+strconv.Itoa(port) {
			t.Error("upstream did not receive localhost Host")
		}
		if r.Header.Get("X-Forwarded-Host") != "" || r.Header.Get("Forwarded") != "" {
			t.Error("trusted spoofed forwarding headers")
		}
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "http://"+r.Host+"/inside?x=1#part")
			w.WriteHeader(302)
			return
		}
		if r.Header.Get("Origin") != "" && r.Header.Get("Origin") != "http://"+r.Host {
			t.Errorf("unexpected upstream origin: %s", r.Header.Get("Origin"))
		}
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")
		w.Header().Set("Set-Cookie", "example=kept; HttpOnly; Path=/")
		fmt.Fprint(w, r.Method+" "+r.URL.RequestURI())
	}))
	defer app.Close()
	target, _ := url.Parse(app.URL)
	port, _ = strconv.Atoi(target.Port())
	uiLoadPolicy = func() (policy, error) { return policy{AllowedPorts: []int{port}}, nil }
	s := testPreviewServe(t)
	proxy := httptest.NewServer(s.proxyHandler(s.ctx, port, "https", "box.test:3000"))
	defer proxy.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, tc := range []struct {
		path, method, host, origin string
		status                     int
	}{
		{"/nested/file.wasm?x=1", "GET", "box.test:3000", "", 200},
		{"/submit", "POST", "box.test:3000", "https://box.test:3000", 200},
		{"/redirect", "GET", "box.test:3000", "", 302},
		{"/", "GET", "evil.example", "", 421},
		{"/", "POST", "box.test:3000", "https://evil.example", 403},
		{"/", "CONNECT", "box.test:3000", "", 405},
	} {
		req, _ := http.NewRequest(tc.method, proxy.URL+tc.path, nil)
		req.Host = tc.host
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("X-Forwarded-Host", "evil.example")
		req.Header.Set("Forwarded", "host=evil.example")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != tc.status {
			t.Fatalf("%s: %d %s", tc.path, res.StatusCode, body)
		}
		if tc.status == 200 && (string(body) != tc.method+" "+tc.path || res.Header.Get("Cross-Origin-Embedder-Policy") != "require-corp" || res.Header.Get("Set-Cookie") != "example=kept; HttpOnly; Path=/") {
			t.Fatalf("changed response: %s %v", body, res.Header)
		}
		if tc.status == 302 && res.Header.Get("Location") != "https://box.test:3000/inside?x=1#part" {
			t.Fatalf("bad redirect: %s", res.Header.Get("Location"))
		}
	}
	if requests != 3 {
		t.Fatalf("rejected requests reached app: %d", requests)
	}
	uiLoadPolicy = func() (policy, error) { return policy{DeniedPorts: []int{port}}, nil }
	req, _ := http.NewRequest("GET", proxy.URL, nil)
	req.Host = "box.test:3000"
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 || requests != 3 {
		t.Fatal("policy change did not revoke existing preview")
	}
}

func TestPreviewProxyWebSocket(t *testing.T) {
	newUIFixture(t, nil)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+r.Host {
			http.Error(w, "bad origin", 403)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		kind, data, err := conn.Read(r.Context())
		if err == nil {
			_ = conn.Write(r.Context(), kind, data)
		}
	}))
	defer app.Close()
	target, _ := url.Parse(app.URL)
	port, _ := strconv.Atoi(target.Port())
	uiLoadPolicy = func() (policy, error) { return policy{AllowedPorts: []int{port}}, nil }
	s := testPreviewServe(t)
	proxy := httptest.NewServer(s.proxyHandler(s.ctx, port, "https", "box.test:3000"))
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	options := &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://box.test:3000"}}, Host: "box.test:3000"}
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/hmr?token=kept", options)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageText, []byte("live reload")); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil || string(data) != "live reload" {
		t.Fatalf("echo=%q error=%v", data, err)
	}
	conn.CloseNow()
	options.HTTPHeader.Set("Origin", "https://evil.example")
	if conn, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http"), options); err == nil || res == nil || res.StatusCode != 403 {
		if conn != nil {
			conn.CloseNow()
		}
		t.Fatalf("foreign websocket accepted: %v %v", res, err)
	}
}

func TestPreviewIPv6Loopback(t *testing.T) {
	newUIFixture(t, nil)
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	app := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "IPv6 app") }))
	app.Listener.Close()
	app.Listener = listener
	app.Start()
	defer app.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	uiLoadPolicy = func() (policy, error) { return policy{AllowedPorts: []int{port}}, nil }
	s := testPreviewServe(t)
	s.listening = loopbackAppListening
	exposed, err := s.open(context.Background(), port, "http", "::1", 4242)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Get("http://[::1]:" + strconv.Itoa(exposed))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || string(body) != "IPv6 app" {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
}
