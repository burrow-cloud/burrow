# ADR-0033: Relicense the whole repository to Apache-2.0

## Status

Accepted. **Supersedes the *license* decision of [ADR-0001](0001-license-and-dco.md)** — the
per-package split (Apache-2.0 client / FSL-1.1-ALv2 control plane + operator) is replaced by a
single **Apache-2.0** license across the entire repository. **ADR-0001's contribution stance —
DCO sign-off on every commit, outside *code* CLA-gated, sole copyright ownership — remains in
force.**

## Context

ADR-0001 chose FSL-1.1-ALv2 for the control plane and operator to protect the commercial
(managed-cloud) play: source-available, non-compete, converting to Apache-2.0 two years after each
release. That was a defensible default. Competitive and community research since then changes the
calculus:

- **Adoption is the bottleneck, not monetization-defense.** Burrow is pre-adoption. The thing that
  kills a self-host tool at this stage is not getting tried, not getting strip-mined at scale.
- **Our ICP reflexively avoids non-OSI and copyleft licenses.** Issue-mining of Coolify / Dokploy /
  Kubero and Reddit sentiment (see `research/competitive-icp-positioning.md`) show the
  self-host audience — increasingly **solo / small-team SaaS founders** as agents lower the
  SaaS-building barrier — treats source-available licenses (BSL/SSPL/FSL) as "not real open source"
  and a bait-and-switch risk ("gives me the shivers"; "no corporation will touch a non-standard
  license"), and treats AGPL as a "ticking time bomb" with corporate hard-bans. Apache, by
  contrast, "removes all friction for adoption — nobody needs to check with legal before trying
  it." **Perception governs adoption, not the license's actual text.**
- **Permissive does not preclude the business.** Coolify (Apache-2.0) reached ~$20k MRR; Supabase
  (Apache-2.0) runs a large managed business. Enterprises pay for **risk transfer** — SLAs, SOC 2,
  legal liability, support — not for core features behind a license wall.
- **The strip-mine threat is scale-stage.** A cloud reselling the software only bites at
  significant scale; at a pre-adoption stage the durable moat is execution, brand, the managed
  product, and the agent-safety architecture — not a restrictive license. There is no adoption tax
  worth paying now to defend against a problem that only appears after we have won.

## Decision

**License the entire repository under Apache-2.0**, and monetize off the permissive core rather
than through the license.

1. **One license, whole repo.** The control plane (`controlplane/`, `cmd/burrowd/`) and operator
   (`operator/`) move from FSL-1.1-ALv2 to **Apache-2.0**, matching the client surface (already
   Apache). Every `SPDX-License-Identifier` header becomes `Apache-2.0`; the FSL `LICENSE` files
   and the per-package split are removed.
2. **Monetize off the core, not the license:**
   - **Managed cloud** — the multi-tenant hosted product (separate, private; not in this repo).
   - **A proprietary enterprise tier** — SSO/SAML, teams/orgs, multi-tenancy, advanced RBAC,
     compliance reporting, support + SLA — sold commercially, kept out of this repo.
   - **Risk transfer** — SLAs, SOC 2, legal liability, support, the assurances enterprises pay for.
   - The agent-safety core (guardrails, audit, least-privilege, secrets handling) stays in the
     open Apache code — it is the differentiation and the trust, never gated.
3. **Contribution policy unchanged.** ADR-0001's DCO sign-off, CLA-gating of outside code, and sole
   copyright ownership **remain in force.**

## Consequences

- **Mechanical relicensing (a follow-up PR):** swap the ~86 FSL `.go` SPDX headers to Apache-2.0;
  remove `LICENSE.FSL` and `controlplane/LICENSE`; rewrite `LICENSING.md`, `COMMERCIAL.md`, the
  README license section, and the `CLAUDE.md` licensing section; drop the now-moot FSL→Apache
  conversion language. The SPDX CI check enforces the new uniform header.
- **Adoption:** the whole repo is OSI Apache-2.0 — no legal-review friction, distro/package-manager
  eligible, comfortable for the solo/small-SaaS-founder ICP.
- **Positioning alignment:** matches the ICP and "production-grade self-hosting + guardrails for
  your agent" flag (research doc; memory `icp-and-positioning`). Public materials still describe
  Burrow as **open core** (the managed/enterprise tiers are proprietary), but the *repository* is
  now unambiguously open source.
- **Trust through consistency:** the research shows the self-host audience rewards clear, stable,
  OSI licensing and punishes restrictive changes — the value of going Apache is precisely that it
  removes the source-available friction; keeping it clean is the point.

## Rejected alternatives

- **Keep FSL-1.1-ALv2 (status quo).** Rejected: the non-OSI "not real open source" / bait-and-switch
  adoption tax outweighs the strip-mine protection at a pre-adoption stage; FSL converts to Apache
  in two years regardless, so this largely arrives at the same endpoint sooner.
- **AGPL-3.0 + commercial dual-license.** Rejected: despite being OSI "real open source" and closing
  the SaaS loophole, the **perception** ("ticking time bomb," corporate hard-bans) repels the
  solo/small-SaaS-founder ICP — even though merely running/communicating with unmodified Burrow does
  not infect a downstream SaaS. Perception governs adoption.
