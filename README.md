<!-- Generated from private documentation source. Do not edit directly. Source SHA256: 8a042cd70a0b3dde1d903eaa8305eb0cf3c3e711ac91fe4ceb97d85425d30a9e -->

# Ormos

A local web UI for inspecting this machine and managing policy-allowed local terminals. The former hosted backend is deprecated. Local UI use has no account system and does not use Clerk.

## Install and run

Use Go matching `go.mod` and `.mise.toml`, or a verified archive from this repository’s releases.

```sh
go install github.com/nicodes/ormos@latest
ormos ui
```

The default listener is `127.0.0.1:8481`. Use `ormos ui --port 9000` to select a port. The UI can open and kill policy-allowed terminals and view their output; interactive input is not part of the current local UI contract. Keep the listener on loopback unless you deliberately configure trusted network access. Legacy relay code remains for compatibility and is not a hosted-service availability claim.
