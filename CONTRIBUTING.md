# Contributing to Burrow

Contributions are welcome, and issues and discussions are the most valuable ones. Bug
reports, design feedback, feature ideas, reproductions, and questions have no strings
attached and are the best way to help and to shape where Burrow goes.

Code contributions are coordinated with the maintainer. Burrow keeps a single copyright
holder, so substantial outside code is accepted under a Contributor License Agreement (CLA).
This is not a judgment about the value of community code. If you would like to contribute
code, open an issue to talk it through first. Community input never affects ownership, which
is why issues and discussions are wide open.

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

## Building for a dev cluster from source

A from-source build must be stamped with a version, or the upgrade gate (ADR-0013) and the
client/server skew gate (ADR-0039) reject the unstamped `v0.0.0` fallback. The two binaries
stamp differently — the CLI via ldflags, burrowd via a source rewrite before `ko build` — so
two Task targets do it consistently:

```sh
task dev-build                                    # install the CLI, stamped with DEV_VERSION
KO_DOCKER_REPO=ghcr.io/you/burrowd task dev-image # build+push the burrowd image, same version
```

`DEV_VERSION` defaults to `v0.13.0-dev`; override it (`task dev-build DEV_VERSION=v0.13.0-dev`).
`dev-image` needs `ko` and a registry the cluster can pull from (`KO_DOCKER_REPO`), and an
optional `KO_PLATFORM` (e.g. `linux/amd64`); it reverts its source stamp on exit, so the tree
is never left dirty.
