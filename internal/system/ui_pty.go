//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// spawnUITerminal opens one PTY on this machine. There is no relay in this
// path: no admission, no fence, no handshake -- the local UI is the only
// client. The PTY belongs to this server and survives browser disconnects.
func spawnUITerminal(shell, cwd string) (*uiTerminal, error) {
	id := newUIID()
	cmd := exec.Command(shell)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		return nil, err
	}
	// A nonblocking duplicate is registered with Go's poller by NewFile. This
	// makes Close wake blocked PTY reads/writes on both Linux and macOS.
	pollable, err := pollableUIFile(ptmx)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = ptmx.Close()
		_ = cmd.Wait()
		return nil, err
	}
	ptmx = pollable
	// Keep ioctl descriptors stable across Close. Read/Write use os.File's own
	// descriptor references and stay outside this lock so blocked input can wake.
	var deviceMu sync.Mutex
	foregroundGroup := func() int {
		deviceMu.Lock()
		defer deviceMu.Unlock()
		group, _ := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPGRP)
		return group
	}
	closeDevice := func() { deviceMu.Lock(); defer deviceMu.Unlock(); _ = ptmx.Close() }
	t := &uiTerminal{
		id: id, shell: shell, cwd: cwd, started: time.Now(), alive: true,
		done: make(chan struct{}), readers: map[chan []byte]bool{},
		input: func(data []byte) error { _, err := ptmx.Write(data); return err },
		resize: func(cols, rows uint16) error {
			deviceMu.Lock()
			defer deviceMu.Unlock()
			return pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
		},
	}
	stopped := make(chan struct{})
	var stopOnce sync.Once
	var foreground int
	t.kill = func() {
		stopOnce.Do(func() {
			select {
			case <-t.done:
				return
			default:
			}
			t.mu.Lock()
			t.alive = false
			t.mu.Unlock()
			foreground = foregroundGroup()
			signalUIProcessGroups(cmd.Process.Pid, foreground, syscall.SIGHUP)
			close(stopped)
		})
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		chunk := make([]byte, 8192)
		for {
			n, err := ptmx.Read(chunk)
			if n > 0 {
				t.append(chunk[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	go func() {
		defer close(t.done)
		stop := false
		select {
		case <-stopped:
			stop = true
		case <-waited:
			// Wait drains the process, not its PTY. Preserve final output before
			// closing subscribers, while bounding descendants that keep the slave open.
			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case <-readDone:
			case <-stopped:
				stop = true
			case <-timer.C:
				fg := foregroundGroup()
				signalUIProcessGroups(cmd.Process.Pid, fg, syscall.SIGKILL)
			}
			timer.Stop()
		}
		if stop {
			timer := time.NewTimer(uiTerminalKillGap)
			select {
			case <-waited:
			case <-timer.C:
			}
			timer.Stop()
			// Also stop the foreground job: interactive shells put it in a different
			// process group. Browser disconnects never enter this ownership teardown.
			current := foregroundGroup()
			signalUIProcessGroups(cmd.Process.Pid, current, syscall.SIGKILL)
			if foreground != current && foreground > 0 {
				_ = syscall.Kill(-foreground, syscall.SIGKILL)
			}
		}
		closeDevice()
		<-readDone
		<-waited
		t.mu.Lock()
		t.alive = false
		for reader := range t.readers {
			close(reader)
			delete(t.readers, reader)
		}
		for update := range t.updates {
			close(update)
			delete(t.updates, update)
		}
		t.mu.Unlock()
	}()
	return t, nil
}

func pollableUIFile(file *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	pollable := os.NewFile(uintptr(fd), file.Name())
	_ = file.Close()
	return pollable, nil
}

func signalUIProcessGroups(shell, foreground int, signal syscall.Signal) {
	if shell > 0 {
		_ = syscall.Kill(-shell, signal)
	}
	if foreground > 0 && foreground != shell {
		_ = syscall.Kill(-foreground, signal)
	}
}
