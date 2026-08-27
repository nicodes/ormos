//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nicodes/ormos/relay"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func TestConcurrentPortsAndReset401StopsBeforeConnect(t *testing.T) {
	withTempConfigDir(t)
	oldClient := httpClient
	t.Cleanup(func() { httpClient = oldClient })
	arrived := make(chan string, 2)
	release := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/system/ports", "/system/terminal-sessions/reset":
			requests.Add(1)
			arrived <- r.URL.Path
			<-release
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid pairing token"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	httpClient = server.Client()
	d := newSystem(systemConfig{RelayURL: "ws" + strings.TrimPrefix(server.URL, "http"), PairingToken: "stale", Shell: "/bin/sh"})
	d.pollPortsFn = func(ctx context.Context) { _, _ = d.fetchConfiguredPorts(ctx) }
	var connects atomic.Int32
	d.connectAndServeFn = func(context.Context) (bool, error) {
		connects.Add(1)
		return false, nil
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(context.Background()) }()
	seen := map[string]bool{}
	for range 2 {
		select {
		case path := <-arrived:
			seen[path] = true
		case <-time.After(2 * time.Second):
			t.Fatal("ports and reset did not both reach the relay")
		}
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, errPairingTokenRevoked) {
			t.Fatalf("Run error = %v, want pairing-token revocation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after authoritative 401")
	}
	if !seen["/system/ports"] || !seen["/system/terminal-sessions/reset"] {
		t.Fatalf("paths reached = %v", seen)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests after revocation = %d, want exactly the two synchronized in-flight requests", got)
	}
	if got := connects.Load(); got != 0 {
		t.Fatalf("connect attempts = %d, want 0 after reset 401", got)
	}
}

func TestConnect401MarksPairingTokenRevoked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	d := newSystem(systemConfig{RelayURL: "ws" + strings.TrimPrefix(server.URL, "http"), PairingToken: "stale", Shell: "/bin/sh"})
	connected, err := d.connectAndServe(context.Background())
	if connected || !errors.Is(err, errPairingTokenRevoked) {
		t.Fatalf("connect = (%v, %v), want false and pairing-token revocation", connected, err)
	}
	if !errors.Is(d.pairingRevocationError(), errPairingTokenRevoked) {
		t.Fatal("401 handshake did not publish revocation to the run loop")
	}
}

func TestReconnectRunsTerminalReconciliation(t *testing.T) {
	withTempConfigDir(t)
	hold := make(chan struct{})
	terminalLists := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/system/connect" {
			ws, err := websocket.Accept(w, r, nil)
			if err == nil {
				<-hold
				_ = ws.Close(websocket.StatusNormalClosure, "")
			}
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/system/terminal-sessions/reset":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/system/terminal-sessions":
			terminalLists++
			if terminalLists == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, `{"sessions":[{"id":"keep","state":"running","generation":4}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	d := newSystem(systemConfig{RelayURL: "ws" + strings.TrimPrefix(server.URL, "http"), Shell: "/bin/sh"})
	d.pollPortsFn = func(context.Context) {}
	d.terminals["keep"] = &terminalSession{id: "keep", recordID: "keep", generation: 4, owner: d, done: make(chan struct{})}
	d.terminals["stale"] = &terminalSession{id: "stale", recordID: "stale", generation: 2, owner: d, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for {
		d.terminalMu.Lock()
		_, keep := d.terminals["keep"]
		_, stale := d.terminals["stale"]
		d.terminalMu.Unlock()
		if keep && !stale {
			close(hold)
			cancel()
			<-done
			d.waitExitReports()
			return
		}
		select {
		case <-deadline:
			t.Fatalf("reconnect reconciliation state keep=%v stale=%v", keep, stale)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestTerminalLifecycleResetGatesCreatePTYAndConnect(t *testing.T) {
	withTempConfigDir(t)
	oldClient := httpClient
	t.Cleanup(func() { httpClient = oldClient })
	resetStarted, releaseReset, resetOK := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/system/terminal-sessions/reset" {
			close(resetStarted)
			<-releaseReset
			close(resetOK)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	httpClient = server.Client()
	d := newSystem(systemConfig{RelayURL: "ws" + strings.TrimPrefix(server.URL, "http"), Shell: "/bin/sh"})
	connected := make(chan struct{})
	d.connectAndServeFn = func(ctx context.Context) (bool, error) {
		close(connected)
		<-ctx.Done()
		return false, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.pollPortsFn = func(context.Context) {}
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	select {
	case <-resetStarted:
	case <-time.After(time.Second):
		t.Fatal("reset did not start")
	}
	if _, err := d.createTerminalSession(context.Background(), "project"); err == nil || !strings.Contains(err.Error(), "reset pending") {
		t.Fatalf("create during held reset = %v", err)
	}
	d.resetMu.Lock()
	d.resetErr = errors.New("reset unavailable")
	d.resetMu.Unlock()
	if _, err := d.createTerminalSession(context.Background(), "project"); err == nil || !strings.Contains(err.Error(), "reset failed; retrying") {
		t.Fatalf("create after failed reset = %v", err)
	}
	d.resetMu.Lock()
	d.resetErr = nil
	d.resetMu.Unlock()
	h := fencedHeader(relay.StreamHeader{Kind: relay.KindTerminal, SessionID: "pending", Cols: 80, Rows: 24})
	if _, err := d.terminal(h, acceptedFenceDeadline(t, h)); err == nil || !strings.Contains(err.Error(), "reset pending") {
		t.Fatalf("PTY start during held reset = %v", err)
	}
	d.terminalMu.Lock()
	if len(d.terminals) != 0 {
		t.Fatal("reset-gated terminal inserted a session")
	}
	d.terminalMu.Unlock()
	select {
	case <-connected:
		t.Fatal("connect started before reset returned 2xx")
	default:
	}
	close(releaseReset)
	select {
	case <-resetOK:
	case <-time.After(time.Second):
		t.Fatal("reset did not complete")
	}
	select {
	case <-connected:
	case <-time.After(time.Second):
		t.Fatal("connect did not start after reset succeeded")
	}
	cancel()
	<-done
}

func TestConfiguredStartupLocksBeforeAgentActions(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	file := filepath.Join(filepath.Dir(source), "run.go")
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var startup *ast.FuncDecl
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "runSystem" {
			startup = fn
			break
		}
	}
	if startup == nil {
		t.Fatal("runSystem not found")
	}
	positions := map[string]token.Pos{}
	ast.Inspect(startup.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if _, wanted := map[string]bool{"acquireStateDirLock": true, "tokenValid": true, "performLogin": true, "newSystem": true, "runTUI": true}[fn.Name]; wanted && positions[fn.Name] == token.NoPos {
				positions[fn.Name] = call.Pos()
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "Run" && positions["Run"] == token.NoPos {
				positions["Run"] = call.Pos()
			}
		}
		return true
	})
	lockPos := positions["acquireStateDirLock"]
	if lockPos == token.NoPos {
		t.Fatal("runSystem does not acquire the state lock")
	}
	for _, action := range []string{"tokenValid", "performLogin", "newSystem", "runTUI", "Run"} {
		if positions[action] == token.NoPos {
			t.Fatalf("runSystem action %s not found", action)
		}
		if lockPos >= positions[action] {
			t.Fatalf("state lock acquisition moved after startup action %s", action)
		}
	}
}

func TestProjectDeletionSynchronouslyReconcilesLocalTerminals(t *testing.T) {
	oldClient := httpClient
	t.Cleanup(func() { httpClient = oldClient })
	var paths []string
	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.Method+" "+req.URL.Path)
		if req.Method == http.MethodDelete && req.URL.Path == "/system/projects/project" {
			return testHTTPResponse(http.StatusNoContent, ""), nil
		}
		if req.Method == http.MethodGet && req.URL.Path == "/system/terminal-sessions" {
			return testHTTPResponse(http.StatusOK, `{"sessions":[]}`), nil
		}
		return testHTTPResponse(http.StatusNotFound, ""), nil
	})}
	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test"}, terminals: make(map[string]*terminalSession)}
	s := &terminalSession{id: "record", recordID: "record", generation: 1, owner: d, done: make(chan struct{})}
	d.terminals["record"] = s
	if err := d.deleteProject(context.Background(), "project"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if !closed {
		t.Fatal("project deletion returned before local terminal teardown")
	}
	d.waitExitReports()
	d.terminalMu.Lock()
	_, mapped := d.terminals["record"]
	d.terminalMu.Unlock()
	if mapped || len(paths) < 2 || paths[0] != "DELETE /system/projects/project" || paths[1] != "GET /system/terminal-sessions" {
		t.Fatalf("delete paths=%v mapped=%v, want DELETE then synchronous reconciliation", paths, mapped)
	}
}

func TestTerminalDeletionSynchronouslyReconcilesLocalSession(t *testing.T) {
	oldClient := httpClient
	t.Cleanup(func() { httpClient = oldClient })
	var paths []string
	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.Method+" "+req.URL.Path)
		if req.Method == http.MethodDelete && req.URL.Path == "/system/terminal-sessions/record" {
			return testHTTPResponse(http.StatusNoContent, ""), nil
		}
		if req.Method == http.MethodGet && req.URL.Path == "/system/terminal-sessions" {
			return testHTTPResponse(http.StatusOK, `{"sessions":[]}`), nil
		}
		return testHTTPResponse(http.StatusNotFound, ""), nil
	})}
	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test"}, terminals: make(map[string]*terminalSession)}
	s := &terminalSession{id: "record", recordID: "record", generation: 1, owner: d, done: make(chan struct{})}
	d.terminals["record"] = s
	if err := d.deleteTerminalSession(context.Background(), "record", 1); err != nil {
		t.Fatal(err)
	}
	d.waitExitReports()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	d.terminalMu.Lock()
	_, mapped := d.terminals["record"]
	d.terminalMu.Unlock()
	if !closed || mapped || len(paths) < 2 || paths[0] != "DELETE /system/terminal-sessions/record" || paths[1] != "GET /system/terminal-sessions" {
		t.Fatalf("delete paths=%v closed=%v mapped=%v", paths, closed, mapped)
	}
}

func TestRunShutdownClosesSessionsAndWaitsForExactExitReport(t *testing.T) {
	oldClient := httpClient
	t.Cleanup(func() { httpClient = oldClient })
	report := make(chan int, 1)
	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && req.URL.Path == "/system/terminal-sessions/record/exit" {
			var body struct {
				Generation int `json:"generation"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			report <- body.Generation
			return testHTTPResponse(http.StatusNoContent, ""), nil
		}
		return testHTTPResponse(http.StatusNotFound, ""), nil
	})}
	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test"}, terminals: make(map[string]*terminalSession), resetDone: true}
	d.pollPortsFn = func(context.Context) {}
	d.sessions = 1
	d.terminals["record"] = &terminalSession{id: "record", recordID: "record", generation: 7, owner: d, done: make(chan struct{})}
	teardownEntered, releaseTeardown := make(chan struct{}), make(chan struct{})
	var once sync.Once
	d.afterTerminalProcessTeardown = func() {
		once.Do(func() {
			close(teardownEntered)
			<-releaseTeardown
		})
	}
	connectStarted := make(chan struct{})
	d.connectAndServeFn = func(ctx context.Context) (bool, error) {
		close(connectStarted)
		<-ctx.Done()
		return false, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { d.Run(ctx); close(runDone) }()
	<-connectStarted
	cancel()
	select {
	case <-teardownEntered:
	case <-time.After(time.Second):
		t.Fatal("Run did not begin synchronous terminal teardown")
	}
	select {
	case <-runDone:
		t.Fatal("Run returned before terminal teardown and report registration completed")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseTeardown)
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not complete graceful shutdown")
	}
	select {
	case generation := <-report:
		if generation != 7 {
			t.Fatalf("exit report generation=%d, want 7", generation)
		}
	case <-time.After(time.Second):
		t.Fatal("graceful shutdown returned without exact-generation exit report")
	}
}

func TestRunShutdownBoundsUndeliverableExitReport(t *testing.T) {
	oldClient := httpClient
	oldTO := terminalExitReportShutdownTO
	t.Cleanup(func() { httpClient, terminalExitReportShutdownTO = oldClient, oldTO })
	terminalExitReportShutdownTO = 25 * time.Millisecond
	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("relay unavailable")
	})}
	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test"}, terminals: make(map[string]*terminalSession), resetDone: true}
	d.pollPortsFn = func(context.Context) {}
	d.sessions = 1
	d.terminals["record"] = &terminalSession{id: "record", recordID: "record", generation: 8, owner: d, done: make(chan struct{})}
	d.connectAndServeFn = func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run hung after shutdown report deadline expired")
	}
}

func TestRunShutdownJoinsPublishedCloseBeforeReturning(t *testing.T) {
	oldClient, oldTO := httpClient, terminalExitReportShutdownTO
	t.Cleanup(func() { httpClient, terminalExitReportShutdownTO = oldClient, oldTO })
	terminalExitReportShutdownTO = 25 * time.Millisecond
	report := make(chan int, 1)
	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			return testHTTPResponse(http.StatusNoContent, ""), nil
		}
		var body struct {
			Generation int `json:"generation"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		report <- body.Generation
		return testHTTPResponse(http.StatusNoContent, ""), nil
	})}
	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test"}, terminals: make(map[string]*terminalSession), resetDone: true}
	d.pollPortsFn = func(context.Context) {}
	d.sessions = 1
	s := &terminalSession{id: "record", recordID: "record", generation: 9, owner: d, done: make(chan struct{})}
	d.terminals["record"] = s
	d.publishedTerminals = map[*terminalSession]struct{}{s: {}}
	d.publishedClose.Add(1)
	s.published = true
	removed := make(chan struct{})
	release := make(chan struct{})
	d.beforeTerminalExitReport = func() {
		close(removed)
		<-release
	}
	connectStarted := make(chan struct{})
	d.connectAndServeFn = func(ctx context.Context) (bool, error) {
		close(connectStarted)
		<-ctx.Done()
		return false, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { d.Run(ctx); close(runDone) }()
	<-connectStarted
	closeDone := make(chan struct{})
	go func() { s.close(); close(closeDone) }()
	select {
	case <-removed:
	case <-time.After(time.Second):
		t.Fatal("close did not reach the post-removal report fence")
	}
	cancel()
	select {
	case <-runDone:
		t.Fatal("Run returned before the published close completed")
	case <-time.After(25 * time.Millisecond):
	}
	// Keep the published close paused beyond the old report deadline. The
	// report is registered only after this fence, so the deadline must start
	// after the close join rather than when shutdown begins.
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("published close did not finish")
	}
	select {
	case generation := <-report:
		if generation != 9 {
			t.Fatalf("exit report generation=%d, want 9", generation)
		}
	case <-time.After(time.Second):
		t.Fatal("published close did not report exact generation")
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after joined close")
	}
}

func TestExitReportUsesShutdownContextWhenRunCancelsFirst(t *testing.T) {
	oldClient, oldTO, oldBackoff := httpClient, terminalExitReportShutdownTO, terminalExitReportBackoff
	t.Cleanup(func() {
		httpClient, terminalExitReportShutdownTO, terminalExitReportBackoff = oldClient, oldTO, oldBackoff
	})
	terminalExitReportShutdownTO = 250 * time.Millisecond
	terminalExitReportBackoff = 5 * time.Millisecond
	registered := make(chan struct{})
	shutdownPaused := make(chan struct{})
	release := make(chan struct{})
	var posts atomic.Int32
	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			return testHTTPResponse(http.StatusNoContent, ""), nil
		}
		if posts.Add(1) == 1 {
			return nil, errors.New("relay unavailable")
		}
		return testHTTPResponse(http.StatusNoContent, ""), nil
	})}
	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test"}, terminals: make(map[string]*terminalSession), resetDone: true}
	d.pollPortsFn = func(context.Context) {}
	d.beforeExitReportContext = func() {
		close(shutdownPaused)
		<-release
	}
	d.connectAndServeFn = func(ctx context.Context) (bool, error) {
		d.reportTerminalExit("record", 10)
		close(registered)
		<-ctx.Done()
		return false, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	select {
	case <-registered:
	case <-time.After(time.Second):
		t.Fatal("exit report worker was not registered")
	}
	cancel()
	select {
	case <-shutdownPaused:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not pause before the report deadline was armed")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not complete with bounded report context")
	}
	if got := posts.Load(); got < 2 {
		t.Fatalf("exit report attempts=%d, want a retry under shutdown context", got)
	}
}

func TestExitReportWorkerSurvivesSlowShutdownPublication(t *testing.T) {
	oldClient, oldTO, oldGrace := httpClient, terminalExitReportShutdownTO, terminalKillGrace
	t.Cleanup(func() { httpClient, terminalExitReportShutdownTO, terminalKillGrace = oldClient, oldTO, oldGrace })
	terminalExitReportShutdownTO = 25 * time.Millisecond
	terminalKillGrace = 2 * time.Millisecond
	posted := make(chan int, 1)
	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			var body struct {
				Generation int `json:"generation"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			posted <- body.Generation
		}
		return testHTTPResponse(http.StatusNoContent, ""), nil
	})}
	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test"}, terminals: make(map[string]*terminalSession)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.runCtx = ctx
	waiting := make(chan struct{})
	d.beforeExitReportContextWait = func() { close(waiting) }
	d.reportTerminalExit("record", 11)
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("report worker did not wait for shutdown context publication")
	}
	d.beforeExitReportContextPublish = func() { time.Sleep(50 * time.Millisecond) }
	done := make(chan struct{})
	go func() { d.finishShutdown(); close(done) }()
	select {
	case generation := <-posted:
		if generation != 11 {
			t.Fatalf("exit report generation=%d, want 11", generation)
		}
	case <-time.After(time.Second):
		t.Fatal("report worker dropped its exact-generation report during slow publication")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after report delivery")
	}
}

func TestShutdownPreventsInFlightAdmissionPublication(t *testing.T) {
	d := newSystem(systemConfig{Shell: "/bin/sh"})
	entered, release := make(chan struct{}), make(chan struct{})
	d.beforePTYStart = func() {
		close(entered)
		<-release
	}
	h := fencedHeader(relay.StreamHeader{Kind: relay.KindTerminal, SessionID: "shutdown", Cols: 80, Rows: 24})
	shutdownStarted := make(chan struct{})
	d.afterShutdownAdmissionInvalidation = func() { close(shutdownStarted) }
	result := make(chan error, 1)
	go func() { _, err := d.terminal(h, acceptedFenceDeadline(t, h)); result <- err }()
	<-entered
	shutdownDone := make(chan struct{})
	go func() { d.finishShutdown(); close(shutdownDone) }()
	<-shutdownStarted
	close(release)
	<-shutdownDone
	if err := <-result; err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("in-flight admission result=%v, want shutdown rejection", err)
	}
	d.terminalMu.Lock()
	_, published := d.terminals["shutdown"]
	d.terminalMu.Unlock()
	if published {
		t.Fatal("shutdown allowed an in-flight admission to publish")
	}
}

func TestReconcileDoesNotHoldTerminalMapLockAcrossRelayFetch(t *testing.T) {
	oldClient := httpClient
	t.Cleanup(func() { httpClient = oldClient })
	fetchStarted, releaseFetch := make(chan struct{}), make(chan struct{})
	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Path != "/system/terminal-sessions" {
			return testHTTPResponse(http.StatusNotFound, ""), nil
		}
		close(fetchStarted)
		<-releaseFetch
		return testHTTPResponse(http.StatusOK, `{"sessions":[]}`), nil
	})}
	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test"}}
	done := make(chan error, 1)
	go func() { done <- d.reconcileSnapshot(context.Background()) }()
	select {
	case <-fetchStarted:
	case <-time.After(time.Second):
		t.Fatal("reconcile fetch did not start")
	}
	mapLockFree := make(chan struct{})
	go func() {
		d.terminalMu.Lock()
		d.terminalMu.Unlock()
		close(mapLockFree)
	}()
	select {
	case <-mapLockFree:
	case <-time.After(time.Second):
		t.Fatal("reconcile held terminalMu across relay HTTP")
	}
	close(releaseFetch)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReconcileSkipsSessionReplacedDuringFetch(t *testing.T) {
	oldClient := httpClient
	t.Cleanup(func() { httpClient = oldClient })
	fetchStarted, releaseFetch := make(chan struct{}), make(chan struct{})
	httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		close(fetchStarted)
		<-releaseFetch
		return testHTTPResponse(http.StatusOK, `{"sessions":[]}`), nil
	})}
	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test"}, terminals: make(map[string]*terminalSession)}
	old := &terminalSession{id: "record", recordID: "record", generation: 1, owner: d, done: make(chan struct{})}
	replacement := &terminalSession{id: "record", recordID: "record", generation: 2, admissionSequence: 1, owner: d, done: make(chan struct{})}
	d.terminals["record"] = old
	done := make(chan error, 1)
	go func() { done <- d.reconcileSnapshot(context.Background()) }()
	<-fetchStarted
	d.terminalMu.Lock()
	d.terminals["record"] = replacement
	d.terminalMu.Unlock()
	close(releaseFetch)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	replacement.mu.Lock()
	closed := replacement.closed
	replacement.mu.Unlock()
	if closed {
		t.Fatal("reconcile closed a replacement that was not in its fetched snapshot")
	}
}

func TestInitialReconnectReconciliationBlocksStreamAcceptance(t *testing.T) {
	withTempConfigDir(t)
	listStarted, releaseList, closeTunnel := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/system/terminal-sessions/reset":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/system/terminal-sessions":
			close(listStarted)
			<-releaseList
			_, _ = io.WriteString(w, `{"sessions":[{"id":"keep","state":"running","generation":4}]}`)
		case r.URL.Path == "/system/connect":
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			sess, err := relay.ClientSession(relay.NetConn(context.Background(), ws))
			if err != nil {
				return
			}
			stream, err := sess.Open()
			if err != nil {
				return
			}
			_ = relay.WriteHeader(stream, relay.StreamHeader{Kind: relay.KindEvent})
			<-closeTunnel
			_ = stream.Close()
			_ = sess.Close()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	d := newSystem(systemConfig{RelayURL: "ws" + strings.TrimPrefix(server.URL, "http"), Shell: "/bin/sh"})
	d.pollPortsFn = func(context.Context) {}
	d.terminals["keep"] = &terminalSession{id: "keep", recordID: "keep", generation: 4, owner: d, done: make(chan struct{})}
	d.terminals["stale"] = &terminalSession{id: "stale", recordID: "stale", generation: 2, owner: d, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	select {
	case <-listStarted:
	case <-time.After(time.Second):
		t.Fatal("initial list fetch did not start")
	}
	// The event stream is already opened by the relay, but cannot be accepted
	// until the blocked authoritative snapshot is released.
	select {
	case <-d.Events():
		t.Fatal("event accepted before initial reconciliation completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseList)
	select {
	case <-d.Events():
	case <-time.After(time.Second):
		t.Fatal("event was not accepted after reconciliation")
	}
	d.terminalMu.Lock()
	_, keep := d.terminals["keep"]
	_, stale := d.terminals["stale"]
	d.terminalMu.Unlock()
	if !keep || stale {
		t.Fatalf("reconciliation state keep=%v stale=%v", keep, stale)
	}
	close(closeTunnel)
	cancel()
	<-done
	d.waitExitReports()
}

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// Relay responses decode through a bounded reader: a relay that answers with
// an endless body must not make the agent buffer without limit.
func TestDecodeRelayJSONBoundsTheBody(t *testing.T) {
	var small struct {
		Name string `json:"name"`
	}
	if err := decodeRelayJSON(strings.NewReader(`{"name":"ok"}`), &small); err != nil || small.Name != "ok" {
		t.Fatalf("small body: %v %q", err, small.Name)
	}

	huge := `{"name":"` + strings.Repeat("x", maxRelayResponse) + `"}`
	var big struct {
		Name string `json:"name"`
	}
	if err := decodeRelayJSON(strings.NewReader(huge), &big); err == nil {
		t.Fatal("a body past the cap must fail to decode, not buffer forever")
	}
}

// pollPorts is the relay-status producer and logf's echo is the actual headless
// stderr path. A custom transport is necessary because net/http's test server
// chooses a canonical printable reason phrase; Response.Status itself is the
// relay-controlled value the production client receives.
func TestHeadlessPortsPollEchoSanitisesRelayStatus(t *testing.T) {
	for name, status := range map[string]string{
		"ESC and C0":           "599 relay\x1b]0;OWNED\a\x1b[2J",
		"C1 and bidi format":   "599 relay\u009b\u202e failed",
		"ordinary printable":   "599 relay maintenance",
		"empty after sanitize": "\x1b\u009b\u202e",
	} {
		t.Run(name, func(t *testing.T) {
			oldClient := httpClient
			t.Cleanup(func() { httpClient = oldClient })
			httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: 599,
					Status:     status,
					Body:       io.NopCloser(strings.NewReader("")),
					Header:     make(http.Header),
				}, nil
			})}

			var echoed bytes.Buffer
			d := &system{
				cfg:        systemConfig{RelayURL: "ws://relay.example", PairingToken: "local-test-token"},
				echoStderr: true,
				echoWriter: &echoed,
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // pollPorts still performs its immediate tick, then exits.
			d.pollPorts(ctx)

			got := echoed.String()
			if !strings.Contains(got, "ports poll failed: relay returned") {
				t.Fatalf("the reachable headless poll error was not echoed: %q", got)
			}
			if sanitize(status) == "" && !strings.Contains(got, "[relay supplied no printable text]") {
				t.Errorf("empty sanitized status has no fixed fallback: %q", got)
			}
			if clean := sanitize(status); clean != "" && !strings.Contains(got, clean) {
				t.Errorf("printable status text did not survive (want %q): %q", clean, got)
			}
			for _, forbidden := range []string{"\x1b", "\u009b", "\u202e", "\a"} {
				if strings.Contains(got, forbidden) {
					t.Errorf("relay-supplied %q reached the headless echo: %q", forbidden, got)
				}
			}
		})
	}

	// Known-good sibling: local log text must remain useful on the same echo
	// path; sanitization is scoped to the relay-derived poll argument.
	var local bytes.Buffer
	d := &system{echoStderr: true, echoWriter: &local}
	d.logf("tunnel error: connection refused")
	if !strings.HasSuffix(local.String(), " tunnel error: connection refused\n") {
		t.Errorf("local log text changed: %q", local.String())
	}
}

// Only a tunnel that lived past the floor resets the reconnect backoff; a
// clean close of a young tunnel is churn and must back off like a failure,
// or a relay that accepts and drops immediately gets a zero-delay TLS loop.
func TestTunnelHealthyFloor(t *testing.T) {
	for _, tc := range []struct {
		connected bool
		lifetime  time.Duration
		want      bool
	}{
		{true, tunnelHealthyFloor + time.Second, true},
		{true, tunnelHealthyFloor, true},
		{true, tunnelHealthyFloor - time.Second, false},
		{true, 100 * time.Millisecond, false},
		{false, tunnelHealthyFloor + time.Second, false},
	} {
		if got := tunnelHealthy(tc.connected, tc.lifetime); got != tc.want {
			t.Fatalf("tunnelHealthy(%v, %s) = %v, want %v", tc.connected, tc.lifetime, got, tc.want)
		}
	}
}
