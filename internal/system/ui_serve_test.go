//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func testServeConfig(t *testing.T, raw string) *serveConfig {
	t.Helper()
	var cfg serveConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	return &cfg
}

func testPreviewServe(t *testing.T) *previewServe {
	t.Helper()
	s := newPreviewServe(context.Background())
	s.listening = func(context.Context, int) error { return nil }
	s.status = func(context.Context) (*serveConfig, error) { return &serveConfig{}, nil }
	s.start = func(ctx context.Context, scheme string, _, _ int) (*serveSession, error) {
		ctx, cancel := context.WithCancel(ctx)
		session := &serveSession{scheme: scheme, ready: make(chan struct{}), done: make(chan struct{}), stop: cancel}
		close(session.ready)
		go func() { <-ctx.Done(); close(session.done) }()
		return session, nil
	}
	t.Cleanup(s.close)
	return s
}

func TestPreviewPreservesExistingServeRoutes(t *testing.T) {
	for _, raw := range []string{
		`{"TCP":{"3000":{"HTTP":true}},"Web":{"box.test:3000":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3000"}}}}}`,
		`{"Foreground":{"someone-else":{"TCP":{"3000":{"HTTPS":true}}}}}`,
		`{"TCP":{"3000":{"TCPForward":"127.0.0.1:4000"}}}`,
		`{"AllowFunnel":{"box.test:3000":true}}`,
	} {
		s := testPreviewServe(t)
		cfg := testServeConfig(t, raw)
		before, _ := json.Marshal(cfg)
		s.status = func(context.Context) (*serveConfig, error) { return cfg, nil }
		original := s.start
		s.start = func(ctx context.Context, scheme string, public, local int) (*serveSession, error) {
			if public == 3000 || local == 3000 {
				t.Fatal("overwrote an existing route or bypassed proxy")
			}
			return original(ctx, scheme, public, local)
		}
		exposed, err := s.open(context.Background(), 3000, "https", "box.test", 4242)
		if err != nil || exposed == 3000 {
			t.Fatalf("port=%d error=%v", exposed, err)
		}
		after, _ := json.Marshal(cfg)
		if string(before) != string(after) {
			t.Fatal("modified existing config")
		}
	}
}

func TestPreviewConcurrentRequestsReuseSessionAndShutdown(t *testing.T) {
	s := testPreviewServe(t)
	original := s.start
	var starts atomic.Int32
	s.start = func(ctx context.Context, scheme string, public, local int) (*serveSession, error) {
		starts.Add(1)
		return original(ctx, scheme, public, local)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if exposed, err := s.open(context.Background(), 3000, "https", "box.test", 4242); err != nil || exposed == 3000 {
				t.Errorf("port=%d error=%v", exposed, err)
			}
		})
	}
	wg.Wait()
	if starts.Load() != 1 {
		t.Fatalf("started %d watchers", starts.Load())
	}
	var route *previewRoute
	for _, value := range s.sessions {
		route = value
	}
	s.close()
	select {
	case <-route.session.done:
	case <-time.After(time.Second):
		t.Fatal("owned watcher survived shutdown")
	}
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(route.localPort)), time.Second); err == nil {
		conn.Close()
		t.Fatal("proxy listener survived shutdown")
	}
}

func TestPreviewSessionLimitAndPruning(t *testing.T) {
	s := testPreviewServe(t)
	for port := 3000; port < 3000+maxPreviewRoutes; port++ {
		if _, err := s.open(context.Background(), port, "https", "box.test", 4242); err != nil {
			t.Fatal(err)
		}
	}
	var setup *previewSetupError
	if _, err := s.open(context.Background(), 4000, "https", "box.test", 4242); !errors.As(err, &setup) || setup.status != http.StatusTooManyRequests {
		t.Fatalf("limit: %v", err)
	}
	for _, route := range s.sessions {
		route.session.stop()
		<-route.session.done
		break
	}
	if _, err := s.open(context.Background(), 4000, "https", "box.test", 4242); err != nil {
		t.Fatalf("did not prune: %v", err)
	}
}

func TestPreviewCanceledSetupStopsItsWatcher(t *testing.T) {
	s := testPreviewServe(t)
	requested, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.start = func(ctx context.Context, scheme string, _, _ int) (*serveSession, error) {
		ctx, stop := context.WithCancel(ctx)
		session := &serveSession{scheme: scheme, ready: make(chan struct{}), done: done, stop: stop}
		go func() { <-ctx.Done(); close(done) }()
		cancel()
		return session, nil
	}
	if _, err := s.open(requested, 3000, "https", "box.test", 4242); err == nil {
		t.Fatal("canceled setup succeeded")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("leaked watcher")
	}
	if len(s.sessions) != 0 {
		t.Fatal("retained failed session")
	}
}

func TestPreviewReportsSafeSetupErrors(t *testing.T) {
	for _, err := range []error{exec.ErrNotFound, errors.New("private account info")} {
		s := testPreviewServe(t)
		s.status = func(context.Context) (*serveConfig, error) { return nil, err }
		_, got := s.open(context.Background(), 3000, "https", "box.test", 4242)
		var setup *previewSetupError
		if !errors.As(got, &setup) || setup.status != http.StatusServiceUnavailable || strings.Contains(got.Error(), "private account info") {
			t.Fatalf("unsafe error: %v", got)
		}
	}
	s := testPreviewServe(t)
	s.listening = func(context.Context, int) error { return errors.New("not listening") }
	s.status = func(context.Context) (*serveConfig, error) { t.Fatal("configured absent app"); return nil, nil }
	if _, err := s.open(context.Background(), 3000, "https", "box.test", 4242); err == nil {
		t.Fatal("accepted absent app")
	}
}

func TestLocalPreviewNeedsNoTailscaleAndRejectsProxyLoops(t *testing.T) {
	s := testPreviewServe(t)
	s.status = func(context.Context) (*serveConfig, error) {
		t.Fatal("local preview called Tailscale")
		return nil, nil
	}
	exposed, err := s.open(context.Background(), 3000, "http", "127.0.0.1", 4242)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := s.open(context.Background(), 3000, "http", "127.0.0.1", 4242)
	if err != nil || reused != exposed {
		t.Fatalf("reuse=%d err=%v", reused, err)
	}
	if _, err := s.open(context.Background(), exposed, "http", "127.0.0.1", 4242); err == nil {
		t.Fatal("allowed a proxy loop")
	}
}

func TestPreviewAPIGuardsProvisioning(t *testing.T) {
	fix := newUIFixture(t, nil)
	fix.srv.hosts = []string{"box:8481"}
	fix.srv.controlPort = 8481
	s := testPreviewServe(t)
	fix.srv.previewServe = s
	var calls int
	s.listening = func(context.Context, int) error { calls++; return nil }
	handler := fix.srv.routes()
	for _, tc := range []struct {
		body, origin string
		code         int
	}{
		{`{"port":3000,"scheme":"http"}`, "http://box:3000", 403},
		{`{"port":3000,"scheme":"http"}`, "", 403},
		{`{"port":8481,"scheme":"http"}`, "http://box:8481", 403},
		{`{"port":5432,"scheme":"http"}`, "http://box:8481", 403},
		{`{"port":9999,"scheme":"http"}`, "http://box:8481", 403},
		{`{"port":0,"scheme":"http"}`, "http://box:8481", 400},
		{`{"port":65536,"scheme":"http"}`, "http://box:8481", 400},
		{`{"port":3000,"scheme":"https"}`, "http://box:8481", 400},
		{`{"port":3000,"scheme":"tcp"}`, "http://box:8481", 400},
		{`{"port":3000,"scheme":"http","target":"evil"}`, "http://box:8481", 400},
		{`{"port":3000,"scheme":"http"}{}`, "http://box:8481", 400},
		{strings.Repeat(" ", 257) + `{}`, "http://box:8481", 400},
	} {
		req := httptest.NewRequest(http.MethodPost, "http://box:8481/api/preview", strings.NewReader(tc.body))
		req.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != tc.code {
			t.Fatalf("%s/%s: %d want %d", tc.body, tc.origin, w.Code, tc.code)
		}
	}
	if calls != 0 {
		t.Fatal("rejected request reached provisioning")
	}
	req := httptest.NewRequest(http.MethodPost, "http://box:8481/api/preview", strings.NewReader(`{"port":3000,"scheme":"http"}`))
	req.Header.Set("Origin", "http://box:8481")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 || calls != 1 {
		t.Fatalf("valid request: %d calls=%d", w.Code, calls)
	}
	uiLoadPolicy = func() (policy, error) { return policy{}, errors.New("bad file") }
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://box:8481/api/preview", strings.NewReader(`{"port":3000,"scheme":"http"}`))
	req.Header.Set("Origin", "http://box:8481")
	handler.ServeHTTP(w, req)
	if w.Code != 403 || calls != 1 {
		t.Fatal("malformed policy allowed provisioning")
	}
}

func TestPreviewRefusesAnotherWorkspace(t *testing.T) {
	fix := newUIFixture(t, nil)
	workspace := fix.start(t)
	target, _ := url.Parse(workspace.URL)
	port, _ := strconv.Atoi(target.Port())
	if err := loopbackAppListening(context.Background(), port); err == nil {
		t.Fatal("accepted a workspace as a preview destination")
	}
}

func TestServeStatusDoesNotWaitForInheritedOutputPipe(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "child-pid")
	t.Setenv("ORMOS_TEST_CHILD_PID", pidfile)
	t.Setenv("PATH", dir)
	script := "#!/bin/sh\n/bin/sleep 30 &\nprintf '%s' $! > \"$ORMOS_TEST_CHILD_PID\"\nprintf '{}'\n"
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		data, _ := os.ReadFile(pidfile)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	result := make(chan error, 1)
	go func() { _, err := readServeStatus(context.Background()); result <- err }()
	select {
	case err := <-result:
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("inherited pipe error = %v, want ErrWaitDelay", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("exited CLI left status waiting on a descendant's pipe")
	}
}
