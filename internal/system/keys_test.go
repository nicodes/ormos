package system

import (
	"os"
	"path/filepath"
	"testing"
)

// The agent's key was written but never loaded: loadOrCreateKey existed,
// d.key was declared, the handshake header and DeriveSessionKeys both read
// it -- and nothing ever assigned it. Every connect dereferenced nil.
//
// These pin the two halves together so they cannot come apart again.

func withTempConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := configFileOverride
	configFileOverride = filepath.Join(dir, "config.json")
	t.Cleanup(func() { configFileOverride = prev })
	return dir
}

func TestNewSystemLoadsTheTerminalKey(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})
	if d.key == nil {
		t.Fatal("newSystem left key nil -- every connect panics and no frame can be sealed")
	}
	// The public half must encode without panicking: that is the exact call
	// the handshake makes.
	if got := encodePublicKey(d.key); got == "" {
		t.Error("encodePublicKey produced nothing")
	}
}

func TestTerminalKeyIsStableAcrossRestarts(t *testing.T) {
	// The key is per-machine, not per-session: the browser learns the public
	// half from the system's record, so regenerating it every start would
	// silently break every already-paired browser.
	withTempConfigDir(t)
	first := newSystem(systemConfig{}).key
	second := newSystem(systemConfig{}).key
	if string(first.Bytes()) != string(second.Bytes()) {
		t.Error("the key changed between starts; it must persist")
	}
}

func TestTerminalKeyIsWrittenPrivate(t *testing.T) {
	dir := withTempConfigDir(t)
	newSystem(systemConfig{})
	st, err := os.Stat(filepath.Join(dir, keyFileName))
	if err != nil {
		t.Fatalf("key file was not created: %v", err)
	}
	// 0600 from creation. It is what makes a terminal private, so another
	// local user must never be able to read it -- not even briefly.
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file is %04o, want 0600", perm)
	}
}

func TestCorruptTerminalKeyIsReportedNotIgnored(t *testing.T) {
	dir := withTempConfigDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, keyFileName), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadOrCreateKey()
	if err == nil {
		t.Fatal("a corrupt key must not be accepted; it would fail every handshake with no useful message")
	}
	if !contains(err.Error(), "delete it") {
		t.Errorf("the error should say how to recover, got: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
