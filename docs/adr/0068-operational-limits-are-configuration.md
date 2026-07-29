# ADR-0068: Operational limits are configuration, set by a human, scoped to an environment

## Status

🟡 Proposed

## TL;DR

The replica ceiling is a **guardrail code**, `app.replica_ceiling`, whose disposition an operator can
set. Setting it to `allow` makes the ceiling advisory — the request goes through. **A limit you can
turn off is not a limit**, and no other guardrail behaves this way: every other code answers "may
this operation happen", while this one is a *number* wearing a disposition's clothes.

The number itself is a hardcoded `50` with no way to change it. So an operator can disable the
ceiling but cannot raise it, which is exactly backwards.

Two decisions:

**Operational limits become configuration, not policy.** A limit is a bound a human sets; exceeding
it is a validation failure, not a policy decision. `app.replica_ceiling` is removed as a guardrail
code.

**Configuration has three tiers, and limits live in the middle one.** App config and secrets belong
to the developer and reach the pod. **Environment configuration** and **cluster configuration** are
operational, set only by a human with the admin kubeconfig, and never reachable from the agent
surface.

Limits are environment-scoped so a production ceiling can differ from a development one — which also
requires widening environment scoping past the `app.` prefix it is stuck behind today.

Refines [ADR-0020](0020-guardrails-as-configurable-policy.md) (removes a code from the guardrail
set), [ADR-0035](0035-environments.md) (environment becomes a configuration scope, not only a
deployment target) and [ADR-0065](0065-what-belongs-on-the-agent-surface.md) (a new operator-only
surface, placed in its tier 1). Supersedes nothing.

## Context

### What exists today

- **`app.replica_ceiling` is a guardrail code.** Its disposition is settable like any other:
  `allow`, `confirm`, or `deny`.
- **The bound is a constant.** `DefaultPolicy()` sets `MaxReplicas: 50` in `controlplane/domain.go`.
  There is no CLI, API, or store path that changes it — `guardrail_policy` is `code TEXT PRIMARY KEY`
  with a disposition and nowhere to put a number.
- **Other operational limits are constants too**, each in whatever file needed it: the build Job's
  three-day TTL in `controlplane/kube/build.go`, the metrics add-on's one-month retention in
  `controlplane/kube/addons.go`, and — added after this record was drafted — the thirty-second
  unschedulable grace period in `controlplane/kube/adapter.go`
  ([ADR-0074](0074-burrow-observes-what-it-manages.md) §2). The pattern is still producing new
  instances, which is the argument for settling it.
- **There is no configuration store.** `burrow_meta` is a single-row table (`CHECK (id)`) holding the
  schema version for the upgrade gate, and nothing else.
- **`burrow cluster` has no `config` subcommand** — it carries `capacity`, `bootstrap`,
  `ingress install`, and `registry install/uninstall`.
- **Environment scoping is limited to one prefix.** `EnvScopable(code)` returns
  `strings.HasPrefix(string(code), "app.")`, so `app.*` codes can be set per environment and
  `dns.*`, `addon.*` cannot.

### What breaks

**The ceiling is defeatable and unraisable.** `guard set app.replica_ceiling allow` turns it off
entirely; nothing turns it up. An operator who legitimately needs 80 replicas has one option, and it
is to remove the limit.

**It conflates two different questions.** A guardrail answers *what happens when this is attempted*.
A ceiling answers *where the line is*. Merging them means the line can be dispositioned away, and it
means the disposition has no useful values: `confirm` on a ceiling is "ask a human to approve
exceeding the limit", which is a limit with extra steps, and `allow` is no limit at all.

**Every other operational limit is invisible.** A build Job's retention and an add-on's metric
retention are decisions the operator has no way to see, let alone change, without reading the source.

### What this record resolves

Where an operational limit lives, who may set it, and at what scope — with the replica ceiling as the
first occupant rather than the subject.

## Decision

### 1. Three tiers of configuration, and they are not interchangeable

| Tier | Owned by | Reaches | Example |
| --- | --- | --- | --- |
| **App config and secrets** | the app's developer | the pod, as environment | `DATABASE_URL`, `LOG_LEVEL` |
| **Environment configuration** | the operator | Burrow's behaviour in that environment | replica ceiling, backup retention |
| **Cluster configuration** | the operator | Burrow's behaviour everywhere | build-Job retention |

App config already exists ([ADR-0028](0028-app-config-and-secrets.md)) and is unchanged. The other
two are new, and the distinction between them is **whether a sensible answer can differ per
environment**. A replica ceiling can; the retention of a build Job's logs cannot meaningfully.

### 2. An operational limit is a bound, and exceeding it is a validation failure

Not a guardrail. There is no disposition, nothing to relax, and no `confirm` path: a request above
the ceiling is refused, and the refusal names the limit, the scope it came from, and that a human
with the operator CLI can change it.

**`app.replica_ceiling` is removed from the guardrail set.** A stored disposition for it becomes
meaningless and is dropped by migration rather than silently ignored — an unrecognized code left in
the table is a setting an operator believes is in force.

### 3. Limits are environment-scoped, falling back to cluster

An environment's value wins; absent one, the cluster value applies; absent that, a built-in default.
The same resolution order guardrail dispositions already use for `<env>.<code>` and `<code>`.

### 4. Setting a limit is operator-only, and structurally so

`burrow cluster config` and its environment-scoped equivalent are operator CLI, admin kubeconfig.
They are **absent from `burrow-agent`** — ADR-0065's tier 1, asserted by the surface guard, not a
denied disposition.

The reason is the same one that makes ADR-0065's tier 2 trustworthy: a bound the agent can raise is
not a bound. `guard set` is already on this side of the line and these belong beside it.

### 5. Environment scoping widens beyond the `app.` prefix

`EnvScopable` keys on `app.`, which was a reasonable shorthand when app-lifecycle codes were the only
ones anyone wanted to scope. It no longer holds: ADR-0065 makes `dns.delete` deny-by-default and an
operator may reasonably want the agent managing DNS in development but not production, and §3 needs
limits scoped the same way.

Environment scoping becomes a property a code or a limit **declares**, rather than one inferred from
its name. Inferring capability from a string prefix is the kind of shortcut that is invisible until
it is wrong.

### 6. The first occupants

- **Replica ceiling** — environment-scoped. The case that prompted this.
- **Build-Job retention** — cluster-scoped; currently three days in `build.go`.
- **Add-on metric retention** — cluster-scoped; currently one month in `addons.go`.
- **Status thresholds** — cluster-scoped; currently one constant, the thirty-second unschedulable
  grace period in `adapter.go`, with more to come as the rest of
  [ADR-0074](0074-burrow-observes-what-it-manages.md) §2's vocabulary is settled (how many restarts
  before a crash loop is an Issue, how long an unbound claim before it is a volume failure).

**Status thresholds are the clearest case for the cluster tier rather than the environment one**, and
worth stating because §3's default pulls the other way. How long a pod may sit unschedulable before
something is actually wrong is a property of **the cluster's scheduler and whether it has an
autoscaler** — a cluster that provisions a node in ninety seconds needs a longer grace than one with
fixed capacity — and not of whether the workload is staging or production. An operator who tuned this
per environment would be encoding the same cluster fact twice and would eventually disagree with
themselves.

They also carry an obligation the other occupants do not: **whatever value applies has to apply
everywhere the same fact is judged.** ADR-0074's ledger is a second consumer of exactly this
threshold, and a status surface that stays silent for thirty seconds while the ledger records a row
at twenty does not merely disagree on tuning — the two hold different definitions of "failure", and
an agent reading both gets contradictory answers about one pod at one moment. A single configured
value is what keeps them honest, which is a stronger reason to move this out of a constant than mere
tidiness.

Each is a constant today, and moving it is a small change that makes an invisible decision visible.
Backup retention ([ADR-0063](0063-object-storage-provider.md) §3) joins them as environment-scoped
when it exists.

## Consequences

- **The ceiling becomes a real limit** — raisable by a human, not removable by a disposition.
- **A new configuration surface exists**, and it will attract things. The tier table in §1 is the
  test: if a value belongs to the developer it is app config, and if it is not an operational limit
  it probably does not belong here at all.
- **Existing `app.replica_ceiling` dispositions are dropped by migration.** An operator who set it to
  `allow` loses that and gets the ceiling back, which is the intended correction and is still a
  behaviour change on upgrade. It should appear in the release notes rather than being discovered by
  a refused scale-up.
- **`EnvScopable` becomes a declaration rather than a prefix check**, which touches every guardrail
  code. Small, mechanical, and it removes a class of surprise where a correctly-named code silently
  is not scopable.
- **Three tiers is one more concept than two.** The distinction between environment and cluster
  configuration will be got wrong occasionally, and the cost of getting it wrong is low — a value in
  the wrong tier is inconvenient, not unsafe.
- **Nothing here is agent-reachable**, so an agent cannot report a limit it is about to exceed unless
  the read side is exposed. Reading a limit is harmless and useful; §4 restricts *setting*. The read
  path should be on the agent surface, and this record does not decide its shape.

## Rejected alternatives

- **Keep `app.replica_ceiling` as a guardrail and add a numeric field to the policy table.** The
  smallest change, and it keeps one configuration surface. Rejected because it preserves the
  conflation: the number would still sit behind a disposition that can turn the limit off, and the
  disposition would still have two values that make no sense for a bound.
- **Make the ceiling a constant and remove the guardrail entirely.** Simpler still, and honest about
  today's behaviour. Rejected because an operator with a real need for a different ceiling has no
  recourse, and because the same argument would leave build retention and metric retention
  permanently invisible.
- **One flat cluster-wide configuration, no environment scope.** Fewer concepts. Rejected because a
  production ceiling and a development ceiling are exactly the case that motivates limits at all, and
  because ADR-0065's tier-2 gradient already establishes environment as the unit operators think in.
- **A `burrow config` group alongside `burrow app config`.** Reads naturally and would collide
  badly: `app config` is developer-owned and pod-bound, this is operator-owned and never reaches a
  pod. Two things named `config` differing in who may set them is how the ceiling ended up inside the
  guardrail set.
- **Store limits in a ConfigMap rather than the database.** Kubernetes-native and inspectable with
  `kubectl`. Rejected for consistency with every other piece of control-plane state, which lives in
  Postgres and migrates with the schema; a ConfigMap has no versioning story and would be a second
  source of truth for operator intent.
