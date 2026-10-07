<!-- Generated from private documentation source. Do not edit directly. Source SHA256: b58d63c12daf52ee222f4247a616d242367cb2aa00a543d7899d05c1b36056fe -->

# Ormos

A terminal and local app preview in your browser. One executable serves the SolidJS UI and interactive terminals on your machine. Local use needs no account, hosted backend, or Clerk.

Automatic localhost preview forwarding requires v0.2.3 or later. Older releases use direct app previews.

## Install and run

Install the latest published prebuilt release on Linux or macOS (x86_64 or arm64):

```sh
curl -fsSL https://github.com/nicodes/ormos/raw/main/install.sh | sh
```

The installer detects your platform, checks the archive's SHA-256 digest and binary version, and installs to `~/.local/bin` without sudo. Go and Node.js are not needed. It adds the standard user bin directory to Bash/Zsh startup files when absent from PATH; open a new terminal afterward, or use `~/.local/bin/ormos` immediately. It does not start Ormos, install a service, or change Tailscale configuration. Run the same command again to update; restart a running Ormos process when ready to use the new binary.

To pin a version or choose another writable directory:

```sh
curl -fsSL https://github.com/nicodes/ormos/raw/main/install.sh | sh -s -- --version 0.2.1
curl -fsSL https://github.com/nicodes/ormos/raw/main/install.sh | sh -s -- --install-dir "$HOME/bin"
```

The short form is `-v 0.2.1`. Omitting a version uses `latest`; `--version latest` and `-v latest` also work. A leading `v` on a version is optional. Custom install directories must be on your PATH. The installer is also included in release assets starting with v0.2.0. The terminal UI requires v0.2.0 or later; automatic preview forwarding requires v0.2.3 or later. You can also manually extract a platform archive from [Releases](https://github.com/nicodes/ormos/releases), or use Go matching `go.mod` and `.mise.toml`:

```sh
go install github.com/nicodes/ormos@latest
```

Start the UI:

```sh
ormos ui
```

Bare `ormos` also starts the local UI. Open the address printed at startup. The default is `http://127.0.0.1:4242` in v0.2.1 and later; v0.2.0 defaults to port 8481. Set a port explicitly with `ormos ui --port 4242` (or another port from 1 through 65535). A single shared header switches between a full-height terminal and local app preview. In terminal view, click the preview icon at the left to show your app; in preview view, click the terminal icon to return to your shells. Switching uses a short opacity fade in both directions, skipped when reduced motion is requested. Both keep running when hidden, and the selected view is remembered after a reload. A fresh workspace starts in terminal view.


## Home-screen installation (next release)

PWA installation support is prepared for the next release; v0.2.5 does not include it. Open your Ormos workspace over HTTPS on your phone. On iPhone, use Safari’s Share menu and choose **Add to Home Screen**; on Android, use your browser’s **Install app** or **Add to Home screen** option. Launching the icon opens Ormos in a standalone window on supporting browsers. Installation options vary by browser. Plain HTTP on a remote hostname does not meet PWA installation requirements; loopback HTTP is supported for local development.

The installed app remains a client of the machine running Ormos. That machine must be reachable and running; private-network setups still need their network connection. No service worker or offline cache is registered. Existing terminal reconnect/history behavior applies, and operating-system suspension may disconnect the client. Keep using the same workspace URL: changing its hostname or port changes browser storage and app identity. Home-screen and browser storage sharing varies by platform, so saved commands or prompts may need recreating in the installed app.

## Terminal and preview controls

The terminal header has tabs and a **+** button. Each tab keeps an independent shell running. Only the selected tab shows its editable name and close button. Click its name to edit directly: Enter or leaving the field saves, Escape cancels, and an empty name keeps the previous name. Names are limited to 24 characters, display in full, and survive reloads without restarting shells. Selecting a tab does not change its width. Closing a tab ends only that shell. The right-hand hamburger has three icon tabs. The keyboard tab provides Keyboard, Esc, Tab, Enter, Ctrl C, Ctrl D and arrow-key controls. The bookmark tab lists saved commands with their titles and command text; Add command, edit and delete manage them. Click a saved command’s title or text to send it to the active terminal and execute it. The speech-bubble tab provides a separate saved-prompts list with the same create, edit and delete controls. Click a saved prompt’s title or text to paste it into the active terminal without pressing Enter. Both lists show only a borderless Edit icon beside the title, with a closely spaced single-line text preview and an ellipsis. Editing reveals the full text; Delete is at the bottom left and Save at the bottom right below the inputs. New items have only Save. Commands and prompts are saved in this browser, not shared between devices. Escape or clicking outside dismisses the popup.

The preview header has one URL bar and a right-hand hamburger with Back, Forward, Refresh and Open in new tab. Enter a local port (`3000`), a port with a path (`3000/about`), a localhost HTTP URL, or a path within the current app. Focusing the URL bar shows a small, scrollable recent-history dropdown, filtered by the text you type. Clearing the field shows all recent addresses, newest first. Click an entry to visit it, or use Up/Down and Enter. Ports listening on the server have green dots; other or unavailable statuses use gray. A green dot does not certify access from another device. With no recent history, a blank field prompts “Enter a port”; unmatched text shows “No matching history.” Before opening an address, the preview shows a centered introduction with only a title and subtitle. Empty messages are centered horizontally and vertically. History stays in this browser. Back and Forward follow addresses entered in Ormos; links and SPA navigation inside the app do not update its address bar. Open in new tab opens the current preview in a separate browser tab. Reloading restores the preview address/history and reconnects to live terminal tabs. There is no sidebar, account login or separate global header.

New terminals open automatically at `~`; use `--cwd /path/to/work` to change it. The Ormos UI defaults to `127.0.0.1:4242`; each preview gets a separate loopback forwarding listener. Local `policy.json` restrictions apply to terminal creation and attachment; malformed policy files deny terminal access. Enter the app's local HTTP port. Ormos forwards requests through a separate preview origin using the workspace's HTTP or HTTPS scheme, with localhost Host/Origin headers for compatible development servers. WebSocket live reload, streaming responses and asset paths are forwarded without rewriting HTML. Localhost URLs are shorthand for that machine, not the phone. The Ormos port is rejected as a preview target. Apps that prohibit embedding can be opened with Open in new tab.

## Private access through Tailscale

Keep Ormos on loopback and expose its UI privately with Tailscale Serve. This is a one-time UI setup; app ports are handled automatically. Use HTTPS for the workspace to support apps requiring a secure browser context, including Godot web exports. Replace the example full DNS name below with your machine’s Tailscale DNS name:

```sh
ormos ui --port 4242 --hosts box.example.ts.net:4242

# In another terminal; preserve any existing Serve routes.
tailscale serve --bg --https=4242 http://127.0.0.1:4242
```

Visit `https://box.example.ts.net:4242` from a device on the same tailnet. Start your app normally in the terminal, then enter its local port, such as `3000`, in Preview. Ormos checks that it is listening on localhost, opens a forwarding listener and automatically exposes that listener privately through Tailscale Serve. There is no per-app setup command or routine Vite allowed-host configuration. Existing Serve routes are preserved. Each app gets an unused public preview port separate from its local app port, so development servers can restart on their normal ports. The URL bar keeps displaying the local app port. Network access rules must permit the workspace and preview ports. Local browser use on localhost requires no Tailscale setup.

There is no application login: anyone permitted to reach the UI can control its terminals as its operating-system user. Keep it private and limit network access to trusted users/devices. Ormos rejects foreign origins for terminal mutations and WebSocket upgrades, and unlisted UI hostnames. Terminal controls cannot be embedded in an iframe. Forwarded previews use a different port/origin from the UI and are sandboxed.

Tailscale must be installed and connected on the machine, and the user running Ormos must have permission to manage Serve. This permission is configured once for that operating-system user; Ormos does not elevate privileges. Existing routes are preserved, and public Funnel routes are never created. Only routes Ormos creates are owned by its foreground CLI sessions; those routes end when Ormos shuts down normally. Apps keep running independently, and existing Serve routes remain configured. At most 32 temporary app routes are retained per Ormos process.

Ormos forwards app requests only to a fixed localhost destination per preview. It does not inject scripts or select ports through a cookie. Each window's iframe connects independently. App access is governed by its listener configuration, Tailscale rules and the app itself; Local `policy.json` port rules control preview forwarding and automatic exposure and deny sensitive ports by default; unreadable policy fails closed, and existing previews recheck policy on every request. These rules do not restrict an app's preexisting direct network access. Terminals belong to the running Ormos process and end when it stops. `ormos relay` and `ormos --config PATH` remain for legacy compatibility; the former hosted backend is deprecated.

In the terminal popup, the plus icon at the right of the tabs adds a command or prompt to the selected list. It is hidden while editing and on the keyboard tab. Save uses a checkmark below the inputs; switching tabs or closing the popup discards unsaved changes.

Saved command and prompt lists use plain rows without card outlines or a storage-caption row. Empty lists show a centered “No saved commands” or “No saved prompts” title and a short subtitle.


The next release adds a one-shot **Shift** control beside Keyboard. Tap Shift, then open the keyboard and type `s` to send `S`; the modifier turns off after one key. Tap Shift again to cancel. Shift also works with Tab (back-tab), arrows and standard US punctuation. Pasted prompts and multi-character commands are unchanged. Changing tabs, switching views or disconnecting clears Shift. Narrow screens can scroll the quick-control row horizontally while keeping normal touch targets.

The next release also restores only the selected terminal tab on a fresh page load. Other shells keep running on the host and load their retained history when first selected; visited tabs stay mounted afterward. Refresh still replays the full retained history of the selected tab, up to 4 MiB, rather than loading older lines when scrolling. Replay is streamed in 64 KiB pieces and compressed when the browser supports it; small interactive output stays uncompressed. Input and resize messages can be processed while replay transfers. Reconnects continue requesting only missed bytes. Content-hashed UI assets are cached between refreshes; HTML and the manifest revalidate so upgrades discover the latest files. A long first replay can still take time to parse on slower phones.

Terminal content fills the pane between the shared header and a bottom quick-control row, with a small 6-pixel inset and no frame border. The bottom row has Keyboard on the left, Esc, Tab, Up, Down and Enter in the middle, and Interrupt (Ctrl+C) on the right. Keyboard, Tab, Enter and arrows use icons with accessible labels and tooltips; Esc and Ctrl+C have text labels. Tap Keyboard to open the software keyboard; tapping or swiping terminal output does not open it. Shortcut buttons send keys without opening the keyboard. These quick controls and the updated saved-item editor layout require v0.2.4 or later. v0.2.5 replaces the square interrupt icon with Ctrl+C text and puts Esc before Tab.

Before loading an app, Ormos prepares its forwarding route and the browser makes a credential-free HEAD reachability check, bounded by a 20-second overall timeout. Failure shows a centered “App unavailable” title and a short subtitle identifying the next step. This check does not certify that embedding is permitted; use Open in new tab if the app blocks iframes. Refresh checks the address again and reloads it. Navigation history retains up to 200 entered addresses; recent history retains 50 unique entries.

The terminal uses a blinking light-gray vertical-bar cursor.
