# ADR-0012: Control-plane state runs in an in-cluster Postgres (no external managed database)

## Status

Accepted.

## Context

The control plane keeps its own durable state — the deploy records and the rollback
handles (ADR-0007) — in a SQL database behind the `controlplane.Database` seam. The
project's initial stack named **Neon**, an external managed Postgres, for this.

On reflection that contradicts what Burrow is. Burrow is a **self-hosted control plane
that operates within the user's own Kubernetes cluster**. Making the control plane's own
state depend on an external managed service means:

- the control plane **reaches out** of the cluster to operate, breaking the
  self-contained, no-egress model;
- self-hosting now requires a **third-party SaaS account** and credentials, in addition
  to the cluster the user already runs;
- the control plane's availability and data residency are **coupled to a service outside
  the cluster the user controls**.

The user controls a Kubernetes cluster; that is the natural, sufficient home for the
control plane's own state.

## Decision

**The control plane's durable state lives in a Postgres instance running in the user's
cluster.** Burrow does not depend on an external managed database for its own operation.

- The adapter (`controlplane/postgres`, built on pgx) connects to any Postgres via a
  DSN; it is database-host-agnostic. The *deployment* runs Postgres **in-cluster** (the
  install path, a later phase, provisions it).
- **Supported floor: Postgres 14+.** The adapter uses only long-stable SQL (`JSONB`,
  `BIGSERIAL`, `ON CONFLICT`, `TIMESTAMPTZ`), so any major from 14 onward behaves
  identically; self-hosters can run whatever recent Postgres their cluster provides.
- Testing follows suit: CI runs the integration tests against a Postgres **service
  container**, and local development points `BURROW_TEST_DATABASE_URL` at any Postgres
  (an ephemeral local instance, a container, etc.) — never a hosted service.

**General principle:** Burrow's core operating dependencies run **in-cluster**; the
control plane does not reach out to external managed services to operate. (An external
*outbound notification* channel such as transactional email, if added later, is not core
state or operation and is out of scope of this principle — it is an optional side
channel, not something the control plane depends on to deploy and operate workloads.)

This supersedes the initial Neon stack choice. It also refines the incidental
"Postgres (Neon)" mention in [ADR-0010](0010-testing-strategy.md); that ADR's testing
strategy (delegate durability to Postgres, no global simulation harness) is unchanged —
only the backing-service detail is corrected here, since accepted ADRs are immutable.

## Consequences

- No code change to the adapter: pgx already speaks to any Postgres. The change is
  architectural and documentary — the docs no longer name Neon, and the deployment story
  is in-cluster Postgres.
- The install path (later phase) must provision and connect an in-cluster Postgres; the
  control plane holds a DSN to a database it operates alongside, not a SaaS credential.
- CI gains a Postgres service container so the integration tests run for real rather than
  skipping.
- The separate managed product may run its own Postgres however it likes (that is its
  concern); this decision governs the open-core, self-hosted control plane.

## Rejected alternatives

- **Neon / an external managed Postgres.** Rejected: external egress, a required SaaS
  account and credential, and availability/residency coupling outside the user's cluster
  — all contrary to a self-contained, in-cluster control plane.
- **An embedded/single-file store (e.g. SQLite).** Rejected: it avoids running a Postgres
  but gives up concurrent access across control-plane replicas and a cluster-grade
  operational story, and it diverges from the managed product's Postgres. Postgres is the
  standard, and running it in-cluster keeps the self-host model intact.
