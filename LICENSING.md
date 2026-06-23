# Licensing

Burrow is **open core**, not "open source" without qualification. The license follows the
package boundary: the client surface is permissively licensed for maximum adoption, and the
control plane — the substantial part and the basis of the managed product — is
source-available under a license that converts to Apache-2.0 over time. See
[ADR-0001](docs/adr/0001-license-and-dco.md) for the decision and reasoning.

**The rule, in one paragraph:** the CLI (`cmd/burrow/`), the MCP server (`mcp/`), and the
module-private shared helpers (`internal/`) are licensed **Apache-2.0**; the control plane
(`controlplane/`, including its binary `cmd/burrowd/`) and the operator (`operator/`) are
licensed **FSL-1.1-ALv2** (Functional Source License 1.1, Apache-2.0 future), which permits
any use except offering a competing product or service and **converts each release to
Apache-2.0 two years after that release ships**. Every `.go` file carries an
`SPDX-License-Identifier` header stating its license, enforced in CI
(`scripts/check-spdx.sh`); when in doubt, the SPDX header on the file is authoritative.

## Per-directory map

| Path | License | What it is |
| --- | --- | --- |
| `cmd/burrow/` | Apache-2.0 | CLI |
| `mcp/` | Apache-2.0 | MCP server (thin, importable translator) |
| `cmd/burrow-mcp/` | Apache-2.0 | MCP server binary |
| `internal/` | Apache-2.0 | module-private shared helpers |
| `controlplane/` | FSL-1.1-ALv2 | control plane: public API (interfaces, App/Release/Policy, constructor) |
| `controlplane/internal/` | FSL-1.1-ALv2 | control plane implementation guts |
| `cmd/burrowd/` | FSL-1.1-ALv2 | control plane binary |
| `operator/` | FSL-1.1-ALv2 | operator: CRD types, reconciler entry |
| `operator/internal/` | FSL-1.1-ALv2 | operator implementation guts |

The FSL packages are deliberately **not** placed under the top-level `internal/`, so a
separate private module (the managed product) can import their public API.

## License files

- Root [`LICENSE`](LICENSE) — Apache-2.0 (the repository default and the license of the
  client surface).
- Root [`LICENSE.FSL`](LICENSE.FSL) — the FSL-1.1-ALv2 text.
- [`controlplane/LICENSE`](controlplane/LICENSE) and [`operator/LICENSE`](operator/LICENSE)
  — the FSL-1.1-ALv2 text governing those trees.

## Direction of travel

This is a deliberate **starting** posture, not a permanent stance. FSL is the reversible
direction: each release opens to Apache-2.0 on its second anniversary automatically, and as
the sole copyright holder the maintainer can relicense more permissively at any time.
Starting protected and opening later is easy; the reverse is not.

## Commercial licenses

Organizations that need terms without the FSL competing-use restriction can obtain a
commercial license — see [COMMERCIAL.md](COMMERCIAL.md). Selling these grants is only
possible while the maintainer holds 100% of the copyright, which is why outside code is
handled the way [CONTRIBUTING.md](CONTRIBUTING.md) describes.
