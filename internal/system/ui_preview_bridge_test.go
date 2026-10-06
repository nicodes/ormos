//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUIPreviewSelectorKeepsLocalPath(t *testing.T) {
	fix := newUIFixture(t, nil)
	server := httptest.NewServer(fix.srv.previewRoutes())
	defer server.Close()
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	for _, path := range []string{"/about?tab=two#section", "//evil.example", "/\\evil.example", "https://evil.example"} {
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/__ormos_preview/3000/", nil)
		query := req.URL.Query()
		query.Set("path", path)
		req.URL.RawQuery = query.Encode()
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if path == "/about?tab=two#section" {
			if res.StatusCode != 302 || res.Header.Get("Location") != path {
				t.Fatalf("redirect: %d %q", res.StatusCode, res.Header.Get("Location"))
			}
		} else if res.StatusCode != 400 {
			t.Fatalf("foreign path %q: %d", path, res.StatusCode)
		}
	}
}

func TestUIPreviewBridgeLeavesAssetsAndLargeBodiesIntact(t *testing.T) {
	for _, tc := range []struct {
		name, kind, encoding, body string
		want                       bool
	}{
		{"html", "text/html; charset=utf-8", "", "<!doctype html><html><head><title>Preview</title></head><body>Hello</body></html>", true},
		{"quoted head", "text/html", "", `<head data-example=">"><title>Quoted</title></head><body>Hello</body>`, true},
		{"nonce", "text/html", "", "<head><script nonce=\"nonce-value\">window.test=true</script></head>", true},
		{"asset", "text/javascript", "", "const test=1;", false},
		{"compressed", "text/html", "gzip", "compressed bytes", false},
		{"large", "text/html", "", strings.Repeat("x", previewHTMLLimit+10), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body)), ContentLength: int64(len(tc.body))}
			res.Header.Set("Content-Type", tc.kind)
			res.Header.Set("Content-Encoding", tc.encoding)
			res.Header.Set("ETag", "original")
			if err := injectPreviewBridge(res); err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if tc.want {
				if !bytes.Contains(body, []byte(`<script src="/__ormos_bridge.js"`)) {
					t.Fatal("missing bridge")
				}
				if res.ContentLength != int64(len(body)) || res.Header.Get("ETag") != "" {
					t.Fatal("stale response metadata")
				}
				if tc.name == "quoted head" && !bytes.Contains(body, []byte(`<head data-example=">"><script`)) {
					t.Fatal("head attribute corrupted")
				}
				if tc.name == "nonce" && !bytes.Contains(body, []byte(`src="/__ormos_bridge.js" nonce="nonce-value"`)) {
					t.Fatal("nonce not reused")
				}
			} else if string(body) != tc.body || res.Header.Get("ETag") != "original" {
				t.Fatal("passthrough changed")
			}
		})
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
