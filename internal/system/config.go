package system

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// systemConfig holds configuration for the cli system. It is public-safe (no
// secrets baked in — the pairing token is obtained at runtime by running `ormos`
// or supplied via the environment) and lives in the cli binary so the system carries
// none of the server's config surface. The json tags are the on-disk format for
// the login-provisioned config file (Shell is resolved at runtime, not stored).
type systemConfig struct {
	RelayURL     string `json:"relayUrl"`     // ws base URL of the relay
	ClientID     string `json:"clientId"`     // stable per-config identity (generated once)
	PairingToken string `json:"pairingToken"` // pairing token minted by the relay
	SystemID     string `json:"systemId"`     // this system's system record id
	Email        string `json:"email"`        // account the system was registered under (empty for device-flow pairings)
	Shell        string `json:"-"`            // shell for terminals; from $SHELL at runtime
}

// generateClientID mints a stable identity for this config. Two systems on one
// host simply use two config files (hence two client ids), keeping them distinct.
func generateClientID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "sys_" + hex.EncodeToString(b)
}

// loadSystemConfig reads system config from the environment; the caller applies
// the saved config file on top.
func loadSystemConfig() systemConfig {
	return systemConfig{
		RelayURL: env("ORMOS_API_URL", "wss://api.ormos.dev"),
		Shell:    env("SHELL", "/bin/bash"),
	}
}

// configFileOverride is the --config path, empty when the flag is absent.
// Everything this machine keeps locally — the login config, the optional policy
// and the audit log — lives together, so pointing at a config file moves its
// siblings with it rather than splitting state across two directories.
var configFileOverride string

// ormosDir returns the directory holding config.json, policy.json and
// sessions.log: the --config file's directory, or ~/.config/ormos.
//
// Built from $HOME rather than os.UserConfigDir(), which resolves through
// XDG_CONFIG_HOME on Linux and lands in ~/Library/Application Support on macOS —
// so the same machine's state moved depending on the environment and the OS.
// This is one fixed, predictable path, relocatable only by --config.
func ormosDir() (string, error) {
	if configFileOverride != "" {
		return filepath.Dir(configFileOverride), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "ormos"), nil
}

// configPath returns the path to the login-provisioned config file.
func configPath() (string, error) {
	if configFileOverride != "" {
		return configFileOverride, nil
	}
	dir, err := ormosDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// loadConfigFile reads the saved config file. A missing file returns an error
// for which os.IsNotExist(err) is true, so callers can distinguish "not logged
// in" from a real read/parse failure. A stored relay URL that is not a
// ws:// or wss:// URL is a validation error, not a config to try anyway.
func loadConfigFile() (systemConfig, error) {
	var cfg systemConfig
	path, err := configPath()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	if cfg.RelayURL != "" {
		u, err := url.Parse(cfg.RelayURL)
		if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") {
			return systemConfig{}, fmt.Errorf("saved relay URL %q is not a ws:// or wss:// URL; fix or delete %s", cfg.RelayURL, path)
		}
	}
	return cfg, nil
}

// saveConfigFile writes the config file, creating the directory. The file holds
// the pairing token (a secret), so it is written 0600 in a 0700 directory,
// analogous to ~/.aws/credentials. The write is crash-atomic (see
// atomicWriteFile): now that #97 makes a corrupt or truncated saved config fatal
// at startup, a crash or power loss mid-write must not brick the agent.
func saveConfigFile(cfg systemConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0o600)
}

// atomicWriteFile writes data to path crash-atomically: it writes a fresh temp
// file in the same directory, fsyncs its contents, renames it into place, then
// fsyncs the directory so the rename itself survives a crash. A reader therefore
// only ever observes the complete old file or the complete new one — never a
// truncated, half-written config (which #97 makes fatal at startup).
//
// It is also symlink-safe and permission-correcting for free. The target is
// never opened for writing; we write a fresh temp file (O_CREATE|O_EXCL, random
// name, so it cannot resolve through an attacker's pre-placed symlink) and rename
// it over path. os.Rename replaces a symlink at path rather than following it, so
// a planted link can never redirect the write to another file. Because the result
// is always the freshly created temp inode, chmod'd to perm, a loosened mode on
// any prior file at path is corrected rather than inherited.
func atomicWriteFile(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// On any failure, close and remove the temp file so no partial write is left
	// behind; after a successful rename tmp no longer exists, so this is skipped.
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	// Best-effort fsync of the directory so the rename (the entry that now points
	// at the new file) is durable across a crash; failing to open it is not fatal.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// clearLoginConfig removes locally saved authentication while preserving the
// relay and stable client identity. Keeping the client id lets a later login to
// the same account re-register this machine instead of creating a duplicate.
func clearLoginConfig() error {
	cfg, err := loadConfigFile()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cfg.PairingToken = ""
	cfg.SystemID = ""
	cfg.Email = ""
	return saveConfigFile(cfg)
}

// httpBaseFromWS derives the http(s) base URL from the relay's ws(s) base, so a
// single stored relay URL serves both the WebSocket tunnel and plain HTTP calls
// (provision, ports).
func httpBaseFromWS(ws string) string {
	switch {
	case strings.HasPrefix(ws, "wss://"):
		return "https://" + strings.TrimPrefix(ws, "wss://")
	case strings.HasPrefix(ws, "ws://"):
		return "http://" + strings.TrimPrefix(ws, "ws://")
	default:
		return ws
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// isLoopbackHost reports whether the ws(s)/http(s) URL targets a loopback host,
// where plaintext is acceptable for local dev.
func isLoopbackHost(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// insecureRemoteRelay reports whether the relay URL would send traffic in
// cleartext (ws:// or http://) to a non-loopback host — i.e. account
// credentials and the pairing token would travel unencrypted.
func insecureRemoteRelay(ws string) bool {
	if strings.HasPrefix(ws, "wss://") || strings.HasPrefix(ws, "https://") {
		return false
	}
	return !isLoopbackHost(ws)
}

// checkRelayTransport refuses a relay URL that would carry the pairing token
// in cleartext to a remote host. ORMOS_INSECURE=1 is the escape hatch for
// pointing at a remote dev relay with no TLS; everything else about the URL
// (loopback cleartext, any wss) needs no opt-in.
func checkRelayTransport(ws string) error {
	if !insecureRemoteRelay(ws) || os.Getenv("ORMOS_INSECURE") == "1" {
		return nil
	}
	return fmt.Errorf("refusing a cleartext connection to remote relay %s: the pairing token would travel unencrypted — use a wss:// URL (set ORMOS_INSECURE=1 to accept the risk)", ws)
}
