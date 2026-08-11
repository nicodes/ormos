package system

import (
	"crypto/ecdh"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nicodes/ormos/relay"
)

// keyFileName holds this machine's long-lived X25519 private key, beside the
// config and the audit log.
const keyFileName = "identity.key"

// maxKeyFileSize bounds the read of the key file. An X25519 private key is 32
// bytes, so this is enormous; it only has to be finite, so a key file that has
// been replaced by something the size of a disk is a failed parse rather than
// an out-of-memory at startup. It is deliberately nowhere near 32 — a cap that
// could truncate a longer file down to exactly key length would turn garbage
// into an accepted key.
const maxKeyFileSize = 4 << 10

// loadOrCreateKey returns this machine's terminal key, generating it on first
// use.
//
// The key is per-machine rather than per-session because the browser has to
// learn the public half before it can speak: it reads it from the system's
// record, which the agent publishes when it connects. A per-session key would
// need a round trip before the first keystroke, which is the trade recorded in
// relay/seal.go — the browser's half of the agreement is ephemeral, this half
// is not, so a compromise of this file exposes past sessions.
//
// That is why it is written 0600, has that mode rechecked and corrected on
// every read (see hardenKeyFile), and never leaves the machine. It is not a
// credential — it cannot be presented to the server to gain anything, and
// losing it costs nothing but a reconnect — but it is the reason a terminal is
// private, so it is treated like one.
func loadOrCreateKey() (*ecdh.PrivateKey, error) {
	dir, err := ormosDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, keyFileName)

	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		hardenKeyFile(f, path)
		raw, err := io.ReadAll(io.LimitReader(f, maxKeyFileSize))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		key, err := ecdh.X25519().NewPrivateKey(raw)
		if err != nil {
			// A corrupt key would fail every handshake with no useful message.
			// Say so plainly; deleting the file regenerates it.
			return nil, fmt.Errorf("reading %s: %w (delete it to generate a new one)", path, err)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	key, err := relay.GenerateAgentKey()
	if err != nil {
		return nil, fmt.Errorf("generating terminal key: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// 0600 from the moment it exists: writing world-readable and chmod-ing
	// after leaves a window in which another local user can read it.
	if err := os.WriteFile(path, key.Bytes(), 0o600); err != nil {
		return nil, fmt.Errorf("writing %s: %w", path, err)
	}
	return key, nil
}

// hardenKeyFile corrects a key file whose mode has been loosened out of band.
//
// The mode is set at creation and, until this existed, never looked at again —
// so a key restored from a backup, copied by hand, or caught by a stray chmod
// stayed readable by every local user for the life of the install, silently.
// atomicWriteFile makes the opposite promise for the config file and says so
// ("a loosened mode on any prior file at path is corrected rather than
// inherited"); this is that invariant, applied to the file that needs it most.
// The seal has no forward secrecy (relay/seal.go), so whoever reads this file
// can decrypt every terminal session anyone ever captured, not merely the next
// one — a window here is retroactive, and closing it late is still worth doing.
//
// Only group and other bits count as loosened. Forcing the mode to exactly
// 0600 would grant write access to a key the owner had deliberately made
// read-only, which is a loosening dressed as a fix.
//
// The chmod goes through the open descriptor rather than the path, so it cannot
// be redirected onto another file by a symlink swapped in between the stat and
// the fix. It is best-effort: a key that can be read but not chmod'd — a
// read-only mount, a file owned by someone else — is still a usable key, and
// refusing to start would trade a narrowed exposure for no agent at all.
func hardenKeyFile(f *os.File, path string) {
	st, err := f.Stat()
	if err != nil {
		return
	}
	perm := st.Mode().Perm()
	if perm&0o077 == 0 {
		return
	}
	// Once, at startup: loadOrCreateKey is called exactly once per process, and
	// on stderr because this runs before either the TUI or the log ring exists.
	fmt.Fprintf(os.Stderr,
		"warning: %s was mode %04o and could be read by other local users; tightening it to 0600\n", path, perm)
	if err := f.Chmod(perm &^ 0o077); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not tighten %s: %v\n", path, err)
	}
}

// publicKeyHeader is how the agent publishes its public key: on the tunnel
// handshake, every connect, rather than once at pairing.
//
// Sending it every time means an agent that predates this — or one whose key
// was regenerated — starts working again by reconnecting, instead of needing to
// be re-paired. The server stores whatever the pairing token's owner sends,
// which is exactly the authority that already controls the record.
const publicKeyHeader = "X-Ormos-Public-Key"

// encodePublicKey renders a public key for the handshake header.
func encodePublicKey(key *ecdh.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(key.PublicKey().Bytes())
}

// Fingerprint is the short base32 fingerprint of this machine's sealing key,
// for out-of-band verification: it is printed on startup (headless) and shown
// in the dashboard (TUI) so a user can read it against what the app pins and
// catch a relay that swapped the key. See relay.Fingerprint.
func (d *system) Fingerprint() string {
	return relay.Fingerprint(d.key.PublicKey().Bytes())
}
