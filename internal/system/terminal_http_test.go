//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicodes/ormos/relay"
)

func TestTerminalExitReportRetriesExactGenerationAndCancels(t *testing.T) {
	oldClient := httpClient
	t.Cleanup(func() { httpClient = oldClient })
	var requests int
	seen := make(chan int, 1)
	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/system/terminal-sessions/retry-record-unique/exit" {
			return testHTTPResponse(http.StatusNotFound, ""), nil
		}
		requests++
		if requests == 1 {
			return nil, errors.New("temporary")
		}
		var body struct {
			Generation int `json:"generation"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		seen <- body.Generation
		return testHTTPResponse(http.StatusNoContent, ""), nil
	})}
	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test"}}
	d.reportTerminalExit("retry-record-unique", 17)
	select {
	case got := <-seen:
		if got != 17 {
			t.Fatalf("generation=%d", got)
		}
	case <-time.After(time.Second):
		t.Fatal("exit report did not retry")
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	d.runCtx = ctx
	d.mu.Unlock()
	requests = 0
	started := make(chan struct{})
	var startedOnce sync.Once
	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/system/terminal-sessions/retry-record-unique/exit" {
			return testHTTPResponse(http.StatusNotFound, ""), nil
		}
		requests++
		startedOnce.Do(func() { close(started) })
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	d.reportTerminalExit("retry-record-unique", 18)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cancellation test request did not start")
	}
	cancel()
	count := requests
	time.Sleep(100 * time.Millisecond)
	if requests != count {
		t.Fatalf("cancellation did not stop retries: %d -> %d", count, requests)
	}
}

func TestTerminalSessionHTTPWireShape(t *testing.T) {
	oldClient := httpClient
	t.Cleanup(func() { httpClient = oldClient })

	requests := 0
	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.Path != "/system/terminal-sessions" || req.Header.Get("Authorization") != "Bearer pairing" {
			t.Fatalf("request = %s auth=%q", req.URL.Path, req.Header.Get("Authorization"))
		}
		switch req.Method {
		case http.MethodGet:
			if req.Body != nil {
				t.Fatal("GET unexpectedly had a body")
			}
			return testHTTPResponse(http.StatusOK, `{"sessions":[{"id":"record","project_id":"project","project_name":"app","session_id":"tab"}]}`), nil
		case http.MethodPost:
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body) != 2 || body["project_id"] != "project" || body["session_id"] != strings.Repeat("A", 24) {
				t.Fatalf("POST body = %#v; want exact snake-case fields", body)
			}
			return testHTTPResponse(http.StatusOK, `{"id":"record","project_id":"project","session_id":"AAAAAAAAAAAAAAAAAAAAAAAA","state":"running","generation":1}`), nil
		default:
			t.Fatalf("method = %s", req.Method)
			return nil, nil
		}
	})}

	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test", PairingToken: "pairing"}}
	got, err := d.fetchTerminalSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := relay.TerminalSessionInfo{ID: "record", ProjectID: "project", ProjectName: "app", SessionID: "tab"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("sessions = %#v, want %#v", got, want)
	}

	oldRandom := terminalSessionRandom
	t.Cleanup(func() { terminalSessionRandom = oldRandom })
	terminalSessionRandom = func(p []byte) (int, error) {
		for i := range p {
			p[i] = 0
		}
		return len(p), nil
	}
	// Eighteen zero bytes encode as 24 'A' characters in raw base64url.
	if info, err := d.createTerminalSession(context.Background(), "project"); err != nil || info.ID != "record" || info.SessionID != strings.Repeat("A", 24) {
		t.Fatalf("create = %+v, %v", info, err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want GET + POST", requests)
	}
}

func TestTerminalSessionCreateRetriesOnlyExplicitConflict(t *testing.T) {
	oldClient, oldRandom := httpClient, terminalSessionRandom
	t.Cleanup(func() { httpClient, terminalSessionRandom = oldClient, oldRandom })

	randomCalls := 0
	terminalSessionRandom = func(p []byte) (int, error) {
		randomCalls++
		for i := range p {
			p[i] = byte(randomCalls)
		}
		return len(p), nil
	}
	requests := 0
	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return testHTTPResponse(http.StatusConflict, `{"error":"duplicate"}`), nil
		}
		return testHTTPResponse(http.StatusOK, `{"id":"record","project_id":"project","session_id":"session","state":"running","generation":1}`), nil
	})}
	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test"}}
	info, err := d.createTerminalSession(context.Background(), "project")
	if err != nil || info.ID == "" || requests != 2 || randomCalls != 2 {
		t.Fatalf("explicit conflict: info=%+v err=%v requests=%d random=%d", info, err, requests, randomCalls)
	}

	for name, transport := range map[string]roundTripFunc{
		"ambiguous transport failure": func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") },
		"non-conflict response": func(*http.Request) (*http.Response, error) {
			return testHTTPResponse(http.StatusBadGateway, `{"error":"upstream"}`), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return transport(req)
			})}
			if _, err := d.createTerminalSession(context.Background(), "project"); err == nil {
				t.Fatal("create succeeded")
			}
			if calls != 1 {
				t.Fatalf("calls = %d, ambiguous/non-conflict failure was retried", calls)
			}
		})
	}

	randomCalls = 0
	requests = 0
	httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return testHTTPResponse(http.StatusConflict, `{"error":"duplicate"}`), nil
	})}
	if _, err := d.createTerminalSession(context.Background(), "project"); err == nil || !strings.Contains(err.Error(), "unique terminal session id") {
		t.Fatalf("conflict exhaustion error = %v", err)
	}
	if requests != 8 || randomCalls != 8 {
		t.Fatalf("conflict exhaustion requests=%d random=%d, want 8", requests, randomCalls)
	}
}

func TestTerminalSessionIDRandomFailure(t *testing.T) {
	old := terminalSessionRandom
	t.Cleanup(func() { terminalSessionRandom = old })
	terminalSessionRandom = func([]byte) (int, error) { return 0, errors.New("entropy unavailable") }
	if _, err := newTerminalSessionID(); err == nil || !strings.Contains(err.Error(), "entropy unavailable") {
		t.Fatalf("error = %v", err)
	}
}

func testHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}
