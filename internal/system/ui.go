//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/nicodes/ormos/internal/nativeapi"
)

// RunUI launches the native Rust API. HTTP, terminal ownership, policy,
// WebSockets and preview forwarding all execute in that API process.
func RunUI(args []string, version string) error {
	path, cleanup, err := nativeapi.Prepare()
	if err != nil {
		return err
	}
	defer cleanup()
	forwarded := append([]string{}, args...)
	forwarded = append(forwarded, "--app-version", version, "--parent-pid", strconv.Itoa(os.Getpid()))
	if configFileOverride != "" {
		forwarded = append(forwarded, "--config", configFileOverride)
	}
	command := exec.Command(path, forwarded...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := command.Start(); err != nil {
		return err
	}
	// Unix keeps the executable image open after exec. Remove the private
	// extraction immediately so even a killed launcher leaves no payload file.
	cleanup()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case received := <-signals:
				_ = command.Process.Signal(received)
			case <-done:
				return
			}
		}
	}()
	err = command.Wait()
	close(done)
	return err
}
