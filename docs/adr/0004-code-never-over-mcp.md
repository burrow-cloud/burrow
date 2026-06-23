# ADR-0004: Code never travels over MCP; the registry is the conveyor belt

## Status

Accepted.

## Context

To deploy an application, two very different kinds of data have to reach the cluster: the
*instruction* ("deploy this thing, with these env vars, running this command") and the
*artifact* (the built container image — potentially hundreds of megabytes). It is
tempting to move both over the one connection the agent already has: the MCP channel. That
temptation must be refused, because MCP is a tool-call channel — a control path, not a
data path. Pushing image bytes through it would be slow, would strain message-size limits,
would bypass the deduplication, caching, and content-addressing that container registries
exist to provide, and would conflate "telling Burrow what to do" with "shipping Burrow the
goods."

Container registries already solve artifact movement: content-addressed, deduplicated,
cacheable, pullable by the cluster's nodes directly, and the native input Kubernetes
expects.

## Decision

**Code never travels over MCP.** The MCP connection carries only tool calls and small
metadata: an image reference, environment variables, a command, a replica count, and the
like. The built container image moves through a **container registry** — pushed by
whoever built it, pulled by the cluster — and never through the MCP connection.

The mental model: **MCP is the remote control; the registry is the conveyor belt.** The
remote control sends small signals; the heavy goods ride the belt. A deploy is the agent
pressing a button on the remote that names an item already on the belt.

## Consequences

- **`deploy` takes an image reference, not image bytes.** The control plane is handed a
  pullable reference (e.g. `registry.example.com/app@sha256:…`) plus small metadata, and
  it instructs Kubernetes to run that image. See
  [ADR-0007](0007-explicit-deploy-by-image-reference.md).
- **The image must be in a registry the cluster can pull from before `deploy` is called.**
  Who pushes it depends on the build path
  ([ADR-0008](0008-two-build-paths.md)): the self-host developer's agent/CLI builds and
  pushes; the managed platform builds server-side and pushes. Either way, by deploy time
  the artifact is on the belt.
- **MCP message sizes stay small and bounded**, so the control surface is fast and never
  bumps protocol limits, regardless of application size.
- **Content-addressing gives integrity and rollback for free.** Deploying and rolling back
  are just naming a different digest already on the belt; the registry guarantees the
  bytes.
- The control plane needs registry pull credentials configured on the cluster (image-pull
  secrets); it does not need to receive, store, or proxy image bytes itself.

## Rejected alternatives

- **Stream image bytes through MCP.** Rejected: slow, hits message-size limits, throws
  away registry dedup/caching/content-addressing, and overloads a control channel with a
  data-plane job.
- **Have the control plane accept an image upload over its own API and push to a
  registry on the caller's behalf.** Rejected for v0.1: it makes the control plane a
  bytes-handling middleman for no benefit when the builder can push directly, and it
  blurs the clean "reference in, run out" contract. (A server-side build path
  ([ADR-0008](0008-two-build-paths.md)) does produce images, but it builds them where it
  runs and pushes them itself — it does not accept uploaded bytes over MCP.)
- **Bake code into a ConfigMap / pass it inline.** Rejected: Kubernetes runs images, not
  source; this just reintroduces shipping bytes over the control path by another name.
