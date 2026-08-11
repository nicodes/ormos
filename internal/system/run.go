//go:build linux || darwin

// Package system is ormos on a personal machine: it opens a single outbound
// WebSocket to the relay and serves terminal and port-proxy streams multiplexed
// over it. With no arguments it runs the tunnel; with a TTY it also shows a
// Bubble Tea status dashboard.
//
// # Supported platforms
//
// Linux and macOS. Every file in this package carries //go:build linux ||
// darwin, and that is the whole statement.
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
// CI holds up both halves: a macos-latest job runs the suite, and a step in
// ci.yml asserts that the agent does not build for Windows or the BSDs and that
// nothing but relay is even selected there. Without that step the claim would
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
	"os"
	"os/signal"
	"syscall"
)

// Main accepts `ormos`, `ormos --config PATH`, `ormos --help` and
// `ormos --version`, and nothing else — no `-h` shorthand, no subcommands.
// Parsed by hand rather than with the flag package, which would accept
// single-dash spellings and add its own -h.
//
// The version is passed in rather than declared here: it is stamped at release
// time with -ldflags "-X main.version=...", and main is the one package name
// that flag can name without knowing this directory's import path.
func Main(args []string, version string) {
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "--help":
		usage()
		return
	case len(args) == 1 && args[0] == "--version":
		fmt.Println(version)
		return
	case len(args) == 2 && args[0] == "--config" && args[1] != "":
		configFileOverride = args[1]
	case args[0] == "--config":
		fmt.Fprintln(os.Stderr, "error: --config needs a path, e.g. --config ~/.ormos-dev/config.json")
		os.Exit(2)
	default:
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n\n", args[0])
		usage()
		os.Exit(2)
	}
	runSystem()
}

func usage() {
	fmt.Fprint(os.Stderr, `ormos — personal remote-access system

usage:
  ormos                    run the system
  ormos --config PATH      run with a different config file
  ormos --help             show this
  ormos --version          print the version

Just run ormos. If this machine isn't registered yet (or its credentials were
revoked by forgetting it in the UI), it shows a short pairing code — approve it
in the web app and the system starts. The dashboard appears when stdout is a
terminal; otherwise it runs headless.

environment:
  ORMOS_API_URL    ws base URL of the ormos API (default wss://api.ormos.dev)
  ORMOS_INSECURE=1 allow a cleartext (ws://) remote relay — otherwise fatal
  SHELL            shell for spawned terminals (default /bin/bash)

Pairing never asks for a password in this terminal: the relay issues a
short-lived code and a human approves it in the web app, so there is no flag
and no environment variable for credentials — and nothing sensitive to leak
through /proc, shell history, or child processes.

--config points at the login config file, default ~/.config/ormos/config.json.
policy.json and sessions.log live beside it, so a second config keeps a machine's
whole local state separate — which is how a dev pairing is kept from overwriting
the one this machine holds for production.

To sign this machine out, delete the saved config (or press L in the dashboard):
  rm ~/.config/ormos/config.json
That does not delete the system from the account; use the app to forget it.
`)
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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Log in on demand: when there is no saved token, or the saved one was
	// revoked (system forgotten in the UI). Pairing is a device-authorization
	// flow — a code approved in the web app — so no credentials are ever typed
	// here: never arguments (world-readable via /proc/<pid>/cmdline, and they
	// land in shell history) and never the environment (inherited by every
	// child, and it lives in dotfiles).
	if cfg.PairingToken == "" || !tokenValid(cfg.RelayURL, cfg.PairingToken) {
		li, err := performLogin(ctx, cfg.RelayURL)
		if err != nil {
			// The user walking away from the pairing screen is not a failure:
			// say so plainly and exit with the conventional 128+SIGINT.
			if errors.Is(err, errLoginCancelled) {
				fmt.Fprintln(os.Stderr, "pairing cancelled")
				os.Exit(130)
			}
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
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

	if !isTTY() {
		d.EchoToStderr(true)
		// Print the sealing-key fingerprint once, up front: headless has no
		// dashboard to show it, and it is what a user reads against the app to
		// confirm the relay has not swapped the key.
		fmt.Fprintf(os.Stderr, "sealing key fingerprint: %s (verify this in the app)\n", d.Fingerprint())
		d.Run(ctx) // blocks until ctx done
		return
	}
	runTUI(ctx, d)
}

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
