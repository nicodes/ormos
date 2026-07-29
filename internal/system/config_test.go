package system

import (
	"os"
	"path/filepath"
	"testing"
)

// useTempConfig points the whole local state at a temp dir for one test. It
// must be --config, not an environment variable: nothing in the environment
// relocates it any more, so a test that tried would write to the developer's
// real ~/.config/ormos.
func useTempConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	prev := configFileOverride
	configFileOverride = path
	t.Cleanup(func() { configFileOverride = prev })
	return path
}

// The config file holds the pairing token, which is a shell on this machine, so
// it must never be group- or world-readable.
func TestSaveConfigFileRoundTripsAndIsOwnerOnly(t *testing.T) {
	useTempConfig(t)
	want := systemConfig{
		RelayURL:     "wss://relay.example.test",
		ClientID:     "sys_stable",
		PairingToken: "ormos_secret",
		SystemID:     "system_id",
		Email:        "old@example.test",
	}
	if err := saveConfigFile(want); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayURL != want.RelayURL || got.ClientID != want.ClientID ||
		got.PairingToken != want.PairingToken || got.SystemID != want.SystemID ||
		got.Email != want.Email {
		t.Fatalf("round trip changed the config: %#v", got)
	}

	path, err := configPath()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %o, want 600", info.Mode().Perm())
	}
}

// A machine with no saved config is simply not registered yet; that must be
// distinguishable from a real read failure so the caller can log in instead.
func TestLoadConfigFileMissingIsNotExist(t *testing.T) {
	configFileOverride = filepath.Join(t.TempDir(), "missing", "config.json")
	t.Cleanup(func() { configFileOverride = "" })
	if _, err := loadConfigFile(); !os.IsNotExist(err) {
		t.Fatalf("missing config error = %v, want an IsNotExist error", err)
	}
}

func TestRelayURLDefaultAndEnvironmentOverride(t *testing.T) {
	t.Setenv("ORMOS_API_URL", "")
	if got := loadSystemConfig().RelayURL; got != "wss://api.ormos.dev" {
		t.Fatalf("default relay = %q, want production relay", got)
	}

	t.Setenv("ORMOS_API_URL", "ws://127.0.0.1:9080")
	if got := loadSystemConfig().RelayURL; got != "ws://127.0.0.1:9080" {
		t.Fatalf("relay override = %q, want local relay", got)
	}
}
