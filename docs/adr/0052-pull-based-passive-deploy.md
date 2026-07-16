# ADR-0052: Pull-based passive deploy — an opt-in, semver-scoped registry watcher on the guarded path

## Status

Accepted. Realizes the optional passive mode anticipated by
[ADR-0007](0007-explicit-deploy-by-image-reference.md); layers on
[ADR-0006](0006-guardrails-in-the-control-plane.md) /
[ADR-0020](0020-guardrails-as-configurable-policy.md). Supersedes nothing. Its on-by-default default
(§2/§5) is superseded by [ADR-0058](0058-auto-deploy-is-opt-in.md): auto-deploy is opt-in, default
`off`; the rest of this ADR stands.

## TL;DR

Every app auto-deploys new versions of its own image, up to a **per-environment level** set by
`burrow app auto-deploy <app> [patch|minor|major|off]` — default **`minor`**, on by default for
every app, new or already deployed. burrowd **polls the registry** for new tags; when one is
within the level, it fires the **same explicit, guarded deploy** it would run for a CLI call —
same rollout, same deploy record, same rollback handle, same audit entry. A tag above the level
(a `1.3.0` under `patch`, a `2.0.0` under `minor`) is **not** deployed silently: it is surfaced
as an *available upgrade* to take with an explicit `burrow app deploy`, or the operator raises
the level. The
watcher is **outbound-only**, so it works on the private, NAT'd, firewalled clusters Burrow's
ICP actually runs, which a push-from-CI model cannot reach. Polling is conservative (default
~5 min) because the explicit deploy is always the immediate path.

## Context

[ADR-0007](0007-explicit-deploy-by-image-reference.md) made the explicit deploy-by-image-
reference call the spine, and explicitly allowed a passive mode "as an option, never the
spine." This ADR specifies that option.

Two facts force the shape:

1. **Push-from-CI cannot serve a private cluster.** burrowd has no public endpoint of its
   own: the CLI reaches it through the Kubernetes API server's service proxy, authenticated
   by the kubeconfig ([ADR-0014](0014-self-host-connectivity-via-kubeconfig.md)). So an
   external CI runner calling burrowd to deploy must be able to reach the cluster's
   **Kubernetes API server**. A managed cluster (e.g. DigitalOcean) exposes that API
   publicly, so push-from-CI works there. But Burrow's first user runs their own VPS or home
   cluster, where the API server is firewalled, behind NAT, or deliberately not exposed. A
   cloud runner cannot reach in, so push-from-CI is a dead end for exactly the user Burrow
   targets.

2. **A pull model needs only outbound connectivity, which every cluster has.** The in-cluster
   control plane already reaches the registry to let Kubernetes pull images. Having burrowd
   *poll* that registry for new tags and act on them requires nothing new inbound. This is
   why pull-based deploy is the correct primary auto-deploy for the private-cluster user.

The common shortcut is a **mutable-tag auto-pull** (put a floating tag like `:latest` on the
workload and repoint it on any push, as Keel or a Flux image policy does). It is rejected
below: it deploys on every push, with no semver control, no guardrail, no deploy record, and
no rollback handle — the opposite of the control Burrow exists to provide.

## Decision

### 1. A pull-based passive deploy mode, opt-in per app, on the guarded spine

burrowd periodically lists the tags of an app's image repository and, when a new tag
satisfies the app's auto-update policy, invokes the **same internal guarded deploy path** an
explicit call uses ([ADR-0006](0006-guardrails-in-the-control-plane.md),
[ADR-0007](0007-explicit-deploy-by-image-reference.md)): the guardrails run, the rollout
happens, the release is recorded with its `Supersedes` chain (the rollback handle), and the
audit log ([ADR-0027](0027-audit-log.md)) gets an entry. A passive deploy is
**indistinguishable downstream** from an explicit one except in its recorded provenance. It
adds **no inbound surface**; it is outbound-only, so it works where push-from-CI cannot.

### 2. A semver-scoped auto-update policy — the control the mutable-tag model lacks

Per app, per environment, an auto-update **level** sets how far the watcher may move the app.
Every level deploys **upgrades only** — it moves to the **highest semver version within the
level's cap that is greater than the running release**, compared by version order, never by
push time. A tag that is a lower version than what is running (a backport, a re-push) is never
taken, so the watcher can only ever move an app forward. For an app on `1.2.5`:

- **`off`** — nothing auto-deploys; explicit CLI or agent deploy only. The explicit call stays
  canonical ([ADR-0007](0007-explicit-deploy-by-image-reference.md)).
- **`patch`** — patches within the *current* minor only: `1.2.6`, `1.2.7`. It does not cross to
  `1.3.0`. You move between minors by hand; after a manual deploy of, say, `1.5.0`, it then
  auto-patches `1.5.x` (never `1.6.0`).
- **`minor`** (the default) — any patch or minor upgrade: `1.2.6`, `1.3.0`, `1.4.0`. Never a
  major.
- **`major`** — anything newer, including a breaking major (`2.0.0`).

So `patch` means "keep me current within my minor, I choose the minors," and `minor` means
"keep me current within my major." The default is **`minor`**, and it applies to **every app,
new or already deployed** — auto-deploy is on by default, not something to switch on; set an
app to `off` to disable it. `major` is deliberately available — not forbidden — for the operator
who wants fully hands-off updates and accepts the breaking-change risk; it is simply not the
default, because a major is the breaking-change class most operators want to take deliberately.
The level is per app and per environment
([ADR-0020](0020-guardrails-as-configurable-policy.md)), so `prod` can sit at `patch` while
`staging` runs `major`, set through the CLI (§6). Defaulting on means that the day this ships,
existing apps begin auto-taking minors; that rollout is called out in the release notes so it is
not a surprise.

### 3. An out-of-scope bump is surfaced, not silently ignored

When a newer tag exists above the level's cap (a `1.3.0` under `patch`, or a `2.0.0` under
`minor`), burrowd does **not** deploy it. It records it as an **available upgrade** on the app
and surfaces it in `status`/history and to the agent, with the exact command to take it
(`burrow app deploy <app> --image <ref>:2.0.0`). The operator takes the bigger jump
deliberately, through the CLI, on the explicit guarded path — and if it is a line they now want
tracked, they raise the level. *Held, and you are told* — the same shape as a confirm guardrail,
where the "confirmation" is running the explicit deploy.

### 4. Semver tags required; floating tags are out of scope

The scope model is defined only over tags that parse as semver. `latest`, a git SHA, and
date tags cannot be classified patch/minor/major, so the watcher ignores them for auto-update
and will not chase a floating tag — chasing one is exactly the mutable-tag behavior this ADR
rejects. This matches the existing house convention that images are pushed with unique,
incrementing semver tags (the agent guidance and MCP instructions already steer this way).

### 5. Safety, provenance, and no thrashing

Auto-update defaults on at `minor` and can be dialed down or turned `off` per app; a version can
be pinned by setting `off` and deploying it explicitly.

**A rollback disables auto-deploy.** A rollback — or any manual deploy that moves the app to a
*lower* version than it is running — sets the app's auto-deploy level to `off`. Otherwise the
watcher would fight a deliberate downgrade: minutes after you roll back off a bad `1.5.0`, it
would re-apply `1.5.0` (or jump to a `1.6.0` that may carry the same defect). Disabling is
predictable (an explicit `off`, not a timed pause that silently resumes) and is surfaced with
its reason — `status` and the agent show "auto-deploy: off (disabled by rollback)". Re-enabling
is a deliberate human action (§6): the agent *can* roll back, which safely stops auto-deploy,
but a human decides when to turn it back on with `burrow app auto-deploy <app> <level>`.

A passive deploy
that fails to roll out uses the same warm rollback (the `Supersedes` chain), and the watcher
**stops re-attempting that tag** so a bad image cannot become a redeploy crash-loop; the
failure is surfaced. The poll interval is bounded and configurable. Every passive deploy is
recorded with its trigger provenance (auto-update, the level, the tag and digest),
distinct from an explicit deploy, so the deploy record and audit log stay legible.

### 6. Setting the level is an operator action; the agent observes

Choosing the auto-deploy level is a governance decision, so it is a human operator action
through the `burrow` CLI: `burrow app auto-deploy <app> [patch|minor|major|off] [--env prod]`
sets it, and `burrow app auto-deploy <app>` shows the current level, the last check, and any
held available upgrade. Choosing `major` — fully hands-off updates — is exactly the kind of
decision that belongs to a human, which is why the level is set through
this CLI. The agent, over `burrow-agent`, can **read** the policy and see available upgrades,
but not set what deploys without a human — consistent with the credential split
([ADR-0038](0038-scoped-agent-credential.md)) that keeps "what may happen unattended" a human
decision.

### 7. Cadence: conservative polling, with the explicit deploy as the immediate path

The watcher polls on a bounded, configurable interval (default ~5 minutes). It is deliberately
not a low-latency channel, because it does not need to be: the **explicit deploy is already the
immediate path**. When an operator or the agent wants a version live right now, they run
`burrow app deploy` (or the agent verb) and it is instant and guarded. Auto-update serves the
*other* case — the patch you push and walk away from. So "why do I have to wait?" has a clean
answer: you do not, that is a different button. This is what lets the poll interval stay
conservative (protecting registry rate limits) without a latency complaint.

To keep it cheap and avoid being throttled: poll authenticated (Burrow already holds the pull
credential), use cheap metadata calls (a tag list plus a manifest `HEAD`) with conditional
requests (ETag / `If-None-Match`) and a cached last-seen digest so only a real change triggers
work, add jitter, and back off on `429`/`5xx` honoring `Retry-After`, widening the interval if
a registry pushes back. Only apps with auto-update active are polled, so request volume is
proportional to the opted-in apps. Docker Hub's per-window manifest limits are the sensitive
case (poll it slower); GHCR (the reference registry, [ADR-0046](0046-registry-onboarding.md))
and the cloud registries are generous. A registry webhook would cut latency but needs an
inbound endpoint the private cluster cannot accept — the same reachability wall that made this
a pull and not a push — so polling is inherent and is engineered to be cheap. The latency bound
is surfaced, not hidden: `status` and the agent see "auto-update: minor, new tags deploy within
~5 min," and the agent is told to run the explicit deploy when the user wants a version live
immediately.

### 8. Guide the agent to semver tags; do not enforce

Auto-update needs semver tags (§4), but Burrow cannot tag the image for the user — the build is
the agent's and never crosses the control channel ([ADR-0004](0004-code-never-over-mcp.md)). So
Burrow guides rather than mints:

- **Orient the agent.** The agent guidance (and the `CLAUDE.md` block written at wiring time)
  tells it to tag releases `major.minor.patch`, never a bare git SHA or `latest`, because
  semver is what unlocks safe auto-updates.
- **Suggest the next tag.** burrowd knows the app's current running tag, so `burrow-agent` can
  read it, parse the semver, and print the next patch/minor/major tag — turning "please use
  semver" into a number the agent just applies to its build.
- **Nudge on a non-semver deploy.** When a deployed tag does not parse as semver, the structured
  deploy result carries a non-blocking hint that auto-update cannot classify it and recommends a
  semver tag. It is a hint, not a gate: [ADR-0007](0007-explicit-deploy-by-image-reference.md)
  lets any reference deploy.

Enforcement is deliberately avoided — a git-hash workflow still deploys fine, it just does not
get auto-update until it adopts semver.

## Consequences

- **Auto-deploy that works for the ICP's private cluster,** because it is outbound-only. The
  push/CI model stays optional and is reserved for public-API (managed) clusters, where a
  cloud runner — or a self-hosted runner, or the operator's own machine — can reach the
  Kubernetes API server.
- **The explicit deploy stays the immediate, canonical path** (§7). Auto-update defaults to
  `minor` and always routes through the same guarded path, so guardrails, the deploy record,
  rollback, and the audit log are never bypassed — a passive deploy is downstream-identical to
  an explicit one.
- **New moving parts in burrowd:** a registry tag poller (a reconcile loop, which fits the
  operator/controller fault-injection testing posture of
  [ADR-0010](0010-testing-strategy.md)), registry tag-listing with auth, semver parsing, a
  per-app policy field, and an "available upgrade" surface. Registry rate limits, auth for
  listing (as distinct from pulling), and registries without a clean tag-list API are
  implementation concerns.
- **A dependency on semver-tagged images.** Workflows that ship floating or non-semver tags
  do not get auto-update; that is acceptable and matches the house convention.
- **Digest and multi-arch nuances** (a retagged digest, a multi-arch index) are edge cases the
  implementation must define, but they do not change this decision.

## Rejected alternatives

- **Mutable-tag auto-pull (Keel / Flux image-policy style).** Rejected as the model: it
  deploys on any push to a floating tag, with no semver control, no guardrail, no deploy
  record, and no rollback handle, and it bypasses the explicit spine. Burrow's thesis is
  control; instant auto-pull is its opposite.
- **Push from cloud CI as the primary auto-deploy.** Rejected as *primary* because it requires
  the CI runner to reach the cluster's Kubernetes API server
  ([ADR-0014](0014-self-host-connectivity-via-kubeconfig.md)), which a private, NAT'd, or
  firewalled cluster does not expose. Kept as an optional path for public-API clusters.
- **Forbidding major auto-updates entirely.** Considered, and rejected: some operators genuinely
  want fully hands-off updates and accept the risk. Instead `major` is an available scope but is
  **not** the default (`minor` is), and a major appearing under a lower scope is surfaced as an
  available upgrade rather than silently applied — so the breaking-change class is opt-in, not
  off-limits.
- **Making passive the spine.** Rejected per [ADR-0007](0007-explicit-deploy-by-image-reference.md):
  the explicit call is where the guardrails, the structured feedback, and the rollback handle
  live. Passive layers on it and never replaces it.
