//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func stubUISharing(t *testing.T, cfg *serveConfig) (*serveSession, *int) {
	t.Helper()
	oldDNS, oldStatus, oldStart := uiTailnetDNS, uiServeStatus, uiStartShare
	t.Cleanup(func() { uiTailnetDNS, uiServeStatus, uiStartShare = oldDNS, oldStatus, oldStart })
	uiTailnetDNS = func(context.Context) (string, error) { return "box.tail.test.ts.net", nil }
	uiServeStatus = func(context.Context) (*serveConfig, error) { return cfg, nil }
	ctx, cancel := context.WithCancel(context.Background())
	session := &serveSession{ready: make(chan struct{}), done: make(chan struct{}), stop: cancel}
	close(session.ready)
	go func() { <-ctx.Done(); close(session.done) }()
	t.Cleanup(func() { stopServeSession(session) })
	calls := new(int)
	uiStartShare = func(_ context.Context, scheme string, args ...string) (*serveSession, error) {
		*calls++
		if scheme != "http" || !reflect.DeepEqual(args, []string{"--http=80", "--set-path=/ormos", "http://127.0.0.1:4567/ormos"}) {
			t.Fatalf("unexpected Serve command: %s %v", scheme, args)
		}
		return session, nil
	}
	return session, calls
}

func TestUIAddressRegistrationAndOwnership(t *testing.T) {
	cfg := testServeConfig(t, `{"TCP":{"443":{"HTTPS":true}},"Web":{"box.test:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3100"}}}}}`)
	before, _ := json.Marshal(cfg)
	session, calls := stubUISharing(t, cfg)
	s := &uiServer{}
	url, owned, err := shareUI(context.Background(), &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4567}, s)
	if err != nil || url != "http://box/ormos/" || owned != session || *calls != 1 {
		t.Fatalf("registration: %s %v", url, err)
	}
	for _, host := range []string{"box", "box:80", "box.tail.test.ts.net", "box.tail.test.ts.net:80"} {
		if !s.hostAllowed(host) {
			t.Fatalf("missing registered host: %s", host)
		}
	}
	if s.hostAllowed("box:3000") || s.hostAllowed("evil.test") {
		t.Fatal("sharing widened hosts outside UI origin")
	}
	after, _ := json.Marshal(cfg)
	if string(before) != string(after) {
		t.Fatal("modified preexisting config")
	}
	stopServeSession(owned)
	select {
	case <-session.done:
	default:
		t.Fatal("owned route survived shutdown")
	}
}

func TestUIAddressRefusesOtherOriginsAndReusesCompatibleRoute(t *testing.T) {
	compatible := `{"TCP":{"80":{"HTTP":true}},"Web":{"box.test:80":{"Handlers":{"/ormos":{"Proxy":"http://127.0.0.1:4567/ormos"}}}}}`
	for _, tc := range []struct {
		name, raw string
		allowed   bool
	}{
		{"same target", compatible, true},
		{"foreground same target", `{"Foreground":{"existing":` + compatible + `}}`, true},
		{"other app", strings.ReplaceAll(compatible, "4567", "7890"), false},
		{"other path same origin", strings.Replace(compatible, `"Handlers":{`, `"Handlers":{"/other":{"Text":"hello"},`, 1), false},
		{"root app", strings.ReplaceAll(compatible, `"/ormos":`, `"/":`), false},
		{"HTTPS", strings.ReplaceAll(compatible, "HTTP", "HTTPS"), false},
		{"TCP forwarding", `{"TCP":{"80":{"TCPForward":"127.0.0.1:8000"}}}`, false},
		{"public Funnel", strings.Replace(compatible, `"TCP":`, `"AllowFunnel":{"box.test:80":true},"TCP":`, 1), false},
		{"duplicate", `{"Foreground":{"a":` + compatible + `,"b":` + compatible + `}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testServeConfig(t, tc.raw)
			before, _ := json.Marshal(cfg)
			_, calls := stubUISharing(t, cfg)
			s := &uiServer{}
			_, owned, err := shareUI(context.Background(), &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4567}, s)
			if (err == nil) != tc.allowed || owned != nil || *calls != 0 {
				t.Fatalf("allowed=%v: %v", tc.allowed, err)
			}
			after, _ := json.Marshal(cfg)
			if string(before) != string(after) {
				t.Fatal("changed existing route")
			}
			if !tc.allowed && len(s.hosts) != 0 {
				t.Fatal("failed setup widened allowed hosts")
			}
		})
	}
}

func TestUIAddressFailuresPreserveLocalAccess(t *testing.T) {
	session, _ := stubUISharing(t, &serveConfig{})
	uiTailnetDNS = func(context.Context) (string, error) { return "", errors.New("private diagnostic") }
	_, _, err := shareUI(context.Background(), &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4567}, &uiServer{})
	if err == nil || strings.Contains(err.Error(), "private diagnostic") {
		t.Fatalf("unsafe error: %v", err)
	}
	uiTailnetDNS = func(context.Context) (string, error) { return "box.tail.test.ts.net", nil }
	session.ready = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, owned, err := shareUI(ctx, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4567}, &uiServer{})
	if err == nil || owned != nil {
		t.Fatal("canceled setup succeeded")
	}
	select {
	case <-session.done:
	default:
		t.Fatal("failed setup leaked watcher")
	}
	_, _, err = shareUI(context.Background(), &net.TCPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 4567}, &uiServer{})
	if err == nil {
		t.Fatal("shared a custom public bind automatically")
	}
}

func TestUIAddressDNSValidation(t *testing.T) {
	for _, name := range []string{"box.tail123.ts.net", "my-box.tail123.ts.net"} {
		if !validTailnetDNS(name) {
			t.Fatal(name)
		}
	}
	for _, name := range []string{"", "evil.test", "box..ts.net", "box/evil.ts.net", "box:80.ts.net", "-box.ts.net", "box\n.ts.net"} {
		if validTailnetDNS(name) {
			t.Fatal(name)
		}
	}
}

func TestUIAddressReadsConnectedMagicDNS(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "state.json")
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte("#!/bin/sh\ncat \"$TEST_TAILNET_JSON\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_TAILNET_JSON", fixture)
	for _, tc := range []struct {
		raw     string
		allowed bool
	}{
		{`{"BackendState":"Running","Self":{"DNSName":"box.tail123.ts.net."},"CurrentTailnet":{"MagicDNSEnabled":true}}`, true},
		{`{"BackendState":"Running","Self":{"DNSName":"box.tail123.ts.net."},"CurrentTailnet":{"MagicDNSEnabled":false}}`, false},
		{`{"BackendState":"Stopped","Self":{"DNSName":"box.tail123.ts.net."},"CurrentTailnet":{"MagicDNSEnabled":true}}`, false},
		{`{"BackendState":"Running","Self":{"DNSName":"evil.example"},"CurrentTailnet":{"MagicDNSEnabled":true}}`, false},
		{`not json`, false},
	} {
		if err := os.WriteFile(fixture, []byte(tc.raw), 0600); err != nil {
			t.Fatal(err)
		}
		dns, err := readTailnetDNS(context.Background())
		if (err == nil) != tc.allowed {
			t.Fatalf("allowed=%v: %v", tc.allowed, err)
		}
		if tc.allowed && dns != "box.tail123.ts.net" {
			t.Fatal(dns)
		}
	}
}

func TestUIListenerFallsBackOnlyForBusyDefault(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:8481")
	if err == nil {
		defer occupied.Close()
	} else if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatal(err)
	}
	ln, err := listenUI("127.0.0.1", uiDefaultPort, false)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ln.Addr().(*net.TCPAddr).Port == uiDefaultPort {
		t.Fatal("reused occupied port")
	}
	if explicit, err := listenUI("127.0.0.1", uiDefaultPort, true); err == nil {
		explicit.Close()
		t.Fatal("ignored explicit port")
	}
}

func TestUIPrefixedRoutesRetainGuardsAndRedirect(t *testing.T) {
	fix := newUIFixture(t, nil)
	handler := fix.srv.routes()
	for _, path := range []string{"/ormos", "/ormos/", "/ormos/api/terminals"} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost"+path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if path == "/ormos" {
			if w.Code != 308 || w.Header().Get("Location") != "/ormos/" {
				t.Fatalf("redirect: %d", w.Code)
			}
		} else if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/ormos/api/action", strings.NewReader(`{"action":"open"}`))
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 || fix.spawnLog.Len() != 0 {
		t.Fatal("prefix bypassed origin guard")
	}
}
