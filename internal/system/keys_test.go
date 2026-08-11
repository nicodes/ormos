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
	// 0700, as MkdirAll creates it in production. t.TempDir hands back a 0777
	// (umask-reduced) directory, which hardenOrmosDir would correct on the
	// first read -- so without this every test that reads a key would carry a
	// startup warning about the test harness rather than about the agent.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
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

// The mode is only set at creation, so a key loosened after the fact — restored
// from a backup, copied by hand, caught by a stray chmod — would otherwise stay
// readable by every local user forever. It decrypts captured past sessions, not
// just future ones, so the correction happens on the read that finds it.
func TestLoosenedTerminalKeyModeIsCorrectedOnRead(t *testing.T) {
	dir := withTempConfigDir(t)
	newSystem(systemConfig{}) // creates the key at 0600
	path := filepath.Join(dir, keyFileName)
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, loose := range []os.FileMode{0o644, 0o604, 0o660, 0o666} {
		if err := os.Chmod(path, loose); err != nil {
			t.Fatal(err)
		}
		key, warnings, err := loadOrCreateKey()
		if err != nil {
			t.Fatalf("loadOrCreateKey after chmod %04o: %v", loose, err)
		}
		// The issue asked for the correction to be reported, not made silently:
		// a key that was world-readable may already have been copied, and the
		// only remedy that actually helps is regenerating it.
		if len(warnings) != 1 || !contains(warnings[0], "delete it to generate a new one") {
			t.Errorf("correcting %04o produced warnings %q, want one naming the remedy", loose, warnings)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("key file left at %04o after a read (was %04o); other local users can still read it", perm, loose)
		}
		// Tightening the file must not disturb its contents: the browser has
		// already learned the public half, so a regenerated key is a silently
		// broken pairing.
		if string(key.Bytes()) != string(want) {
			t.Error("the key changed while its mode was being corrected")
		}
	}
}

// Only group and other bits are corrected. 0444 is both world-readable (so it
// must be corrected) and owner-read-only (so the owner's bits must survive):
// forcing the mode to exactly 0600 would grant write access to a key the owner
// had deliberately made read-only, which is a loosening dressed up as a fix.
// The 0400 case alone cannot pin this — it returns early and never reaches the
// chmod at all.
func TestTerminalKeyModeCorrectionKeepsTheOwnerBits(t *testing.T) {
	dir := withTempConfigDir(t)
	newSystem(systemConfig{})
	path := filepath.Join(dir, keyFileName)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateKey(); err != nil {
		t.Fatalf("loadOrCreateKey on a 0444 key: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o400 {
		t.Errorf("key file is %04o, want 0400 — group and other cleared, owner untouched", perm)
	}
}

// A symlink at the key path must not be followed. Following it would adopt
// whatever it points at as this machine's identity and apply the mode
// correction to a file that was never ours.
func TestTerminalKeySymlinkIsRefused(t *testing.T) {
	dir := withTempConfigDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "planted")
	if err := os.WriteFile(elsewhere, make([]byte, 32), 0o666); err != nil {
		t.Fatal(err)
	}
	// Explicitly, because the umask would otherwise decide the mode and the
	// assertion below would be measuring the umask rather than this code.
	if err := os.Chmod(elsewhere, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, keyFileName)); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	if _, _, err := loadOrCreateKey(); err == nil {
		t.Fatal("a symlink at the key path was followed; it must be refused")
	}
	// And the planted file must be untouched — neither read as a key nor
	// chmod'd on our behalf.
	st, err := os.Stat(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o666 {
		t.Errorf("the symlink target was chmod'd to %04o; nothing outside the key path may be touched", perm)
	}
}

// A DANGLING link is refused at the READ, not at the create: an O_NOFOLLOW open
// returns ELOOP for a dangling link as much as a live one -- not ENOENT -- so
// control never reaches the create branch for either. This pins that, and that
// nothing is written to the attacker's path.
func TestDanglingKeySymlinkIsRefusedAtTheRead(t *testing.T) {
	dir := withTempConfigDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "attacker-chosen")
	if err := os.Symlink(target, filepath.Join(dir, keyFileName)); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	_, _, err := loadOrCreateKey()
	if err == nil {
		t.Fatal("a dangling symlink at the key path was accepted; it must be refused")
	}
	if !contains(err.Error(), "symbolic link") {
		t.Errorf("the error should name the cause, got: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("a private key was written through the symlink to the attacker's path")
	}
}

// The create branch's own guard. O_EXCL is not about symlinks -- those are
// already gone by here -- it is about two agents starting at once, both seeing
// ENOENT, and the second overwriting the key the first has already published.
// The loser must fail loudly rather than quietly win.
func TestWriteNewKeyNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, keyFileName)
	existing := []byte("a key that is already here")
	if err := os.WriteFile(path, existing, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeNewKey(path, make([]byte, 32)); err == nil {
		t.Fatal("writeNewKey overwrote an existing key file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(existing) {
		t.Error("the existing key was modified")
	}

	// And it does create when there is genuinely nothing there, at 0600.
	fresh := filepath.Join(dir, "fresh.key")
	if err := writeNewKey(fresh, make([]byte, 32)); err != nil {
		t.Fatalf("writeNewKey on a free path: %v", err)
	}
	st, err := os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("new key created %04o, want 0600", perm)
	}
}

// A group- or world-writable state directory defeats every check on the files
// inside it: an attacker who cannot read a 0600 key can still unlink it and
// leave their own in its place.
func TestStateDirectoryModeIsCorrectedOnRead(t *testing.T) {
	dir := withTempConfigDir(t)
	newSystem(systemConfig{})
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	_, warnings, err := loadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("state directory left at %04o; another local user can still replace the key inside it", perm)
	}
	if len(warnings) == 0 {
		t.Error("correcting the state directory was not reported")
	}
}

// A key the owner deliberately made read-only must stay read-only.
func TestTightTerminalKeyModeIsLeftAlone(t *testing.T) {
	dir := withTempConfigDir(t)
	newSystem(systemConfig{})
	path := filepath.Join(dir, keyFileName)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateKey(); err != nil {
		t.Fatalf("loadOrCreateKey on a 0400 key: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o400 {
		t.Errorf("key file is %04o, want 0400 left untouched", perm)
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
	_, _, err := loadOrCreateKey()
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

// The ownership refusal itself cannot be exercised as an unprivileged user --
// creating a file owned by somebody else needs a second uid, which CI does not
// have. What can be pinned is that the uid is actually read: a fileOwner that
// silently reported "unknown" would disable the check with nothing failing.
func TestFileOwnerReadsTheUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := fileOwner(fi)
	if !ok {
		t.Fatal("fileOwner could not read the uid; the ownership check would never refuse anything")
	}
	if owner != os.Getuid() {
		t.Errorf("fileOwner = %d, want this process's uid %d", owner, os.Getuid())
	}
}

// A directory at the key path must be refused rather than chmod'd and reported
// as a loosened key.
func TestTerminalKeyPathMustBeARegularFile(t *testing.T) {
	dir := withTempConfigDir(t)
	if err := os.MkdirAll(filepath.Join(dir, keyFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateKey(); err == nil {
		t.Fatal("a directory at the key path was accepted")
	} else if !contains(err.Error(), "regular file") {
		t.Errorf("the error should say what is wrong, got: %v", err)
	}
}
