# ADR-0016: CLI distribution via Homebrew and the CLI-driven upgrade lifecycle

## Status

Proposed. The near-term `burrow upgrade` command (the in-cluster half) is decided and
slated for a v0.1.x release; the CLI distribution mechanism (a Homebrew tap and signed
release binaries) is the part this record proposes for a later release and awaits
maintainer approval. This ADR does not track build status — see
[docs/ROADMAP.md](../ROADMAP.md) for sequencing.

## Context

Two related questions sit on top of the install work:

1. **How does a developer get and update the `burrow` CLI?** Today the only path is
   `go build ./cmd/burrow` from a checkout. That is fine for contributors and wrong for
   users: a solo developer with a DigitalOcean cluster should not need a Go toolchain to
   run Burrow.
2. **How does a developer upgrade the *control plane* running in their cluster?** Today
   `burrow install` mints a fresh API token and Postgres password on every run, so
   re-running it over an existing install clobbers the secrets — the new DB password no
   longer matches the existing data volume, and the old token dies. The only "upgrade" is
   `kubectl delete namespace burrow` + reinstall, which also destroys all control-plane
   state (deploy history, release records — the reason Postgres is there at all).

The two questions share an answer: **the CLI is the upgrade driver, and the CLI's version
pins the control-plane version it installs.** `burrow install` already defaults to an
immutable, pinned `burrowd` image ([ADR-0014](0014-self-host-connectivity-via-kubeconfig.md)
connectivity; the pinned tag is a release constant in the CLI). So "upgrade the cluster"
is naturally "get a newer CLI, then have it roll the control plane forward." This keeps a
single source of truth for what-version-goes-together and avoids a separate cluster-side
updater.

## Decision

**The CLI is the unit a user installs and upgrades, and it drives control-plane upgrades.**

Near-term (v0.1.x), in this repo:

- **`burrow upgrade`** performs an in-place, **state-preserving** control-plane upgrade. It
  reads the existing install Secrets (`burrowd-api-token`, `burrowd-db`) from the cluster
  and reuses them, re-renders the manifests with the **new** `burrowd` image (and the
  existing app namespace), and applies — rolling only the `burrowd` Deployment; Postgres
  and its PersistentVolumeClaim are untouched. It then waits for readiness, reusing the
  same deployment-readiness poll as `install`.
- The upgrade target **defaults to the CLI's own pinned release** (the `defaultBurrowdImage`
  constant), so `brew upgrade burrow` followed by `burrow upgrade` is the whole update path.
  `--burrowd-image` overrides for off-release builds.
- Schema migrations ride the upgrade for free: `burrowd` runs embedded goose migrations on
  startup behind the single-minor-step upgrade gate
  ([ADR-0013](0013-database-migrations-and-upgrade-policy.md)), which carries the schema
  forward and **refuses** a version jump that is too large rather than corrupting state.
- **`burrow install` refuses to run over an existing install** (detected by the presence of
  the control-plane Secrets) and points the user at `burrow upgrade`, so the
  secret-clobbering footgun is closed rather than documented around.

Later (the part this ADR proposes for a future release):

- **Distribute the CLI via a Homebrew tap** (`brew install burrow-cloud/tap/burrow`) plus
  signed release binaries attached to GitHub releases for non-brew platforms. Release
  automation builds the cross-platform `burrow` binaries on tag and updates the tap formula.
- The composed upgrade story becomes: **`brew upgrade burrow` → `burrow upgrade`** — the
  first updates the local CLI to the latest release, the second rolls the in-cluster control
  plane to the version that CLI pins. The CLI may grow a `burrow upgrade --check` that
  reports when the cluster's running control plane lags the CLI's pinned release.

## Consequences

- One source of truth for compatible versions: a given `burrow` CLI knows exactly which
  `burrowd` it installs and upgrades to, so the client and control plane move together and
  the single-minor-step migration gate (ADR-0013) is enforced on a known path.
- `burrow upgrade` needs the same cluster reach as `install`/`deploy` — the ambient
  kubeconfig and the API-server proxy (ADR-0014) — plus `get` on Secrets in the
  control-plane namespace to read the values it must preserve. No new credential model.
- The license boundary is unaffected: the CLI is Apache-2.0 ([ADR-0001](0001-license-and-dco.md)),
  the Homebrew formula and release tooling are packaging, and the FSL `controlplane`
  packages are not imported by the CLI.
- Homebrew distribution adds release-engineering surface (a tap repository, formula
  updates, binary signing/notarization on macOS). That cost is why the distribution half is
  proposed for a later release rather than taken on with v0.1.x.

## Rejected alternatives

- **A separate in-cluster updater (the control plane upgrades itself).** Rejected: it
  splits the source of truth for version compatibility across the cluster and the client,
  and it puts upgrade orchestration inside the very component being upgraded. Keeping the
  CLI as the driver is simpler and matches the install path users already run.
- **Make `burrow install` idempotent and reuse it for upgrades.** Rejected as the primary
  path: overloading one verb to both bootstrap (mint secrets) and upgrade (preserve them)
  is exactly the ambiguity that caused the secret-clobbering footgun. A distinct `upgrade`
  verb with an `install` guard makes the intent explicit. (`install` becoming
  secret-preserving-if-present remains a possible later simplification, but the two verbs
  stay distinct in the UX.)
- **`curl | sh` as the primary installer.** Rejected as the headline path: opaque and
  hard to upgrade cleanly. A Homebrew tap (and plain release binaries) is more inspectable
  and gives `brew upgrade` for free; a convenience script can come later if asked for.
- **Ship the CLI only as a container image.** Rejected: the CLI runs on the developer's
  laptop and shells out to `kubectl` and a local Docker build (client-side build path,
  [ADR-0008](0008-two-build-paths.md)); a native binary is the right form factor, not a
  container.
