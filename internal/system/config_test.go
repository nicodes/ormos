//go:build unix

package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

// A crash or power loss mid-write must not leave a truncated, half-written
// config — #97 makes such a file fatal at startup, which would brick the agent.
// The write is crash-atomic (temp + fsync + rename), so a failed save leaves the
// previous complete config untouched rather than a partial one. We force the
// save to fail after the old file exists by making its directory unwritable, so
// the temp file cannot be created, then assert the old config survived intact and
// no temp litter was left behind.
func TestSaveConfigFileFailedWritePreservesOldConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so the write cannot be forced to fail")
	}
	path := useTempConfig(t)
	dir := filepath.Dir(path)

	old := systemConfig{RelayURL: "wss://old.example.test", PairingToken: "old_secret"}
	if err := saveConfigFile(old); err != nil {
		t.Fatal(err)
	}
	oldBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := saveConfigFile(systemConfig{RelayURL: "wss://new.example.test", PairingToken: "new_secret"}); err == nil {
		t.Fatal("save into an unwritable directory must fail")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	gotBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBytes) != string(oldBytes) {
		t.Fatalf("failed save changed the config on disk:\n got %s\nwant %s", gotBytes, oldBytes)
	}
	cfg, err := loadConfigFile()
	if err != nil || cfg.PairingToken != "old_secret" {
		t.Fatalf("old config not recoverable after failed save: cfg=%#v err=%v", cfg, err)
	}
	assertNoTempLitter(t, dir)
}

// The save writes a fresh temp file and renames it over the target, so a symlink
// planted at the config path is replaced, never followed — a planted link can't
// redirect the secret-bearing write to another file the attacker can read.
func TestSaveConfigFileReplacesSymlinkInsteadOfFollowing(t *testing.T) {
	path := useTempConfig(t)
	dir := filepath.Dir(path)

	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("DO NOT TOUCH"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}

	cfg := systemConfig{RelayURL: "wss://relay.example.test", PairingToken: "tok"}
	if err := saveConfigFile(cfg); err != nil {
		t.Fatal(err)
	}

	if got, err := os.ReadFile(victim); err != nil || string(got) != "DO NOT TOUCH" {
		t.Fatalf("symlink target was followed and overwritten: got %q, err %v", got, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("config path is still a symlink; the write followed it instead of replacing it")
	}
	loaded, err := loadConfigFile()
	if err != nil || loaded.PairingToken != "tok" {
		t.Fatalf("config not written to the real path: %#v, err %v", loaded, err)
	}
	assertNoTempLitter(t, dir)
}

// The result always lands as a fresh 0600 file even when a prior file at the path
// had a looser mode, so a hand-loosened config can't leak the pairing token.
func TestSaveConfigFileCorrectsLoosePermissions(t *testing.T) {
	path := useTempConfig(t)
	if err := os.WriteFile(path, []byte("{}"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := saveConfigFile(systemConfig{RelayURL: "wss://relay.example.test", PairingToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %o, want 600 (loose mode not corrected)", info.Mode().Perm())
	}
}

// assertNoTempLitter fails if the save left any temp file behind in dir; only the
// config file (and, in the symlink test, the victim) should remain.
func assertNoTempLitter(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
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

// config.json holds the pairing token — a bearer credential for the relay — so
// it gets the same treatment identity.key does. Nothing covered this: the whole
// self-heal could be reverted to a plain os.ReadFile and the suite stayed green.
func TestLoosenedConfigModeIsCorrectedOnRead(t *testing.T) {
	dir := withTempConfigDir(t)
	if err := saveConfigFile(systemConfig{RelayURL: "wss://relay.example", PairingToken: "t"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, warning, err := loadConfigFileChecked()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PairingToken != "t" {
		t.Errorf("the config did not survive the correction: %+v", cfg)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("config.json left at %04o; the pairing token is still readable by other local users", perm)
	}
	// Tightening the mode does not un-leak a bearer token, so the operator has
	// to be told the remedy rather than have it fixed silently.
	if !contains(warning, "sign out and pair again") {
		t.Errorf("the correction was not reported with its remedy: %q", warning)
	}
}

// A symlink at config.json must not be followed: it would redirect the read and
// the mode correction onto a file of someone else's choosing.
func TestConfigSymlinkIsRefused(t *testing.T) {
	dir := withTempConfigDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "planted.json")
	if err := os.WriteFile(elsewhere, []byte(`{"relayUrl":"wss://evil.example.test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, "config.json")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	if _, err := loadConfigFile(); err == nil {
		t.Fatal("a symlink at config.json was followed")
	}
}

// A directory (or any non-regular file) at config.json is refused rather than
// chmod'd and reported as a loosened config.
func TestConfigPathMustBeARegularFile(t *testing.T) {
	dir := withTempConfigDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "config.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfigFile()
	if err == nil {
		t.Fatal("a directory at config.json was accepted")
	}
	if !contains(err.Error(), "regular file") {
		t.Errorf("the error should say what is wrong, got: %v", err)
	}
}

// The one check here that fails CLOSED. A file owned by someone else is a
// planted file: for identity.key, adopting one publishes the planter's public
// half and seals every terminal to a key they hold. Creating a file owned by
// another uid needs a second uid, which CI does not have — so fileOwner is a
// var, and this forces the mismatch.
func TestForeignOwnedPrivateFileIsRefused(t *testing.T) {
	dir := withTempConfigDir(t)
	newSystem(systemConfig{}) // creates identity.key owned by us

	real := fileOwner
	t.Cleanup(func() { fileOwner = real })
	fileOwner = func(os.FileInfo) (int, bool) { return os.Getuid() + 1, true }

	_, _, err := loadOrCreateKey()
	if err == nil {
		t.Fatal("a key file owned by another user was adopted")
	}
	for _, want := range []string{"owned by uid", "refusing to use it"} {
		if !contains(err.Error(), want) {
			t.Errorf("the error should say why, got: %v", err)
		}
	}
	// And it must not have been rewritten or removed on the way out.
	if _, err := os.Stat(filepath.Join(dir, keyFileName)); err != nil {
		t.Errorf("the refused key file was disturbed: %v", err)
	}
}

// O_NOFOLLOW does not save us from a FIFO: opening a named pipe for reading
// BLOCKS until a writer appears, and the regular-file refusal cannot run until
// the open returns. Without O_NONBLOCK the agent wedges at startup with nothing
// printed — a hang, not an error, which is the worst shape a failure can take.
// The timeout is what makes this a real test: it fails by not finishing.
func TestFifoAtAPrivatePathDoesNotHang(t *testing.T) {
	dir := withTempConfigDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create a fifo here: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := loadConfigFile()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a fifo at config.json was accepted")
		}
		if !contains(err.Error(), "regular file") {
			t.Errorf("the error should say what is wrong, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loadConfigFile blocked on a fifo; startup would hang with nothing printed")
	}
}

// The remedy belongs to the file, not to the directory around it. Welding them
// together told the operator, on a first run with no key on disk at all, to
// "delete it to generate a new one and re-pair this machine" — about a key that
// had never existed and a machine that had never been paired.
func TestDirectoryWarningCarriesNoFileRemedy(t *testing.T) {
	dir := withTempHome(t)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	// No key on disk: the only correction possible is the directory's.
	_, warnings, err := loadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 {
		t.Fatalf("want exactly the directory warning, got %q", warnings)
	}
	if !contains(warnings[0], "reachable by other local users") {
		t.Errorf("the directory warning is missing: %q", warnings[0])
	}
	for _, forbidden := range []string{"delete it", "re-pair", "already have been copied"} {
		if contains(warnings[0], forbidden) {
			t.Errorf("the directory warning carries a file remedy (%q): %q", forbidden, warnings[0])
		}
	}
}

// Signing out must work in exactly the state the README tells the user to fix.
// The file being refused is the one holding the token they are trying to
// revoke, so refusing to read it must not mean refusing to revoke it.
func TestSignOutWorksWithAnUnreadableConfig(t *testing.T) {
	dir := withTempConfigDir(t)
	path := filepath.Join(dir, "config.json")
	elsewhere := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(elsewhere, []byte(`{"pairingToken":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	if err := clearLoginConfig(); err != nil {
		t.Fatalf("sign-out refused with an unreadable config: %v", err)
	}
	// The local pairing must be gone.
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Error("the config was left in place, so the machine is still paired locally")
	}
}

// A config that exists but cannot be read is NOT the same as no config:
// swallowing the read error minted a fresh client id and silently registered
// a duplicate machine on the account instead of re-registering this one.
func TestLoginRefusesAnUnreadableConfig(t *testing.T) {
	dir := withTempConfigDir(t)
	path := filepath.Join(dir, "config.json")
	elsewhere := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(elsewhere, []byte(`{"pairingToken":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	_, err := performLogin(context.Background(), "wss://relay.example.test")
	if err == nil {
		t.Fatal("login proceeded past an unreadable config; it would have minted a duplicate machine")
	}
	if !contains(err.Error(), "reading the saved config") {
		t.Errorf("the error should name what failed, got: %v", err)
	}
}

// The same remedy separation as the key path: a corrected directory must not
// carry the remedy for a copied pairing token — on a first run there is no
// token, and the operator would be told to re-pair a machine that was never
// paired.
func TestConfigDirectoryWarningCarriesNoTokenRemedy(t *testing.T) {
	dir := withTempHome(t)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	// No config on disk: the only correction possible is the directory's.
	_, warning, err := loadConfigFileChecked()
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if !contains(warning, "reachable by other local users") {
		t.Errorf("the directory warning is missing: %q", warning)
	}
	for _, forbidden := range []string{"pairing token", "sign out and pair again", "already have been copied"} {
		if contains(warning, forbidden) {
			t.Errorf("the directory warning carries a file remedy (%q): %q", forbidden, warning)
		}
	}
}
