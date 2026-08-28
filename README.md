# ormos

The public system agent for [ormos](https://ormos.dev), a personal remote-access
tool. It runs on a machine you own, makes one outbound encrypted connection to
the hosted relay, and serves terminal sessions and explicitly configured local
ports through that connection.

The agent never opens a listening socket. It does, however, accept authenticated
instructions from the Ormos relay to start shells and connect to local services,
so review the policy section below before leaving it running on an important
machine.

## Supported platforms

Linux and macOS, on `amd64` and `arm64`. That is the whole list: CI tests and
releases ship exactly those four targets, and nothing else is claimed as
supported.

There is no Windows build and none is planned. The agent's job is to act on the
machine it runs on — allocate a PTY, poll it, read a terminal's foreground
process group, signal a shell that will not exit — which it does through
`golang.org/x/sys/unix`. The root package and every file in `internal/system`
carry `//go:build (linux && !android) || (darwin && !ios)`, so `go build .` for
Windows reports that the package does not exist there rather than failing on a
list of missing syscalls.

The tag names the platforms rather than saying `unix`, which would also claim
the BSDs and Solaris. Nothing is tested there, so nothing is claimed there. The
`!android` and `!ios` halves matter for the same reason: Go sets the `linux` tag
on Android and the `darwin` tag on iOS, so a plain `linux || darwin` would build
the whole agent for both. The shared [`relay`](./relay) package is untagged and stays portable —
the hosted backend imports it.

CI tests on Linux and macOS, cross-compiles every shipped target on Linux, and
asserts that on every other platform Go supports the agent is not selected at
all. A release is gated on the
macOS tests too, since the Darwin archives are what people download.

On Linux and macOS, live port status reports TCP listeners bound to a loopback
or wildcard address. Linux reads `/proc/net/tcp*`; macOS uses the base-system
`netstat`. Before the agent discloses that listing to the relay, it applies the
same local port policy that governs proxy connections.

## Run or install

Ormos requires Go 1.25.

Run the latest release directly:

```sh
go run github.com/nicodes/ormos@latest
```

Or install it:

```sh
go install github.com/nicodes/ormos@latest
ormos
```

Prebuilt Linux and macOS archives, checksums, and build provenance are attached
to each [GitHub release](https://github.com/nicodes/ormos/releases). Verify a
download before running it:

```sh
gh attestation verify ormos_Linux_x86_64.tar.gz --repo nicodes/ormos
```

On first run, Ormos shows a short pairing code and a URL. Approve the code in
the web app and the machine registers itself, saving only the resulting pairing
token. No credentials are typed into the terminal, and none are accepted as
flags or environment variables. Later runs reconnect using the saved pairing.

```text
ormos                    run the system
ormos --config PATH      use an isolated config and state directory
ormos --help             show help
ormos --version          print the version
```

The production relay is `wss://api.ormos.dev`. Set `ORMOS_API_URL` to use
another relay; plaintext URLs are accepted only for loopback development.

## Local state and policy

State lives under `~/.config/ormos/`:

| File | Purpose |
| --- | --- |
| `config.json` | Pairing token and system identity; mode `0600` |
| `identity.key` | Long-lived terminal sealing key; mode `0600` |
| `policy.json` | Optional restrictions enforced by this machine |
| `sessions.log` | Local JSON-lines audit trail of relay requests |
| `sessions.log.1` | The previous generation of that trail, kept across one roll |

`--config /some/path/config.json` moves all of these into that config file's
directory, which is useful for keeping development and production pairings
separate.

`sessions.log` is append-only but not unbounded: at 4 MiB it is renamed to
`sessions.log.1`, replacing any previous generation, and a fresh log starts. Two
files of recent history, never more. The roll is taken under a file lock, so two
agents sharing one state directory cannot roll each other's history away; when
the lock cannot be taken the roll waits for an uncontended write rather than
renaming unlocked — on a filesystem with no flock support at all, that means
the log grows past the bound. The live file's mode is corrected on every open;
the rolled file's is corrected once at startup and inherited from the live file
at each roll.

Whenever the agent reads `config.json` or `identity.key` it re-checks what it
finds there, so a copy restored from a backup or loosened by a stray `chmod`
does not quietly stay readable by other local users:

- group and other permission bits are cleared from the file, and from
  `~/.config/ormos` itself — a writable state directory would let another local
  user replace these files rather than read them;
- a symbolic link at either path is refused rather than followed. If you keep
  `config.json` in a dotfiles repo, point `--config` at the real file instead of
  symlinking it into place — a link is now a startup error, not a warning;
- a named pipe or directory at either path is refused too, rather than blocking
  the agent forever on an open that never returns;
- a file owned by another user is refused outright. Adopting a planted
  `identity.key` would publish the planter's public key as this machine's and
  seal every terminal to a key they hold.

A correction to either file is reported on startup, because tightening a mode
does not undo the exposure. For `identity.key` the seal has no forward secrecy,
so whoever read it can decrypt captured traffic from past sessions and the only
real remedy is regenerating the key. For `config.json` the pairing token in it
is a bearer credential, so the remedy is signing out and pairing again.

The state-directory lock only coordinates stock agents on this host that use the
same directory. Copying `config.json` and its pairing token to another host is a
bearer-token compromise, not something this local lock can protect against. The
corresponding backend deployment must also reject a lifecycle reset while the
current tunnel is registered; this local lock is not a substitute for that
remote safeguard.

The agent also retains a bounded generation fence for up to 1,024 terminal
record IDs per process lifetime; exceeding that ceiling fails closed and requires
an agent restart.

Owner bits are left as they are, so a key deliberately made `0400` stays `0400`.

An optional policy can limit what the relay may ask this machine to do:

```json
{
  "allowedRoots": ["~/code"],
  "allowedPorts": [3000, 5173],
  "deniedPorts": [],
  "terminalsDisabled": false
}
```

- `allowedRoots` confines terminal working directories after resolving
  symlinks. An empty list allows any directory.
- `allowedPorts`, when non-empty, is the only set of local ports the agent will
  connect to. Otherwise built-in rules still reject privileged and well-known
  service ports.
- `deniedPorts` always wins.
- `terminalsDisabled` refuses terminal sessions while retaining port previews.

Policy changes are re-read while the agent is running. The relay's configured
port list is an additional check, not a replacement for local policy.

## Security model

The agent holds a per-system pairing token and an X25519 terminal key. Terminal
frames are sealed end to end between the browser and agent; the relay carries
the ciphertext but cannot read terminal contents. Port-preview traffic is
proxied in plaintext at the relay, and this is structural, not a missing
feature: the server terminates TLS to the browser's iframe, so it must hand
over plaintext however the bytes arrived from the agent. Terminals can be
sealed because xterm.js is JavaScript running in the page and can hold a key;
an iframe has no such hook — the browser's own HTTP stack loads the document
and its sub-resources.

A compromised or malicious relay can still request shells and local
connections within the limits of `policy.json`. Run the agent as an
unprivileged user and set explicit roots and ports when that boundary matters.
The local audit log is evidence for the machine owner, but it is not tamper-proof
against a shell running as the same user.

The shared Go package [`relay`](./relay) defines the control DTOs, tunnel
framing, and terminal sealing protocol used by both the public agent and the
private hosted backend.

## Protocol compatibility

The tunnel handshake negotiates `X-Ormos-Stream-Fence-Version`. A genuinely
absent header identifies released v0 agents; explicit values `1`, `2`, `3`, and
`4` identify the corresponding contracts. Empty, duplicate, comma-joined,
malformed, and unknown values are refused rather than interpreted as a legacy
downgrade. Backends must inspect all header values, not only the first one.

V4 terminal actions are keyed directly by authenticated system ID, durable
terminal record ID, and exact lifecycle generation. Their requested working
directory and existing action fence/expiry are bound into the sealed stream key
schedule. V4 proxy actions are system-scoped by authenticated system ID and
numeric port. Project/session composites and human-readable project, terminal,
or port labels do not route or authorize either action. V3 retains its released
project-composite terminal identity during the compatibility window.

Protocol changes must be rolled out in this order:

1. deploy backend support for both the currently released agent and the new
   format;
2. publish the new agent release;
3. remove old-format support only after the compatibility window.

V0-v3 support may be retired only in a later coordinated change after the
compatibility window; adding v4 does not itself change those versions' behavior.

## Development

```sh
go test -race ./...
go vet ./...
go run . --version
go run . --protocol-version
```

The repository is intentionally limited to the public agent and shared
protocol. The hosted backend, application, and deployment configuration are not
part of this repository.
