//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/ormos/internal/ui"
)

// `ormos ui` is the product's screen, and its safety story is contract pins,
// stated as tests:
//
//	a. no listener on a public interface by accident -- the default bind is
//	   loopback, and the wildcard arrives only when somebody types it;
//	b. no Clerk/PocketBase/relay remnants in the web tree (source-level);
//	c. the allowlist is enforced server-side -- a request for a
//	   non-allowlisted action is refused whatever the page sent;
//	d. every read the page consumes is served from this machine's own files;
//	e. injection never reaches a spawn -- nothing the page sends passes
//	   through a shell, and cwd must resolve inside the policy's roots.

type uiFixture struct {
	srv      *uiServer
	spawnLog *strings.Builder
}

func newUIFixture(t *testing.T, roots []string) *uiFixture {
	t.Helper()
	log := &strings.Builder{}
	oldSpawn, oldPorts, oldPolicy := uiSpawnTerminal, uiListeningPorts, uiLoadPolicy
	t.Cleanup(func() { uiSpawnTerminal, uiListeningPorts, uiLoadPolicy = oldSpawn, oldPorts, oldPolicy })
	uiListeningPorts = func() ([]int, error) { return []int{5432, 3000, 9999}, nil }
	uiLoadPolicy = func() (policy, error) {
		return policy{AllowedRoots: roots, AllowedPorts: []int{3000, 9999}, DeniedPorts: []int{9999}}, nil
	}
	uiSpawnTerminal = func(shell, cwd string) (*uiTerminal, error) {
		log.WriteString(shell + "\x00" + cwd + "\n")
		term := &uiTerminal{id: "t_stub", shell: shell, cwd: cwd, started: time.Now(), alive: true}
		term.kill = func() {
			term.mu.Lock()
			term.alive = false
			term.mu.Unlock()
		}
		return term, nil
	}
	static, err := ui.Dist()
	if err != nil {
		t.Fatal(err)
	}
	return &uiFixture{
		srv:      &uiServer{version: "test", static: static, hostname: "fixture-host", terms: map[string]*uiTerminal{}},
		spawnLog: log,
	}
}

func (f *uiFixture) start(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(f.srv.routes())
	t.Cleanup(ts.Close)
	host := strings.Split(strings.TrimPrefix(ts.URL, "http://"), ":")[0]
	if !net.ParseIP(host).IsLoopback() {
		t.Fatalf("the test listener is not on loopback: %s", ts.URL)
	}
	return ts
}

func postJSON(t *testing.T, url string, body string) (int, map[string]any) {
	t.Helper()
	res, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestUIBindContract(t *testing.T) {
	if uiDefaultBind != "127.0.0.1" {
		t.Fatalf("default bind must stay loopback, got %q", uiDefaultBind)
	}
	for _, ok := range []string{"localhost", "127.0.0.1", "::1", "0.0.0.0", "192.168.1.4"} {
		if err := validateUIBind(ok); err != nil {
			t.Fatalf("validateUIBind(%q) rejected an address: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "example.com", "1.2.3.4.5", "0.0.0.0/0", "http://x"} {
		if err := validateUIBind(bad); err == nil {
			t.Fatalf("validateUIBind(%q) accepted garbage", bad)
		}
	}
}

func TestUINoDeadStackRemnants(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	banned := []string{"clerk", "pocketbase", "ormos.dev", "wss://"}
	var files []string
	dir := filepath.Join(root, "ui", "src")
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files,
		filepath.Join(root, "ui", "index.html"),
		filepath.Join(root, "ui", "package.json"),
		filepath.Join(root, "ui", "vite.config.ts"))
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(data))
		for _, needle := range banned {
			if strings.Contains(lower, needle) {
				t.Fatalf("dead-stack remnant %q found in %s", needle, path)
			}
		}
	}
}

func TestUIAllowlistEnforcedServerSide(t *testing.T) {
	fix := newUIFixture(t, nil)
	ts := fix.start(t)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"shutdown refused", `{"action":"shutdown"}`, http.StatusForbidden},
		{"shell metachar action refused", `{"action":"rm -rf /"}`, http.StatusForbidden},
		{"kill without id", `{"action":"kill"}`, http.StatusBadRequest},
		{"kill unknown id", `{"action":"kill","id":"t_nope"}`, http.StatusNotFound},
		{"traversal-looking id", `{"action":"kill","id":"../../../etc/passwd"}`, http.StatusNotFound},
		{"malformed body", `{"action":`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, out := postJSON(t, ts.URL+"/api/action", tc.body)
			if got != tc.want {
				t.Fatalf("got %d, want %d (%v)", got, tc.want, out)
			}
		})
	}
	res, err := http.Get(ts.URL + "/api/action")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != http.MethodPost {
		t.Fatalf("GET /api/action: status=%d Allow=%q", res.StatusCode, res.Header.Get("Allow"))
	}
	got, _ := postJSON(t, ts.URL+"/api/action", `{"action":"open","shell":"/bin/sh"}`)
	if got != http.StatusOK {
		t.Fatalf("allowlisted open: %d", got)
	}
	if !strings.Contains(fix.spawnLog.String(), "/bin/sh\x00") {
		t.Fatalf("spawn was not asked for the allowlisted shell: %q", fix.spawnLog.String())
	}
	got, _ = postJSON(t, ts.URL+"/api/action", `{"action":"kill","id":"t_stub"}`)
	if got != http.StatusOK {
		t.Fatalf("allowlisted kill: %d", got)
	}
}

func TestUIReadsAreLocal(t *testing.T) {
	fix := newUIFixture(t, []string{"~/code"})
	ts := fix.start(t)

	code, out := getJSON(t, ts.URL+"/api/ports")
	if code != http.StatusOK {
		t.Fatalf("ports: %d", code)
	}
	rows, _ := out["ports"].([]any)
	if len(rows) != 3 {
		t.Fatalf("ports rows: %v", rows)
	}
	got := map[int]bool{}
	for _, row := range rows {
		m := row.(map[string]any)
		got[int(m["port"].(float64))] = m["allowed"].(bool)
	}
	if !got[3000] || got[5432] || got[9999] {
		t.Fatalf("policy filtering wrong: %v", got)
	}

	code, out = getJSON(t, ts.URL+"/api/system")
	if code != http.StatusOK {
		t.Fatalf("system: %d", code)
	}
	if out["version"] != "test" || out["hostname"] != "fixture-host" {
		t.Fatalf("system identity: %v", out)
	}
	roots, _ := out["allowedRoots"].([]any)
	if len(roots) != 1 || roots[0] != "~/code" {
		t.Fatalf("allowedRoots: %v", out["allowedRoots"])
	}

	dir := t.TempDir()
	logPath := filepath.Join(dir, auditFileName)
	lines := []string{
		`{"time":"2026-09-19T01:00:00Z","event":"list-ports","allowed":true}`,
		`{"time":"2026-09-19T01:01:00Z","event":"terminal","detail":"relay asked","allowed":false}`,
		`{"time":"2026-09-19T01:02:00Z","event":"proxy","port":3000,"allowed":true}`,
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldOverride := configFileOverride
	configFileOverride = filepath.Join(dir, "config.json")
	t.Cleanup(func() { configFileOverride = oldOverride })
	code, out = getJSON(t, ts.URL+"/api/audit")
	if code != http.StatusOK {
		t.Fatalf("audit: %d", code)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 3 {
		t.Fatalf("audit entries: %v", entries)
	}
	if entries[1].(map[string]any)["event"] != "terminal" {
		t.Fatalf("audit order: %v", entries)
	}
}

func TestUIInjectionNeverReachesSpawn(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	fix := newUIFixture(t, []string{root})
	ts := fix.start(t)
	refused := []struct {
		name string
		body string
	}{
		{"shell not allowlisted", `{"action":"open","shell":"/bin/rm"}`},
		{"shell with arguments", `{"action":"open","shell":"/bin/sh -c id"}`},
		{"cwd traversal", `{"action":"open","shell":"/bin/sh","cwd":"` + sub + `/../../etc"}`},
		{"cwd symlink escape", `{"action":"open","shell":"/bin/sh","cwd":"` + link + `"}`},
		{"cwd nonexistent", `{"action":"open","shell":"/bin/sh","cwd":"` + filepath.Join(root, "nope") + `"}`},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := postJSON(t, ts.URL+"/api/action", tc.body)
			if got != http.StatusBadRequest {
				t.Fatalf("got %d, want 400", got)
			}
		})
	}
	if fix.spawnLog.Len() != 0 {
		t.Fatalf("a refused request still spawned: %q", fix.spawnLog.String())
	}
	got, _ := postJSON(t, ts.URL+"/api/action", `{"action":"open","shell":"/bin/sh","cwd":"`+sub+`"}`)
	if got != http.StatusOK {
		t.Fatalf("policy-rooted cwd refused: %d", got)
	}
	// The server logs the RESOLVED path; on darwin /var is a symlink to
	// /private/var, so the assertion compares resolutions, not spellings.
	resolvedSub, err := filepath.EvalSymlinks(sub)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fix.spawnLog.String(), "\x00"+resolvedSub) {
		t.Fatalf("spawn got an unresolved cwd: %q", fix.spawnLog.String())
	}
}

func TestUIServeOverLoopback(t *testing.T) {
	fix := newUIFixture(t, nil)
	ts := fix.start(t)

	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 4096)
	n, _ := res.Body.Read(body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body[:n]), "ormos") {
		t.Fatalf("index: status=%d", res.StatusCode)
	}

	var asset string
	entries, err := fs.ReadDir(fix.srv.static, "assets")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".js") {
			asset = "assets/" + e.Name()
		}
	}
	if asset == "" {
		t.Fatal("no js asset in the embedded dist")
	}
	res, err = http.Get(ts.URL + "/" + asset)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("asset %s: status=%d type=%q", asset, res.StatusCode, res.Header.Get("Content-Type"))
	}

	code, _ := getJSON(t, ts.URL+"/api/definitely-not-a-route")
	if code != http.StatusNotFound {
		t.Fatalf("unknown api route: %d", code)
	}

	res, err = http.Get(ts.URL + "/projects")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("SPA fallback: %d", res.StatusCode)
	}
}

func TestUITerminalListAndOutput(t *testing.T) {
	fix := newUIFixture(t, nil)
	ts := fix.start(t)
	term := &uiTerminal{id: "t_live", shell: "/bin/sh", cwd: "/", started: time.Now(), alive: true}
	term.append([]byte("hello from pty"))
	fix.srv.terms["t_live"] = term

	code, out := getJSON(t, ts.URL+"/api/terminals")
	if code != http.StatusOK {
		t.Fatalf("terminals: %d", code)
	}
	rows, _ := out["terminals"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["id"] != "t_live" {
		t.Fatalf("terminals rows: %v", rows)
	}

	code, out = getJSON(t, ts.URL+"/api/terminal/t_live/output")
	if code != http.StatusOK || out["output"] != "hello from pty" {
		t.Fatalf("output: %d %v", code, out)
	}

	code, _ = getJSON(t, ts.URL+"/api/terminal/t_missing/output")
	if code != http.StatusNotFound {
		t.Fatalf("missing terminal output: %d", code)
	}
}

func TestUITerminalLimit(t *testing.T) {
	fix := newUIFixture(t, nil)
	ts := fix.start(t)
	oldMax := uiMaxTerminals
	uiMaxTerminals = 1
	t.Cleanup(func() { uiMaxTerminals = oldMax })
	if code, _ := postJSON(t, ts.URL+"/api/action", `{"action":"open","shell":"/bin/sh"}`); code != http.StatusOK {
		t.Fatalf("first open: %d", code)
	}
	if code, _ := postJSON(t, ts.URL+"/api/action", `{"action":"open","shell":"/bin/sh"}`); code != http.StatusTooManyRequests {
		t.Fatalf("second open: %d, want 429", code)
	}
	fix.srv.mu.Lock()
	n := len(fix.srv.terms)
	fix.srv.mu.Unlock()
	if n != 1 {
		t.Fatalf("terminals after limit: %d", n)
	}
}

func TestUIPortsAreSorted(t *testing.T) {
	fix := newUIFixture(t, nil)
	ts := fix.start(t)
	_, out := getJSON(t, ts.URL+"/api/ports")
	rows, _ := out["ports"].([]any)
	var ports []int
	for _, row := range rows {
		ports = append(ports, int(row.(map[string]any)["port"].(float64)))
	}
	if !sort.IntsAreSorted(ports) {
		t.Fatalf("ports not sorted: %v", ports)
	}
}
