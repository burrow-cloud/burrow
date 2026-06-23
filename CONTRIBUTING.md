# Contributing to Burrow

Burrow welcomes contributors — with one unusual rule about code that is important to state
plainly up front.

## The short version

- **Issues and discussions are the way to contribute.** Bug reports, design feedback,
  feature ideas, reproductions, and questions are genuinely wanted and have no strings
  attached. This is the most valuable way to help, and it is unrestricted.
- **Outside code pull requests are not merged.** Not because they are unwelcome in spirit,
  but because the maintainer keeps **sole copyright** of Burrow so it can be offered under
  commercial licenses (see [COMMERCIAL.md](COMMERCIAL.md) and
  [ADR-0001](docs/adr/0001-license-and-dco.md)). Merging outside code under a simple
  sign-off would leave copyright split across contributors and forfeit that ability.
- **If that changes, it will be via a CLA.** Should outside code contributions be opened up,
  it will be through a Contributor License Agreement that grants the maintainer the
  relicensing and sublicensing rights the commercial-license model requires — not under the
  DCO alone.

## Why

Burrow is open core. The control plane and operator are source-available under FSL-1.1-ALv2,
which converts to Apache-2.0 over time ([LICENSING.md](LICENSING.md)). A second, deliberate
goal is the ability to sell commercial license grants to organizations that need terms
without the FSL competing-use restriction. That "sell exceptions" model only works while one
party owns 100% of the copyright. Keeping sole ownership is what keeps that option open; it
is not a judgment about the value of community code.

Community **input** does not affect ownership at all, which is why issues and discussions are
fully open.

## How to contribute, concretely

- **Found a bug?** Open an issue with a clear reproduction.
- **Have a design opinion or feature idea?** Open a discussion or an issue. Design happens in
  the open; the ADRs in [`docs/adr/`](docs/adr/) are where decisions are recorded.
- **Want to point at a fix?** Describe it in an issue (a diff, a sketch, or a link is fine to
  illustrate). The maintainer may implement it; the issue is the contribution.

## Provenance and sign-off

All commits in this repository are signed off under the Developer Certificate of Origin with
`git commit -s`. The DCO sign-off is a provenance attestation and remains required on every
commit; it is separate from the copyright/CLA point above (the DCO alone is not the basis on
which outside code would be accepted).

## Code style

If you are working in a fork or under a future CLA, follow the Go conventions and workflow in
[CLAUDE.md](CLAUDE.md): `gofmt`, `go vet`, wrapped errors, dependencies passed explicitly,
small focused changes, the seam discipline, and the SPDX header rule
([LICENSING.md](LICENSING.md)).
