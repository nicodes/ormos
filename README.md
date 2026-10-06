<!-- Generated from private documentation source. Do not edit directly. Source SHA256: 75526d65f67f1bf839e5ee56906bc86769400eceb35490208a23c62656ac0bf2 -->

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

The preview header has one URL bar and a right-hand hamburger with Back, Forward, Refresh and Open in new tab. Enter a local port (`3000`), a port with a path (`3000/about`), a localhost HTTP URL, or a path within the current app. Focusing the URL bar shows a small, scrollable recent-history dropdown, filtered by the text you type. Clearing the field shows all recent addresses, newest first. Click an entry to visit it, or use Up/Down and Enter. Listening ports have green dots; other or unavailable statuses use gray. With no recent history, a blank field prompts “Enter a port”; unmatched text shows “No matching history.” Before opening an address, the preview shows a centered introduction with a port example. Empty messages are centered horizontally and vertically. History stays in this browser. Normal links and SPA history update the address bar. Open in new tab opens the current preview in a separate browser tab. Reloading restores the preview address/history and reconnects to live terminal tabs. There is no sidebar, account login or separate global header.

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

In the terminal popup, the plus icon at the right of the tabs adds a command or prompt to the selected list. While editing, it becomes a checkmark that saves the item; switching tabs or closing the popup discards unsaved changes. It is hidden on the keyboard tab.

Saved command and prompt lists use plain rows without card outlines or a storage-caption row. Empty lists show a centered “No saved commands” or “No saved prompts.”

Terminal content fills the pane below the shared header with a small 6-pixel inset and no frame border.

When the selected port has no reachable app, the preview displays a dark, centered “No app listening” page with the port number and a short instruction to start the app and refresh. The response is not cached, so Refresh can pick up an app once it starts.
