# ADR-0001: License, contribution policy, and DCO

## Status

**Accepted.**

The maintainer's decision is recorded below: a split license (Option B) on a per-package
boundary, paired with sole copyright ownership, a dual-licensing (sell-exceptions)
business model, and a contribution policy in which outside *code* is CLA-gated while DCO
sign-off remains required on all commits for provenance.

## Context

Burrow is an open-core project. This repository is the open core: the single-tenant
control plane, the operator, the MCP server, and the CLI, packaged so a developer can
self-host the whole thing. A separate, private product — the multi-tenant managed cloud
(billing, teams, dashboard, SSO) — is *not* in this repository.

Three facts shape the decision:

1. **The MCP server and the CLI are thin client pieces, and for them adoption is
   everything.** They are valuable in proportion to how many agents and developers use
   them. Any friction — license review, copyleft, "can my company use this?" — directly
   suppresses the thing that makes them worth building. This argues for the most permissive
   license available (Apache 2.0).

2. **The control plane and operator are the substantial engineering effort and the
   technical basis of the future managed business.** A permissive license there lets a
   well-resourced competitor stand up a managed Burrow-as-a-service in direct competition,
   contributing nothing back — the failure mode source-available licenses such as the
   Business Source License (BSL) and the Functional Source License (FSL) exist to prevent.
   These licenses let anyone read, modify, and self-host the code and forbid only offering
   it as a competing hosted service; both convert to a fully open license after a fixed
   delay (FSL: 2 years to Apache 2.0).

3. **The maintainer has two distinct commercial goals, and the second one drives the
   contribution policy.** The first is to *reopen* the core over time (the FSL conversion).
   The second is to **sell commercial license grants** to organizations that want terms
   without the FSL competing-use restriction — the "sell exceptions" model. Selling
   exceptions is only possible while one party owns **100% of the copyright**. The moment
   outside code is merged under a bare sign-off, copyright is split and that option is
   forfeited for that code.

A single license across the whole repository cannot optimize both the
adoption-maximizing client surface and the business-protecting core. And the standard
"DCO, merge community PRs freely" posture is incompatible with goal 3.

## Decision

### License — Option B, split on the package boundary

- **Apache-2.0** on the client surface: the MCP server (`mcp/`, `cmd/burrow-mcp/`), the CLI
  (`cmd/burrow/`), and the module-private shared helpers (`internal/`).
- **FSL-1.1-ALv2** (Functional Source License 1.1, Apache 2.0 future) on the product: the
  control plane (`controlplane/`, `cmd/burrowd/`) and the operator (`operator/`). Each
  release converts to Apache-2.0 on its second anniversary.

The **license boundary follows the package boundary**, and the FSL packages are
deliberately kept **out of the top-level `internal/`** so a separate private module (the
managed product) can import their public API:

| Path | License |
| --- | --- |
| `cmd/burrow/` | Apache-2.0 |
| `mcp/`, `cmd/burrow-mcp/` | Apache-2.0 |
| `internal/` | Apache-2.0 |
| `controlplane/`, `controlplane/internal/`, `cmd/burrowd/` | FSL-1.1-ALv2 |
| `operator/`, `operator/internal/` | FSL-1.1-ALv2 |

The root `LICENSE` stays Apache-2.0 (the repository default and the client-surface
license); `LICENSE.FSL` holds the FSL text, mirrored as `controlplane/LICENSE` and
`operator/LICENSE`. Every `.go` file carries an `SPDX-License-Identifier` header matching
its directory, enforced in CI. The per-directory rule is documented in
[LICENSING.md](../../LICENSING.md).

This is recorded as a **deliberate starting posture, not a permanent stance.** FSL is the
reversible direction: each release auto-opens to Apache-2.0 after two years, and as sole
copyright holder the maintainer can relicense more permissively at any time. Starting
protected and opening later is easy; the reverse is not.

### Ownership and contribution policy — sole ownership, dual licensing

To keep the sell-exceptions model open, the maintainer **is and intends to remain the sole
copyright holder** of Burrow. The contribution policy follows from that:

- **Community input is welcome, unrestricted, and does not affect ownership.** Issues,
  discussions, design feedback, bug reports, and reproductions are the primary and
  fully-open way to contribute.
- **Outside code is not merged under the DCO alone.** A DCO leaves copyright with the
  contributor, which would split ownership and forfeit commercial relicensing of that code.
  Outside code is therefore **declined, or accepted only under a CLA** that grants the
  maintainer relicensing and sublicensing rights.
- **DCO sign-off (`git commit -s`) remains required on all commits** as a provenance
  attestation. It is necessary but, for outside code, not sufficient — the CLA is the gate.

This reverses the DCO-over-CLA conclusion an earlier draft of this ADR reached: for
*outside code*, a CLA is required precisely because sole ownership is a stated goal. DCO
remains the universal sign-off mechanism. See [CONTRIBUTING.md](../../CONTRIBUTING.md) and
[COMMERCIAL.md](../../COMMERCIAL.md).

### Naming

Burrow is described publicly as **"open core,"** never "open source" without
qualification — most of the surface is Apache-2.0, but the control plane and operator are
source-available until their FSL conversion. Honest framing is required by
[ADR-0009](0009-honest-status.md), and the README and LICENSING.md adhere to it.

## Why this combination (reasoning)

- **The split matches the economics.** Adoption runs through the client surface, so
  Apache-2.0 there removes all friction. Engineering value and resale risk concentrate in
  the control plane and operator, so FSL protects exactly those.
- **FSL over BSL** for the protected packages: FSL is purpose-built for this open-core
  shape, has a shorter and more readable text than BSL's parameterized template, and has a
  fixed, well-understood 2-year conversion to Apache-2.0 — a more community-legible promise
  than BSL's common 4-year window. Its single carve-out ("any use except a competing
  product/service") is the narrowest restriction that still protects the business.
- **CLA-gated outside code is required by goal 3, not by preference.** The sell-exceptions
  model is load-bearing for the business and depends on sole copyright; a bare-DCO
  open-contribution model is simply incompatible with it. Community input stays fully open,
  so the cost lands only on outside *code*, which is the narrowest place it can land.

## Consequences

- The repository carries Apache-2.0 for the client surface and FSL-1.1-ALv2 for the control
  plane and operator, with a root `LICENSE` (Apache), `LICENSE.FSL`, and per-tree
  `controlplane/LICENSE` and `operator/LICENSE`, plus SPDX headers on every Go file and a
  CI check (`scripts/check-spdx.sh`) enforcing the boundary.
- The FSL packages live outside the top-level `internal/` so the private managed module can
  import their public API; module-private shared helpers that must stay Apache live in
  `internal/`.
- Burrow is marketed and documented as "open core," never unqualified "open source"
  ([ADR-0009](0009-honest-status.md)).
- Contribution is **input-open, code-closed-unless-CLA**: issues and discussions are the way
  to contribute; outside-code PRs are declined or CLA-gated; all commits are DCO-signed.
  Documented in [CONTRIBUTING.md](../../CONTRIBUTING.md).
- Commercial licenses (exceptions to the FSL competing-use restriction) are offered by the
  sole copyright holder; see [COMMERCIAL.md](../../COMMERCIAL.md).
- Dependencies must remain license-compatible with this scheme; a future ADR may pin a
  permissive-only dependency policy.
- The repository can go public once these files are in place (this decision is the gate).

## Rejected alternatives

- **All Apache-2.0 across the repository.** Rejected: no protection for the managed business
  and no basis for selling exceptions — a competitor could legally resell the control plane
  as a service. (It remains the clean fallback if resale protection is ever judged not worth
  the open-core framing cost; it is not the choice here.)
- **All BSL/FSL across the repository.** Rejected: it would put a non-OSI usage restriction
  on the thin client pieces where adoption is the entire point, adding friction exactly
  where Burrow can least afford it.
- **DCO-only, merge community code freely (no CLA).** Rejected — and this is the reversal
  from the earlier draft: it splits copyright and forecloses the sell-exceptions model,
  which is an explicit maintainer goal. DCO is retained for sign-off/provenance, but it is
  not the basis on which outside code is accepted.
- **A non-converting source-available or proprietary license** (e.g. SSPL, or BSL with no
  Change Date). Rejected: the eventual conversion to Apache is what keeps Burrow an honest,
  reversible open-core project rather than an enclosure, and SSPL's copyleft-on-service
  terms are heavier and more adoption-hostile than FSL's single carve-out.
