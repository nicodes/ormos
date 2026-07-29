// Package system is ormos on a personal machine: it opens a single outbound
// WebSocket to the relay and serves terminal and port-proxy streams multiplexed
// over it. With no arguments it runs the tunnel; with a TTY it also shows a
// Bubble Tea status dashboard.
package system

import (
	"context"
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
revoked by forgetting it in the UI), you'll be prompted for your account
email/password to register it, and then it starts. The dashboard appears when
stdout is a terminal; otherwise it runs headless.

environment:
  ORMOS_API_URL  ws base URL of the ormos API (default wss://api.ormos.dev)
  SHELL          shell for spawned terminals (default /bin/bash)

Your email and password are typed in at the prompt and nowhere else — there is
no flag and no environment variable for them. Arguments are readable by any
local user via /proc and land in shell history; environment variables are
inherited by every child process and tend to end up in dotfiles.

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
	if fileCfg, err := loadConfigFile(); err == nil {
		cfg.ClientID = fileCfg.ClientID
		cfg.SystemID = fileCfg.SystemID
		cfg.Email = fileCfg.Email
		if fileCfg.PairingToken != "" {
			cfg.PairingToken = fileCfg.PairingToken
		}
		if os.Getenv("ORMOS_API_URL") == "" && fileCfg.RelayURL != "" {
			cfg.RelayURL = fileCfg.RelayURL
		}
	}

	// Log in on demand: when there is no saved token, or the saved one was
	// revoked (system forgotten in the UI).
	if cfg.PairingToken == "" || !tokenValid(cfg.RelayURL, cfg.PairingToken) {
		// Credentials are always typed in: never arguments (world-readable via
		// /proc/<pid>/cmdline, and they land in shell history) and never the
		// environment (inherited by every child, and it lives in dotfiles).
		li, err := performLogin(cfg.RelayURL)
		if err != nil {
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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	d := newSystem(cfg)
	d.setCancel(cancel) // let the relay request a graceful shutdown (UI Stop/Forget)

	if !isTTY() {
		d.EchoToStderr(true)
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
