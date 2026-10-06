<!-- Generated from private documentation source. Do not edit directly. Source SHA256: 8ef8bf6ba4aa5b3e8e284f6357cf6e06caa0566f893ceb112fda987d875baf27 -->

# Ormos

A terminal and local app preview in your browser. One executable serves the SolidJS UI and interactive terminals on your machine. Local use needs no account, hosted backend, or Clerk.

## Install and run

Use Go matching `go.mod` and `.mise.toml`, or a verified archive from this repository’s releases.

```sh
go install github.com/nicodes/ormos@latest
ormos ui
```

Bare `ormos` also starts the local UI. Open `http://127.0.0.1:8481`. Type in the terminal, start a web app, then enter its local port or localhost URL in the preview address bar. Terminal and preview panes sit beside each other on desktop and stack on a phone. The Keyboard button and extra terminal keys support touch devices. Each pane has tabs and a **+** button. Terminal tabs keep independent shells running; closing a terminal tab ends that shell. Preview tabs remember their local port and navigation history, with back, forward and refresh controls. The address bar accepts a port (`3000`), a port with a path (`3000/about`), a localhost HTTP URL, or a path within the current app. Normal links and SPA history update the address bar. Reloading Ormos restores its tabs and reconnects to live terminal sessions. There is no global header or side navigation.

New terminals open automatically at `~`; use `--cwd /path/to/work` to change it. `--port` defaults to 8481, and the separate loopback preview listener's `--preview-port` defaults to 8482. Local `policy.json` restrictions apply to terminal directories and preview ports; malformed policy files deny access. App previews use a separate browser origin from terminal controls. Apps that prohibit embedding may need a separate browser window. Preview navigation tracking requires the app to permit the injected navigation bridge script.

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

Terminals belong to the running Ormos process and end when it stops. Each preview tab keeps its own port and history; only the selected iframe is mounted, and selecting another tab loads its saved location. The proxy's selected port is shared across separate Ormos browser windows, so use one workspace window when previewing different apps. `ormos relay` and `ormos --config PATH` remain for legacy compatibility; the former hosted backend is deprecated.
