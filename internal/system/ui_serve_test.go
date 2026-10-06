//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
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
	s.start = func(ctx context.Context, scheme string, _ int) (*serveSession, error) {
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
	compatible := `{"TCP":{"3000":{"HTTP":true}},"Web":{"box.test:3000":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3000"}}}}}`
	for _, tc := range []struct {
		name, raw string
		allowed   bool
	}{
		{"background compatible", compatible, true},
		{"foreground compatible", `{"Foreground":{"someone-else":` + compatible + `}}`, true},
		{"different app", strings.ReplaceAll(compatible, "127.0.0.1:3000", "127.0.0.1:4000"), false},
		{"different protocol", strings.ReplaceAll(compatible, "HTTP", "HTTPS"), false},
		{"TCP service", `{"TCP":{"3000":{"TCPForward":"127.0.0.1:4000"}}}`, false},
		{"additional handler", strings.Replace(compatible, `"Handlers":{`, `"Handlers":{"/private":{"Text":"existing"},`, 1), false},
		{"public funnel", strings.Replace(compatible, `"TCP":`, `"AllowFunnel":{"box.test:3000":true},"TCP":`, 1), false},
		{"duplicate port", `{"Foreground":{"a":` + compatible + `,"b":` + compatible + `}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testPreviewServe(t)
			cfg := testServeConfig(t, tc.raw)
			before, _ := json.Marshal(cfg)
			s.status = func(context.Context) (*serveConfig, error) { return cfg, nil }
			s.start = func(context.Context, string, int) (*serveSession, error) {
				t.Fatal("changed a preexisting route")
				return nil, nil
			}
			err := s.ensure(context.Background(), 3000, "http")
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v: %v", tc.allowed, err)
			}
			if !tc.allowed {
				var setup *previewSetupError
				if !errors.As(err, &setup) || setup.status != http.StatusConflict {
					t.Fatalf("wrong error: %v", err)
				}
			}
			if len(s.sessions) != 0 {
				t.Fatal("claimed ownership of existing route")
			}
			after, _ := json.Marshal(cfg)
			if string(before) != string(after) {
				t.Fatal("modified existing config")
			}
		})
	}
}

func TestPreviewConcurrentRequestsReuseSessionAndShutdown(t *testing.T) {
	s := testPreviewServe(t)
	original := s.start
	var starts atomic.Int32
	s.start = func(ctx context.Context, scheme string, port int) (*serveSession, error) {
		starts.Add(1)
		return original(ctx, scheme, port)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if err := s.ensure(context.Background(), 3000, "http"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if starts.Load() != 1 {
		t.Fatalf("started %d watchers", starts.Load())
	}
	session := s.sessions[3000]
	s.cancel()
	select {
	case <-session.done:
	case <-time.After(time.Second):
		t.Fatal("owned watcher survived shutdown")
	}
}

func TestPreviewSessionLimitAndPruning(t *testing.T) {
	s := testPreviewServe(t)
	for port := 3000; port < 3000+maxPreviewRoutes; port++ {
		if err := s.ensure(context.Background(), port, "http"); err != nil {
			t.Fatal(err)
		}
	}
	var setup *previewSetupError
	if err := s.ensure(context.Background(), 4000, "http"); !errors.As(err, &setup) || setup.status != http.StatusTooManyRequests {
		t.Fatalf("limit: %v", err)
	}
	session := s.sessions[3000]
	session.stop()
	<-session.done
	if err := s.ensure(context.Background(), 4000, "http"); err != nil {
		t.Fatalf("did not prune: %v", err)
	}
}

func TestPreviewCanceledSetupStopsItsWatcher(t *testing.T) {
	s := testPreviewServe(t)
	requested, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.start = func(ctx context.Context, scheme string, _ int) (*serveSession, error) {
		ctx, stop := context.WithCancel(ctx)
		session := &serveSession{scheme: scheme, ready: make(chan struct{}), done: done, stop: stop}
		go func() { <-ctx.Done(); close(done) }()
		cancel()
		return session, nil
	}
	if err := s.ensure(requested, 3000, "http"); err == nil {
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
		got := s.ensure(context.Background(), 3000, "http")
		var setup *previewSetupError
		if !errors.As(got, &setup) || setup.status != http.StatusServiceUnavailable || strings.Contains(got.Error(), "private account info") {
			t.Fatalf("unsafe error: %v", got)
		}
	}
	s := testPreviewServe(t)
	s.listening = func(context.Context, int) error { return errors.New("not listening") }
	s.status = func(context.Context) (*serveConfig, error) { t.Fatal("configured absent app"); return nil, nil }
	if err := s.ensure(context.Background(), 3000, "http"); err == nil {
		t.Fatal("accepted absent app")
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
