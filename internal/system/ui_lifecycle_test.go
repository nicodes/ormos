//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"context"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestUITerminalNaturalExitDrainsOutput(t *testing.T) {
	dir := t.TempDir()
	shell := filepath.Join(dir, "output-shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nhead -c 262144 /dev/zero | tr '\\000' x\nprintf FINAL_TERMINAL_OUTPUT\n"), 0700); err != nil {
		t.Fatal(err)
	}
	want := append(bytes.Repeat([]byte("x"), 262144), []byte("FINAL_TERMINAL_OUTPUT")...)
	for i := 0; i < 5; i++ {
		term, err := spawnUITerminal(shell, dir)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			term.mu.Lock()
			alive := term.alive
			term.mu.Unlock()
			if !alive {
				break
			}
			if time.Now().After(deadline) {
				term.kill()
				t.Fatal("test shell did not exit")
			}
			time.Sleep(time.Millisecond)
		}
		got := []byte(term.output())
		if !bytes.Equal(got, want) {
			t.Fatalf("exit %d lost final PTY output: got %d bytes, want %d", i, len(got), len(want))
		}
	}
}

func TestUIShutdownWaitsForStubbornShell(t *testing.T) {
	dir := t.TempDir()
	shell := filepath.Join(dir, "stubborn-shell")
	pidfile := filepath.Join(dir, "pid")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\ntrap '' HUP\nprintf '%s' $$ > pid\nwhile :; do sleep 1; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	term, err := spawnUITerminal(shell, dir)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	deadline := time.Now().Add(3 * time.Second)
	for pid == 0 {
		data, _ := os.ReadFile(pidfile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		if time.Now().After(deadline) {
			term.kill()
			t.Fatal("test shell did not start")
		}
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	server := &uiServer{terms: map[string]*uiTerminal{term.id: term}}
	server.closeTerminals()
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatal("shutdown returned with a live shell that ignores SIGHUP")
	}
}

func TestUITerminalCloseWakesBlockedInput(t *testing.T) {
	dir := t.TempDir()
	shell := filepath.Join(dir, "blocked-shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\ntrap '' HUP\nstty -echo -icanon\nprintf READY\nsleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	term, err := spawnUITerminal(shell, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { term.kill(); <-term.done })
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(term.output(), "READY") {
		if time.Now().After(deadline) {
			t.Fatal("test shell did not start")
		}
		time.Sleep(time.Millisecond)
	}
	wrote := make(chan error, 1)
	go func() { wrote <- term.input(context.Background(), bytes.Repeat([]byte("x"), 1<<20)) }()
	select {
	case <-wrote:
		t.Fatal("fixture input did not block")
	case <-time.After(50 * time.Millisecond):
	}
	term.kill()
	term.kill() // Concurrent/repeated kill requests share one teardown.
	select {
	case <-term.done:
	case <-time.After(uiTerminalKillGap + time.Second):
		t.Fatal("PTY teardown did not finish")
	}
	select {
	case err := <-wrote:
		if err == nil {
			t.Fatal("blocked input succeeded after closing PTY")
		}
	case <-time.After(time.Second):
		t.Fatal("closing the PTY did not wake blocked input")
	}
}

func TestUIShutdownRacesPendingOpen(t *testing.T) {
	fixture := newUIFixture(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var term *uiTerminal
	uiSpawnTerminal = func(shell, cwd string) (*uiTerminal, error) {
		close(entered)
		<-release
		var err error
		term, err = spawnUITerminal(shell, cwd)
		return term, err
	}
	server := fixture.start(t)
	response := make(chan int, 1)
	go func() {
		status, _ := postJSON(t, server.URL+"/api/action", `{"action":"open","shell":"/bin/sh"}`)
		response <- status
	}()
	<-entered
	closed := make(chan struct{})
	go func() { fixture.srv.closeTerminals(); close(closed) }()
	// Synchronize on closing, rather than relying on scheduler sleeps.
	for {
		fixture.srv.mu.Lock()
		closing := fixture.srv.closing
		fixture.srv.mu.Unlock()
		if closing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	select {
	case status := <-response:
		if status != 503 {
			t.Fatalf("pending open status = %d, want 503", status)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("pending open hung")
	}
	<-closed
	select {
	case <-term.done:
	default:
		t.Fatal("shutdown leaked the pending terminal")
	}
	status, _ := postJSON(t, server.URL+"/api/action", `{"action":"open","shell":"/bin/sh"}`)
	if status != 503 {
		t.Fatalf("open after shutdown status = %d", status)
	}
}

func TestUITerminalKillStopsForegroundJob(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	term, err := spawnUITerminal("/bin/bash", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { term.kill(); <-term.done })
	if err := term.input(context.Background(), []byte("sh -c 'trap \"\" HUP; echo $$ > foreground; exec sleep 30'\r")); err != nil {
		t.Fatal(err)
	}
	var pid int
	deadline := time.Now().Add(3 * time.Second)
	for pid == 0 {
		data, _ := os.ReadFile(filepath.Join(dir, "foreground"))
		pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		if time.Now().After(deadline) {
			t.Fatal("foreground fixture did not start")
		}
		time.Sleep(time.Millisecond)
	}
	group, err := unix.Getpgid(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-group, syscall.SIGKILL) })
	term.kill()
	select {
	case <-term.done:
	case <-time.After(uiTerminalKillGap + time.Second):
		t.Fatal("foreground teardown timed out")
	}
	// Orphan reaping belongs to init; verify it cannot execute, including zombies
	// by checking that the group has no live sleep job through signal delivery.
	deadline = time.Now().Add(time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("foreground job survived terminal teardown")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestUITerminalStoppingStillCountsTowardLimit(t *testing.T) {
	fixture := newUIFixture(t, nil)
	done := make(chan struct{})
	fixture.srv.terms["stopping"] = &uiTerminal{alive: false, done: done}
	oldLimit := uiMaxTerminals
	uiMaxTerminals = 1
	t.Cleanup(func() { uiMaxTerminals = oldLimit })
	server := fixture.start(t)
	status, _ := postJSON(t, server.URL+"/api/action", `{"action":"open"}`)
	if status != 429 || fixture.spawnLog.Len() != 0 {
		t.Fatal("stopping PTY was not counted toward the process limit")
	}
	close(done)
	status, _ = postJSON(t, server.URL+"/api/action", `{"action":"open"}`)
	if status != 200 {
		t.Fatalf("completed teardown did not release a slot: %d", status)
	}
}

func TestUIActionRejectsTrailingAndUnknownJSON(t *testing.T) {
	fixture := newUIFixture(t, nil)
	server := fixture.start(t)
	for _, body := range []string{`{"action":"open"} {"action":"open"}`, `{"action":"open"} garbage`, `{"action":"open","unexpected":true}`, `{"action":"open"} ` + strings.Repeat(" ", 4096)} {
		status, _ := postJSON(t, server.URL+"/api/action", body)
		if status != 400 {
			t.Errorf("malformed action status=%d, want 400", status)
		}
	}
	if fixture.spawnLog.Len() != 0 {
		t.Fatal("malformed action reached spawn")
	}
}
