# ormos

The public system agent for [ormos](https://ormos.dev), a personal remote-access
tool. It runs on a machine you own, makes one outbound encrypted connection to
the hosted relay, and serves terminal sessions and explicitly configured local
ports through that connection.

The agent never opens a listening socket. It does, however, accept authenticated
instructions from the Ormos relay to start shells and connect to local services,
so review the policy section below before leaving it running on an important
machine.

## Run or install

Ormos requires Go 1.25 and currently supports Linux and macOS.

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
| `key` | Long-lived terminal sealing key; mode `0600` |
| `policy.json` | Optional restrictions enforced by this machine |
| `sessions.log` | Local JSON-lines audit trail of relay requests |

`--config /some/path/config.json` moves all four files into that config file's
directory, which is useful for keeping development and production pairings
separate.

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
proxied in plaintext at the relay because HTTP responses must be rewritten and
served to the browser.

A compromised or malicious relay can still request shells and local
connections within the limits of `policy.json`. Run the agent as an
unprivileged user and set explicit roots and ports when that boundary matters.
The local audit log is evidence for the machine owner, but it is not tamper-proof
against a shell running as the same user.

The shared Go package [`relay`](./relay) defines the control DTOs, tunnel
framing, and terminal sealing protocol used by both the public agent and the
private hosted backend.

## Protocol compatibility

The `v0.1` wire format is unchanged from the backend version it was extracted
from. Breaking protocol changes must be rolled out in this order:

1. deploy backend support for both the currently released agent and the new
   format;
2. publish the new agent release;
3. remove old-format support only after the compatibility window.

There is no automatic version negotiation in `v0.1`, so publishing a breaking
agent before compatible server support would strand users of `@latest`.

## Development

```sh
go test -race ./...
go vet ./...
go run . --version
```

The repository is intentionally limited to the public agent and shared
protocol. The hosted backend, application, and deployment configuration are not
part of this repository.
