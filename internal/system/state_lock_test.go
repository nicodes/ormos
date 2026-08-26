//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStateDirLockChild(t *testing.T) {
	if os.Getenv("ORMOS_STATE_LOCK_HELPER") != "1" {
		return
	}
	configFileOverride = filepath.Join(os.Getenv("ORMOS_STATE_LOCK_DIR"), "config.json")
	lock, err := acquireStateDirLock()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	// This marker stands for reset/connect/TUI startup: the real process reaches
	// those only after the same lock is acquired in runSystem.
	fmt.Println("reset/connect reached")
	if os.Getenv("ORMOS_STATE_LOCK_EXIT") == "1" {
		os.Exit(0) // process death must release the kernel advisory lock
	}
	_ = lock.Close()
}

func TestStateDirSingletonLockBlocksSecondProcessBeforeActions(t *testing.T) {
	dir := t.TempDir()
	oldOverride := configFileOverride
	configFileOverride = filepath.Join(dir, "config.json")
	t.Cleanup(func() { configFileOverride = oldOverride })
	first, err := acquireStateDirLock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })

	child := exec.Command(os.Args[0], "-test.run", "^TestStateDirLockChild$")
	child.Env = append(os.Environ(), "ORMOS_STATE_LOCK_HELPER=1", "ORMOS_STATE_LOCK_DIR="+dir)
	output, err := child.CombinedOutput()
	if err == nil {
		t.Fatalf("second process unexpectedly acquired state lock: %q", output)
	}
	if strings.Contains(string(output), "reset/connect reached") || !strings.Contains(string(output), "already in use") {
		t.Fatalf("second process output=%q; it must fail before reset/connect", output)
	}

	_ = first.Close()
	child = exec.Command(os.Args[0], "-test.run", "^TestStateDirLockChild$")
	child.Env = append(os.Environ(), "ORMOS_STATE_LOCK_HELPER=1", "ORMOS_STATE_LOCK_DIR="+dir, "ORMOS_STATE_LOCK_EXIT=1")
	if output, err = child.CombinedOutput(); err != nil || !strings.Contains(string(output), "reset/connect reached") {
		t.Fatalf("first process exit did not release lock: output=%q err=%v", output, err)
	}
	second, err := acquireStateDirLock()
	if err != nil {
		t.Fatalf("lock remained held after child exit: %v", err)
	}
	_ = second.Close()
}

func TestStateDirLockRejectsUnsafeLockPaths(t *testing.T) {
	oldOverride := configFileOverride
	t.Cleanup(func() { configFileOverride = oldOverride })

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		configFileOverride = filepath.Join(dir, "config.json")
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, stateLockFileName)); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireStateDirLock(); err == nil || !strings.Contains(err.Error(), "open state lock") {
			t.Fatalf("symlink lock accepted: %v", err)
		}
	})

	t.Run("fifo", func(t *testing.T) {
		dir := t.TempDir()
		configFileOverride = filepath.Join(dir, "config.json")
		if err := syscall.Mkfifo(filepath.Join(dir, stateLockFileName), 0o600); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() { _, err := acquireStateDirLock(); result <- err }()
		select {
		case err := <-result:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("FIFO lock accepted: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("FIFO lock open blocked")
		}
	})

	t.Run("foreign owner", func(t *testing.T) {
		dir := t.TempDir()
		configFileOverride = filepath.Join(dir, "config.json")
		oldOwner := fileOwner
		t.Cleanup(func() { fileOwner = oldOwner })
		fileOwner = func(os.FileInfo) (int, bool) { return os.Getuid() + 1, true }
		if _, err := acquireStateDirLock(); err == nil || !strings.Contains(err.Error(), "owned by uid") {
			t.Fatalf("foreign-owner lock accepted: %v", err)
		}
	})
}

func TestStateDirLockRetriesInterruptedFlock(t *testing.T) {
	dir := t.TempDir()
	oldOverride, oldFlock := configFileOverride, stateDirFlock
	t.Cleanup(func() { configFileOverride, stateDirFlock = oldOverride, oldFlock })
	configFileOverride = filepath.Join(dir, "config.json")
	calls := 0
	stateDirFlock = func(fd int, how int) error {
		calls++
		if calls == 1 {
			return syscall.EINTR
		}
		return oldFlock(fd, how)
	}
	lock, err := acquireStateDirLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if calls != 2 {
		t.Fatalf("flock calls=%d, want EINTR retry then lock", calls)
	}
}
