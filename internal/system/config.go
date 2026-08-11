package system

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	cfg, _, err := loadConfigFileChecked()
	return cfg, err
}

// loadConfigFileChecked is loadConfigFile plus any warning about the state it
// found on disk, for the one caller that has somewhere to put it.
//
// The warning matters as much here as it does for the key: config.json holds
// the pairing token, which is a bearer credential for the relay. Tightening the
// mode does not un-leak it, so whoever was able to read it can still use it
// until the machine is re-paired — and the operator has to be told that, not
// have it fixed silently.
func loadConfigFileChecked() (systemConfig, string, error) {
	var cfg systemConfig
	path, err := configPath()
	if err != nil {
		return cfg, "", err
	}
	// Through openPrivateFile so a config.json loosened out of band is tightened
	// on the read, not only the next time something happens to save it. It holds
	// the pairing token, and saves are rare: a login, a logout, and nothing else
	// for the life of the install.
	f, dirWarning, fileWarning, err := openPrivateFile(path)
	warning := dirWarning
	if fileWarning != "" {
		// The remedy belongs only to the file: tightening the mode does not
		// un-leak a bearer token.
		fileWarning += " — the pairing token in it may already have been copied; sign out and pair again to mint a new one"
		warning = strings.TrimPrefix(warning+"; "+fileWarning, "; ")
	}
	if err != nil {
		return cfg, warning, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfigFileSize))
	if err != nil {
		return cfg, warning, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, warning, err
	}
	if cfg.RelayURL != "" {
		u, err := url.Parse(cfg.RelayURL)
		if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") {
			return systemConfig{}, warning, fmt.Errorf("saved relay URL %q is not a ws:// or wss:// URL; fix or delete %s", cfg.RelayURL, path)
		}
	}
	return cfg, warning, nil
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

// maxConfigFileSize bounds the read of config.json. The real file is a few
// hundred bytes; this only has to be finite, so a config replaced by something
// enormous is a failed parse rather than an out-of-memory at startup.
const maxConfigFileSize = 1 << 20

// openPrivateFile opens one of the agent's private files for reading: the
// config holding the pairing token, or the terminal sealing key.
//
// Both are written 0600 into a 0700 directory. But a mode set at creation is a
// mode nobody looks at again, and a file restored from a backup, copied by hand
// or caught by a stray chmod stays readable by every local user for the life of
// the install, silently. atomicWriteFile corrects that on every write and says
// so; this is the same invariant applied on every read, which for identity.key
// is the only one it ever gets — that file is written once and read forever.
//
// Three checks, in the order in which their failures matter.
//
// O_NOFOLLOW, because a plain open follows symlinks. Without it a link planted
// at the path redirects both the read and the chmod below onto a file of
// someone else's choosing — the read adopts its contents, and the chmod is
// applied to a file that was never ours. atomicWriteFile is symlink-safe by
// construction (a fresh O_EXCL temp, renamed over the path); an open is not, so
// it has to say so itself.
//
// Ownership, because a file this user does not own is not this machine's file.
// For identity.key that is decisive rather than tidy: adopting a planted key
// publishes the planter's public half on the system's record, and every
// terminal is then sealed to a key they hold. This fails closed. It is the one
// condition here that is better answered by refusing to start than by warning
// and carrying on.
//
// Mode, last, because it is the recoverable one. Group and other bits are
// cleared through the open descriptor, so a path swapped between the stat and
// the fix cannot redirect it. Only those bits: forcing 0600 would grant write
// access to a file the owner had deliberately made read-only, which is a
// loosening dressed up as a fix.
//
// Corrections are returned as warnings rather than printed, because the right
// place to put them depends on the caller — stderr is wiped by the TUI's alt
// screen a moment later, so the agent puts them in the log ring as well. The
// directory's and the file's are returned separately: only the file's earns a
// remedy like "delete it and re-pair", and welding that onto a directory
// correction told the operator to delete a key that had never existed.
func openPrivateFile(path string) (f *os.File, dirWarning, fileWarning string, err error) {
	// The directory first, so both read paths get it rather than only the key's.
	// A group- or world-WRITABLE state directory defeats every check below it:
	// an attacker who cannot read a 0600 file can still unlink it and leave
	// their own in its place, which is the planted-file case with the right
	// owner already on it.
	dirWarning = hardenOrmosDir()

	// O_NONBLOCK because O_NOFOLLOW does not save us from a FIFO: opening a
	// named pipe for reading BLOCKS until a writer appears, and the regular-file
	// check below cannot run until the open returns. Without this, a fifo at
	// this path wedges the agent at startup with nothing printed. It is a no-op
	// on a regular file.
	f, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, dirWarning, "", fmt.Errorf("%s is a symbolic link; refusing to follow it (move the real file into place, or point --config at it)", path)
		}
		return nil, dirWarning, "", err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, dirWarning, "", err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, dirWarning, "", fmt.Errorf("%s is not a regular file", path)
	}
	if owner, ok := fileOwner(st); ok && owner != os.Getuid() {
		f.Close()
		return nil, dirWarning, "", fmt.Errorf("%s is owned by uid %d, not by this user (uid %d); refusing to use it", path, owner, os.Getuid())
	}
	perm := st.Mode().Perm()
	if perm&0o077 == 0 {
		return f, dirWarning, "", nil
	}
	want := perm &^ 0o077
	if err := f.Chmod(want); err != nil {
		return f, dirWarning, fmt.Sprintf("%s is mode %04o, readable by other local users, and its mode could not be corrected: %v", path, perm, err), nil
	}
	return f, dirWarning, fmt.Sprintf("%s was mode %04o, readable by other local users; corrected to %04o", path, perm, want), nil
}

// fileOwner returns the uid owning fi, and whether it could be determined.
//
// A var, not a func, so a test can force a mismatch. Creating a file owned by
// somebody else needs a second uid, which CI does not have — and without this
// seam the one check here that FAILS CLOSED would be the only one with no test
// behind it.
var fileOwner = func(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// hardenOrmosDir clears group and other bits from the state directory, for the
// same reason openPrivateFile does it to the files inside.
//
// MkdirAll sets 0700 at creation and is a no-op on a directory that already
// exists, whatever its mode — so this was the hole underneath the file checks.
// A group- or world-WRITABLE ~/.config/ormos defeats them entirely: an attacker
// who cannot read a 0600 identity.key can still unlink it and put their own
// there, which is the planted-key case openPrivateFile refuses, handed to them
// with the right owner on it.
//
// Only ever applied to the default ~/.config/ormos, which is a directory this
// agent creates and owns the meaning of. Under --config the state directory is
// filepath.Dir of whatever path was given, which can be $HOME or any other
// directory the agent has no business rewriting the mode of: `ormos --config
// ~/config.json` would silently turn $HOME into 0700 on every start, breaking
// ~/public_html and anything else that depends on being reachable. The read is
// never refused on the directory's mode; only the correction is gated, so
// under --config a loose directory is left exactly as it is.
//
// Best-effort and silent about the ordinary case: it returns a warning only
// when it had to change something.
func hardenOrmosDir() string {
	if configFileOverride != "" {
		return ""
	}
	dir, err := ormosDir()
	if err != nil {
		return ""
	}
	// Through a descriptor, for the same reason openPrivateFile does it to the
	// files: a path-based stat followed by a path-based chmod is two operations
	// on a name, and the name can be swapped between them. O_DIRECTORY refuses
	// anything that is not a directory, O_NOFOLLOW refuses a symlink standing
	// where the state directory should be.
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.IsDir() {
		return ""
	}
	perm := st.Mode().Perm()
	if perm&0o077 == 0 {
		return ""
	}
	want := perm &^ 0o077
	if err := f.Chmod(want); err != nil {
		return fmt.Sprintf("%s is mode %04o, reachable by other local users, and its mode could not be corrected: %v", dir, perm, err)
	}
	return fmt.Sprintf("%s was mode %04o, reachable by other local users; corrected to %04o", dir, perm, want)
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
		// The config cannot be read — it is a symlink, a fifo, owned by
		// somebody else, or too corrupt to parse. Signing out must still work: this is exactly the state
		// the README tells the user to fix, and the file being refused is the
		// one holding the token they are trying to revoke. Removing it revokes
		// the token locally, which is what sign-out is for. The client id is
		// lost with it, so the next login registers a new system rather than
		// re-registering this one; that is a far smaller problem than being
		// unable to sign out at all.
		path, perr := configPath()
		if perr != nil {
			return err
		}
		if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
			return err
		}
		return nil
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
