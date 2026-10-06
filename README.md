<!-- Generated from private documentation source. Do not edit directly. Source SHA256: efd42a7e7e643f1da6734399072632bc099c898a672c7f08ef4cd47c92632448 -->

# Ormos

A terminal and local app preview in your browser. One executable serves the SolidJS UI and interactive terminals on your machine. Local use needs no account, hosted backend, or Clerk.

## Install and run

Use Go matching `go.mod` and `.mise.toml`, or a verified archive from this repository’s releases.

```sh
go install github.com/nicodes/ormos@latest
ormos ui
```

Bare `ormos` also starts the local UI. Open `http://127.0.0.1:8481`. A single shared header switches between a full-height terminal and local app preview. In terminal view, click the preview icon at the left to show your app; in preview view, click the terminal icon to return to your shells. Switching uses a short opacity fade in both directions, skipped when reduced motion is requested. Both keep running when hidden, and the selected view is remembered after a reload. A fresh workspace starts in terminal view.

The terminal header has tabs and a **+** button. Each tab keeps an independent shell running. Only the selected tab shows its editable name and close button. Click its name to edit directly: Enter or leaving the field saves, Escape cancels, and an empty name keeps the previous name. Names are limited to 24 characters, display in full, and survive reloads without restarting shells. Selecting a tab does not change its width. Closing a tab ends only that shell. The right-hand hamburger has three icon tabs. The keyboard tab provides Keyboard, Esc, Tab, Ctrl C, Ctrl D and arrow-key controls. The bookmark tab lists saved commands with their titles and command text; Add command, edit and delete manage them. Click a saved command’s title or text to send it to the active terminal and execute it. The speech-bubble tab provides a separate saved-prompts list with the same create, edit and delete controls. Click a saved prompt’s title or text to paste it into the active terminal without pressing Enter. Both lists show borderless, transparent edit/delete icons beside the title and a single-line text preview with an ellipsis; editing reveals the full text. Commands and prompts are saved in this browser, not shared between devices. Escape or clicking outside dismisses the popup.

The preview header has one URL bar and a right-hand hamburger with Back, Forward, Refresh and Open in new tab. Enter a local port (`3000`), a port with a path (`3000/about`), a localhost HTTP URL, or a path within the current app. Focusing the URL bar shows a small, scrollable recent-history dropdown, filtered by the text you type. Clearing the field shows all recent addresses, newest first. Click an entry to visit it, or use Up/Down and Enter. Ports listening on the server have green dots; other or unavailable statuses use gray. A green dot does not certify access from another device. With no recent history, a blank field prompts “Enter a port”; unmatched text shows “No matching history.” Before opening an address, the preview shows a centered introduction with only a title and subtitle. Empty messages are centered horizontally and vertically. History stays in this browser. Back and Forward follow addresses entered in Ormos; links and SPA navigation inside the app do not update its address bar. Open in new tab opens the current preview in a separate browser tab. Reloading restores the preview address/history and reconnects to live terminal tabs. There is no sidebar, account login or separate global header.

New terminals open automatically at `~`; use `--cwd /path/to/work` to change it. Ormos has one listener, defaulting to `127.0.0.1:8481`. Local `policy.json` restrictions apply to terminal creation and attachment; malformed policy files deny terminal access. Preview URLs load directly on the same hostname as Ormos, at the selected app port, using the workspace's HTTP or HTTPS scheme. Localhost URLs are shorthand for that machine, not the phone. The Ormos port is rejected as a preview target. Apps that prohibit embedding can be opened with Open in new tab.

## Private access through Tailscale

Keep Ormos on loopback and expose its UI privately with Tailscale Serve. Expose each localhost app separately, or bind the app to the machine's Tailscale IP. The following example uses HTTP and a placeholder MagicDNS name:

```sh
ormos ui --hosts box:8481

# In another terminal; preserve any existing Serve routes.
tailscale serve --bg --http=8481 http://127.0.0.1:8481

# For an app listening only on localhost:3000:
tailscale serve --bg --http=3000 http://127.0.0.1:3000
```

Visit `http://box:8481` from a device on the same tailnet, then enter `3000` to load `http://box:3000` directly. Network access rules must permit both ports. Tailscale encrypts the connection between devices. HTTPS setups need the full Tailscale DNS name and HTTPS exposure for both UI and apps.

There is no application login: anyone permitted to reach the UI can control its terminals as its operating-system user. Keep it private and limit network access to trusted users/devices. Ormos rejects foreign origins for terminal mutations and WebSocket upgrades, and unlisted UI hostnames. Terminal controls cannot be embedded in an iframe. Direct previews use a different port/origin from the UI and are sandboxed.

Ormos does not forward app requests, inject scripts, or select ports through a cookie. Each window's iframe connects independently. App access is governed by its listener configuration, Tailscale rules and the app itself; Ormos's legacy port policy does not control direct browser access. Terminals belong to the running Ormos process and end when it stops. `ormos relay` and `ormos --config PATH` remain for legacy compatibility; the former hosted backend is deprecated.

In the terminal popup, the plus icon at the right of the tabs adds a command or prompt to the selected list. While editing, it becomes a checkmark that saves the item; switching tabs or closing the popup discards unsaved changes. It is hidden on the keyboard tab.

Saved command and prompt lists use plain rows without card outlines or a storage-caption row. Empty lists show a centered “No saved commands” or “No saved prompts” title and a short subtitle.

Terminal content fills the pane below the shared header with a small 6-pixel inset and no frame border.

Before loading an app, the browser makes a credential-free HEAD reachability check, with a five-second timeout. Connection failure shows a centered “App unavailable” title and a short subtitle to expose the port through Tailscale and refresh. This check does not certify that embedding is permitted; use Open in new tab if the app blocks iframes. Refresh checks the address again and reloads it. Navigation history retains up to 200 entered addresses; recent history retains 50 unique entries.

The terminal uses a blinking light-gray vertical-bar cursor.
