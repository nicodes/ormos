package system

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicodes/ormos/relay"
)

func TestExpandHomeExpandsTildeAndEnv(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	t.Setenv("ORMOS_TEST_ROOT", "code")
	if got := expandHome("~"); got != home {
		t.Fatalf("expandHome(~) = %q, want %q", got, home)
	}
	if got := expandHome("~/app"); got != filepath.Join(home, "app") {
		t.Fatalf("expandHome(~/app) = %q", got)
	}
	// Local policy configuration keeps $VAR expansion.
	if got := expandHome("~/$ORMOS_TEST_ROOT/app"); got != filepath.Join(home, "code", "app") {
		t.Fatalf("expandHome(~/$ORMOS_TEST_ROOT/app) = %q", got)
	}
}

// The relay may ask for any directory string; what it may not do is have this
// machine's environment expanded for it. "$SECRET/work" answering differently
// from a literal path tells the relay whether SECRET is set, and to what.
func TestExpandRelayCwdRejectsEnvVars(t *testing.T) {
	t.Setenv("ORMOS_TEST_ROOT", "code")
	for _, p := range []string{"$ORMOS_TEST_ROOT/app", "~/work/$ORMOS_TEST_ROOT", "/opt/$ORMOS_TEST_ROOT", "${ORMOS_TEST_ROOT}"} {
		if _, err := expandRelayCwd(p); err == nil {
			t.Fatalf("expandRelayCwd(%q) must refuse a residual $", p)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	for p, want := range map[string]string{
		"~":          home,
		"~/work/app": filepath.Join(home, "work", "app"),
		"/opt/app":   "/opt/app",
		"relative":   "relative",
	} {
		got, err := expandRelayCwd(p)
		if err != nil || got != want {
			t.Fatalf("expandRelayCwd(%q) = %q, %v; want %q", p, got, err, want)
		}
	}
}

// End to end through the terminal handler: a cwd the relay could use to probe
// the environment is refused, not silently downgraded to the default
// directory — a shell in the default directory is still a shell.
func TestTerminalRefusesDollarCwd(t *testing.T) {
	withTempConfigDir(t) // no policy file: everything would otherwise be allowed
	d := &system{terminals: make(map[string]*terminalSession), audit: newAuditor()}
	header := relay.StreamHeader{Kind: relay.KindTerminal, SessionID: "probe", Cwd: "$HOME", Cols: 80, Rows: 24}
	if _, err := d.terminal(header); err == nil || !strings.Contains(err.Error(), "$") {
		t.Fatalf("terminal($HOME) error = %v, want a refusal", err)
	}
	if d.terminals["probe"] != nil {
		t.Fatal("a refused cwd must not leave a session behind")
	}
}
