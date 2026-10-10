//go:build (linux && !android) || (darwin && !ios)

// Package system serves the local Ormos terminal UI. Hosted relay command
// entries fail before reading credentials or contacting a service.
//
// # Supported platforms
//
// Linux and macOS. Every file in this package carries
// //go:build (linux && !android) || (darwin && !ios), and that is the whole
// statement.
//
// The agent's job is to act on the machine it runs on: allocate a PTY, poll its
// master descriptor, read a terminal's foreground process group, deliver SIGHUP
// and then SIGKILL to a shell that will not exit, and take a file lock over the
// audit log. That is golang.org/x/sys/unix, which has no Windows implementation
// of any of it, so `go build .` for Windows reports that the package does not
// exist there rather than listing the symbols it is missing.
//
// The tag names the two platforms rather than saying `unix`, which would also
// select the BSDs and Solaris. Nothing here is tested on those, and this is a
// program that hands out shells on the machine it runs on — the PTY and
// process-group paths are exactly the code that misbehaves quietly on a kernel
// nobody exercised. A build tag is a claim about where this is known to work,
// so it must not be wider than the set CI actually runs.
//
// The !android and !ios halves are not pedantry: Go sets the linux tag on
// android and the darwin tag on ios, so a plain `linux || darwin` silently
// included both, and the whole agent selected and built for them. Android is
// Linux — /proc/net/tcp parsing, PTY allocation and process-group signalling
// all compile there, on a security model nothing here has considered.
//
// CI holds up both halves: a macos-latest job runs the suite, and a step in
// ci.yml asserts that on every other platform Go knows about — Windows, the
// BSDs, Solaris, illumos, AIX, plan9, wasm, android and ios — nothing but relay
// is even selected. Without that step the claim would
// be unenforceable — `go build ./...` SKIPS packages with no buildable files,
// so a file that lost its tag would go unnoticed by every build in the pipeline.
//
// The shared relay package is deliberately untagged: it is pure Go, the hosted
// relay imports it, and it cross-compiles for Windows today.
package system

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/nicodes/ormos/relay"
)

// Main defaults to the local UI and accepts explicit UI and inspection flags.
// The version is stamped by the release build. uiMainFn is the local dispatch
// seam used to verify argument forwarding without starting a real server.
var uiMainFn = RunUI

func Main(args []string, version string) {
	if len(args) == 0 {
		args = []string{"ui"}
	}
	switch {
	case args[0] == "relay" || args[0] == "--config":
		fmt.Fprintln(os.Stderr, "error: hosted relay mode is retired; use the local UI with private network access")
		os.Exit(2)
	case len(args) == 1 && args[0] == "--help":
		usage()
		return
	case len(args) == 1 && args[0] == "--version":
		fmt.Println(version)
		return
	case len(args) == 1 && args[0] == "--protocol-version":
		fmt.Println(relay.StreamFenceVersion)
		return
	case len(args) >= 1 && args[0] == "ui":
		if err := uiMainFn(args[1:], version); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(2)
		}
		return
	default:
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n\n", args[0])
		usage()
		os.Exit(2)
	}
}

var usageText = `ormos — a local terminal and app preview

usage:
  ormos                    serve the local web UI
  ormos ui [options]       serve the local web UI with explicit options
  ormos --help             show this
  ormos --version          print the version

UI options:
  --port N                 terminal UI port (default 8481)
  --cwd PATH               initial terminal directory (default home)
  --hosts HOST:PORT,...     allowed UI hostnames, including their ports
  --bind IP                UI bind address (default 127.0.0.1)

Serve the loopback UI privately through Tailscale Serve for phone access.
Enter a local HTTP app port; Ormos forwards and exposes previews automatically.
Use an HTTPS workspace for Godot and other apps requiring a secure context.
There is no application account or login. Trusted network access is required.
Terminals end when Ormos stops; browser disconnects leave them running.

Hosted relay commands are retired. Saved local UI entries and private access remain supported.
`

func usage() {
	fmt.Fprint(os.Stderr, usageText)
}

func runSystem() {
	// Precedence: env > saved config > defaults.
	cfg := loadSystemConfig() // relay default/env, $SHELL
	fileCfg, cfgWarning, cfgErr := loadConfigFileChecked()
	// Before anything else, and on the error path too: "config.json was
	// world-readable" is the most important thing that can have happened here,
	// and it must not be swallowed because the read then failed for some other
	// reason. This is the one read that happens before the log ring exists, so
	// stderr is the only destination.
	if cfgWarning != "" {
		fmt.Fprintf(os.Stderr, "warning: %s\n", cfgWarning)
	}
	if err := cfgErr; err == nil {
		cfg.ClientID = fileCfg.ClientID
		cfg.SystemID = fileCfg.SystemID
		cfg.Email = fileCfg.Email
		if fileCfg.PairingToken != "" {
			cfg.PairingToken = fileCfg.PairingToken
		}
		if os.Getenv("ORMOS_API_URL") == "" && fileCfg.RelayURL != "" {
			cfg.RelayURL = fileCfg.RelayURL
		}
	} else if !os.IsNotExist(err) {
		// A config that exists but cannot be used must not silently fall
		// through to re-pairing — least of all a relay URL that failed
		// validation, which is exactly the case worth stopping for.
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// A cleartext remote relay is fatal before any token crosses the wire —
	// in TUI mode a log line has no guaranteed receiver, so this cannot wait
	// for the run loop to warn about it.
	if err := checkRelayTransport(cfg.RelayURL); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	stateLock, err := acquireStateDirLock()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer stateLock.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	for {
		// Log in on demand: when there is no saved token, or the saved one was
		// authoritatively revoked. A throttled or unavailable startup check is
		// neither valid nor revoked: preserve the token and stop without starting
		// the concurrent ports/reset/connect loops.
		needsLogin, loginStateErr := loginRequired(&cfg)
		if loginStateErr != nil {
			writeLoginError(os.Stderr, loginStateErr)
			os.Exit(1)
		}
		if needsLogin {
			li, loginErr := performLogin(ctx, cfg.RelayURL)
			if loginErr != nil {
				// The user walking away from the pairing screen is not a failure:
				// say so plainly and exit with the conventional 128+SIGINT.
				if errors.Is(loginErr, errLoginCancelled) {
					fmt.Fprintln(os.Stderr, "pairing cancelled")
					os.Exit(130)
				}
				writeLoginError(os.Stderr, loginErr)
				os.Exit(1)
			}
			cfg.ClientID = li.ClientID
			cfg.PairingToken = li.PairingToken
			cfg.SystemID = li.SystemID
			cfg.Email = li.Email
		}
		if cfg.PairingToken == "" {
			fmt.Fprintln(os.Stderr, "error: no pairing token")
			os.Exit(1)
		}

		d := newSystem(cfg)
		d.setCancel(cancel) // let the relay request a graceful shutdown (UI Stop/Forget)
		var runErr error
		if !isTTY() {
			d.EchoToStderr(true)
			// Print the sealing-key fingerprint once, up front: headless has no
			// dashboard to show it, and it is what a user reads against the app to
			// confirm the relay has not swapped the key.
			fmt.Fprintf(os.Stderr, "sealing key fingerprint: %s (verify this in the app)\n", d.Fingerprint())
			runErr = d.Run(ctx) // blocks until ctx done or the token is revoked
		} else {
			runErr = runTUI(ctx, d)
		}
		if !errors.Is(runErr, errPairingTokenRevoked) {
			return
		}
		if err := clearRevokedLogin(&cfg); err != nil {
			writeLoginError(os.Stderr, fmt.Errorf("clear revoked pairing token: %w", err))
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "pairing token was revoked; starting device pairing")
	}
}

// loginRequired classifies the saved credential before any concurrent agent
// activity starts. A missing or authoritatively revoked token needs pairing;
// throttling, relay faults and unexpected responses are errors so callers keep
// the credential rather than destroying it on an inconclusive answer.
func loginRequired(cfg *systemConfig) (bool, error) {
	if cfg.PairingToken == "" {
		return true, nil
	}
	valid, err := tokenValid(cfg.RelayURL, cfg.PairingToken)
	if err != nil {
		return false, err
	}
	if valid {
		return false, nil
	}
	if err := clearRevokedLogin(cfg); err != nil {
		return false, fmt.Errorf("clear revoked pairing token: %w", err)
	}
	return true, nil
}

// clearRevokedLogin keeps the durable config and the running copy in step. The
// relay URL and stable client ID survive so re-pairing the same machine does not
// create a duplicate, while every account credential is removed before a fresh
// device code is shown.
func clearRevokedLogin(cfg *systemConfig) error {
	if err := clearLoginConfig(); err != nil {
		return err
	}
	cfg.PairingToken = ""
	cfg.SystemID = ""
	cfg.Email = ""
	return nil
}

// writeLoginError runs after the pairing TUI has restored the live terminal.
// The wrapped error may contain a relay JSON error, detail, response body, or
// HTTP reason phrase, so it must be sanitized at this final output boundary.
func writeLoginError(w io.Writer, err error) {
	fmt.Fprintf(w, "error: %s\n", sanitizeRelayOutput(err.Error()))
}

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
