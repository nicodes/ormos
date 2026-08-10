package system

import (
	"crypto/ecdh"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nicodes/ormos/relay"
)

// keyFileName holds this machine's long-lived X25519 private key, beside the
// config and the audit log.
const keyFileName = "identity.key"

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
// That is why it is written 0600 and never leaves the machine. It is not a
// credential — it cannot be presented to the server to gain anything, and
// losing it costs nothing but a reconnect — but it is the reason a terminal is
// private, so it is treated like one.
func loadOrCreateKey() (*ecdh.PrivateKey, error) {
	dir, err := ormosDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, keyFileName)

	raw, err := os.ReadFile(path)
	if err == nil {
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
