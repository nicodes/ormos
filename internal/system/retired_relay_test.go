//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRetiredRelayHelper(t *testing.T) {
	if os.Getenv("ORMOS_RETIRED_ENTRY_TEST") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			Main(os.Args[i+1:], "fixture")
			t.Fatal("retired command returned")
		}
	}
}

func TestRetiredRelayCommandsNeverReadSavedConfigOrContactAService(t *testing.T) {
	var contacted atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { contacted.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("fixture retained configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"relay"}, {"--config", path}, {"--config"}} {
		cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestRetiredRelayHelper$", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "ORMOS_RETIRED_ENTRY_TEST=1", "ORMOS_API_URL="+server.URL)
		body, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(body), "hosted relay mode is retired") {
			t.Fatalf("retired route was reachable: %v %s", err, body)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "fixture retained configuration" || contacted.Load() != 0 {
		t.Fatal("retirement touched saved state or contacted a service")
	}
}
