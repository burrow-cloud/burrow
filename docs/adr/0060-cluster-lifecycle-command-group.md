# ADR-0060: `cluster` is the cluster-lifecycle command group; operate verbs stay portable

## Status

✅ Accepted

## TL;DR

`burrow cluster` is the home for every command that sets up and manages a Burrow cluster —
installing and upgrading the control plane, inspecting capabilities, and provisioning shared
infrastructure — because those operations are inherently self-hosted: they drive a kube context. So
`install` and `upgrade` move under it (`burrow cluster install`, `burrow cluster upgrade`), joining
`bootstrap`, `ingress`, `registry`, and `capacity`. Everything else — the operate verbs (`deploy`,
`logs`, `scale`, `status`, `rollback`, …), `env`, and `agent` — is portable across a self-hosted
control plane and the managed cloud, so it stays at the top level. The managed cloud is reached
generically through the same client (an endpoint URL and a token), so the CLI needs no "managed
mode" and reveals nothing about the managed product. The old top-level `burrow install` /
`burrow upgrade` remain as deprecated, hidden aliases that print a one-line migration hint, so
existing muscle memory and scripts keep working. Extends ADR-0024 and ADR-0037; interacts with
ADR-0045 (the OSS/managed boundary) and ADR-0054 (install is control-plane-only). Supersedes
nothing.

## Context

The CLI is grouped by the task a person is doing (ADR-0024, ADR-0037). `burrow cluster` already
held the commands that stand up and manage the cluster substrate — `bootstrap` (install plus a
single-VPS k3s baseline, ADR-0044), `ingress`, `registry` (ADR-0054), and `capacity` — while
`install` and `upgrade` sat at the top level next to the operate verbs. That is an inconsistency:
`bootstrap` is install-plus-baseline and lives under `cluster`, yet plain `install` did not, so two
commands that do nearly the same job lived at different altitudes.

The deeper line is which commands are self-hosted-only and which are portable. Installing or
upgrading the control plane, provisioning ingress, or reading raw cluster capacity all require the
operator's own kube context — they act on the Kubernetes cluster directly, which only a self-hoster
has. The operate verbs, `env`, and `agent` do not: they speak to a control plane over its API, and
that control plane can equally be one the user self-hosts or one the managed cloud runs on their
behalf. The managed product is a separate module that imports the same public API and wraps it
(ADR-0045); a managed endpoint is reached with the same client the self-host path uses, given an
endpoint URL and a token. Nothing about the CLI's command surface should have to know which it is
talking to.

## Decision

### 1. `cluster` is the cluster-lifecycle group

`burrow cluster` is the surface for setting up and managing a Burrow cluster, and every command
that requires a kube context lives under it. `install` and `upgrade` move to `burrow cluster
install` and `burrow cluster upgrade`, alongside the existing `bootstrap`, `ingress`, `registry`,
and `capacity`. Bare `burrow cluster` keeps its read-only capability report. The "Get started"
help guidance and the first-run banner point at `burrow cluster install`.

### 2. Operate verbs, `env`, and `agent` stay portable and top-level

The operate verbs (`deploy`, `status`, `logs`, `scale`, `rollback`, `run`, and the rest under
`app`), `env`, `agent`, `config`, `guard`, and `audit` stay at the top level. They act through the
control-plane API, not a kube context, so they work unchanged against a self-hosted control plane or
the managed cloud. The managed cloud is reached generically — the same client, given an endpoint URL
and a token — so the CLI needs no "managed mode" switch and its command surface reveals nothing about
the managed product. This preserves the module boundary and the pluggable transport/auth seam of
ADR-0045.

### 3. Deprecated top-level aliases ease the transition

The old spellings `burrow install` and `burrow upgrade` remain as thin top-level aliases that
delegate to the same command constructor and are marked deprecated. Invoking one still runs, and
prints a one-line hint pointing at the new `burrow cluster` spelling; Cobra excludes deprecated
commands from the main help, so they are hidden there while still executing. Burrow is pre-1.0, so
a reorganization is acceptable now, but the aliases keep existing muscle memory and scripts working
through the transition.

## Consequences

- The cluster-lifecycle commands present coherently under one noun, matching where `bootstrap`
  already lived; a user setting up a cluster finds install, upgrade, ingress, registry, and capacity
  in one place.
- The top-level surface is now exactly the portable commands, which is the same surface a managed
  cloud user would see — the self-host-only commands are quarantined under `cluster`.
- Two spellings exist during the deprecation window, and scripts calling the old form see a hint on
  stderr/stdout until they migrate. The aliases are hidden from help so they do not clutter the
  everyday surface.
- Documentation and examples lead with `burrow cluster install` / `burrow cluster upgrade`; older
  references keep working through the aliases and are updated opportunistically.

## Rejected alternatives

- **Leave `install`/`upgrade` at the top level.** Rejected: it keeps the inconsistency with
  `bootstrap` (install-plus-baseline already under `cluster`) and mixes self-host-only commands into
  the portable top-level surface.
- **Remove the old spellings outright.** Rejected as a needless courtesy break: even pre-1.0, thin
  deprecated aliases cost little and spare every existing script and habit a hard failure.
- **Add a "managed mode" flag or a managed-specific command group.** Rejected: the managed cloud is
  reached generically through the same client (endpoint URL + token, ADR-0045), so no mode switch is
  needed, and adding one would leak the managed product into the OSS CLI's surface for no functional
  gain.
