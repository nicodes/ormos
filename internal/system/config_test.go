package system

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// The stored relay URL is validated when the config is loaded: a scheme that
// is not ws/wss (a hand-edit, a downgrade to http://) is an error, not a URL
// to dial anyway.
func TestLoadConfigFileValidatesRelayScheme(t *testing.T) {
	write := func(t *testing.T, relayURL string) {
		t.Helper()
		path := useTempConfig(t)
		cfg := fmt.Sprintf(`{"relayUrl": %q, "pairingToken": "tok"}`, relayURL)
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("rejects non-ws schemes", func(t *testing.T) {
		write(t, "http://relay.example.com")
		if _, err := loadConfigFile(); err == nil {
			t.Fatal("an http:// relay URL must fail validation")
		}
	})
	t.Run("rejects schemeless URLs", func(t *testing.T) {
		write(t, "relay.example.com")
		if _, err := loadConfigFile(); err == nil {
			t.Fatal("a schemeless relay URL must fail validation")
		}
	})
	for _, u := range []string{"wss://api.ormos.dev", "ws://127.0.0.1:9080"} {
		t.Run("accepts "+u, func(t *testing.T) {
			write(t, u)
			cfg, err := loadConfigFile()
			if err != nil || cfg.RelayURL != u {
				t.Fatalf("loadConfigFile = %q, %v", cfg.RelayURL, err)
			}
		})
	}
}

// Cleartext to a remote relay is refused at run time, with ORMOS_INSECURE=1
// as the explicit escape hatch. Loopback cleartext (local dev) needs none.
func TestCheckRelayTransport(t *testing.T) {
	if err := checkRelayTransport("ws://relay.example.com"); err == nil {
		t.Fatal("cleartext remote relay must be refused")
	} else if !strings.Contains(err.Error(), "cleartext") {
		t.Fatalf("error = %v, want a cleartext refusal", err)
	}
	for _, u := range []string{"wss://api.ormos.dev", "ws://127.0.0.1:9080", "ws://localhost:9080"} {
		if err := checkRelayTransport(u); err != nil {
			t.Fatalf("%s must not need the escape hatch: %v", u, err)
		}
	}
	t.Setenv("ORMOS_INSECURE", "1")
	if err := checkRelayTransport("ws://relay.example.com"); err != nil {
		t.Fatalf("ORMOS_INSECURE=1 must allow a cleartext remote relay: %v", err)
	}
}
