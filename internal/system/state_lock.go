//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

const stateLockFileName = "agent.lock"

var stateDirFlock = unix.Flock

// stateDirLock is kept open for the whole configured agent lifetime. flock is
// advisory, process-owned kernel state: a crash closes the descriptor and the
// kernel releases the lock without a stale pid file to interpret.
type stateDirLock struct{ file *os.File }

func acquireStateDirLock() (*stateDirLock, error) {
	dir, err := ormosDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	_ = hardenOrmosDir()
	path := filepath.Join(dir, stateLockFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = f.Close()
		}
	}()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat state lock: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("state lock %s is not a regular file", path)
	}
	if owner, ok := fileOwner(st); ok && owner != os.Getuid() {
		return nil, fmt.Errorf("state lock %s is owned by uid %d, not by this user (uid %d)", path, owner, os.Getuid())
	}
	if err := f.Chmod(st.Mode().Perm() &^ 0o077); err != nil {
		return nil, fmt.Errorf("secure state lock: %w", err)
	}
	var lockErr error
	for {
		lockErr = stateDirFlock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if lockErr != unix.EINTR {
			break
		}
	}
	if lockErr != nil {
		if lockErr == unix.EWOULDBLOCK || lockErr == unix.EAGAIN {
			return nil, fmt.Errorf("state directory %s is already in use by another agent", dir)
		}
		return nil, fmt.Errorf("lock state directory %s: %w", dir, lockErr)
	}
	closeOnError = false
	return &stateDirLock{file: f}, nil
}

func (l *stateDirLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}
