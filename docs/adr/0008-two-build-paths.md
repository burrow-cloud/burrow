# ADR-0008: Two build paths for two users

## Status

Accepted.

## Context

Burrow deploys images by reference ([ADR-0004](0004-code-never-over-mcp.md),
[ADR-0007](0007-explicit-deploy-by-image-reference.md)). But someone has to *build* the
image and get it onto the registry first. There are two distinct users with two distinct
expectations:

- The **self-host developer** (Burrow's first user): already has a cluster, already has a
  local toolchain and a registry, and is comfortable with their agent or CLI running a
  build. They want to keep the build on their own machine/CI and just hand Burrow a
  reference.
- The **managed-platform user** (later): wants to point Burrow at a git reference and have
  the platform build the image for them — no local Docker, no registry to manage.

Forcing one path on both users is wrong: server-side build is unnecessary machinery for
the self-host developer, and requiring a local build defeats the point of the managed
product.

## Decision

Burrow supports **two build paths**, selected by who is operating:

1. **Client-side build (self-host developer, v0.1).** The agent or the CLI builds the
   image locally and pushes it to a registry the cluster can pull from, then calls `deploy`
   with the reference. Burrow does not build anything; it receives a ready reference. This
   is the **only build path in v0.1**.
2. **Server-side build (managed platform, later).** The platform builds the image from a
   git reference, pushes it to a registry, and deploys the result. The user never touches a
   local build. This path is **out of scope for v0.1** and is noted here so the v0.1
   interfaces do not foreclose it.

Both paths converge on the same thing: an image in a registry, deployed by reference
through the same guarded control-plane path ([ADR-0007](0007-explicit-deploy-by-image-reference.md)).
The difference is only *where the build runs and who pushes*.

## Consequences

- **v0.1 ships only the client-side path.** The control plane stays out of the build
  business for the first slice; it takes a reference and runs it. This keeps the v0.1
  surface small ([docs/PLAN.md](../PLAN.md)).
- **The deploy contract is build-path-agnostic.** Because both paths end at "a reference in
  a registry," `deploy` looks identical regardless of who built the image — the control
  plane need not know which path produced the artifact.
- **Server-side build is anticipated, not built.** v0.1 interfaces (the deploy call, the
  registry-pull assumption) are shaped so a later server-side builder can push images and
  call the same deploy path without redesign. The server-side builder runs in the control
  plane's environment and pushes to a registry; it does **not** accept image bytes over MCP
  ([ADR-0004](0004-code-never-over-mcp.md)).
- **Honest scoping in docs.** Until server-side build ships, docs describe only the
  client-side path as working ([ADR-0009](0009-honest-status.md)); the managed path is
  described as planned.

## Rejected alternatives

- **Only ever build server-side.** Rejected: it imposes build infrastructure and git-based
  triggers on the self-host developer who already has a toolchain and just wants to deploy
  a reference — and it would make v0.1 depend on a build pipeline Burrow does not need yet.
- **Only ever build client-side.** Rejected: it forecloses the managed product's core
  convenience (point at git, get a deploy) and would force every future managed user
  through a local build.
- **One configurable build path that does both.** Rejected as premature: the two paths run
  in different trust/compute environments (user's machine vs. platform) and arrive on
  different timelines; collapsing them now would over-engineer v0.1. They already converge
  at the registry, which is the only place convergence is needed.
