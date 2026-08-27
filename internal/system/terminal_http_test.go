//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/ormos/relay"
)

func TestTerminalExitReportRetriesExactGeneration(t *testing.T) {
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
	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test"}, resetDone: true}
	started := time.Now()
	d.reportTerminalExit("retry-record-unique", 17)
	select {
	case got := <-seen:
		if elapsed := time.Since(started); elapsed >= time.Second {
			t.Fatalf("focused retry took %s, want under 1s", elapsed)
		}
		if got != 17 {
			t.Fatalf("generation=%d", got)
		}
	case <-time.After(900 * time.Millisecond):
		t.Fatal("exit report did not retry")
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

	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test", PairingToken: "pairing"}, resetDone: true}
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

func TestTerminalMutationGenerationWireShape(t *testing.T) {
	oldClient := httpClient
	t.Cleanup(func() { httpClient = oldClient })
	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if req.Method == http.MethodPost || req.Method == http.MethodDelete {
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
		}
		if req.Method == http.MethodDelete && req.URL.Path == "/system/terminal-sessions/record" {
			if len(body) != 1 || body["generation"] != float64(4) {
				t.Fatalf("delete body=%#v", body)
			}
			return testHTTPResponse(http.StatusNoContent, ""), nil
		}
		if req.Method == http.MethodGet && req.URL.Path == "/system/terminal-sessions" {
			return testHTTPResponse(http.StatusOK, `{"sessions":[]}`), nil
		}
		return testHTTPResponse(http.StatusNotFound, ""), nil
	})}
	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test"}, resetDone: true, terminals: make(map[string]*terminalSession)}
	if err := d.deleteTerminalSession(context.Background(), "record", 4); err != nil {
		t.Fatal(err)
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
		var body struct {
			SessionID string `json:"session_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		return testHTTPResponse(http.StatusOK, `{"id":"record","project_id":"project","session_id":"`+body.SessionID+`","state":"running","generation":1}`), nil
	})}
	d := &system{cfg: systemConfig{RelayURL: "ws://relay.test"}, resetDone: true}
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

func TestCreateTerminalSessionRequiresRequestedIdentity(t *testing.T) {
	oldClient, oldRandom := httpClient, terminalSessionRandom
	t.Cleanup(func() { httpClient, terminalSessionRandom = oldClient, oldRandom })
	terminalSessionRandom = func(p []byte) (int, error) { clear(p); return len(p), nil }
	for name, body := range map[string]string{
		"wrong project": `{"id":"record","project_id":"other","session_id":"AAAAAAAAAAAAAAAAAAAAAAAA","state":"running","generation":1}`,
		"wrong session": `{"id":"record","project_id":"project","session_id":"other","state":"running","generation":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return testHTTPResponse(http.StatusOK, body), nil
			})}
			d := &system{cfg: systemConfig{RelayURL: "ws://relay.test"}, resetDone: true}
			if _, err := d.createTerminalSession(context.Background(), "project"); err == nil {
				t.Fatal("create accepted a response for a different identity")
			}
		})
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
