<!-- Generated from private documentation source. Do not edit directly. Source SHA256: c1ed73782512c913e4adf6d43f734df7f01ea1fea0b89526494f72f574652a73 -->

# Ormos

A terminal and local app preview in your browser. One executable serves the SolidJS UI and interactive terminals on your machine. Local use needs no account, hosted backend, or Clerk.

## Install and run

Use Go matching `go.mod` and `.mise.toml`, or a verified archive from this repository’s releases.

```sh
go install github.com/nicodes/ormos@latest
ormos ui
```

Bare `ormos` also starts the local UI. Open `http://127.0.0.1:8481`. Type in the terminal, start a web app, then enter its local port or localhost URL in the preview address bar. The preview sits above the terminal on both desktop and phone. The eye button at the left of each pane’s header collapses or expands that pane; its header remains available and the other pane fills the freed space. Collapsing preserves running shells and the current preview, and the choice is remembered after a reload. The hamburger button at the right of the terminal header opens Keyboard, Esc, Tab, Ctrl C, Ctrl D and arrow-key controls for the active terminal. The menu closes with Escape or a click outside it. The terminal has tabs and a **+** button. Terminal tabs keep independent shells running; closing a terminal tab ends that shell. The preview has one URL bar at the top, with back, forward and refresh controls. The icon at the right of the URL bar opens the current preview in a separate browser tab. The address bar accepts a port (`3000`), a port with a path (`3000/about`), a localhost HTTP URL, or a path within the current app. Normal links and SPA history update the address bar. Reloading Ormos restores the preview address/history and terminal tabs, reconnecting to live terminal sessions. There is no global header or side navigation.

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

Terminals belong to the running Ormos process and end when it stops. The preview keeps one address and navigation history. The proxy's selected port is shared across separate Ormos browser windows, so use one workspace window when previewing different apps. `ormos relay` and `ormos --config PATH` remain for legacy compatibility; the former hosted backend is deprecated.
