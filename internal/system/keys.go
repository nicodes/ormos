//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"crypto/ecdh"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

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
// use, alongside any warnings about the state it found on disk.
//
// The key is per-machine rather than per-session because the browser has to
// learn the public half before it can speak: it reads it from the system's
// record, which the agent publishes when it connects. A per-session key would
// need a round trip before the first keystroke, which is the trade recorded in
// relay/seal.go — the browser's half of the agreement is ephemeral, this half
// is not, so a compromise of this file exposes past sessions.
//
// That is why it is written 0600, has its mode, its ownership and the state
// directory around it rechecked on every read (openPrivateFile,
// hardenOrmosDir), and never leaves the machine. It is not a credential — it
// cannot be presented to the server to gain anything, and losing it costs
// nothing but a reconnect — but it is the reason a terminal is private, so it
// is treated like one.
//
// Warnings are returned rather than printed because the caller knows where they
// will be seen: the TUI's alt screen wipes stderr moments later, so the agent
// puts them in the log ring as well.
func loadOrCreateKey() (*ecdh.PrivateKey, []string, error) {
	dir, err := ormosDir()
	if err != nil {
		return nil, nil, err
	}
	path := filepath.Join(dir, keyFileName)

	// openPrivateFile hardens the state directory as well as the file, so its
	// warning is collected on every path out of here -- including the error
	// ones. "the key was world-readable" must not be lost because the read then
	// failed for some other reason.
	f, dirWarning, fileWarning, err := openPrivateFile(path)
	var warnings []string
	if dirWarning != "" {
		warnings = append(warnings, dirWarning)
	}
	if fileWarning != "" {
		// The remedy belongs to the FILE, not to the directory around it: the
		// seal has no forward secrecy, so tightening the key's mode does not
		// undo the exposure — anything captured while it was readable stays
		// decryptable by whoever read it. Attaching this to a directory
		// correction would tell the operator to delete a key that may never
		// have existed.
		warnings = append(warnings, fileWarning+" — it may already have been copied; delete it to generate a new one and re-pair this machine")
	}
	if err == nil {
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, maxKeyFileSize))
		if err != nil {
			return nil, warnings, fmt.Errorf("reading %s: %w", path, err)
		}
		key, err := ecdh.X25519().NewPrivateKey(raw)
		if err != nil {
			// A corrupt key would fail every handshake with no useful message.
			// Say so plainly; deleting the file regenerates it.
			return nil, warnings, fmt.Errorf("reading %s: %w (delete it to generate a new one)", path, err)
		}
		return key, warnings, nil
	}
	if !os.IsNotExist(err) {
		return nil, warnings, err
	}

	key, err := relay.GenerateAgentKey()
	if err != nil {
		return nil, warnings, fmt.Errorf("generating terminal key: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, warnings, err
	}
	// 0600 from the moment it exists: writing world-readable and chmod-ing
	// after leaves a window in which another local user can read it.
	if err := writeNewKey(path, key.Bytes()); err != nil {
		return nil, warnings, fmt.Errorf("writing %s: %w", path, err)
	}
	return key, warnings, nil
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

// writeNewKey creates the key file, and only ever creates it.
//
// O_EXCL is the guard here, and what it guards is the create/create race, not
// the symlink: two agents starting at once both see ENOENT from the read above
// and both arrive here, and without O_EXCL the second silently overwrites the
// key the first has already generated and published — leaving one of them
// sealing against a key the relay does not hold. O_EXCL makes the loser fail
// loudly instead of quietly winning.
//
// It also refuses any path that appeared since the read, symlink or not, which
// is the belt to O_NOFOLLOW's braces. A symlink is already refused earlier: an
// O_NOFOLLOW open returns ELOOP for a DANGLING link as much as a live one — not
// ENOENT — so control never reaches this function for either.
func writeNewKey(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
