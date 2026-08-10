# Security policy

ormos is a relay you run on your own machine, and by design it accepts remote
instructions to open shells and terminals on that machine. A flaw in how it
authenticates or scopes those instructions is a direct path to code execution,
so security reports are taken seriously.

## Reporting a vulnerability

Please report vulnerabilities **privately** — not in a public issue, pull
request, or discussion, where the details would be visible before a fix exists.

Use GitHub's private vulnerability reporting: open the **Security** tab of this
repository and choose **Report a vulnerability**. That opens a private advisory
visible only to you and the maintainers.

Please include:

- what an attacker can do, and the impact;
- the steps or a proof of concept to reproduce it;
- the version or commit you observed it on.

You will get an acknowledgement, and the fix and disclosure will be coordinated
with you through the advisory.

## Supported versions

Fixes are made against the latest release and `main`. There is no long-term
support branch; upgrade to the newest release to receive security fixes.

## Verifying a download

Release archives are published with build-provenance attestations. Before
running a downloaded binary, verify it came from this repository's release
workflow:

```sh
gh attestation verify ormos_Linux_x86_64.tar.gz --repo nicodes/ormos
```
