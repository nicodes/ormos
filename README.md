<!-- Generated from private documentation source. Do not edit directly. Source SHA256: ad85966cc5131d6bc2c79792feb1573603a38665ad08e1b251f8831b7ebd994d -->

# Ormos

A terminal and local app preview in your browser. One executable serves the SolidJS UI and interactive terminals on your machine. Local use needs no account, hosted backend, or Clerk.

## Install and run

Use Go matching `go.mod` and `.mise.toml`, or a verified archive from this repository’s releases.

```sh
go install github.com/nicodes/ormos@latest
ormos ui
```

Bare `ormos` also starts the local UI. Open `http://127.0.0.1:8481`. Type in the terminal, start a web app, then enter its local port in Preview. Terminal and preview panes sit beside each other on desktop and stack on a phone. The Keyboard button and extra terminal keys support touch devices. Reloading the page reconnects to its existing terminal while Ormos stays running. New terminal replaces the current shell.

The terminal starts in Ormos's launch directory; use `--cwd /path/to/work` to change it. `--port` defaults to 8481, and the separate loopback preview listener's `--preview-port` defaults to 8482. Local `policy.json` restrictions apply to terminal directories and preview ports; malformed policy files deny access. App previews use a separate browser origin from terminal controls. Some apps restrict embedding; use the preview's new-tab button for those apps.

## Private access through Tailscale

Keep Ormos on loopback and expose both listeners with Tailscale Serve. Substitute your machine's actual Tailscale DNS name:

```sh
ormos ui --cwd /path/to/work \
  --hosts box.example.ts.net:8481,box.example.ts.net:8482 \
  --preview-url https://box.example.ts.net:8482

# In another terminal; preserve any existing Serve routes.
tailscale serve --bg --https=8481 http://127.0.0.1:8481
tailscale serve --bg --https=8482 http://127.0.0.1:8482
```

Visit `https://box.example.ts.net:8481` from a device on the same tailnet with network permission to reach both ports. There is no application login: anyone permitted to reach the listener can control its terminals. Keep this service private and grant access only to trusted devices/users. Ormos rejects foreign browser origins and unlisted proxy hostnames.

Terminals belong to the running Ormos process and end when it stops. Preview selection is shared across tabs in the same browser. `ormos relay` and `ormos --config PATH` remain for legacy compatibility; the former hosted backend is deprecated.
