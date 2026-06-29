# Licensing

All of Burrow's code in this repository is licensed under **Apache-2.0** — the CLI
(`cmd/burrow/`), the MCP server (`mcp/`), the control plane (`controlplane/`, `cmd/burrowd/`),
the operator (`operator/`), and the shared helpers (`internal/`). Read, modify, self-host, and
integrate against any of it freely. See
[ADR-0033](docs/adr/0033-relicense-to-apache.md) for the decision and reasoning.

Burrow is **open core**: the code here is open source under Apache-2.0 and fully
self-hostable, while the multi-tenant **managed cloud** and the **enterprise tier** (SSO/SAML,
teams and organizations, advanced RBAC, compliance, support with an SLA) are separate,
proprietary products that do **not** live in this repository — see
[COMMERCIAL.md](COMMERCIAL.md).

Every `.go` file carries an `SPDX-License-Identifier: Apache-2.0` header above its copyright
line, enforced in CI (`scripts/check-spdx.sh`); the SPDX header on the file is authoritative.

## License file

- Root [`LICENSE`](LICENSE) — Apache-2.0, governing the entire repository.

## Layout note

The control plane and operator are kept **out of the top-level `internal/`** so a separate
private module (the managed product) can import their public API — a module boundary, not a
license boundary.

## Contributions

Burrow is authored under sole copyright ownership; outside code contributions are CLA-gated and
every commit carries a DCO sign-off, as described in [CONTRIBUTING.md](CONTRIBUTING.md).
