# Capabilities

**What this answers: "can Burrow do X?" — without reading the source and guessing.**

Burrow's capabilities are not all reachable from the code path you would expect them to be
on. Two real examples, both of which have produced the wrong conclusion:

- **Pulling from a private registry.** `burrow config registry login` writes a
  `burrow-registry` `dockerconfigjson` Secret into the app namespace and patches it onto that
  namespace's `default` ServiceAccount, so app Pods inherit it
  ([ADR-0017](adr/0017-private-registry-authentication.md)). None of that appears in
  `controlplane/kube/adapter.go` — the app Deployment carries no `imagePullSecrets` and no
  `serviceAccountName` — so reading the deploy path suggests private images are unsupported.
  They are supported.
- **A database.** `burrow addon install postgres` stands up an instance for one environment,
  with a database and a role per app ([ADR-0031](adr/0031-postgres-addon.md),
  [ADR-0067](adr/0067-one-database-instance-per-environment.md) §1) — one per environment by
  default, with `--name` for a second beside it
  ([ADR-0091](adr/0091-an-environment-may-hold-more-than-one-postgres-instance.md) §1) and `pg_dump`/`pg_restore`
  backups ([ADR-0032](adr/0032-postgres-backups.md)). Nothing in the deploy path mentions a
  database; the connection string arrives through the app's Secret at attach time.

Reading one code path and finding nothing is not evidence a capability is absent. Nor is an
ADR evidence a capability is present — Burrow's ADRs record decisions, not implementation
status ([ADR-0009](adr/0009-honest-status.md)), and several accepted decisions are not built.
This file is the place both questions get answered together.

## How to read this

- **Scope: the code on `main`.** Every capability listed in a table below exists in the code.
  For which *release* carries it, the [README](../README.md) version table is authoritative.
- **Decided but not built** is called out explicitly, inline where an ADR would otherwise
  mislead, and collected in [Decided but not built](#decided-but-not-built).
- **Limits are stated next to the capability, not in a footnote.** A reference that lists only
  strengths causes exactly the wrong conclusions this file exists to prevent. See also
  [What Burrow does not do](#what-burrow-does-not-do).
- Two CLIs appear throughout: `burrow`, the operator CLI, and `burrow-agent`, the scoped
  channel an agent drives ([ADR-0049](adr/0049-burrow-agent-scoped-cli-control-channel.md)).
  Where a verb exists on only one of them, the table says so.

---

## Deploying and running an app

| Capability | Command | What it does | ADR |
| --- | --- | --- | --- |
| Deploy by image reference | `burrow app deploy <app> --image <ref>` | Creates or updates one `apps/v1` Deployment named after the app. This is the spine — every other path (build, auto-deploy, rollback) ends here. | [0007](adr/0007-explicit-deploy-by-image-reference.md), [0011](adr/0011-kubernetes-integration.md) |
| Wait for the rollout | on by default; `--wait=false` to opt out | The deploy waits for the new replicas to become ready and reports what happened, bounded by `deploy.settle_timeout`. A rollout that does not become ready exits non-zero, names the reason and the pod's own explanation, and says the previous version may still be serving. `--wait=false` returns at submission and reports the outcome as unknown. | [0092](adr/0092-a-deploy-reports-its-rollout.md) |
| Override the entrypoint | `burrow app deploy <app> --image <ref> -- ./worker --queue emails` | Sets the container's `command`. There is no separate `args`. | [0007](adr/0007-explicit-deploy-by-image-reference.md) |
| Set replicas | `--replicas N` on deploy, or `burrow app scale <app> <n>` | `scale` issues a replicas-only patch and records no release. `--replicas 0` on a deploy means "keep the current count". | [0020](adr/0020-guardrails-as-configurable-policy.md) |
| Autoscale | `burrow app autoscale <app> --min --max --cpu [--memory]` | Creates an `autoscaling/v2` HorizontalPodAutoscaler targeting the Deployment. Defaults: min 1, max 10, CPU 80%, memory off. `burrow app autoscale <app> off` deletes it. | [0020](adr/0020-guardrails-as-configurable-policy.md) |
| Roll back | `burrow app rollback <app>` | Re-applies the release the current one supersedes — **exactly one step back**; there is no "roll back to release X". A failed `pre-rollback` hook aborts it; `--skip-hooks` (operator CLI only) rolls back without running the hook, leaves it configured, and records the skip. See [Lifecycle hooks](#lifecycle-hooks). | [0007](adr/0007-explicit-deploy-by-image-reference.md), [0080](adr/0080-a-rollback-is-not-blocked-by-its-own-hook.md) |
| Wait for the rollback's rollout | on by default; `--wait=false` to opt out | The rollback waits for the restored image to become ready, on the same `deploy.settle_timeout` a deploy waits on. One that does not become ready exits non-zero, names the reason and the pod's own explanation, and says two things a failed deploy does not: the release being rolled back **away from** may still be serving, and rolling back **again returns to that same release** — so the way out is a release picked from `burrow app history` and deployed. | [0093](adr/0093-a-rollback-reports-its-rollout.md) |
| Release history | `burrow app history <app>` | Lists releases newest-first from the control-plane database. A release Burrow applied whose pods never became ready reads as `deployed (not ready: <reason>)` — the status records what was applied, the qualifier what the rollout did. | [0012](adr/0012-in-cluster-postgres.md), [0092](adr/0092-a-deploy-reports-its-rollout.md) |
| Status | `burrow app status <app>`, `burrow app list` | Live workload state: desired/ready/updated replicas, availability, and — when the app is blocked — an actionable issue naming the fix, plus a machine-usable reason from a closed set: `ImagePullBackOff`, `ErrImagePull`, `Unschedulable`, `VolumeUnavailable`, `CrashLoopBackOff`, `CreateContainerConfigError`, `OOMKilled`, `ProgressDeadlineExceeded`, `DeadlineExceeded`. Conditions that resolve on their own (`ContainerCreating`, `PodInitializing`) are deliberately not reported. An app that is **serving** can still carry an issue: a container killed and restarted in place reports the kill for the ten minutes after it, so a pod being OOM-killed and coming back is not read as healthy, and its availability is left as reported because it really is serving. A crash loop carries a bounded tail of the container's own output; a missing config or secret **key** is named, a value never is. `status` also carries the app's recent failure history from the ledger (last 24h, up to 10 rows, resolved episodes included) and the observation coverage over that window. | [0011](adr/0011-kubernetes-integration.md), [0074](adr/0074-burrow-observes-what-it-manages.md) |
| What is broken, cluster-wide | `burrow failures`, `burrow-agent failures` | Lists the ledger's failures across every managed object. See [Failure ledger](#failure-ledger). | [0074](adr/0074-burrow-observes-what-it-manages.md) |
| Run a one-off command | `burrow app run <app> -- ./migrate` | Runs the command as a `batch/v1` Job from the app's **currently deployed image**, in the app's namespace, with the app's config and per-app Secret injected. Waits synchronously, captures the exit code and combined output. | [0048](adr/0048-one-off-command-runner.md) |
| Run a command around a deploy | `burrow app hook set <app> --on pre-deploy -- ./migrate`, plus `hook list` / `hook unset` | Stores a command per (app, environment, phase). `pre-deploy` runs on **every** deploy path, automated ones included, from the image **being deployed**, before anything reaches the cluster; its failure aborts the deploy. `post-deploy` runs after the rollout settles, whether it succeeded or failed, told the outcome and — on failure — a machine-readable reason. `pre-rollback` is unset by default and runs from the image being rolled back **away from**. See [Lifecycle hooks](#lifecycle-hooks). | [0072](adr/0072-deploy-and-run-lifecycle-hooks.md) |
| Auto-deploy on a new tag | `burrow app auto-deploy <app> <patch\|minor\|major\|off>` | burrowd polls the registry (~5 min, jittered) and fires the same guarded deploy for an in-level upgrade. Outbound-only, so it works on a NAT'd cluster. | [0052](adr/0052-pull-based-passive-deploy.md), [0058](adr/0058-auto-deploy-is-opt-in.md) |
| Declare a health endpoint | `burrow app health set <app> --path /healthz [--port N]` | Turns the default TCP readiness check into an HTTP GET of that path; `burrow app health unset` returns to the default and `burrow app health show` reports what is in force. See [Health checks](#health-checks). | [0076](adr/0076-health-checks-readiness-only-and-dependencies-at-deploy-time.md) |
| Deploy-time dependency checks | `burrow app checks show <app>`, `burrow-agent checks <app>`, plus `checks disable` / `checks enable` | After a deploy, Burrow verifies the things it **provisioned** for the app from inside the app's own container: an attached database (connect with the app's own `DATABASE_URL`, `SELECT 1`) and a published port (request it, report the status code). Derived from the registry, never configured. **Reported, never fatal** — a failure does not roll back and does not fail the deploy. See [Deploy-time dependency checks](#deploy-time-dependency-checks). | [0076](adr/0076-health-checks-readiness-only-and-dependencies-at-deploy-time.md) |
| Delete an app | `burrow app delete <app>` | Removes the workload, its Service and Ingress, and its release history. Guarded by `app.delete`, **denied by default** — relax the guardrail (ideally per environment) before the verb will run on either CLI. A **locked** app refuses regardless of the disposition, until somebody runs `burrow unlock <app>`. | [0024](adr/0024-cli-command-taxonomy.md), [0065](adr/0065-what-belongs-on-the-agent-surface.md) |
| Lock a thing against destruction | `burrow lock <app>`, `burrow lock addon <instance>`, and `burrow unlock` for either | Locked, the operations that cannot be undone refuse: deleting the app, removing the add-on instance, and `addon detach --delete-data`. Everything else is untouched. **Operator CLI only** — neither verb exists on `burrow-agent`. See [Locks](#locks). | cloud ADR-0060 |

Every verb above except `auto-deploy` and `hook` also exists on `burrow-agent` (`deploy`, `scale`,
`autoscale`, `rollback`, `run`, `delete`, `apps`, `status`, `history`, `health`/`health set`/
`health unset`, `checks`). Setting the auto-deploy level, setting a lifecycle hook, and turning
the deploy-time dependency check off are deliberately operator actions: each is standing
authority for something that happens with nobody watching. `rollback` is on both CLIs; its
`--skip-hooks` flag is on the operator CLI only, and `burrow-agent guard` reports it as a capability
the agent does not have, so an agent that meets a blocked rollback can say who closes it. The
`--wait=false` opt-out on `deploy` and `rollback` is operator-only for a related reason: being told
an operation worked when it did not is the agent's exposure, so the flag that restores that answer
is a human's to reach for.
Declaring a health endpoint deliberately is not, because the agent is the only party that can
write one into the application's code.

### What the app Pod actually looks like

The Pod template is a fixed literal in Go. It sets exactly this: one container (named after
the app) with `image`, `command`, `env`, and `envFrom`; the labels
`app.kubernetes.io/name` and `app.kubernetes.io/managed-by`; a `burrow.cloud/release`
annotation so every release rolls the workload; `prometheus.io/*` scrape annotations when
`--metrics-port` is given; and a **readiness probe when, and only when, Burrow knows a port to
check** (see [Health checks](#health-checks) below).

Nothing else. **The app Pod has no `Volumes` and no `VolumeMounts`**, and no resource
requests or limits, **no liveness or startup probe**, no `securityContext`, no
`serviceAccountName`, no `imagePullSecrets`, no `nodeSelector` and no tolerations. There is no
CLI or API surface for any of them. Pods therefore run under the namespace's `default`
ServiceAccount — which is how the private-registry pull secret reaches them (see
[Registries](#registries-and-credentials)).

### Health checks

Readiness only ([ADR-0076](adr/0076-health-checks-readiness-only-and-dependencies-at-deploy-time.md)),
and the defaults err toward *not* failing a deploy:

| Situation | What Burrow sets |
| --- | --- |
| The app is published (`burrow app publish`), so an exposure records a container port | A **TCP connect** on that port |
| The app is not published and declares nothing | **No probe at all** — identical to the behaviour before probes existed |
| `burrow app health set <app> --path /healthz [--port N]` | An **HTTP GET** of that path |

Burrow does **not** scan for a listening port, assume 8080, or guess `/healthz`: a probe it
invented would fail a working deploy for a reason the user could not see. It **never sets a
liveness probe**, because a liveness probe restarts the container and a wrong one manufactures
the crash loop it was meant to detect.

The probe's timings are fixed, not configurable: period 10s, timeout 5s, failure threshold 6
(so roughly a minute of continuous failure before a serving pod leaves its Service), no initial
delay. A pod that never passes stalls the rollout and leaves the previous pods serving, which is
the direction the whole design fails in.

**A readiness probe never checks a dependency.** The probe can address only the pod's own port —
the type carrying it has no field for a host and no exec handler — and a declared path that
looks like a URL or names a host is refused before it is stored. One shared Postgres backs every
app in an environment, so a readiness check that touched it would remove every replica of every
app from service the moment that database blipped. Dependency checks belong at deploy time
(ADR-0076 §4) — see [Deploy-time dependency checks](#deploy-time-dependency-checks).

A failing probe surfaces through the existing vocabulary rather than a new one: pods never become
ready, the rollout does not progress, and `burrow app status` reports
`ProgressDeadlineExceeded` ([ADR-0074](adr/0074-burrow-observes-what-it-manages.md) §2).

### Deploy-time dependency checks

Once a deploy has landed, Burrow checks the things **it provisioned** for the app
([ADR-0076](adr/0076-health-checks-readiness-only-and-dependencies-at-deploy-time.md) §4). The list
is **derived from the registry, never configured** — Burrow attached the database and wrote the
connection string into the app's Secret, and Burrow recorded the port the app publishes and made the
Service in front of it, so it does not have to be told what the app depends on to verify what it gave
it:

| What Burrow provisioned | What the check does |
| --- | --- |
| A database and login role on the environment's Postgres instance (`burrow addon attach postgres`) | Connects with the app's own connection string — `DATABASE_URL`, or the variable the attach named — and runs `SELECT 1` |
| A published port (`burrow app publish`) | Requests the app's port through its Service and reports the status code |

**It runs inside the app's own container** — the app's image, filesystem, service account,
namespace, network policy, config, and Secret. A check run anywhere else proves the *cluster* can
reach the database and not that the *app* can, and the difference is exactly where misconfiguration
lives. Because that image may have no shell and no client tools, an **init container running Burrow's
own image copies the `burrowd` binary into an emptyDir** the check container executes it from, so
nothing at all is required of the app's image.

**A failed check is reported and never fatal.** It does not roll back, does not retroactively fail
the release, and does not stop the deploy being recorded `deployed` — Burrow does not roll back by
itself ([ADR-0072](adr/0072-deploy-and-run-lifecycle-hooks.md) §6). The result rides back on the
deploy in a `dependencies` array and is recorded in the audit log; a check that could block a deploy
would be a new way for an app to become undeployable during an incident.

Each result carries an outcome (`passed`, `failed`, `skipped`) and, when it is not `passed`, a reason
from a closed set: `CredentialUnset`, `CredentialUnparsable`, `HostUnresolvable`,
`ConnectionRefused`, `AuthenticationFailed`, `TimedOut`, `QueryFailed`, `Unreachable`,
`CheckNotRun`, `NotDerivable`. `skipped` is deliberately distinct from `failed`: a check pod that
could not be scheduled says nothing about the database.

**No secret value ever leaves the check.** The credential is read from the check container's own
environment and handed to the driver; the driver's message is *discarded* rather than wrapped,
because a driver may quote the connection string it was given. What a failure may name is the host
and port — *selected* from the config the driver's own parser produced, rather than filtered out of
the string — and a SQLSTATE. The credential is parsed up front for the same reason: `sql.Open` defers
parsing, so a malformed connection string would otherwise be reported as an unreachable database.

**An app Burrow provisioned nothing for runs no check pod at all**, so an unpublished app with no
database costs nothing. `burrow app checks disable <app>` turns the check off for an app and
`burrow app checks enable <app>` puts it back; the read is on both CLIs and the write is
operator-only.

The check waits for the rollout to **settle** before it runs, which is when
[ADR-0072](adr/0072-deploy-and-run-lifecycle-hooks.md) §4's `post-deploy` phase fires, so the exposure
check reaches the release that was just deployed rather than the one it replaced. A rollout that did
*not* settle is checked anyway: an app that cannot reach the database it was given is a common reason
a rollout never becomes ready.

One thing §4 describes is **not** built: there is no volume check. The only volume Burrow authors on
a user's Pod is the **read-only** Secret projection of [ADR-0089](adr/0089-a-secret-can-reach-an-app-as-a-file.md),
which nothing can create, read back and delete a file under — so a volume dependency would still have
to be invented rather than derived.

The one escape hatch is `Adapter.WithPodMutator` ([ADR-0061](adr/0061-deploy-pod-mutator-seam.md)),
a `func(*corev1.PodSpec)` applied on both create and update. It is a **compile-time seam for an
embedder**: nothing in this repository wires one, and it is not reachable from the CLI or the
API. A nil mutator leaves the Deployment byte-for-byte unchanged, which is pinned by a test.

It also covers the **one-off command Job** of `burrow app run`
([ADR-0048](adr/0048-one-off-command-runner.md)), which runs the app's own image in the app's
namespace with the app's environment and therefore faces the same admission and scheduling
constraints as the app itself.

Pods running **Burrow's own images** take a second hook, `Adapter.WithPlatformPodMutator`
([ADR-0073](adr/0073-placement-policy-reaches-every-authored-pod.md)): the add-on instance
Deployment, the log-collector DaemonSet, the metrics-collector Deployment, and the backup and
restore Jobs. Two hooks rather than one because the two sets take different placement policy — an
operator may want the tenant's image on tenant-only nodes and their own Postgres and collectors
somewhere else — and one hook could serve that only by having the operator guess which kind of pod
it was handed. The build Job has a third hook of its own, `WithBuildPodMutator`.

Both hooks are equally unwired here, and a nil mutator leaves every one of those objects
byte-for-byte unchanged, pinned per path by a test. **Wiring nothing sandboxes nothing**: this is a
seam that makes an operator's isolation reachable, not isolation Burrow enforces. Enforcement is
admission policy on the cluster, not a hook the same binary can decline to wire.

Pods Burrow **causes to exist but does not author** take a third seam,
`Adapter.WithControllerPodPlacement` ([ADR-0077](adr/0077-placement-policy-for-pods-burrow-does-not-author.md)).
Where a third-party controller composes the pod from a custom resource Burrow creates, there is no
pod spec to hand a `func(*corev1.PodSpec)`, so this one is a **value** — `kube.PodPlacement`: node
selector, tolerations, node/pod affinity, topology spread — translated into the fields that
controller offers. Policy the target has no field for is **refused when it is wired**, naming the
JSON path, because a CRD's structural schema prunes unknown fields silently and an operator who is
not told their policy was dropped believes it is in force. The zero value writes nothing at all.
Nothing in this repository wires it. It has somewhere to land: the Postgres add-on instance is a
CloudNativePG `Cluster`, and the wired policy is written into its `spec.affinity` and
`spec.topologySpreadConstraints` ([ADR-0066](adr/0066-postgres-on-cloudnativepg.md) §1).

### Lifecycle hooks

A hook is a command Burrow runs at a named moment, stored per app, environment, and phase
([ADR-0072](adr/0072-deploy-and-run-lifecycle-hooks.md)). It exists because auto-deploy ships an
image with **nobody present**: a user who enables it and changes their schema otherwise has no
supported way to migrate. **Unset means nothing runs**, which is exactly how Burrow behaved before
hooks existed.

| Phase | When it runs | From which image | Default |
| --- | --- | --- | --- |
| `pre-deploy` | Before **any** deploy's image reaches the cluster — an explicit `burrow app deploy`, a build that ends in a deploy, and an unattended auto-deploy alike | the image **being deployed**, so the migration ships with the code that needs it | unset |
| `post-deploy` | After the rollout settles, **whether it succeeded or failed**, and after a rollback too | the image now **serving** | unset |
| `pre-rollback` | Before a rollback puts the older image back | the image being rolled back **away from**, because that is where the code that knows how to undo its own migration lives | unset, and leaving it unset is correct for anyone who migrates forward only |

**A rollback fires `pre-rollback` and never `pre-deploy`**, even though a rollback is mechanically a
deploy of an older image. Rolling back B to A, A's migration tool does not know B's migration
exists, so running it would step back one of *A's own* migrations instead — worse than doing
nothing. This is pinned by a test.

**A failed `pre` hook aborts what it ran ahead of** — the deploy, or the rollback. For a rollback
that abort has a way past it, because two different failures arrive as the same one: the migration
revert failed (aborting is right), or the hook could not run at all — it will not pull, the Job will
not schedule, the command has a typo — in which case the schema is fine and the rollback is blocked
by something unrelated, at the moment an incident leaves least room for a detour.

```sh
burrow app rollback web --skip-hooks
```

It does not run the hook, **leaves the hook configured** (the older way past was `hook unset`, which
deletes it), states the skip in its own output, and records it in the audit log — so "we rolled back
around a broken hook" is a fact somebody can find afterwards
([ADR-0080](adr/0080-a-rollback-is-not-blocked-by-its-own-hook.md)). Reach for it when the hook
could not *run*, not when the revert itself failed: past a failed revert the older code serves
against a schema nobody stepped back.

**The flag exists on `rollback` and nowhere else, and only on the operator CLI.** A deploy can wait
for a broken hook to be fixed; an outage cannot — and the same flag on `deploy` would be a way to
routinely skip migrations. Deciding that a safety step does not apply is a judgement about the
situation, which is not on the agent surface: a rollback the agent runs still aborts, with a message
naming the command a human runs, which the agent relays.

**There is no `post-rollback`.** A rollback fires `post-deploy`, told that it was a rollback:
"did this settle and is it serving?" is the same question whichever direction the image moved, and a
fourth phase would be a second name for one answer. This is pinned by a test too.

### What a post-deploy hook is told

A `post-deploy` hook runs **whether the rollout succeeded or failed** — a hook that fired only on
success could not report the case it exists for, which is the crashloop an unattended 3am push
actually produces. It is told the outcome through environment variables, beside the app's own config
and Secret:

| Variable | Value |
| --- | --- |
| `BURROW_HOOK_PHASE` | the phase running; set on **every** hook, so one script can serve several |
| `BURROW_APP`, `BURROW_ENVIRONMENT`, `BURROW_IMAGE` | what the hook is talking about; set on every hook |
| `BURROW_DEPLOY_KIND` | `deploy` or `rollback` |
| `BURROW_DEPLOY_OUTCOME` | `succeeded` or `failed`, and nothing else |
| `BURROW_DEPLOY_REASON` | on failure, a member of [ADR-0074](adr/0074-burrow-observes-what-it-manages.md)'s closed vocabulary — `CrashLoopBackOff`, `Unschedulable`, `OOMKilled`, `CreateContainerConfigError`, `ImagePullBackOff`, `ErrImagePull`, `VolumeUnavailable`, `ProgressDeadlineExceeded`, `DeadlineExceeded`, `WorkloadMissing`. **Set and empty on success**, so a script under `set -u` does not abort before it can report |
| `BURROW_DEPLOY_DETAIL` | one Burrow-authored line of context — replica counts, a pod's phase, a container's waiting reason |
| `BURROW_RELEASE` | the release being reported on |

The reason is the point: a hook branches on `CreateContainerConfigError` and knows a retry is
pointless, where a message inviting interpretation would have it retry. It is the **same string**
`burrow app status` and `burrow failures` report for the same pod at the same moment — there is one
vocabulary, not a second one invented for hooks.

**The detail never carries the application's own output.** A crash-loop `Issue` on the live status
surface includes a bounded tail of the app's previous log, which may contain anything it printed;
read live and discarded that is an accepted trade, and it is not one for a value copied into a Job's
environment where it would sit in a Kubernetes object. The hook gets the reason and Burrow's own
replica summary; the app's output stays in `burrow app logs`.

**The deploy waits, so a slow verdict is a slow deploy call** — and the same is true of a rollback,
which waits on the same bound. The wait is bounded by
`deploy.settle_timeout` (5m by default) and ends early on any blocking condition, but a rollout
wedged for a reason no pod reports takes the whole bound. Both clients wait it out: the bound each
call gets is derived from the bound the control plane declares for that call, so a client can no
longer give up on work the server is still entitled to be doing. Lower `deploy.settle_timeout` for
that environment if a shorter verdict matters.

**A failed `post-deploy` hook undoes nothing.** By the time it runs the image is live and the release
is recorded, so there is nothing left to abort — the failure is audited and comes back as a hint on
an otherwise successful deploy. **Burrow never rolls back by itself**
([ADR-0072](adr/0072-deploy-and-run-lifecycle-hooks.md) §6): the remedy for a failed deploy is a
judgement about blast radius and data, and an automatic rollback of a deploy whose `pre-deploy`
migration already ran can leave the schema ahead of the code it just restored. The hook is told what
happened and decides — including by calling `burrow app rollback` itself.

**What "succeeded" means.** The rollout completed — the newest revision is the only one left and it
is available — and no pod reported a blocking condition. For an app that declares a health endpoint
(or is published) that includes passing its readiness probe
([ADR-0076](adr/0076-health-checks-readiness-only-and-dependencies-at-deploy-time.md)); for an app
that declares nothing and is not published there is no probe, so it means the containers started. A
smoke test is the natural `post-deploy` hook: Burrow tells the hook the deploy *happened*, and the
hook decides whether it *worked*.

The command runs as a `batch/v1` Job in the app's namespace, from the named image, with the app's
config and per-app Secret injected exactly as `burrow app run` does — the same machinery, the same
ten-minute bound, the same pod mutator. **A failed hook aborts the operation it preceded**: the new
image does not roll out, the running version keeps serving, and the failure comes back as the
deploy's own failure (HTTP 422, `code: hook_failed`) carrying the phase, the exit code, and the
command's captured output. The Job is left for an hour so a failure can be inspected. A hook that
does not finish inside the run window, or whose pod cannot start, is a failure of the same kind.

Hooks are **serialized per app and environment**: two pushes in quick succession queue rather than
running two migration Jobs against one database. The lock is in-process, which is sound because
burrowd runs a single replica.

Limits worth knowing: setting a hook is an **operator action** — `burrow app hook` is not on the
`burrow-agent` surface, because a pre-deploy hook is standing authority for a command that runs on
every deploy. **Burrow does not understand migrations** — it runs your command, and versioning,
ordering, and idempotency stay your tool's job. A hook shares `burrow app run`'s **ten-minute**
bound, so a migration slower than that is reported as a failure. The audit log records the phase,
the command, the image, and the exit code, and **never the command's output**. Every deploy and every
rollback already **waits for its rollout to settle** before returning, bounded by
`deploy.settle_timeout` (see [Operational limits](#operational-limits)), so a `post-deploy` hook adds
no wait — and neither does a deploy-time dependency check. One observation is made and handed to
every party that wants it, so they can never report differently on the same rollout and the bound is
spent at most once per operation.

### Deploy and rollback wait for the rollout and report what it did

Both wait for the rollout before they answer, bounded by `deploy.settle_timeout` and ending early on
any blocking condition ([ADR-0092](adr/0092-a-deploy-reports-its-rollout.md),
[ADR-0093](adr/0093-a-rollback-reports-its-rollout.md)). A rollout that does not become ready is
reported as such, names the reason from the closed set and the pod's own explanation, and exits
non-zero. `--wait=false` returns at submission and reports the outcome as **unknown** — never as
good; it is on the operator CLI only.

The **release status is not the rollout**. `deployed` records that Burrow applied the release, and
it is what a rollback walks back from; whether its Pods came up is recorded beside it and shown by
`burrow app history` as `deployed (not ready: <reason>)`.

**Nothing rolls back by itself.** A rollout that does not become ready leaves the previous ReplicaSet
serving, which is Kubernetes behaving correctly; the report says which release that is, and the
remedy is an explicit `burrow app rollback` or a deploy of a release chosen from the history. For a
rollback that itself does not come up, the release still serving is the one being rolled back *away
from*, and rolling back again returns to that same release — so the way out is picking a release from
`burrow app history` and deploying its image.

An app with a **`post-deploy` hook** ([Lifecycle hooks](#lifecycle-hooks)) or a **derived dependency**
([Deploy-time dependency checks](#deploy-time-dependency-checks)) adds no second wait: one
observation of the rollout is made and handed to every party that wants it.

Release history is unbounded: releases are never pruned, only deleted wholesale when the app is.

---

## Getting an image built

Two build paths, plus the option of bringing an image you built yourself
([ADR-0008](adr/0008-two-build-paths.md)).

| Path | Command | What it does | Limits |
| --- | --- | --- | --- |
| Client-side | `burrow app deploy <app> --image <ref> --build <dir>` | Shells out to `docker build -t <ref> <dir>` then `docker push <ref>`, then deploys. | Needs a working Docker daemon and a prior `docker login` on the operator's machine. `--image` is required. Burrow inspects nothing — the directory must build under `docker build`. |
| In-cluster from a git ref | `burrow app build <app> --source <repo> --ref <sha-or-tag> [--image <target>]` | burrowd clones the ref and builds in a Job in the dedicated `burrow-builds` namespace, then the built image rejoins the guarded deploy path with its digest pinned. Only the git reference crosses the control channel, never code ([ADR-0004](adr/0004-code-never-over-mcp.md)). | See below. |

The in-cluster build ([ADR-0053](adr/0053-in-cluster-build-from-source.md)), concretely: an
`alpine/git:2.45.2` init container does a depth-1 fetch and checkout; the build container is
`ghcr.io/burrow-cloud/burrow-builder` and picks **buildah when a Dockerfile is present** and
the **Cloud Native Buildpacks lifecycle when it is not**. One attempt (`backoffLimit: 0`),
250m CPU / 512Mi requested and 2 CPU / 2Gi limited, a 30-minute wait, and a
`ttlSecondsAfterFinished` on the finished Job set from `build.job_retention` (three days by default
— see [Operational limits](#operational-limits); a success is deleted immediately, a failure is
left in place for diagnosis). Job names are content-derived, so re-running the same
repo/ref/target reuses a succeeded or active build. A capacity pre-flight refuses the build
when no node has room.

**A build's pods say which app they are building.** The Job and the pod it creates both carry
`burrow.cloud/build-app` and `burrow.cloud/build-environment` (the app and environment the build's
deploy targets) and `app.kubernetes.io/component: build`. The pod matters on its own because a log
collector reads the labels of the pod a line came from and knows nothing about the Job above it, so
build output is attributable to an app by its plain name, and the component label is what separates
it from that app's own running pods. `app.kubernetes.io/name` on a build's objects is the Job's own
content-derived name, not the app — it is the selector Burrow reads a build's pods by. A build
started without a recorded intent carries the component label and nothing else, because nothing
recorded an app.

**A build that succeeds is not discarded when its client goes away.** The build Job outlives the
request that started it, so it also carries what it is for — the app, the environment, and the
reference its deploy pins. burrowd sweeps for build Jobs that succeeded and whose deploy never ran
(every minute by default; `BURROW_BUILD_RECONCILE_INTERVAL` changes the cadence or turns it off) and
finishes them through the same guarded deploy path, at most once per image. That covers a dropped
connection, a `Ctrl-C`, and a control plane restarted while a build was running. Its deploys are
audited as `recovered=build`, so the trail never claims somebody was present. Three consequences
worth knowing:

- A recovered deploy carries **no confirmation**, because nobody is there to give one, so a
  `confirm`-disposition `app.deploy` guardrail holds it. The hold is recorded in the audit log and
  the build is left in place, so re-running the same build with `--confirm` reuses the image instead
  of building it again.
- A build that has been **overtaken is not deployed**. If anything was released for that app after
  the build finished — or the app was deleted — recovery would move the app backwards or resurrect
  it, so the build is set aside instead. It stays available to re-run deliberately.
- There is **no build id to ask about**. A build's outcome is visible once it lands
  (`burrow app history`) or, when it was held, in `burrow audit` — not while it is in flight.

Sharp edges on that path:

- The **build container runs `privileged`** with `seccompProfile: Unconfined` and a writable
  root filesystem ([ADR-0059](adr/0059-oss-build-container-runs-privileged.md), superseding
  [ADR-0056](adr/0056-build-security-context-for-the-oss-builder.md)) — building a container
  inside a container needs it, and the trust argument is that the cluster and the source both
  belong to the same single tenant. The clone init container keeps the restricted floor.
- The **Buildpacks (no-Dockerfile) path cannot push to the plain-HTTP in-cluster registry.**
  It fails fast and says so; push to an external registry with `--image` instead.
- **Private git works, over HTTPS only.** A source-provider credential
  ([ADR-0057](adr/0057-source-provider-credentials.md)) is
  mounted as a git credential helper and a registry auth file, keyed by host —
  `github.com`/`ghcr.io` and `gitlab.com`/`registry.gitlab.com` only. An `ssh://` or `git@`
  remote gets no credential, and self-hosted GitHub Enterprise or GitLab hosts are not mapped,
  so both clone anonymously.
- `burrow-agent build` requires `--image`; the operator CLI can omit it.

---

## Reaching an app at a URL

| Capability | Command | What it does | ADR |
| --- | --- | --- | --- |
| Publish at a hostname | `burrow app publish <app> --host <fqdn> --port <n>`, `burrow-agent publish` (alias: `expose`) | The whole chain in one operation: a ClusterIP Service (`80` → the container port), an Ingress for that one host, the DNS record when a provider is configured, a pre-flight that confirms the host resolves to this cluster and answers on port 80, then the cert-manager annotation and the wait for the certificate. **TLS is on by default**; `--tls=false` publishes plain HTTP and is refused on an HSTS-preloaded domain. The result carries `reachable`, and when it is false, `blocked_on` and `next`. Both `--host` and `--port` are required. | [0018](adr/0018-reaching-an-app-at-a-url.md), [0041](adr/0041-flatten-path-to-a-reachable-app.md) |
| Remove routing | `burrow app unpublish <app>`, `burrow-agent unpublish` (alias: `unexpose`) | Deletes that Service and Ingress. Leaves the Deployment, the TLS Secret, and any DNS record alone. Not guardrailed. | [0024](adr/0024-cli-command-taxonomy.md) |
| Install the routing substrate | `burrow cluster ingress install --email <you>` | Installs ingress-nginx `controller-v1.11.3` (cloud manifest, so a `type=LoadBalancer` Service) and cert-manager `v1.16.2` if absent, then a Let's Encrypt `ClusterIssuer` (default name `letsencrypt`; `--staging` selects the staging directory). Detect-and-skip per component. | [0022](adr/0022-routing-backend-and-supported-kubernetes.md), [0042](adr/0042-use-existing-ingress-controller.md), [0043](adr/0043-public-reachability-is-a-loadbalancer.md) |
| Public DNS records | `burrow app domain add <host> --address <ip>` / `--app <app>`; `burrow app domain remove <host>` | Creates, updates, or deletes an `A` or `CNAME` record at a configured provider. `A` when the address parses as IPv4, `CNAME` otherwise. Idempotent. | [0018](adr/0018-reaching-an-app-at-a-url.md), [0023](adr/0023-provider-credentials.md) |
| Diagnose reachability | `burrow app reachability <app> [--wait --timeout]` | Walks the chain and reports the first broken link in `blocked_on`: `deployment` → `workload` → `ingress` → `ingress controller` → `tls certificate` → `dns` → reachable. Resolves the host over public DNS and compares against the controller-assigned address. `--wait` polls (3s, 3-minute default). | [0018](adr/0018-reaching-an-app-at-a-url.md), [0041](adr/0041-flatten-path-to-a-reachable-app.md) |

**DNS providers actually implemented: DigitalOcean and Cloudflare.** Both are hand-rolled HTTP
clients supporting `A` and `CNAME` records, with a longest-suffix zone match and a 3600s TTL.
Any other provider type returns "not implemented". `burrow config provider types` also lists
`github` and `gitlab`, which are *source* providers for the build path, not DNS.

TLS is **HTTP-01 only**; there is no DNS-01 solver, even though a DNS provider token is
already present. Certificates are issued through cert-manager, not by Burrow.

Limits worth knowing before you plan around this surface:

- **The IngressClass is the hardcoded literal `"nginx"`**, and so is the cert-manager HTTP-01
  solver class. [ADR-0042](adr/0042-use-existing-ingress-controller.md) decides that Burrow
  should detect and bind to the cluster's existing controller; **that detection is not built**.
  Capability probing recognizes an ingress-nginx controller only, and `burrow cluster
  bootstrap` disables k3s's Traefik precisely so ingress-nginx owns ingress.
- **One hostname per app.** The Ingress is named after the app and carries a single rule with a
  single host and a `/` prefix path; publishing again replaces it. No path-based routing, no
  multiple hosts, no multiple backends.
- **HTTP(S) only.** No TCP or UDP exposure, no websocket configuration, and no way to set a
  custom ingress annotation — the only annotation Burrow ever writes on an app Ingress is
  `cert-manager.io/cluster-issuer`, and only once the publish pre-flight has passed (or on an
  `expose --tls`, which is the primitive and runs no pre-flight).
- **The certificate is requested only after a pre-flight passes.** A publish routes the host over
  plain HTTP first, writes the DNS record, then checks that the host resolves to this cluster — at
  the zone's own nameservers when burrowd can reach them, a recursive resolver otherwise — and that
  a plain-HTTP request to the ACME challenge path is answered. Only then is the cert-manager
  annotation attached, which is what opens the ACME order. A path that cannot work therefore spends
  none of Let's Encrypt's failed-authorization budget, and cert-manager's own self-check is the
  backstop rather than the first line. The pre-flight's HTTP request leaves burrowd, so it traverses
  the cluster's egress and the load balancer; a cluster whose egress cannot reach its own external
  address reports `blocked_on: "http path"` and requests no certificate.
- **`publish` writes DNS only when a provider is configured**, and never with `--no-dns`. With no
  provider it writes nothing, reports the address to point the host at, and still runs the
  pre-flight — so a host pointed by hand publishes on the next run.
- **A deploy with a port does not create a Service on its own.**
  [ADR-0041](adr/0041-flatten-path-to-a-reachable-app.md) decides it should; there is no `--port`
  on `deploy`, and the only app Service in the code is created by publish.
- `reachability` never probes the app itself. "Reachable" means every link in the chain is in
  place, not that the app answered.

---

## Configuration and secrets

Two separate stores with deliberately different transport ([ADR-0028](adr/0028-app-config-and-secrets.md),
[ADR-0029](adr/0029-secrets-through-the-control-plane.md)). `deploy` takes no environment
arguments; both stores are sourced at deploy time.

| | `burrow app config` | `burrow app secret` |
| --- | --- | --- |
| Commands | `set <app> KEY=VALUE`, `unset`, `list` | `set <app> KEY --stdin` (or `KEY=VALUE`), `unset`, `list`, `mount`, `unmount`, `mounts` |
| Stored in | the control-plane Postgres (`app_env`) | a Kubernetes Secret, `burrow-app-<app>-secrets`, in the app's namespace |
| Reaches the Pod as | individual `env` entries inlined in the Pod template | `envFrom` a `secretRef` on that one Secret (`optional: true`), plus a **file per mounted key** |
| `list` shows | keys **and values** | **keys only** |
| Scope | **app-global** — the same values apply in every environment | **per-environment**, because the Secret lives in the environment's namespace |
| On set/unset | re-applies the workload so the change rolls; `--no-restart` skips it | patches a `burrow.cloud/restarted-at` annotation so the change rolls; `--no-restart` skips it. An app with a `--no-env` key re-applies the workload instead (see below) |
| Guarded by | `app.config`, **confirm** by default — the write rolls the app, whatever the value is ([ADR-0098](adr/0098-a-config-write-is-guarded.md)). `--no-restart` is not a way past it: the value still lands in the store and the next deploy carries it | nothing. `secret set` is absent from the agent binary and `secret unset` carries no value |
| With no running release | persisted, applied at the next deploy | persisted, applied at the next deploy |
| On `burrow-agent` | `config set` / `config unset` / `config` — **yes** | `secret unset`, `secret mount` / `unmount` / `mounts`, and key listing — **`secret set` does not exist on the agent binary** |

Secret values traverse the control-plane API, are written straight into the Kubernetes Secret,
and are never written to Postgres, never logged, and never audited (the audit record carries
sorted **key names** only). The same Secret is what `burrow app run`, a lifecycle hook and the
deploy-time dependency check inject, so a one-off command sees `DATABASE_URL` and everything else
exactly as the app does — including through the same door: a `--no-env` key reaches those Jobs as a
**file**, not as a variable every child process of the command would inherit.

**A secret key can be mounted as a file** ([ADR-0089](adr/0089-a-secret-can-reach-an-app-as-a-file.md)).
`burrow app secret mount <app> KEY` projects that one key into a file; `--filename` names it
something else, and `--dir` moves the directory for the whole app. The value does not move — same
Secret, same writer — only the projection changes.

| | |
| --- | --- |
| Where | one Secret volume per app, mounted **read-only** at `/run/secrets` (`--dir` overrides it **per app**, never per key) |
| Mode | `0400`, owned by the container's user — an app that drops privileges and *then* opens the file needs its own `runAsUser`, not a different mount |
| What is in it | **only the keys that were mounted** (`items`), so mounting one key does not put every other secret the app holds on disk |
| The path | arrives as `BURROW_SECRETS_DIR`; the **value never does**. Point an app that hardcodes a path variable at the file with `burrow app config set` |
| Rotation | a whole-volume Secret mount is updated in place by the kubelet, so `secret set` replaces the file and restarts the pod; `--no-restart` gives an app that re-reads the file rotation with no downtime |
| The variable | **stays** unless you pass `--no-env`. Mounting adds a file; on its own it does not remove the environment variable, because the code that reads the file has to be deployed before the variable it replaces disappears |
| Rollback | a mount is app configuration, not a release property, so a rollback **keeps** the file (and keeps a `--no-env` key out of the environment) |

`burrow app secret mounts <app>` lists what is mounted and where. `mount` refuses a key that is not
set: an app that starts, finds no file, and fails at the moment it needs the credential is the
failure this exists to avoid.

**`--no-env` makes a key file-only**, and it has a stated price. `envFrom` sources the Secret
wholesale and cannot exclude one key, so the first `--no-env` key on an app switches its Pod template
to an enumerated `secretKeyRef` per remaining key. On that app, `secret set` of a **new** key
re-applies the workload rather than bumping the restart annotation — slower, and the key still
arrives. Every other path that writes a key into an app's Secret out of band — `addon attach`,
`addon detach`, a restore cutover — rolls the app the same way, so an attach still lands
`DATABASE_URL` in the container. **An app with no `--no-env` key keeps `envFrom` and its Pod
template is unchanged.**
A key is file-only only while it is mounted: re-mount with `--no-env=false`, or `unmount` it, and the
variable is back. A mount that does not mention `--no-env` leaves the marking as it is, so renaming a
file cannot silently return a credential to the environment.

Two more limits: keys must match `^[A-Za-z_][A-Za-z0-9_]*$`, and **Burrow enforces no size
limit** on a value — the effective ceiling is Kubernetes' own Secret size limit, unenforced
and unsurfaced. The config store is guardrailed by `app.config`
([ADR-0098](adr/0098-a-config-write-is-guarded.md)); the secret store is not, because setting a value
is absent from the agent binary and removing a key carries no value.

---

## Registries and credentials

| Capability | Command | What it does | ADR |
| --- | --- | --- | --- |
| Private registry pull | `burrow config registry login <host> -u <user> --password-stdin` | Writes a `kubernetes.io/dockerconfigjson` Secret named **`burrow-registry`** into the app namespace and patches it onto that namespace's **`default` ServiceAccount**, so app Pods inherit it. Multi-host: repeated logins merge into one `auths` map. | [0017](adr/0017-private-registry-authentication.md) |
| | `burrow config registry logout <host>` | Removes one host; removing the last one deletes the Secret and detaches it from the ServiceAccount. | [0017](adr/0017-private-registry-authentication.md) |
| | `burrow config registry list` | Lists configured hosts. Credentials are never printed. | [0017](adr/0017-private-registry-authentication.md) |
| Vendor tokens | `burrow config provider add <cloudflare\|digitalocean\|github\|gitlab> [--name]` | Reads the token hidden from a TTY or from stdin, sends it over the control-plane API, and burrowd writes it into the `burrow-credentials` Secret. DNS tokens are **verified against the vendor before anything is written**; `github`/`gitlab` tokens are not. | [0023](adr/0023-provider-credentials.md), [0030](adr/0030-credentials-through-the-control-plane.md), [0057](adr/0057-source-provider-credentials.md) |
| Object-storage destination | `burrow config provider add s3 --endpoint <url> --access-key-id <id> [--region] [--bucket \| --create-bucket] [--retention-days N] [--confirm]` | Registers an S3-compatible backup destination. The credential is a **pair**: the id is a flag, the secret access key is read hidden from a TTY or from stdin, and both land as **two keys in the same `burrow-credentials` Secret**. Before anything is written it creates or verifies the bucket, **writes and deletes a probe object**, and reconciles the bucket's lifecycle rules against `--retention-days`. Once registered, `burrow addon backup postgres <app>` writes there and verifies the object before recording the backup. | [0063](adr/0063-object-storage-provider.md), [0023](adr/0023-provider-credentials.md) |
| In-cluster registry | `burrow cluster registry install --host <fqdn>` | Deploys Zot (`ghcr.io/project-zot/zot-linux-amd64:v2.1.2`), a 5Gi PVC, a ClusterIP Service on 5000, and an nginx Ingress with cert-manager TLS and basic auth. Wires burrowd's default build push target, and installs the generated pull credential through the `registry login` path. `uninstall` reverses it. | [0054](adr/0054-install-is-control-plane-only.md) |

Three things about this area that are easy to get wrong:

- **`burrow config registry` and `burrow cluster registry` are different things.** The first is
  external *pull* credentials; the second is an in-cluster registry to *push* to.
- **The registry login runs with the operator's kubeconfig, not through burrowd.** burrowd
  never sees the credential and cannot read that Secret. It is written **per namespace**, so an
  additional environment needs its own login (`--app-namespace`); there is no `--env` flag on
  the command.
- **burrowd never contacts the registry to pull** ([ADR-0040](adr/0040-burrowd-never-contacts-the-registry.md)) —
  the kubelet does, using the inherited pull secret. burrowd's one outbound registry call is
  tag listing for auto-deploy, and **that call is anonymous**: a private repository returns 401
  and never auto-deploys. That gap is known and open.

[ADR-0046](adr/0046-registry-onboarding.md) (auto-wiring the developer's existing code-provider
registry) is 🟡 Proposed and deliberately unbuilt; only its in-cluster-registry half shipped,
via ADR-0054.

### The object-storage provider is a backup destination, and only that

`burrow config provider add s3` registers somewhere **outside the cluster** for a backup to go
([ADR-0063](adr/0063-object-storage-provider.md)), and with one registered `burrow addon backup
postgres <app>` writes there — see [Backups](#backups) for what "wrote there" is allowed to mean.

What registration does, and why each part is there:

- **It is addressed by endpoint, not by vendor.** `--endpoint` is the S3-compatible API URL and
  the vendor is whoever answers it, so there is no vendor list to maintain. S3 compatibility is
  a spectrum rather than a standard, so a vendor is supported when it has been *tested*, not
  when it claims compatibility.
- **The credential is a pair, held as two keys in the one `burrow-credentials` Secret.** Not a
  second Secret — that would have meant widening burrowd's `resourceNames: ["burrow-credentials"]`
  grant, which is the tightest grant Kubernetes offers. The `providers` row records the two key
  *names* plus the non-secret endpoint, region and bucket, so the destination can be inspected
  without reading a Secret at all.
- **It verifies the destination by writing and deleting a probe object.** A wrong key or a
  typo'd endpoint fails *there*, while somebody is watching — not at the first scheduled backup,
  silently.
- **The bucket is recorded, never inferred.** `--create-bucket` has Burrow create its own with a
  readable prefix and a random component (bucket namespaces are global per vendor, so a fixed
  name is both likely taken and guessable) and record the name it created. `--bucket` points it
  at one that already exists, which is verified rather than assumed. Burrow only ever writes to
  the bucket it recorded.
- **It reconciles bucket lifecycle against backup retention, and refuses disagreement.** A rule
  that expires objects sooner than a retained backup needs them leaves a backup set that lists
  fine and cannot be restored — discovered during recovery. `--retention-days` declares how long
  backups must stay restorable; with no window declared, nothing prunes Burrow's backups, so any
  age-expiring rule is refused. Where the lifecycle configuration **cannot be read** — the vendor
  does not serve the API, or the credential may not read it — the check is reported as
  **unknown**, never as verified.
- **Creating a bucket is `confirm`-guarded (`bucket.create`); deleting one is not a Burrow
  operation at all.** Deletion's blast radius is every backup the platform holds, and a bucket
  name lives in a global namespace, so a mistaken argument could reach outside the cluster
  entirely. It is absent from both CLIs and `burrow-agent`'s `guard` reports it as such, naming
  the vendor's own tool as what performs it.

**Scope this credential to one bucket at the vendor wherever the vendor permits it.** It grants
write access to the store that will hold every backup, which makes it the most consequential key
in `burrow-credentials` — more so than a DNS token, whose worst case is a record you can put
back. The Secret's RBAC is the same scoped grant as every other provider's; the tighter control
available is at the vendor, and it is worth taking.

And what it deliberately does **not** do, which is as much of the decision as the feature:
no object browser, no `cp`/`sync`/`ls` of arbitrary prefixes, no presigned URLs, and no bucket
policy, IAM, replication or cross-region surface. The vendor's own CLI is better at all of those.
A capability enters here only when a Burrow feature requires it.

---

## Add-ons

A curated, compiled-in catalog of permissively licensed backing services
([ADR-0025](adr/0025-building-block-addons.md)). Three of the four install into the `burrow-addons`
namespace as a single-replica `Recreate` Deployment plus a ClusterIP Service, with a
ReadWriteOnce PVC when they need storage. **There are exactly four.**

`postgres` is the exception, and it is the whole of
[ADR-0066](adr/0066-postgres-on-cloudnativepg.md) §1: it is a **CloudNativePG `Cluster`** and
nothing else. Burrow writes one custom resource and the operator composes the StatefulSet, the pod,
the volume and the services from it. That makes the operator a **cluster prerequisite**:
`burrow cluster postgres install` first, once per cluster, with a kubeconfig — installing
cluster-scoped CustomResourceDefinitions needs cluster-admin, which the agent does not have
(see [Cluster lifecycle](#cluster-lifecycle)). Installing the add-on on a cluster without it is
refused by name, naming that command.

| Add-on | Image | Port | Storage | Extra workload |
| --- | --- | --- | --- | --- |
| `logs` | `victoriametrics/victoria-logs:v1.51.0` | 9428 | 10Gi PVC | a Fluent Bit DaemonSet (`fluent/fluent-bit:3.2.10`) reading `/var/log`, tolerating all taints. Each record is keyed by its source `filename` and carries `kubernetes_pod_name`, parsed out of that filename rather than read from the Kubernetes API — the collector holds no credential and needs none |
| `metrics` | `victoriametrics/victoria-metrics:v1.115.0` | 8428 | 10Gi PVC, retention per `addon.metric_retention` (default 744h — the month it was) | a vmagent Deployment (`victoriametrics/vmagent:v1.115.0`) scraping the app **and** add-on namespaces |
| `cache` | `valkey/valkey:8.0` | 6379 | **none — ephemeral** | — |
| `postgres` | `ghcr.io/cloudnative-pg/postgresql:17.10-minimal-trixie`, run by the CloudNativePG operator (`1.30.0`) — the **minimal** operand variant, because the standard one bundles barman-cloud, which shells out to GPL-3.0 tooling (ADR-0066 §3) | 5432 | 10Gi, on a claim the operator composes and names `<instance>-1` | none — CNPG's instance manager exports the metrics a sidecar used to |

`burrow addon install <type>` takes **no tuning flags** — no `--size`, no `--storage-class`,
no `--retention`, no version override. (`--env` selects which environment the instance serves, not
how it is built.) Sizes and images are compile-time constants; the metrics add-on's retention is the
one tunable, set cluster-wide with `burrow cluster config set addon.metric_retention <duration>`
before the instance is installed ([Operational limits](#operational-limits)). No PVC sets a
`storageClassName`, so every volume lands on the cluster default
StorageClass. `burrow addon remove <type>` deletes the workload — the Deployment, Service and
collectors, or for `postgres` the `Cluster` — and **keeps the data PVC**: reinstalling the add-on
lands on the same claim and picks the data back up, so for `postgres` the databases, roles, and role
passwords survive and attached apps reconnect on their existing `DATABASE_URL`. Keeping it takes an
extra step under CloudNativePG, because the operator stamps the `Cluster` on the claims it composes
and deleting the `Cluster` would otherwise take them: the claims are **disowned before the `Cluster`
is deleted**, and `--delete-data` deletes them by name rather than leaving them to a garbage
collector ([ADR-0064](adr/0064-addon-removal-keeps-its-data.md) §1). Destroying the volume is the separate, explicit
`--delete-data`. The removal output names the volume it kept and how to reclaim it; the
confirmation the `addon.remove` guardrail holds names the affected apps.

**Which of the two it is rides the route, so an older control plane cannot decide it by default.**
`burrow addon remove` is `DELETE /v1/addons/{name}/data/keep` and `--delete-data` is
`.../data/delete`. A control plane too old to know that keeping is even an option has neither route
and refuses the removal, naming both versions and the upgrade, with the add-on and its data still
there — rather than reading the silence as permission to destroy the volume. The rule this follows,
and the other calls that follow it, are under [Cluster lifecycle](#cluster-lifecycle).

**`--delete-data` has a second gate the guardrail does not provide.** On a terminal it prints a
warning naming the data volume, its namespace, the attached apps whose databases are in it, and
the backup volume that survives — then requires the add-on's name to be **typed back** before
anything is removed; anything else typed, an empty line, or EOF aborts and nothing is deleted.
With no terminal to type into it **refuses** rather than proceeding, unless
`--acknowledge-data-loss` is passed, so a script that destroys databases had to say so in the
script. `--confirm` satisfies the `addon.remove` guardrail and is deliberately not that
acknowledgement. The enumeration behind the notice is best-effort: a control plane that will not
answer degrades it to the volume-concrete message rather than making a wedged add-on unremovable.
The prompt is a legibility device for humans, **not a security control** — what keeps an agent
away from this is that the whole verb is absent from `burrow-agent`
([ADR-0064](adr/0064-addon-removal-keeps-its-data.md) §2).

**`--delete-data` takes a final backup first, and removes nothing if it fails.** Where an
object-storage provider is registered, burrowd dumps every attached database to that store
*before* it destroys anything, and a backup that does not arrive aborts the whole removal with
the volume, the workload and the registry row all intact — the error naming the app and the
closed reason (`Unschedulable`, `StoreRejected`, …) rather than a timeout. "Arrived" means the
`Backup` row says `completed` at an object-store destination, which [ADR-0063](adr/0063-object-storage-provider.md)
§7 only allows once the object was written *and* read back; an in-cluster dump is never accepted
as the final backup, because it shares a failure domain with the volume about to be destroyed.
With several stores registered burrowd refuses to guess and `--backup-destination` names one.
`--skip-final-backup` destroys the data without one and says so in the output — it exists because
an add-on is often removed *because* it is wedged, and a wedged instance cannot be dumped
([ADR-0064](adr/0064-addon-removal-keeps-its-data.md) §5). With **no** object-storage provider
registered the behaviour is what it always was, and the output says no off-cluster copy was
taken. An instance that will not say which databases it holds is refused rather than destroyed
blind: `--skip-final-backup` is the way past that too.

**Removal is operator CLI only** — the whole verb, not just `--delete-data`, is absent from
`burrow-agent`, as `detach` and `restore` are. Because there is one add-on instance per type per
cluster and every app's database lives on the one shared `postgres`, removing it is removing
*the* add-on and takes every attached app down at once, which is
[ADR-0065](adr/0065-what-belongs-on-the-agent-surface.md) §2 tier 1: not compiled into the agent
binary at all.

**A retained volume stays visible after the removal that created it.** `burrow addon list` reports
the claims an earlier removal left behind in a section of their own, below the add-on table: the
claim name, the add-on it belonged to, whether it holds the add-on's `data` or its `backup` dumps,
its size, and its namespace — plus both ways out, reinstalling to get the data back or
`kubectl delete pvc` to get the storage back. In `--json` they are a separate `retained_volumes`
array alongside `addons`, so a retained claim is never readable as a running add-on. Nothing
reclaims them automatically and nothing should
([ADR-0064](adr/0064-addon-removal-keeps-its-data.md) §6).

Retained claims are found by reading the **cluster**, not the registry: a removed add-on leaves no
registry row, which is exactly why its volume used to be invisible. A claim is Burrow's because of
the labels it was created with (`app.kubernetes.io/managed-by=burrow` plus `burrow.cloud/addon`
naming the type), never because of its name, so a claim of your own in `burrow-addons` is not
reported. A claim is *retained* when no installed add-on of its type owns it — which is what keeps
a live add-on's own volume out of the section.

The listing reports **size, not cost.** The claim knows its capacity; the price per GiB belongs to
the provider, and ADR-0064 leaves that choice open. Reporting cost would need a per-provider price
per storage class and region, obtained from the provider's API or a table Burrow would have to keep
current — a confident wrong number about money is worse than an honest one about bytes.

[ADR-0066](adr/0066-postgres-on-cloudnativepg.md) is **partly built**. §1 is the mechanism: the
`Cluster` is the only one, created per environment and read for status, and the instance is reached,
attached to, dumped and restored exactly where the Deployment it replaced was —
`<instance>.burrow-addons.svc:5432`, opened as `burrow_admin`. §2 and §3 are the backups: with an
object-storage provider registered, `burrow addon install postgres` also writes a pgBackRest `Stanza`
naming that environment's own repository and adds the plugin to the `Cluster` as its write-ahead-log
archiver, so the archive runs continuously; a `ScheduledBackup` takes a base backup daily and asks
for the first one **immediately**, so a new instance is not archiving write-ahead log with nothing to
replay it onto; and `burrow addon backup-instance postgres` creates a `Backup` object on demand. The
install reports what it actually wired — read back off the `Cluster` and the `Stanza`, not off the
destination it resolved — including whether the repository holds a base backup yet. An instance
created before immediate first backups existed reports none, and the install names
`burrow addon backup-instance postgres` as the way to take one. Burrow creates the
custom resources and reads `.status` — it runs no backup tool, constructs no Job, and handles no
superuser credential on that path. Retention is **Burrow's** declared window (`--retention-days` on
the provider), written into the repository as a number of days, so the bucket's lifecycle rules and
pgBackRest's expiry are reconciled against one policy rather than two. An instance installed with no
object-storage provider registered archives nowhere and is byte-for-byte the `Cluster` it was
before; re-running the install after registering one wires it. An install on a cluster that has a
destination but **no plugin** is not refused — the database installs, and the result says plainly
that this instance was created without archiving and what to run to fix it. An instance keeps the
repository it was created against: an install that resolves a *different* bucket is refused rather
than re-pointing the stanza, because every backup already taken would become unreachable from the
stanza that wrote them.

§4's restore is now built as `burrow addon restore-instance postgres`: it takes a physical backup of
the instance's current state first and stops without changing anything if that backup does not reach
the store, replaces the instance with one CloudNativePG recovers from the repository, waits for it to
serve, and reissues every attached app's connection string — each into the variable that app is attached under — and restarts it. It is instance-scoped — every
database on the instance goes back to the same point together — so it is a separate verb from `addon
restore <addon> <app>`, is absent from `burrow-agent`, and asks for the instance's name to be typed
back on a terminal.

**What has never been tried is the thing that matters.** The code above is exercised against fakes: a
fake Kubernetes API, a fake dynamic client that reconciles nothing, and a fake object store. **No
restore from a real object-storage destination has been run — not against Backblaze B2, not against
any vendor — and the pgBackRest CNPG-I plugin is experimental by its maintainers' own word.** Passing
tests say Burrow composes the `Cluster` CloudNativePG 1.30.0's published schema describes and sequences
the steps in the order it intends; they say nothing about whether pgBackRest will hand the data back.
ADR-0066 §3 requires a real restore from the real destination before this is relied on, and that has
not happened. Until it does, the only recovery path this project has actually watched work is
[ADR-0032](adr/0032-postgres-backups.md)'s single-app `pg_dump` / `pg_restore`, which is kept
deliberately (§4) and is what `burrow addon restore` means; a physical backup is refused there by name
rather than applied to one app. Backup SIZE is also not reported for a physical backup:
`Backup.status` carries none, there is no `VolumeSnapshot` to join to under the plugin method, and
sizing a pgBackRest backup means summing objects under its repository path — so those rows list with
no size rather than a made-up one.
[ADR-0067](adr/0067-one-database-instance-per-environment.md) is built in full: one
instance **per environment** (§1), and the first environment a registered one named `prod` mapped
to the existing app namespace (§2–§3).

| Capability | Command | What it does |
| --- | --- | --- |
| Install | `burrow addon install <type> [--env] [--name <instance>] [--archive-destination <provider>]` | As above, for one environment — each gets its own instance (ADR-0067 §1). `metrics` additionally needs RBAC the CLI stages client-side first. The result states what the instance does about backups, on the human output and in `--json` as a `backups` object with a closed `state` (`archiving`, `none`, `unknown`) and, for an archiving Postgres instance, a `base_backup` (`present`, `requested`, `none`, `unknown`) — read back off the instance rather than inferred from what the install resolved. Add-on types with no backup path at all say so instead of staying silent. |
| List | `burrow addon list` / `burrow-agent addons` | Type, mode (`installed`/`connected`), backend, endpoint, capabilities. This is how an app is pointed at `cache` — read the endpoint and set it as config. `burrow addon list` additionally reports the volumes an earlier removal kept, in their own section (`retained_volumes` in `--json`). |
| Attach an app | `burrow addon attach postgres <app> [--as <NAME>] [--env] [--name <instance>] [--confirm]` | **Postgres only.** Confirm-gated by `addon.attach` ([ADR-0095](adr/0095-attaching-a-database-is-held-for-a-human.md)), scoped to the instance and env-scopable: the held message names the instance and the variable, and on an app that is already attached it leads with the password rotation, which is the one part of an attach nothing can undo. A held attach provisions nothing. On the named environment's instance, writes two CloudNativePG `DatabaseRole` objects — the login role `app_<app>` the app connects as, and the login-less `app_<app>_data` it is a member of, which owns the app's data across a detach ([ADR-0090](adr/0090-a-detach-keeps-the-database.md) §1) — and a `Database` (`<app>`, owned by the login role), then waits for the operator to apply them — burrowd runs no admin SQL and holds no superuser connection string ([ADR-0066](adr/0066-postgres-on-cloudnativepg.md) §2). It then connects **as the app's own role** to revoke `CONNECT` from `PUBLIC` on that database, the one statement no custom resource can express, and writes the generated connection string into the app's Secret in that environment's namespace, then restarts the workload there. Because the operator reconciles it, an attach takes seconds rather than milliseconds and a control plane restarted mid-attach leaves the operator finishing rather than an app half-provisioned. Re-attaching rotates the password. The URL is never returned, logged, or audited. The variable is `DATABASE_URL` unless `--as` names another, and the chosen name is recorded with the attachment, so detach, rotation and a restore's cutover all follow it. `--as` on an already-attached app **moves** the variable — the new name is written and the old one removed, since the rotation leaves it dead — and the result reports the removed name. A name the app's config or Secret already holds is refused, naming what holds it. An **app name longer than 54 characters is refused at attach**, naming the limit: the data role's name is nine bytes longer than the database's and PostgreSQL truncates an identifier at 63, so past that length the two roles above would be one role that has to be both able and unable to log in. The budget is checked only here — a longer attachment made by an earlier release can still be detached. On an instance that has a standby, a read-only connection string is written beside the first, under the attachment's own name with `_READ` on the end ([ADR-0081](adr/0081-a-postgres-instance-may-have-a-standby.md) §2); on a standby-less instance there is nothing for it to resolve to, so it is absent rather than present and dead. |
| Detach | `burrow addon detach postgres <app> [--env] [--name <instance>] [--delete-data]` | Removes the variable the attachment was written under (`DATABASE_URL` unless it named another), rolls the app off the database, and **drops the app's login role, keeping the database and its rows** ([ADR-0090](adr/0090-a-detach-keeps-the-database.md) §1) — so nothing that was issued to the app still authenticates, and attaching the same app again adopts the database that is there and hands the data back. Confirm-gated by `addon.detach`, which guards the app losing its access rather than the data being destroyed. `--delete-data` destroys the database as well: it closes it to new connections and deletes the `Database` and both `DatabaseRole` objects with a `delete` reclaim policy, and CloudNativePG runs the `DROP DATABASE` and `DROP ROLE` **on that environment's instance**. That flag additionally requires the app's name typed back on a terminal, refuses off one without `--acknowledge-data-loss`, and is **operator CLI only** — absent from `burrow-agent` (ADR-0090 §2, ADR-0065 §2). |
| Remove | `burrow addon remove <name> [--env] [--delete-data] [--skip-final-backup] [--backup-destination <provider>]` | Tears the add-on's workload down and **keeps its data volume** unless `--delete-data` is passed. Confirm-gated by `addon.remove`; the held message names the volume, the attached apps for `postgres`, and whether a final backup is taken first. `--delete-data` additionally requires the add-on's name typed back on a terminal, and refuses off one without `--acknowledge-data-loss`. With an object-storage provider registered it takes a final backup of every attached database first and removes **nothing** if it fails; `--skip-final-backup` destroys the data without one and announces it (ADR-0064 §5). **Operator CLI only** — absent from `burrow-agent` entirely (ADR-0065 §2). |
| Configure an instance | `burrow addon config postgres [<setting> <value>]` | **Postgres only.** Changes an instance that already exists ([ADR-0082](adr/0082-an-addon-instance-is-configured-after-it-exists.md)). Bare, it lists what can be set and what it is set to, read off the `Cluster` rather than the registry. `standbys <n>` patches `spec.instances` (one more than the standby count) — adding the first standby writes a read address into every attached app's Secret beside its connection string, pointing at CloudNativePG's `-ro` service, and restarts them; removing the last one withdraws that address and restarts them again ([ADR-0081](adr/0081-a-postgres-instance-may-have-a-standby.md) §2). `storage <size>` patches `spec.storage.size` and **cannot be undone**: a smaller size than the instance already has is refused at the point of asking rather than written and left to fail in a status field. Growing proceeds; reducing the standby count prints the affected apps by name, asks for the instance's name to be typed back, and refuses off a terminal without `--confirm`. **Operator CLI only** — absent from `burrow-agent`, because it provisions hardware. |
| Connect an existing backend | `burrow addon connect <loki\|prometheus> --endpoint <url> [--auth]` | Registers a backend you already run — **deploys nothing**. Only `loki` (logs) and `prometheus` (metrics) are connectable. Operator CLI only. |
| Query logs | `burrow addon logs [query] [--limit]` / `burrow-agent logs-query` | LogsQL against VictoriaLogs, or LogQL against Loki. Limit clamps to 200 when out of range or unset, capped at 1000. |
| Query metrics | `burrow addon metrics <query>` / `burrow-agent metrics-query` | PromQL **instant** query against VictoriaMetrics or Prometheus. |
| Run a statement | `burrow addon sql postgres <app> -c '…'` / `burrow-agent addon sql` | **Postgres only.** burrowd opens one connection to that environment's instance **as the app's own role** and runs the statement, returning column names, rows, a row count and a truncation flag — a table for a human, the rows under `--json`. Guarded by `addon.sql`, **denied by default**. |

**`addon sql` is how Burrow reads a row out of a database it provisioned**
([ADR-0087](adr/0087-running-sql-against-an-attached-database.md)). It replaces the only route there
was — `burrow app run web -- psql "$DATABASE_URL" -c '…'` — which needed a database client in a
production image, returned a text blob where every other verb returns structured output, and could
not reach a database whose app would not start.

Five things about it are decisions rather than details:

- **It targets one app's database.** The add-on type and the app together name it, the same pair
  `attach` and `detach` take. There is no form that reaches the instance, `template1`, or another
  app's database, and that is structural rather than a check: burrowd connects with the credential
  `attach` already minted for that app, so the **credential** chooses the database and the caller
  does not. The role is `app_<app>`, never the instance superuser, so the statement can touch exactly
  what the application can touch.
- **No connection to the database leaves the cluster.** There is no port-forward and no proxy —
  adding one would make the operator's kubeconfig a credential for tenant data — and because the
  statement runs independently of the application, a database whose app is crash-looping is still
  queryable.
- **Burrow does not tell a read from a write.** One guardrail code gates whether the statement runs,
  not what it does. A `SELECT` can delete (`WITH d AS (DELETE … RETURNING *) SELECT * FROM d`), a
  function call is whatever the function is, and `COPY … TO PROGRAM` is not a query at all — so
  classifying would take a parser plus a model of every reachable function, and a gate labelled
  "reads are allowed" that lets a `DELETE` through is worse than no gate. A `--read-only` mode, where
  Postgres itself refuses the write inside a `READ ONLY` transaction, is the one credible softer
  default and ADR-0087 §6 defers it deliberately.
- **Denied by default**, and env-scopable: `burrow guard set --env dev addon.sql allow`. Deny rather
  than confirm because there is no upper bound on what a statement does, and a human reading a
  hundred-line statement is not meaningfully approving it. The verb IS compiled into `burrow-agent`
  ([ADR-0065](adr/0065-what-belongs-on-the-agent-surface.md) §3 tier 2), so the agent can see that it
  exists and is closed and ask for it, rather than meeting `unknown command` and reaching for a shell.
- **Every run is bounded** by `addon.sql_timeout` and `addon.sql_rows` (see
  [Operational limits](#operational-limits)), on one connection that is closed when the statement
  finishes. A result cut short at the cap says so; it is never silently short. `--json` renders a
  NULL as `null` rather than as an empty string.

**The statement text is written to the audit log.** That is what makes the capability accountable,
and it means a literal in a `WHERE` clause — an email address, a token somebody pasted in — is
recorded and readable by anyone with audit access. ADR-0087 states this as a cost rather than
mitigating it: redacting a literal means parsing the statement, which it refuses to do.

**Add-on instances are per environment, and every add-on operation names one.**
`burrow addon install postgres --env staging` stands up a second instance (`burrow-postgres-staging`)
beside `prod`'s `burrow-postgres`, with its own `Cluster`, its own volume and its own superuser
credential; attach,
detach, backup and restore all act on the named environment's instance
([ADR-0067](adr/0067-one-database-instance-per-environment.md) §1). Databases keep their simple
names, so `web` in staging and `web` in production are two databases on two servers — the isolation
is the instance, not a naming convention. An operation that names no environment while more than one
is registered is refused rather than defaulted (ADR-0047 §1), and the provisioning seam takes the
environment non-optionally: there is no value meaning "whichever instance is there".

An install predating this keeps the names it had: the default environment (`prod`) resolves to
`burrow-postgres`, the same instance name, the same Secret, so nothing is renamed. The unqualified instance
name belongs to whichever environment is the default, so renaming that environment from `default`
to `prod` (§2) renamed no instance. Sharing one instance across environments is not supported and
cannot be expressed (ADR-0067 §5); a user who wants one server runs one environment.

**One instance per environment is the default, not the maximum.**
`burrow addon install postgres --name analytics` stands a SECOND instance up beside the
environment's own, for a service that wants its own blast radius, its own resource ceiling or its
own upgrade schedule ([ADR-0091](adr/0091-an-environment-may-hold-more-than-one-postgres-instance.md)
§1). Every add-on verb that acts on an instance takes `--name` to select one, and naming none means
what it has always meant — the environment's own instance — so an operator who never types the flag
cannot tell the ceiling was lifted.

An instance's name in the cluster is **looked up in the registry, never derived** (§2). Each
environment's first instance keeps the name it has (`burrow-postgres`, `burrow-postgres-staging`)
and is labelled with that same name, so nothing on a live install moves and a guardrail disposition
somebody already wrote (`prod.burrow-postgres.addon.remove`) keeps matching. Every later instance
gets a generated `burrow-postgres-<id>` the operator never types; `burrow addon list` shows the
label beside it, which is where somebody holding a generated name off `kubectl get` finds out what
it is.

**An app may hold more than one attachment**, one per instance, and a second one has to name its own
variable: `DATABASE_URL` belongs to the first, and Burrow refuses naming the conflict rather than
inventing `DATABASE_URL_2` — a variable the application was never told to read (§3). The backup
claim, `addon config` and `addon sql` all follow the instance rather than the environment (§4), so
two instances in one environment both holding a database called `web` never meet. An instance still
belongs to exactly one environment (§6).

**Creating an instance stays a person's job.** `addon install --name` is absent from
`burrow-agent`: it provisions a pod and a volume nobody approved, which is ADR-0065's line.
Attaching to an instance that already exists adds a database and a role rather than a server, which
is ADR-0065's third tier, so `addon attach --name` is on the agent surface (§5) and held for
confirmation by `addon.attach` rather than withheld.

Postgres metrics are always on and report connection and transaction health plus
`pg_stat_statements` slow-query stats. Under CloudNativePG they come from the operator's own
instance manager on port 9187 rather than from a sidecar Burrow adds; the `Cluster` carries the same
scrape annotations, and the metrics scraper discovers the add-on namespace, so installing the two in
either order works ([ADR-0051](adr/0051-postgres-always-exports-metrics.md)).

Limits: only Postgres supports `attach`/`detach` — cache, logs, and metrics are wired by
reading the endpoint from `addon list` and setting it as app config. Log queries take **no
time range** (the Loki adapter hardcodes the last hour). Metrics queries are **instant only**;
a range query exists in the engine but no CLI, agent verb, or API route reaches it. Add-on
readiness is judged from the store Deployment alone, so a broken Fluent Bit DaemonSet or
vmagent still reports ready. The store itself is judged on **answering** rather than starting: every
Deployment-backed add-on carries a readiness probe on the port its endpoint names — an HTTP `/health`
for the logs and metrics stores, a TCP connect for the cache, which speaks no HTTP — so the endpoint
an install hands back is one that accepts connections, and `addon list` does not report a store
`installed` before it can be reached. What a probe cannot prove is that the store is useful: a
metrics store that answers `/health` and has nothing writing to it is ready and empty. Postgres
readiness is the `Cluster`'s ready-instance count, which is "a server is serving" and not "the
operator agrees the instance is healthy".

---

## Backups

Logical `pg_dump` / `pg_restore` backups for the Postgres add-on
([ADR-0032](adr/0032-postgres-backups.md)).

| Capability | Command | What it does |
| --- | --- | --- |
| Back up an app's database | `burrow addon backup postgres <app> [--destination <provider>]` | Runs `pg_dump -Fc` in a one-shot Job (`postgres:17-alpine`), writing `/backups/<app>/<id>.dump` on **that environment's** backup PVC. With an object-storage provider registered, the same Job then writes that dump to the store and **reads it back** before the backup is recorded as completed. Waits up to 10 minutes; records id, environment, claim, path, destination, object key, size, and status in the control-plane database. |
| Back up a whole instance | `burrow addon backup-instance postgres [--destination <provider>]` | **Physical.** Creates a `postgresql.cnpg.io/v1 Backup` with method `plugin`; CloudNativePG hands it to pgBackRest, which writes a base backup into that environment's repository in the object store. Burrow runs no backup tool. The row says completed only once the backup's manifest is read back from the store. Requires an object-storage provider and an instance wired to it; there is no in-cluster tier. Covers **every** database on the instance and can only be restored as the whole instance, with `burrow addon restore-instance`. |
| List backups | `burrow addon backups postgres [<app>] [--all-environments]` | Reads the control-plane database, newest first, with a `WHERE` column saying which backups left the cluster. Scoped to the environment the invocation is targeting — `--env` names another, `--all-environments` (`-A`) spans them all. |
| Backup health | `burrow addon backup-health postgres [<app>] [--all-environments]` | Reports what Burrow itself observed: how long ago the last backup completed, how long ago the last one **left the cluster**, the last failure with its closed reason, how many rows are still pending, and whether each registered object-storage destination answers **right now**. Read-only, and computed from Burrow's own `Backup` rows rather than from any backup engine's status fields. Scoped like the listing above. |
| Restore | `burrow addon restore postgres <app> --backup <id> --confirm` | Runs `pg_restore --clean --if-exists` into the app's database, overwriting live contents. The backup must belong to the app **and** to the environment being restored into, and its dump must be on that environment's claim. Confirm-gated by the `addon.restore` guardrail. **Operator CLI only** — absent from `burrow-agent`. |
| Restore a whole instance | `burrow addon restore-instance postgres (--backup <id> \| --to-time <RFC3339> \| --latest) --confirm` | **Physical.** Takes a base backup of the instance's current state first and stops with nothing changed if it does not reach the store (`--skip-safety-backup` overrides, for an instance too broken to back up); removes the instance and its data volume; creates a `Cluster` with a `bootstrap.recovery` from the repository under the same name, so every consumer resolves it unchanged; waits for it to serve; **asks the recovered instance which app databases it actually holds, and fails the restore if it holds none**; then reissues each attached app's `DATABASE_URL` and restarts it, because a recovered instance holds the login roles as they were at the recovery point. Exactly one recovery target is required — nothing is assumed. Confirm-gated by `addon.restore_instance`, and on a terminal the **instance's** name is typed back after a notice listing every app by name; off a terminal it refuses without `--acknowledge-data-loss`. **Operator CLI only** — absent from `burrow-agent`. **Never exercised against a real object store.** |

The limits are as important as the capability:

- **With no object-storage provider registered, the dump never leaves the cluster.** It lands on
  a 10Gi ReadWriteOnce PVC, on the default StorageClass, in the same `burrow-addons` namespace as
  the database it came from — so it shares a failure domain with its source. That is recorded on the row (`destination: cluster`) rather than left to be inferred,
  and the listing shows it, because a set of in-cluster dumps should not be able to read as a
  backup strategy. Registering a destination (`burrow config provider add s3`) is what fixes it.
- **Each environment has its own backup claim** ([ADR-0067](adr/0067-one-database-instance-per-environment.md)
  §1), named for the instance the dumps came from: `burrow-postgres-backups` for `prod`,
  `burrow-postgres-<env>.backups` for every other environment. The backup and restore Jobs of an
  environment mount that claim and no other, so one environment's dumps are neither restorable into
  another nor readable from it. The claim survives `addon remove`, including `--delete-data`
  ([ADR-0064](adr/0064-addon-removal-keeps-its-data.md) §4), and `addon list` reports each retained
  claim with the environment it served.
- **The PVC and the store are a tier, not alternatives.** `pg_dump` always writes to the volume,
  and `pg_restore` reads from it: [ADR-0066](adr/0066-postgres-on-cloudnativepg.md) §4 keeps the
  single-app logical restore deliberately, and it is the thing that reads the volume. The object
  store is what takes the backup out of the database's failure domain.
- **A completed backup means the bytes reached the destination, and nothing weaker.** The write
  is signed with the dump's own SHA-256, so the endpoint validates the bytes it stored and
  refuses a truncated transfer; the object is then **read back** and its length compared before
  anything is recorded. A write that does not complete is retried (four attempts, exponential
  backoff) because a transient network failure is the common case and a retried-and-succeeded
  backup is not an incident — but a destination that ANSWERS and refuses is not retried, since a
  revoked credential does not become a valid one by being asked again. Every path that is not a
  verified object records `failed` with a reason from a closed set (`StoreUnreachable`,
  `StoreRejected`, `ObjectNotReadable`, `DumpFailed`, `NotRecorded`) or an
  [ADR-0074](adr/0074-burrow-observes-what-it-manages.md) §2 `IssueReason` when the Job never started. **No
  row ever says `completed` for bytes that did not arrive.**
- **The destination credential reaches the pod only through a Job-owned Secret**, mounted
  read-only, never as a Job env var or a command-line argument, and it is deleted on every path
  out of the backup — success or failure. The vendor's own error text goes to the Job's pod log,
  never into the `Backup` row: a vendor error body is the one place an access key id is known to
  be echoed back.
- **Physical recovery is built and unproven.** `burrow addon restore-instance postgres` stands a
  replacement `Cluster` up from the repository, waits for it, and cuts `DATABASE_URL` over — and
  **no restore from a real object-storage destination has ever been run.** Everything asserted about
  it is asserted against fakes. The recovery `Cluster` matches CloudNativePG 1.30.0's published CRD
  schema and the pgBackRest plugin's own restore example at the pinned release, which is a statement
  about field names rather than about data coming back. The recovery path also **destroys the
  instance's current data volume** — CloudNativePG reattaches a `Cluster` to a claim left lying under
  its name instead of recovering, so the old volume cannot be kept and the name reused — which is why
  the safety backup is taken first and why skipping it is an explicit flag. The recovery path that
  has actually been watched work is still the per-app `pg_dump` / `pg_restore` pair, which recovers
  one app's database to the moment of its dump.
- **A restore proves the databases came back, not the rows in them.** Before it reconnects
  anything, a physical restore asks the recovered instance which Burrow-provisioned app databases it
  holds. An instance that holds **none**, where apps were attached to it, fails the restore
  outright — that is what recovering from a repository with no base backup for the stanza looks like
  from the outside, and without the check it reports success over an empty database. No app is cut
  over on that path, deliberately: re-provisioning would create each app's database fresh and empty
  under its own name and the apps would start writing into it. What the check **cannot** say is
  whether the data inside those databases is the data expected — Burrow does not know what an app's
  rows should look like — so a restore to a point inside the window still needs someone to look at
  their own data. When the instance will not answer at all, the result says `NOT VERIFIED` rather
  than passing quietly; when a recovery legitimately predates an app's attachment, that app is named
  as one whose data did not come back and the restore stands.
- **The cutover finds an app through its workload or its release history, not through its Secret.**
  A physical restore reissues `DATABASE_URL` to every app it can enumerate in the environment: the
  workloads running there, plus the apps the registry says Burrow has deployed there. An app that
  was attached and **never deployed** appears in neither, so it is absent from the confirmation's
  list, is not cut over, and keeps a connection string the recovered instance no longer honours
  until someone runs `burrow addon attach postgres <app>` again. Closing that needs a way to list
  the Secrets in a namespace, which does not exist.
- **Backup age is REPORTED, and nothing alerts on it.** `burrow addon backup-health postgres`
  answers [ADR-0063](adr/0063-object-storage-provider.md) §7's question on demand — destination
  reachability, the age of the last successful backup, the age of the last one that left the
  cluster, the last failure — from Burrow's own rows, which is what
  [ADR-0066](adr/0066-postgres-on-cloudnativepg.md) §5 requires, since a backup engine's own status
  fields can report stale values rather than absent ones. What does not exist is a **threshold**:
  nothing schedules a backup yet, so any "no successful backup in N hours" would be a number Burrow
  invented and then alerted on. The surface reports the ages; the alert waits for the scheduling
  that gives a threshold a meaning.
- **Logical dumps are never scheduled.** No CronJob exists anywhere in the tree and the control
  plane is not granted `cronjobs` RBAC; every `burrow addon backup postgres <app>` is an explicit
  command. Physical base backups ARE on a schedule, and it is not Burrow's: a `ScheduledBackup`
  custom resource that CloudNativePG fires daily (ADR-0066 §2).
- **There is no retention or pruning.** No delete-backup command, no "keep last N", no
  expiry — dumps and their database rows accumulate until the PVC fills.
- **Point-in-time recovery is physical only.** A logical dump recovers one app's database to the
  moment of that dump and nothing finer; the write-ahead-log window belongs to the pgBackRest
  repository, and `burrow addon restore-instance postgres --to-time` is the only thing that reads
  it — for the whole instance, never for one app.
- **The backup PVC outlives everything, deliberately.** Removing the Postgres add-on keeps it,
  and so does `burrow addon remove postgres --delete-data`: dumps outliving the database they
  came from is the point of taking them, and it is what makes destroying the data survivable.
  Their records stay listed, and the removal output names the volume so the storage is not a
  surprise. Reclaiming it is a manual `kubectl delete pvc`.
- **`--delete-data` takes a final backup first where one can be taken.** With an object-storage
  provider registered, every attached database is dumped to that store before anything is
  destroyed, and a backup that does not reach it aborts the removal with nothing deleted
  ([ADR-0064](adr/0064-addon-removal-keeps-its-data.md) §5). With none registered the behaviour is
  unchanged and the only copy that survives is whatever the retained backup PVC already held —
  which the output says. `--skip-final-backup` is the override for an instance too broken to dump,
  and it announces itself. This is **not a backup regime**: a dump taken at teardown is the
  least-exercised path in the product, running against an instance that may already be unhealthy.
- A failed backup or restore Job is left in place for diagnosis rather than reaped.

Scheduled backups with retention are decided as a follow-on in ADR-0032 and are **not built**.
[ADR-0066](adr/0066-postgres-on-cloudnativepg.md) decides they arrive by a different route
entirely — CloudNativePG doing the archiving, scheduling, retention and point-in-time recovery,
with Burrow creating a custom resource instead of orchestrating `pg_dump` — and it is **not
built** either: every command in the table above is the ADR-0032 Job path.

---

## Targets — which Burrow you talk to

A **target** is where the control plane is ([ADR-0078](adr/0078-the-cli-points-at-a-target.md)).
There are two kinds: **Burrow Cloud**, the managed product, or a **Kubernetes cluster** you hold a
kubeconfig context for. It sits one level above an environment: the target says which control plane,
an environment handle says which environment inside it.

| Command | What it does |
| --- | --- |
| `burrow auth login` | Asks where you use Burrow. `burrow-cloud.dev` is the first entry and the default; `Other` lists the contexts already in your kubeconfig so you pick a cluster by a name you recognise. `--cloud` / `--context <name>` select without a prompt. For a cluster it then asks that Burrow for a credential of your own, using your kubeconfig once to prove you are an operator of it, and stores it under `~/.burrow/credentials/`; `--name` records who you are on that install's audit trail. It issues `burrow-agent` a credential of its own at the same time, under `~/.burrow/agents/`, so revoking the agent does not sign you out. A cluster that is unreachable, has no Burrow, runs a control plane too old for it, or already has an admin leaves the target recorded and your commands on the install's shared token, and says which. |
| `burrow auth login --invite <invitation>` | Exchanges an invitation an admin issued for a credential of your own, created on your machine by that exchange and never sent anywhere. Needs `--context <cluster>` to say which cluster, and no cluster admin: the invitation is your identity and the kubeconfig is the route. A refused exchange fails the command and records nothing, because there is no shared token for an invited person to fall back on. |
| `burrow auth invite <name>` | Records somebody on this Burrow and prints an INVITATION for them, which expires after a day, can be exchanged once, and Burrow refuses for anything else. `--admin` lets them invite people in turn. Inviting somebody already recorded issues another invitation and does not change their admin bit. Admin only. |
| `burrow auth status` | Lists the configured targets, marks the active one, says what each is, and flags a target whose kube context is no longer in your kubeconfig. Local only; contacts no cluster. |
| `burrow auth switch <name>` | Makes an already-configured target active, without re-authenticating. |

**Every command that changes something says where it changed it — once.** An app command names it
before it works; a cluster-wide one names it on the line that says what it did:

```
$ burrow app deploy web --image ghcr.io/acme/web:1.4.0
targeting prod-cluster
deployed web as release rel-7f2 (image ghcr.io/acme/web:1.4.0, 2 replica(s), deployed)

$ burrow guard set app.deploy allow
guardrail app.deploy set to allow on prod-cluster
```

The name shown is the one you chose in the picker — the same string `burrow auth status` prints — or,
with no target selected, the environment handle registered for the cluster it reached. Only when
Burrow has no name of its own for where a command went does it fall back to naming the kube context.
A per-invocation `--context` override names what you overrode it *to*. `--json` carries the same
thing as a `target` member of the result, and `burrow-agent`'s outcome envelope carries it too, so an
agent relaying a result can say where it happened. Read-only commands deliberately do not print it:
this is about irreversible acts, and a target stamped on every listing is noise.

Which of the two places it appears follows from which is the only one available. The app commands
resolve an environment and announce it on stderr ahead of the work; the cluster-wide ones — `guard`,
`cluster config`, add-ons, credentials, `audit`, `failures` — announce nothing, so the result line is
where they say it. Saying it in both places is the thing that is never right: two answers to one
question, in two vocabularies, leaves you working out whether they disagree.

Targets live in `~/.burrow/config` (or `$BURROW_CONFIG`), alongside the environment handles, under
`targets:` and `currentTarget:`. A Kubernetes target records the **context name and never a copy of
your credential**, so rotating the kubeconfig, re-issuing a certificate, or letting a provider CLI
manage it all keep working with nothing here going stale. The name itself can still go stale — a
context you rename is a context nothing here points at any more — so a target or a pinned handle
naming a context your kubeconfig does not have is refused, by name, rather than quietly resolved
somewhere else. `burrow auth status` marks such a target and `burrow env list` marks such a handle,
so it is visible before a deploy rather than after one.

**Authenticating is not installing.** `burrow auth login` applies no manifests and changes nothing on
a cluster, so the second person to use a cluster brings their own kubeconfig context and installs
nothing. It does talk to the Burrow already running there, to ask for a credential of your own, which
is authentication rather than installation. Installing still names a context explicitly
(`burrow cluster install <context>`).

**Installing registers a target, and does not select one.** A cluster you install Burrow into is a
target by definition, so `burrow cluster install` and `burrow join` record one for the context they
acted on — which is what makes `burrow auth status` show your own install and `burrow auth switch`
able to reach it, whether or not you have ever run `burrow auth login`. Whichever target is active
stays active: installing says a cluster runs Burrow, not that your next command should go there, and
the install says out loud when commands are still going somewhere else. An environment handle
registered before any of this carries its cluster as a target too — an environment is an environment
*under* a target, so there is no state where you have one and not the other.

With a Kubernetes target selected, it decides the cluster your commands act on; a pinned environment
handle still narrows it when the handle belongs to that same cluster, and a pin left over from a
different cluster is ignored rather than silently redirecting you. **With no target selected nothing
changes** — the CLI follows your kube context exactly as it did before. A target naming a context the
kubeconfig no longer holds is an error that says so and names the way out, rather than a confusing
failure at connect time. "Which cluster a command acts on" below has the full order of precedence
and the one deliberate exception.

`burrow auth login` also **offers** to restrict a detected coding agent to `burrow-agent`, defaulting
to yes on an interactive run. It asks rather than assumes, because it writes to a third-party tool's
configuration file; declining leaves a working setup, and `burrow agent claude install` does it later.
Detection is by the tool's own config directory (`~/.claude/`), not by a name on `$PATH`. A
non-interactive run prints the pointer and asks nothing.

**Signing in to `burrow-cloud.dev`** is an RFC 8628 device authorization with PKCE, and it is in this
open-source CLI: one binary, two targets. Selecting the managed product prints a short code, opens
your browser on the approval page with the code already filled in, and waits. **Check the code in the
browser matches the one in your terminal before approving** — that comparison is the point of showing
it. With no browser to open, the URL is printed for you to visit yourself.

Approving issues **two** credentials, yours and `burrow-agent`'s, each revocable on its own from the
console's credential list. Both are written to files readable only by you and **neither token is ever
displayed**; `burrow auth login` names both paths so you can inspect or delete them:

| | |
| --- | --- |
| Yours | `~/.burrow/credentials/burrow-cloud.dev.json` |
| `burrow-agent`'s | `~/.burrow/agents/burrow-cloud.dev.json` |

They do not expire and there is no refresh: these credentials are **revoked**, not renewed. Nothing
about this runs for a Kubernetes target — choosing `Other` needs no account and makes no request to
the managed product.

The managed product is **named** `burrow-cloud.dev` and **addressed** at
`console.burrow-cloud.dev`. The name is what the target and the two credential files above are keyed
to; the console is where the control plane answers, so it is the host the sign-in, every application
command, and the credential list at `https://console.burrow-cloud.dev/settings` go to. The apex
serves the website.

With a Burrow Cloud target active, the **application commands act through it**: `burrow app list`,
`deploy`, `status`, `logs`, `config`, `secret`, `scale`, `rollback` and their `burrow-agent` siblings
call the managed control plane over HTTPS, authenticated by the credential sign-in stored. There is
no cluster and no kubeconfig anywhere in that path, which is the product. `burrow` presents your
credential and `burrow-agent` presents its own, never each other's — that separation is what makes
revoking one of them mean something. If a credential is refused, the message says it was likely
revoked, names the credential's id so you can find the right row in the console, and points at
`burrow auth login`; no token is ever printed. The kubeconfig-shaped flags (`--kubeconfig`,
`--context`, `--namespace`) name a cluster, so against the managed product they are refused by name
rather than quietly ignored.

The **reads the managed control plane answers act through it too**: `burrow env list`, `burrow guard
list`, `burrow cluster config list`, `burrow audit`, `burrow failures`, `burrow addon list`,
`burrow addon logs`, `burrow addon metrics` and `burrow addon backup-health` call the same routes over HTTPS. Each of them once
refused, not because a tenant lacked the thing being asked for but because the command shared a
connection path with a write that needed a kubeconfig
([cloud #202](https://github.com/burrow-cloud/cloud/issues/202)). `env list` reads the registered
environments rather than local handles there, and marks the default. `addon list` reads the tenant's
own registry — the backing services the platform runs for that tenant — and omits the
retained-volume section, which describes claims in a cluster the tenant does not have.

`addon logs` and `addon metrics` are POSTs, because a query does not fit in a path. The verb is the
transport, not the test: neither changes anything, and on the managed product both are scoped to the
tenant's own namespaces by the backend the platform registered for them.

**`burrow addon attach postgres <app>` acts through it as well**, and it is the one command that
*changes* something and does. Giving an app a database is what the managed product does for a tenant:
the route is `POST /v1/addons/attach`, it is what every attach on the platform goes through, and the
result is the `DATABASE_URL` the app reads. It refused all the same, which left a tenant with a
documented command that could not do the documented thing — and left their `burrow-agent`, the surface
[ADR-0065](adr/0065-what-belongs-on-the-agent-surface.md) keeps deliberately narrower than the
human's, able to attach a database the human's own CLI would not
([cloud #215](https://github.com/burrow-cloud/cloud/issues/215)).

**`burrow addon sql postgres <app>` acts through it as well**, and it is the second command that
changes something and does. The database holds the tenant's own data, and their `burrow-agent` could
already run a statement against it while their own `burrow` refused — the same inversion attach had,
over the rows themselves. What it was also waiting on was somewhere to run the statement, since the
connection is opened *from* burrowd; the managed product replaced that seam with one that carries the
statement to the fleet and runs it beside the database (cloud ADR-0039). It is a *change* rather than
a read because Burrow does not tell the two apart and does not try
([ADR-0087](adr/0087-running-sql-against-an-attached-database.md) §6).

A change needs more than a route that answers, which is why there are two of them. The refusals below are
a statement about what the product offers rather than a control that enforces it: the managed control
plane allows every guardrail on a person's credential, so anything refused here would be served to the
same person's bearer token by anything that spoke HTTP
([cloud #208](https://github.com/burrow-cloud/cloud/issues/208)). What moves a command across is the
product having decided a tenant may.

Holding an attach is a separate question from reaching it, and it has a separate answer:
`addon.attach` is a guardrail at `confirm`
([ADR-0095](adr/0095-attaching-a-database-is-held-for-a-human.md)), evaluated in the control plane,
where it binds every caller rather than the one that happens to be this binary. Which disposition a
*managed* tenant meets is the platform's to set, in its own policy, the same way every other code is.

The **rest of the cluster and policy surface refuses** while the managed product is selected, naming
the target and pointing at `burrow auth switch <name>`. Three distinct reasons, all still true:

- **It acts on a cluster with your kubeconfig.** `config registry ...` writes a pull Secret;
  `env add` creates a namespace and burrowd's Role in it before it registers anything.
- **It changes the tenant, and whether a tenant may is a product decision.** `guard set`,
  `cluster config set`, `addon ...` (install, connect, remove, detach, backup, restore),
  `config provider add`, `app domain add` / `remove`. Each is a question about what a managed tenant
  may do to an instance the platform operates and pays for. `attach` and `sql` are not on this list
  any more: for both, the question had been answered everywhere except the client.
- **It describes the operator's cluster rather than the tenant.** `cluster` and `cluster capacity`
  report nodes, headroom, and the top consumers across every tenant. `config provider list` sits
  here too: the managed product registers no third-party provider, so the listing would only invite
  a registration that is not offered.

`addon backups` refuses for a reason of its own, and it is neither of the first two: its route
answers and what it returns is the tenant's, but on the managed product the backups are the
*platform's*, taken of an instance the tenant does not operate and recorded nowhere in the tenant's
registry. It would print an empty table and point at `burrow addon backup` to fill it — a verb the
managed API no longer offers. What a tenant is told about the backups of their database is a product
statement, tracked as [cloud #302](https://github.com/burrow-cloud/cloud/issues/302).

`addon backup-health` shared that reason and no longer refuses. The reason showed that the *answer*
was missing, not that the question was wrong — whether your data is safe is a fair question from
whoever owns the data — and a verdict about coverage is the shape the platform's own backups get
reported in, where a listing of the tenant's records has nothing to list. Until #302 lands it
reports the engine's own empty answer.

`env use` / `follow` / `rename` / `remove` never refuse — they read and write local handles only.

Three things are deliberately **not** refused *here*. The cluster-lifecycle commands — `cluster
install`, `cluster upgrade`, `cluster bootstrap`, `join`, and the `cluster ingress` /
`cluster registry` / `cluster postgres` / `cluster metrics` provisioners — stay kubeconfig-only,
because installing *into* Burrow Cloud is not a thing that can be asked for
([ADR-0078](adr/0078-the-cli-points-at-a-target.md) §3), so installing a cluster while the managed
product is selected stays legal. They answer to a different rule instead: they refuse unless the
cluster is one you named (cloud ADR-0038 §1, [below](#which-cluster-a-command-acts-on)). `burrow auth`
is how you see and change the active target, so refusing there would strand you. And
`--control-plane` names a control plane outright, so no target is consulted for it.

Only Claude Code has built-in agent wiring, so the detection table's other rows (`~/.codex/`,
`~/.cursor/`, `~/.codeium/windsurf/`) are recorded but not actionable. And §4 of the ADR — every
mutating command naming the target it changed — is **not** built for every command; per-app commands
and the privileged mutating ones print the resolved target today
([#414](https://github.com/burrow-cloud/burrow/issues/414)).

### Which cluster a command acts on

With a **cluster** target selected, it decides the cluster for every command that reaches one — the
per-app commands, and `guard`, `cluster config`, `addon ...`, `env add`, `audit`, `failures`,
`app domain ...`, `config provider ...` and `config registry ...` alike
([ADR-0084](adr/0084-everyone-who-uses-burrow-carries-their-own-token.md) §4). Nothing on that path
reads `kubectl`'s current context any more, so `kubectx` and `burrow auth switch` no longer have to
be kept in agreement by hand.

The order of precedence, highest first:

| | What decides the cluster |
| --- | --- |
| `--control-plane <url>` | That URL, outright. No target is consulted. |
| `--context <name>` | That kube context, for this one invocation. An explicit choice keeps winning, and the command says so when the active target names a different cluster. |
| The active target | Its kube context. |
| Nothing selected | The kubeconfig's current context, exactly as before targets existed. |

A target naming a context your kubeconfig no longer holds is an error that says so and names the way
out, rather than quietly falling back to the current context.

The **cluster-lifecycle commands follow a stricter rule**: they act on a cluster you named, or they
refuse (cloud ADR-0038 §1). `cluster install`, `cluster upgrade`, `cluster bootstrap`, `join`, and the
`cluster ingress` / `cluster registry` / `cluster postgres` / `cluster metrics` provisioners still
reach a cluster through a kubeconfig and never through the managed product — installing *into* it is
not a thing that can be asked for ([ADR-0078](adr/0078-the-cli-points-at-a-target.md) §3) — but none
of them will infer which cluster from `kubectl`'s current context. Installing while the managed
product is selected stays possible; doing it without saying which cluster does not.

How each one is given its cluster depends on what it does:

| | How it is given a cluster |
| --- | --- |
| `burrow cluster install <context>` | The context you name as an argument. With none, it lists your contexts and installs nothing. |
| `burrow cluster bootstrap` | The single-node cluster it just created, through the k3s kubeconfig it wrote. |
| `burrow join` | The kubeconfig it is recording admin access into, carried in the token. |
| `burrow cluster upgrade`, `cluster ingress install`, `cluster registry` (status/install/uninstall), `cluster postgres install`, `cluster metrics install` | `--context`, else the active target when it is a **cluster** target. With neither, the command refuses, naming the cluster it would have acted on and the flag that would name it. `--context` wins whatever the active target is, and says so when the target names a different cluster. |

A `--dry-run` that only renders (`cluster install`, `cluster postgres install`,
`cluster metrics install`, `cluster ingress install`) contacts no cluster and so needs no context.
`cluster upgrade --dry-run` does need one: it reads the running install's secrets to render.

**This breaks scripts** that relied on `kubectl config use-context` deciding for them, and the repair
is to name a context. That is the intended cost: the alternative was a privileged operation on a
cluster nobody chose, announced by a warning nothing reads.

---

## Environments

Two distinct things share the name, and conflating them is the usual source of confusion
([ADR-0035](adr/0035-environments.md), [ADR-0036](adr/0036-environment-selection.md)).

- A **local handle** in `~/.burrow/config` — a named pointer to a kube context, a control-plane
  namespace, an app namespace, a burrowd environment name, and the scoped agent kubeconfig.
  Client-side state; no cluster contact.
- A **registered environment** in burrowd — a row mapping a name to the Kubernetes namespace
  its apps deploy into. Install creates one, named **`prod`**, mapped to the app namespace
  (`burrow-apps`) — not to `burrow-apps-prod`
  ([ADR-0067](adr/0067-one-database-instance-per-environment.md) §2–§3). `prod` and the retired
  `default` are both reserved names for `burrow env add`.

| Command | What it does |
| --- | --- |
| `burrow env` / `env list` | Lists local handles, marks the active one, and marks any whose kube context is no longer in your kubeconfig. `--discover` probes every kube context for an installed burrowd and registers a handle for each. With a Burrow Cloud target active there are no local handles: the listing reads the **registered environments** from the control plane instead and marks the default. `--discover` has no kubeconfig to walk there and is refused. |
| `burrow env add <name>` | Creates the namespace and burrowd's Role/RoleBinding in it, registers the environment with burrowd, and records a local handle. Namespace defaults to `<app-namespace>-<name>`. |
| `burrow env use <name>` / `env follow` | Pins the active environment, or clears the pin so it follows the current kube context. `env use --context <context>` re-points the handle first, which is what a renamed kube context needs. |
| `burrow env rename <old> <new>` | Renames a local handle. |
| `burrow env remove <name>` | Deletes the **local handle only** — and the minted agent kubeconfig under `~/.burrow/agents/`. It does not unregister the environment in burrowd. |
| `burrow-agent environments` | Lists what the agent can see. Read-only, local. |

A pinned handle whose kube context has been **renamed away** keeps working when the handle carries a
scoped agent credential: that credential holds the API server, the CA and the token, so nothing on
the operate path reads the recorded name. The command says once that the name is stale, names the
environment rather than the context that no longer exists, and carries on. A handle with no scoped
credential has only that name to reach the cluster with, so it is refused instead — and `env list`
and `auth status` mark the stale entry either way.

Most operate verbs take `--env <name>`. Mutating operations resolve it through a forcing
function ([ADR-0047](adr/0047-agent-environment-safety.md)): when more than one environment is
registered and no environment is named, the operation is **refused** with a structured error
listing the alternatives, rather than falling back to the ambient default. Read-only
operations are exempt by design. An unreachable target names the other environments but never
switches.

The name is a guardrail decision rather than naming taste.
[ADR-0065](adr/0065-what-belongs-on-the-agent-surface.md) makes `app.delete` and `dns.delete`
deny-by-default and expects the operator to relax them per environment; an environment called
`default` invites `guard set --env default app.delete allow` as the obvious way to stop the
friction, and production has then been relaxed without the word ever being typed. A consequence
worth stating: someone whose only cluster is genuinely a sandbox gets an environment called `prod`
with production-grade defaults and will find them strict. Install says so in its output. The default
environment's guardrail policy **is** the global policy, so `guard set --env prod app.delete deny`
and `guard set app.delete deny` are the same write; an environment added later diverges from that
baseline with its own `--env` row.

An install predating ADR-0067 gains `prod` on its first start under a version that carries it,
pointing at the namespace its apps are already in. Its stored environment name moves from `default` to
`prod` in one migration; **nothing in the cluster moves or is renamed** — apps stay in `burrow-apps`
and the Postgres instance stays `burrow-postgres`, with the same volume and superuser credential.

Limits: `env remove` and `env add` are asymmetric, as noted above — unregistering an
environment in burrowd is not reachable from either CLI. ADR-0047 also specifies the same
forcing function on the *local handle* axis; that half is **not built** (it was specified for
the MCP layer, since removed). Per-environment guardrails are real but partial — see below.

The Postgres collision that used to come with a second environment is closed: stateful add-ons are
per-environment, so two environments' apps of the same name have separate databases on separate
instances ([ADR-0067](adr/0067-one-database-instance-per-environment.md) §1, under
[Add-ons](#add-ons)).

---

## Guardrails

Guardrails are policy evaluated in the control plane, between the agent and the cluster
([ADR-0006](adr/0006-guardrails-in-the-control-plane.md),
[ADR-0020](adr/0020-guardrails-as-configurable-policy.md)). Three dispositions:

- **`allow`** — proceeds.
- **`confirm`** — the operation does **not** run; it returns a structured "needs confirmation"
  result (HTTP 422) until the same call is repeated with `--confirm`. `burrow-agent` surfaces
  this as `held_for_confirmation` with exit code 2 and never self-confirms.
- **`deny`** — refused; `--confirm` cannot bypass it. An unset or invalid disposition also
  reads as deny.

These are all eighteen, in listing order, with their defaults. **Env-scopable** says whether a
guardrail can be set for one environment; **`--name`** says what one name of it refers to — the one
app or the one add-on instance its effect stops at, or nothing where the effect is wider than either:

| Code | Gates | Default | Env-scopable | `--name` |
| --- | --- | --- | --- | --- |
| `app.deploy` | deploying a new release | `allow` | yes | app |
| `app.scale_to_zero` | scaling an app to zero | `confirm` | yes | app |
| `app.expose_public` | exposing an app at a public hostname | `confirm` | yes | app |
| `dns.write` | creating or updating a public DNS record | `confirm` | no | — |
| `dns.delete` | deleting a public DNS record | `deny` | no | — |
| `addon.install` | installing an add-on | `confirm` | no | add-on instance |
| `addon.remove` | removing an add-on | `confirm` | no | add-on instance |
| `addon.attach` | giving an app its own database on an add-on instance and wiring it in (re-attaching rotates its password) | `confirm` | **yes** | add-on instance |
| `addon.detach` | detaching an app from an add-on, ending its access to its data (the database is kept) | `confirm` | no | add-on instance |
| `addon.restore` | restoring a database over its live contents | `confirm` | no | add-on instance |
| `addon.restore_instance` | rewinding a whole Postgres instance, taking every app's database on it back together | `confirm` | no | add-on instance |
| `addon.sql` | running a statement against one app's database on a relational add-on | `deny` | **yes** | add-on instance |
| `app.delete` | deleting an app entirely | `deny` | yes | app |
| `app.rollback` | rolling back to the previous release | `allow` | yes | app |
| `app.autoscale` | configuring autoscaling | `allow` | yes | app |
| `app.run` | running a one-off command in the app's image | `confirm` | yes | app |
| `app.config` | setting or removing an app's non-secret config vars, which rolls the app | `confirm` | yes | app |
| `bucket.create` | creating a bucket at an object-storage provider | `confirm` | no | — |

The three with no `--name` act on things outside the cluster that no app or add-on owns. Where a
guardrail names two things — `addon detach <addon> <app>` reaches one app's database on one instance —
`--name` means the **instance**, because that is where the data lives and where the effect stops
([ADR-0085](adr/0085-a-guardrail-can-name-the-app-it-guards.md) §3). Setting a guardrail for
something it does not name is refused with a sentence saying how far it does reach.

`addon.sql` and `addon.attach` are the two `addon.` codes that are **env-scopable**, and in both cases
that is a declaration rather than an oversight in the others. An add-on operation names an
environment, but its instance label already carries it, so the `--name` tier already draws the
per-environment line for most of them. What these two want is a gradient — a statement about the
**environment** rather than about one server. `addon.sql` wants `allow` in development and `deny` in
production ([ADR-0087](adr/0087-running-sql-against-an-attached-database.md) §5); `addon.attach` wants
`allow` in a sandbox where an agent building three services should not stop three times, and
`confirm` in production ([ADR-0095](adr/0095-attaching-a-database-is-held-for-a-human.md) §2). The
general rule also weakened when an environment gained the ability to hold more than one instance
([ADR-0091](adr/0091-an-environment-may-hold-more-than-one-postgres-instance.md)): per-instance and
per-environment stopped being the same statement.

`burrow guard list [--env <name>] [--name <thing>]` shows the effective disposition and, for
anything narrower than the whole cluster, which tier it came from: set for the named app or add-on
instance, set for the environment, or inherited from the global policy or the built-in default.
`burrow guard set [--env <name>] [--name <thing>] [--binds <kind>] <code> <allow\|confirm\|deny>`
persists an override in the control-plane database.

`burrow-agent guard [--env <name>] [--name <thing>]` can **read** the same three tiers and cannot
set any of them — structurally, the verb does not exist on that binary. It takes `--name` so an
agent can ask about the thing it is about to act on rather than about the cluster: without it a
guardrail denying one app would read as allowed, which is the opposite of what an agent needs to see
([ADR-0065](adr/0065-what-belongs-on-the-agent-surface.md) §7). Its `--json` answer carries a
`source` on each entry naming the tier that supplied the disposition, and a `scope` object naming
what the answer is about, so an agent can tell a person which of the three rules to move.

Three tiers resolve, most specific first ([ADR-0085](adr/0085-a-guardrail-can-name-the-app-it-guards.md) §2):

```
burrow guard set app.deploy confirm                          # every app, everywhere
burrow guard set --env staging app.deploy allow              # every app in staging
burrow guard set --env prod --name website app.deploy deny   # one app
```

`--name` requires `--env`, on the read as much as on the set: on its own a name cannot be told apart
from an environment of the same name, since both are DNS labels, so it is refused rather than
guessed at. Reading is where guessing would cost most — an answer for every app in the `website`
environment, returned for the `website` app, is the wider policy wearing the narrower one's label.

A fourth axis says **which kind of caller a disposition binds**
([ADR-0094](adr/0094-a-guardrail-can-bind-the-agent-and-leave-the-human-alone.md)). Without it a
`deny` refuses whoever asked, the operator included, which is why operators reach for `confirm` in
places that warrant the hard gate:

```
burrow guard set --binds agent --env prod --name burrowd-cloud app.deploy deny   # agents only
burrow guard set --binds user --env dev app.delete allow                         # people only, in dev
```

`--binds` takes `user`, `agent` or `machine` — the credential kinds recorded at issuance
([ADR-0084](adr/0084-everyone-who-uses-burrow-carries-their-own-token.md) §3), read from the stored
credential row and never from the request. The disposition stays one word; what gains a dimension is
the key it is stored under.

The **target narrows before the caller**. Within each tier the kind-bound disposition answers for
that kind and the tier's unbound disposition answers for everybody else, so a cluster-wide
`--binds agent` never overrides something set deliberately for one app, and a key that does not bind
the asking caller's kind is not an answer at all — resolution continues to the next tier, and the
built-in `deny` at the bottom binds every kind. The axis relaxes as readily as it tightens, which is
the second example above.

`--binds` needs an install people have signed in to. The shared install token carries no kind, so a
bound disposition would bind nobody; the set is refused rather than stored, naming `burrow auth
login` as the fix. An install that never signs anybody in is unaffected by any of this: a disposition
set without `--binds` binds every caller, exactly as every disposition always has.

`guard list` resolves for the kind of the **caller asking**, so an agent reading the policy sees what
binds the agent rather than what binds the human — a listing that answered otherwise would have the
agent plan an operation the policy has already refused. A listing containing any bound disposition
grows a `BINDS` column, and `--json` carries `binds` on the entries that have one. A refusal from a
bound disposition names the kind and says an operator can run it with their own credential, which is
a next step that leaves the guardrail in place.

Both `guard` surfaces report a **second kind of limit** alongside the dispositions: the
capabilities absent from the `burrow-agent` binary, each with what it is, why it is held back,
and the operator command that performs it
([ADR-0065](adr/0065-what-belongs-on-the-agent-surface.md) §7). The two groups are separate keys
in `--json` — `guardrails` and `absent_capabilities` — because they are different answers: a
`deny` is a limit an operator can move with `guard set`, an absent capability is not on the
binary at all. See [The agent surface](#the-agent-surface).

On the operator CLI the **human** listing prints dispositions only, and points at
`burrow agent capabilities` for the other list. The two are different kinds of answer, not two
halves of one setting: `guard list` shows policy an operator chose and `guard set` can change,
while `burrow agent capabilities` shows the shape of another binary, which no disposition moves.
`burrow agent capabilities` reads a compiled-in catalogue, so it answers with no cluster and no
credential, and `--json` carries the same `absent_capabilities` list under the same key.

Limits:

- **`app.delete` and `dns.delete` are denied, and a deny is not a hold** — no `--confirm`
  opens one, on either CLI ([ADR-0065](adr/0065-what-belongs-on-the-agent-surface.md) §3).
  Deleting an app destroys its release history and deleting a DNS record takes an application
  off the internet, so neither rests on a prompt someone might not read. **That default is a
  floor, not a fixed setting.** The expected shape is a per-environment gradient — `burrow guard
  set --env dev app.delete allow`, `--env staging app.delete confirm`, production left denied —
  and the refusal message says so, because relaxing one guardrail globally to unblock a sandbox
  relaxes production with it. `burrow app delete` is how a human deletes an app once the
  guardrail permits it.
- **Which guardrails can be scoped to an environment is a property each one declares**
  ([ADR-0068](adr/0068-operational-limits-are-configuration.md) §5), not one read off the `app.`
  prefix as it used to be. The app-level codes are scopable because the operations they gate always
  name an environment, along with `addon.sql` and `addon.attach`. The rest of the cluster-level codes
  (`dns.*`, the other `addon.*`, `bucket.create`) are not, because their dispositions are looked up
  with no environment at all; setting one with `--env` is rejected. `dns.delete`'s deny is therefore still all-or-nothing: an operator who wants the agent
  managing DNS in development but not production must pick one answer for both. Widening it is now
  a change to that declaration plus the lookup at its call site rather than a rename.
- **`app.replica_ceiling` is no longer a guardrail.** It is an operational limit — see
  [Operational limits](#operational-limits) below. Exceeding it is a validation failure, not a
  disposition, and any stored disposition for it was dropped by the schema migration.
- **Several mutating operations are not guardrailed at all**:
  `app health set/unset`, `app secret set/unset`, `app unpublish`, `addon backup`, `addon connect`,
  `config provider add`, `app auto-deploy`, `env add/remove`, and `guard set` itself. Some are
  deliberate (a secret value is absent from the agent binary entirely); `unpublish` taking an app
  offline without a gate is worth knowing. `app config set/unset` was on this list until
  [ADR-0098](adr/0098-a-config-write-is-guarded.md): "config vars are non-secret" described the
  value and left out that writing one re-applies the running workload, so it rolls the app.
  `addon attach` was on this list until
  [ADR-0095](adr/0095-attaching-a-database-is-held-for-a-human.md): "attach provisions rather than
  destroys" was true and left out that it provisions on a server every other app shares, restarts the
  app, and rotates a password on a re-attach.
- Removing an environment does not cascade to its guardrail overrides; they are orphaned and
  would apply again if the name were reused.

---

## Locks

A lock is **state on a thing** — one app, or one add-on instance — and it is deliberately not a
guardrail (cloud ADR-0060). A guardrail asks *who is calling* and answers accordingly; a lock asks
nothing about the caller, and refuses the person who set it.

```sh
burrow lock website            # and: burrow lock addon burrow-postgres
burrow unlock website          # and: burrow unlock addon burrow-postgres
```

**Locked, these refuse:**

| Operation | Refused because |
| --- | --- |
| `burrow app delete <app>` | the workload, routing and release history do not come back |
| `burrow addon remove <instance>` | the instance is what holds the data |
| `burrow addon detach <addon> <app> --delete-data` | the database and every row in it are destroyed |

**Locked, everything else proceeds normally**: deploys, rollbacks, scaling, restarts, attaching,
config changes, and an ordinary `addon detach`, which keeps the database so a re-attach gets it
back. Those all undo by doing them again, and a lock that interrupted them would be one an operator
turns off and leaves off.

The refusal **names the unlock command** and carries the `locked` code over the API (HTTP 422). It
is not `needs_confirmation`: `--confirm` cannot satisfy it, and neither can calling as somebody
else. Deleting a locked app therefore takes two commands — an unlock, then a confirmed delete — and
neither makes the other redundant.

A few properties worth stating plainly:

- **It is not a security control**, and nothing here should be read as one. Anyone with write
  access to the namespace deletes the same objects with `kubectl` and never touches Burrow. What a
  lock buys is that the path *through Burrow* takes a separate command whose only purpose is to
  permit destruction.
- **Neither verb is on the agent surface.** `burrow-agent` can neither lock nor unlock; a
  mechanism whose value is that a person performs a second deliberate act is worth nothing if one
  caller performs both. The agent *can* see a lock — it rides on `status`, `burrow app list`, and
  `burrow addon list` — and `burrow-agent guard` reports both verbs as absent, so an agent that
  meets a locked refusal can say what stands in the way and who removes it.
- **`unlock` takes no `--confirm`.** The unlock *is* the deliberate act; a confirmation on top
  would make the pair ceremony.
- **Both acts are audited**, as `lock` and `unlock`. The unlock is the line worth reading: an
  unlock with no deletion after it is a lock somebody removed and forgot to restore.
- **It is per environment.** `prod`'s lock on `website` says nothing about `staging`'s, which is
  the point — acting on production while meaning staging is the mistake the mechanism exists for.

---

## Operational limits

A limit is a **bound a human sets**, and it is deliberately not a guardrail
([ADR-0068](adr/0068-operational-limits-are-configuration.md)). A guardrail answers *what happens
when this is attempted*; a limit answers *where the line is*. Exceeding one is a **validation
failure**: there is no disposition on it, nothing to relax, and no `--confirm` that opens it. The
refusal names the limit, the tier the effective bound came from, and the command that raises it.

| Limit | Bounds | Default | Tier |
| --- | --- | --- | --- |
| `app.replica_ceiling` | the largest replica count a deploy, a scale, or an autoscaler's maximum may ask for | `50` (1 – 2147483647) | environment or cluster |
| `build.job_retention` | how long a finished in-cluster build Job and its pods are kept before Kubernetes reaps them | `72h0m0s` (1m – 8760h) | cluster |
| `addon.metric_retention` | how long the metrics add-on keeps samples, applied when its instance is created | `744h0m0s` (24h – 87600h) | cluster |
| `status.unschedulable_grace` | how long a pod must have been unschedulable before Burrow reports it as an Issue | `30s` (0s – 1h) | cluster |
| `status.image_pull_grace` | how long a pull failure must persist before the failure ledger opens a row for it | `15s` (0s – 10m) | cluster |
| `deploy.settle_timeout` | how long a deploy waits for its rollout to settle before telling a `post-deploy` hook what it observed | `5m0s` (10s – 30m) | environment or cluster |
| `addon.sql_timeout` | how long one `addon sql` statement may run before the database aborts it | `30s` (1s – 5m) | environment or cluster |
| `addon.sql_rows` | the largest number of rows one `addon sql` statement returns before the result is reported as truncated | `500` (1 – 10000) | environment or cluster |

Two operator-owned tiers, resolved in the order guardrail dispositions already use — the
environment's value, then the cluster's, then the built-in default:

- `burrow cluster config list [--env <name>]` shows every limit with its effective value, the tier
  that value came from, and the built-in default it reverts to.
- `burrow cluster config set <limit> <value>` sets the **cluster** value, which applies everywhere.
- `burrow cluster config set --env <name> <limit> <value>` sets that **environment's** value, which
  wins there. `--env prod` writes the cluster value: `prod` is the environment install created and
  the baseline the others diverge from ([ADR-0067](adr/0067-one-database-instance-per-environment.md)
  §2), exactly as for `guard set`.

**The whole surface is absent from `burrow-agent`** (ADR-0068 §4, ADR-0065 tier 1), asserted by the
surface-guard test, for the reason `guard set` is: a bound the agent can raise is not a bound. The
agent gets the refusal and the operator command to relay, not the lever. Reading a limit is harmless
and useful, but ADR-0068 leaves the shape of an agent-side read undecided, so there is none yet.

Limits:

- **Three of the seven are cluster-only**, and deliberately: how long a build's logs are worth
  keeping, how long metric samples are kept, and how long a pod may sit unschedulable are all facts
  about the cluster. An operator who tuned the last of them per environment would be encoding the
  same scheduler fact twice and would eventually disagree with themselves. The replica ceiling and
  the settle timeout go the other way: a production ceiling and a development ceiling are the case
  that motivates limits at all, and a production rollout of twenty replicas takes longer to settle
  than a development one of one. The two `addon.sql` bounds go the same way: a development database
  is disposable and an agent exploring it is the whole value, while a statement holding locks in
  production is an outage.
- **`deploy.settle_timeout` costs nothing unless the deploy has something to report on.** It is the
  bound on the only wait Burrow performs on the deploy path, and that wait happens when there is a
  `post-deploy` hook to tell the result to or a derived dependency to check once the rollout is
  ready — so an app with neither is unaffected, and an app with an attached database is not. The wait
  also ends early on any blocking condition, so the bound is reached only by a rollout wedged for a
  reason no pod reports. Both clients size their own request bound from it, so raising it does not
  produce a caller that gives up while burrowd is still working.
- **`status.unschedulable_grace` is ONE value, everywhere.** The live status surface, the Job
  waiters behind `app run` / build / backup / restore, and the failure ledger
  ([ADR-0074](adr/0074-burrow-observes-what-it-manages.md)) all judge the same pod against the same
  threshold, so a change to it moves all of them together. Two surfaces disagreeing here would not
  be a tuning difference — they would hold different definitions of "failure". It is spent **once**
  on each path: the live surface withholds a scheduling refusal younger than the grace, and the
  ledger's watch reports the refusal immediately and holds the row for the same grace before opening
  it ([ADR-0079](adr/0079-the-observer-watches-and-latches.md) §3).
- **The `status.` limits are the ledger's dwells.** `status.image_pull_grace` is the second one, and
  unlike the unschedulable grace it is spent only by the ledger: `burrow app status` reports a pull
  failure the moment the cluster does, because a live read answers about this second, while the
  ledger is the record that must not fill with registry hiccups that resolved themselves. The
  reasons that are **already the outcome of waiting** — `OOMKilled`, `CrashLoopBackOff`,
  `ProgressDeadlineExceeded`, `DeadlineExceeded` — have no dwell at all and are recorded the instant
  they are reported.
- **Two of them apply at creation, not retroactively.** `build.job_retention` is written onto each
  build Job as `ttlSecondsAfterFinished`, and `addon.metric_retention` onto the metrics add-on's
  container args, so a change reaches the next build and the next install of the add-on. A running
  metrics instance keeps the retention it was created with until it is removed and reinstalled.
  `status.unschedulable_grace` has no such lag: it is read on every inspection.
- **A limit reads live.** burrowd reads the values on each operation rather than at startup, so a
  `cluster config set` takes effect on the next deploy, build, or status call with no restart. If
  the database cannot be read, every limit falls back to its built-in default and burrowd logs that
  it did — an unavailable database must not become a failed deploy.
- **There is no `unset`.** A value returns to the default by being set back to it explicitly, the
  same as for a guardrail disposition.
- **Values are validated on the way in**, against the limit's kind and permitted range, and stored
  in canonical form. A value that later stops parsing (a hand-edited row) is skipped on read and the
  next tier applies, so a bad row cannot wedge a deploy.
- Removing an environment does not cascade to its limit values, exactly as it does not cascade to
  its guardrail overrides.

---

## Audit log

An append-only record in the control-plane Postgres ([ADR-0027](adr/0027-audit-log.md)). A
guarded operation writes two rows: the decision (`allowed`, `held`, or `denied`) and then the
execution (`executed` or `failed`).

Each row carries the timestamp, operation, target, an allow-listed `args` map, the guardrail
code and disposition, the outcome, a result string, the caller, the principal, and the
client version.

**The principal is who acted and the caller is what kind of credential they held**
([ADR-0084](adr/0084-everyone-who-uses-burrow-carries-their-own-token.md) §9). Once somebody has
signed in, their rows name them (`ada`) and say whether the action came from their own terminal
(`user`), the agent on their machine (`agent`), or an automation (`machine`) — so the same person's
own deploy and their agent's deploy are one principal and two callers, and the difference is
readable. The kind is read from the stored credential row and never from anything the request
claimed about itself. The principal is recorded as the **name**, and it stays true afterwards: a
principal is retired by being marked revoked, never deleted, and the name is unique across the
install, so a row keeps naming exactly one person after they are gone.

A request that presents the install's **shared token** names nobody, and the row says so:
`shared-agent` / `control-plane`, the same pair every row carried before per-person credentials
existed. Nothing infers an actor from an install having only one — a shared token is exactly the
credential anybody may be holding. The same pair is recorded for an internal reconcile, which has no
request behind it at all.

Operations recorded: `deploy`, `scale`, `autoscale`, `rollback`, `app_delete`,
`expose`, `dns_write`, `dns_delete`, `addon_install`, `addon_remove`, `addon_attach`,
`addon_detach`, `addon_backup`, `addon_restore`, `run`.

Read it with `burrow audit` or `burrow-agent audit`, filtered by `--app`, `--operation`,
`--outcome`, and `--limit` (default 200, newest first).

Limits: **`args` never contains a secret value** — where environment data is involved the row
records sorted key *names* only. There is **no time-range filter and no environment filter**;
the environment appears inside `args` but is not queryable. There is **no retention or
pruning**, so the table grows without bound. There is no update or delete path by design.
Writes are best-effort: a failed append is logged and swallowed rather than failing the
operation it describes. `guard set`, `addon connect`, `unpublish`, and provider registration are
**not** audited. A `publish` writes the rows of the links it composes — an `expose` row for the
plain-HTTP routing, a `dns_write` row for the record, and a second `expose` row when the pre-flight
passes and the certificate is attached — rather than one row of its own.

---

## Failure ledger

A record in the control-plane Postgres of what the cluster **did** after Burrow acted on it
([ADR-0074](adr/0074-burrow-observes-what-it-manages.md)). It is **not** the audit log and does
not share its table: the audit log records what Burrow was *asked* to do and what it *decided*,
is append-only, and is never pruned; this records the outcome, mostly unrequested by anyone, and
it *is* pruned.

burrowd sweeps the objects the registry says it owns and writes one row per **(object, reason)**
— an app, add-on, backup, or exposure, and a reason from a closed vocabulary. Each row carries a
`first_seen` ("when did it start"), a `last_seen` ("is it still happening"), a `resolved_at`
(null while active), an occurrence count, and one bounded, Burrow-authored detail line. Two
reasons on one object are two rows with independent lifetimes.

Read it with `burrow failures` or `burrow-agent failures`, filtered by `--kind`, `--name`,
`--env`, `--reason`, `--since <duration>`, `--all`, and `--limit` (default 500, oldest first).
By default it lists what is still broken; `--since` widens to a window of history and `--all` to
the whole retained history.

**Grouped, on the human CLI only.** `burrow failures` groups rows by shared reason and orders
them oldest first, because one cause routinely produces many rows and the earliest one in a
cascade is usually the thing worth fixing. That grouping is presentation: the JSON form and the
API return **rows, not groups**, so an agent correlates on its own terms.

**It never claims a cause.** Rows sharing a reason and a window are placed beside each other as
a hint about where to look. Burrow does not assert that one caused another, and two unrelated
failures in the same minute will sometimes be shown together.

**Coverage travels with every answer.** `coverage` reports the observation windows behind the
result and the literal `gaps` between them — stretches in which nothing was observing. An empty
list over a gap is not evidence that nothing broke, and both CLIs say so rather than printing a
clean list. Sweeps that ran but could not read every object are reported as degraded coverage.

**The observer watches, and latches a transition before recording it**
([ADR-0079](adr/0079-the-observer-watches-and-latches.md)). A failure is recorded when the cluster
reports it, not at the next sample — but only once it has **persisted for a dwell**, and its row is
closed only once the condition has been **clear for one**. The dwell is per reason: a scheduling
failure waits out `status.unschedulable_grace`, a pull failure `status.image_pull_grace`, and a
reason that is already the outcome of waiting (`OOMKilled`, `CrashLoopBackOff`, an elapsed deadline)
waits for nothing. So an app that goes unready for ten seconds during a rolling update leaves no
rows, and one that has been unschedulable for a minute leaves one that says when it started. A
**dropped watch is a gap**, treated exactly as a burrowd restart is: coverage ends where the watch
lost its place and resumes when the re-list completes.

Limits: retention prunes **resolved** rows and elapsed windows after 30 days (an active failure
is never pruned). The ledger deliberately **lags the cluster by the dwell** — it is the record of
what happened, and `burrow app status` is the place to look for what is happening this second. A
flap that never exceeds its dwell is **invisible** here, which is correct for the ledger's purpose
and worth knowing. burrowd **observes and never remediates**. **No row carries a secret value** —
the detail is one Burrow-authored line, and a crash loop's application output stays with the live
status surface and `app logs`.

---

## The agent surface

`burrow-agent` is a separate binary from `burrow`: capability-reduced, credential-free,
JSON-first, and invoked directly by the agent
([ADR-0049](adr/0049-burrow-agent-scoped-cli-control-channel.md)). Its surface is closed and
pinned by tests that fail if a verb is added or removed.

**Read-only:** `apps`, `status`, `history`, `next-tag`, `logs`, `config`, `secret` (keys),
`reachability`, `secret mounts`, `cluster`, `cluster capacity`, `addons`, `backups`, `logs-query`,
`metrics-query`, `guard`, `audit`, `failures`, `health`, `checks`, `providers`, `environments`.

**Mutating** (each returns an outcome envelope — `executed` / `held_for_confirmation` /
`denied` / `error`, exit codes 0/2/3/1): `deploy`, `build`, `rollback`, `scale`, `autoscale`,
`run`, `publish` (alias `expose`), `unpublish` (alias `unexpose`), `domain add`, `domain remove`, `addon install`, `addon attach`,
`addon backup`, `config set`, `config unset`, `health set`, `health unset`, `secret unset`,
`secret mount`, `secret unmount`, `delete`.

**Absent from the agent binary entirely** — these are operator actions on the `burrow` CLI:
`cluster install`, `cluster upgrade`, `cluster bootstrap`, `cluster ingress install`,
`cluster registry install`, `cluster postgres install`, `cluster config set`, `join`, `env add`,
`guard set`, `app secret set`, `app auto-deploy`, `app hook set`, `app checks enable`/`app checks disable`,
`lock`, `unlock`,
`addon remove`, `addon remove --delete-data`,
`addon connect`, `addon detach`, `addon detach --delete-data`,
`addon restore`, `addon restore-instance`, `addon config`,
`config provider add`, `config registry login`, `agent <tool> install`,
`app publish`/`unpublish` under those names, and the client-side `--build` deploy path.
**Bucket deletion** is in that list too and is the one entry Burrow does not carry on *either*
CLI ([ADR-0063](adr/0063-object-storage-provider.md) §5): it destroys every backup the platform
holds, and a bucket name lives in a global namespace, so a mistaken argument can reach outside
the cluster entirely. `guard` reports it with the vendor's own tool as the answer to "who can".

Those verbs are not prose alone. They are entries in a capability catalogue
(`internal/agentsurface`), which is also the table the surface-guard test reads as its closed
allow-list: every capability appears once, tagged with the surface that carries it, so the two
readers cannot disagree about where a verb sits. That is what lets `guard` report each absent
capability with what it is and who can run it, below.

What qualifies a verb for this surface is
[ADR-0065](adr/0065-what-belongs-on-the-agent-surface.md): a capability belongs unless its effect
reaches beyond the app the agent was asked about (**scope** — disqualifying outright, so the verb
is not compiled in) or a human cannot restore the prior state afterwards (**reversibility** — the
operator decides, so it ships as a guardrail denied by default). `addon remove` is the scope case:
add-ons are one instance per type per environment and every app in an environment has its database
on that environment's `postgres`, so it removes *the* add-on for an environment and takes every
attached app in it with it. The surface-guard test
asserts its absence rather than leaving it to be a property of the current command tree.

The reversibility tier ships too: `app.delete`, `dns.delete` and `addon.sql` are `deny` by default,
so `burrow-agent delete`, `burrow-agent domain remove` and `burrow-agent addon sql` exist, are
refused, and say what would change the answer (see [Guardrails](#guardrails)).

**An absent capability is legible rather than a dead end** (ADR-0065 §7). `burrow agent capabilities`
lists them on the operator CLI, and `burrow guard list --json`
and `burrow-agent guard` both report them alongside the dispositions: what each one
is, why it is not on the agent surface, who can perform it, and the command that person runs. So an
agent asked to remove an add-on relays "that is not something I can do, and here is who can" instead
of `unknown command`. That matters beyond politeness: ADR-0065 §5 notes a dead end is what invites an
agent to route around the control channel entirely and reach for `kubectl` or a shell, which is
the failure [ADR-0021](adr/0021-guardrails-require-control-plane-only-agent-access.md) says Burrow
cannot close from the inside.

**The list is the same on every target; who performs each one is not.** `burrow-agent` is one binary
and withholds the same capabilities for the same reasons whether it is pointed at a self-hosted
cluster or at Burrow Cloud, so the catalogue, and every reason in it, reads identically on both. The
answer to "who can" moves, because on the managed product the reader is a tenant and roughly half the
operator commands are refused to one. Each entry that needs it therefore carries a second remedy:
either a command a tenant can run — `burrow cluster config list` and `burrow guard list` show the
limits and dispositions in force, `burrow auth login` is how a machine gets access — or, where the
platform performs the capability itself, a plain statement of that and no command at all. On a
self-hosted install the listing is unchanged: every row names the operator command it always did.

`burrow-agent` derives the report by subtracting the command paths it actually registers from the
catalogue, so a verb taken out of the binary becomes legible in `guard` with no second edit. The
report enumerates the agent surface to anything that can read it, which ADR-0065 §7 accepts
outright: the CLI is open source and `--help` already reveals it, so the read carries no access
control.

The narrowing is structural, not advisory: the binary lacks the verb, its
kubeconfig lacks the permission, and the control plane gates the operation anyway.
`burrow agent claude install` wires it up for Claude Code — the only tool name with a built-in
recipe — by merging permission rules into `~/.claude/settings.json` and an orientation block
into `~/.claude/CLAUDE.md`, idempotently, backing up any file it edits. Any other tool prints
the rules to apply by hand.

For the scoped credential minted at install, what each of the four credentials that reach a
cluster can and cannot do, `BURROW_AGENT_REQUIRE_SCOPED=1` fail-closed behaviour, and the
multi-user join, see **[docs/HARDENING.md](HARDENING.md)** — that document is the source of
truth and this one does not restate it.

---

## Cluster lifecycle

| Capability | Command | What it does | ADR |
| --- | --- | --- | --- |
| Install the control plane | `burrow cluster install <kube-context>` | Provisions the control-plane, app, add-on, and build namespaces; the `burrowd` ServiceAccount and its narrowly scoped namespace Roles; the API-token and database Secrets; an empty `burrow-credentials` Secret; the `burrow-agent` ServiceAccount, its Role (proxy to burrowd, read the API token — nothing else) and long-lived token Secret; one read-only ClusterRole for capability detection; **the control plane's own Postgres**; and the `burrowd` Deployment and Service. Ensures metrics-server as a baseline. By default the database is a **CloudNativePG `Cluster`** (1 instance, 1Gi), so install applies the operator and waits for its controller first; `--database plain` keeps the older single Deployment (`postgres:18`, 1 replica, 1Gi PVC) for a cluster that will not accept cluster-scoped CRDs. | [0012](adr/0012-in-cluster-postgres.md), [0086](adr/0086-burrow-installs-one-kind-of-postgres.md), [0038](adr/0038-scoped-agent-credential.md), [0054](adr/0054-install-is-control-plane-only.md), [0060](adr/0060-cluster-lifecycle-command-group.md) |
| Provision a VPS | `burrow cluster bootstrap` (on the VPS) | Installs k3s (Traefik disabled, servicelb kept, so the node IP is a free LoadBalancer) plus the control plane, then prints a `burrow join <token>`. Refuses under ~1900 MiB RAM without `--yes`. **Burrow never SSHes anywhere** — this is run once, over your own SSH session. | [0044](adr/0044-single-vps-k3s-cluster.md) |
| Join from a laptop | `burrow join <token>` | Merges the admin context into the ambient kubeconfig and writes the scoped agent kubeconfig under `~/.burrow/agents/`. Idempotent. | [0044](adr/0044-single-vps-k3s-cluster.md) |
| Upgrade | `burrow cluster upgrade` | Re-renders the install manifest with a new burrowd image, preserving the API token, database password, and namespaces read back from the cluster. Postgres and its volume are untouched, including which shape it is: an upgrade re-renders the database already running and never moves an install from one to the other. Migrations are applied by burrowd at startup. | [0013](adr/0013-database-migrations-and-upgrade-policy.md) |
| Install the PostgreSQL operator | `burrow cluster postgres install` | Applies the pinned CloudNativePG release (`1.30.0`) from its own upstream artifact, skipping when a controller is already running and re-applying when the CRDs are there without one. It then installs the pinned pgBackRest plugin (`0.0.3`), the component an instance archives its write-ahead log through, independently of the operator's own state and skipped when cert-manager is absent, since the plugin's manifest needs it. Needs **cluster-admin** (it installs cluster-scoped CRDs), which is why it is an operator step and absent from the agent. It is a **prerequisite of `burrow addon install postgres`**, which is refused by name on a cluster without it. It installs the operator only — no database. | [0066](adr/0066-postgres-on-cloudnativepg.md) |
| Install the metrics-server baseline | `burrow cluster metrics install` | Applies the same pinned metrics-server baseline `cluster install` and `cluster bootstrap` ensure, for a cluster they did not: one installed before the baseline existed, or one where `--no-metrics-server` was passed and the operator has since changed their mind. Detected first and skipped when the cluster already **serves** `metrics.k8s.io` — a usable version, not the bare group name, since the aggregation layer keeps the group of a metrics-server that has stopped answering — so a vendor's copy (k3s, GKE, AKS) is never installed over. A Metrics API that is registered and serving nothing is reported and left alone, exiting non-zero: that is a broken metrics-server rather than a missing one, and re-applying the manifest repairs none of its usual causes; `--force` applies the baseline anyway, for the case where the workload is gone and only the `APIService` is left. `--dry-run` prints the manifest without contacting the cluster. Without it nothing reports live CPU/memory usage, so `kubectl top`, HPA autoscaling, and the utilization layer of capacity reporting have nothing to read. | [0054](adr/0054-install-is-control-plane-only.md) |
| Inspect the cluster | `burrow cluster` | Ingress classes, default StorageClass, LoadBalancer support and whether it is free or billable, cert-manager, metrics-server, the CloudNativePG operator (present / running / its release beside the one Burrow targets), which shape the control plane's own database runs in and whether it archives off-cluster, cloud provider, configured DNS providers. | [0034](adr/0034-agent-native-onboarding.md), [0043](adr/0043-public-reachability-is-a-loadbalancer.md) |
| Scheduling headroom | `burrow cluster capacity` | Per-node and cluster-total allocatable, committed and free CPU/memory and pod counts, top consumers, and whether a build would fit. Computed from node allocatable versus summed Pod requests — no metrics-server needed. | [0054](adr/0054-install-is-control-plane-only.md) |
| Version | `burrow version` | The CLI version, the `burrow-agent` on your `PATH`, the installed control plane's image tag, and the latest published release. The control-plane line reads the cluster the active target names; with the managed product selected it reports that the line does not apply there rather than reading whichever cluster your kubeconfig happens to point at, since a managed control plane is the operator's to version and upgrade. `--context` names a cluster for the one invocation and still wins. | [0016](adr/0016-cli-distribution-and-upgrade-lifecycle.md), [0039](adr/0039-cli-control-plane-version-skew.md) |

`install` is idempotent and multi-user safe: it applies server-side with `kubectl` nowhere in
the path, and if Burrow is already installed it does **not** re-mint secrets — it performs a
local join, writing only `~/.burrow`. Additive cluster components are deliberately separate
opt-in commands rather than `--with-*` flags ([ADR-0054](adr/0054-install-is-control-plane-only.md)).

The CLI reaches burrowd through the **Kubernetes API-server service proxy**, authenticated by
your kubeconfig, with Burrow's own token sent only in the `X-Burrow-Token` header
([ADR-0014](adr/0014-self-host-connectivity-via-kubeconfig.md),
[ADR-0015](adr/0015-token-header-only-x-burrow-token.md)) — so no inbound endpoint is needed. A
direct `--control-plane <url> --token <t>` is also supported.

Version skew ([ADR-0039](adr/0039-cli-control-plane-version-skew.md)): every request carries
`X-Burrow-Client-Version`. burrowd serves any client within one minor and never hard-blocks on
a version difference alone; a client more than one minor behind gets HTTP 426 naming the
upgrade command, and an unknown route returns a structured error saying to upgrade.

That check sees **routes**, not request parameters: an older control plane ignores a query
parameter it does not know and answers 200 — and every burrowd released before this one drops an
unrecognised **body field** the same way. So a parameter that **narrows the scope of a write** rides
the route instead. The guardrail
name tier is the case that established the rule —
`burrow guard set --env prod --name web app.deploy deny` is `PUT /v1/guard/name/web/app.deploy`,
so a control plane without the tier refuses the call rather than writing the same deny for every
app in the environment. The refusal names both versions and the upgrade, and nothing is written.

The same shape carries the **read**, on both CLIs: `guard list --name` and `burrow-agent guard
--name` are `GET /v1/guard/name/web`. A widened read is not merely cosmetic — a policy that denies
one app, reported as the environment's, is what an agent would go on to act against — so it is
refused with nothing shown rather than answered one tier out.

**Every write now follows the rule.** Each segment is named after the parameter it replaces, so a
route reads as the request it carries:

| Call | Route | What an older control plane would otherwise have done |
| --- | --- | --- |
| `burrow addon remove <name>` | `DELETE /v1/addons/{name}/data/keep` | **Destroyed the data volume**, and every attached app's database with it, on a request that asked to keep it — see below |
| `burrow addon remove <name> --delete-data` | `DELETE /v1/addons/{name}/data/delete` | Destroyed the volume with no final backup, since it takes none |
| `burrow app delete <app> --env staging` | `DELETE /v1/apps/{app}/env/{env}` | Deleted the **default environment's** app — workload, routing and release history |
| `burrow addon detach postgres <app>` | `POST /v1/addons/detach/data/keep` | **Dropped the app's database**, on a request that asked to keep it — the same inversion as the removal above (ADR-0090) |
| `burrow addon detach postgres <app> --delete-data` | `POST /v1/addons/detach/data/delete` | Detached on whatever terms it had of its own, rather than as the named destructive operation |
| `burrow addon detach postgres <app> --env staging` | `POST /v1/addons/detach/data/keep/env/{env}` | Reached the default environment's database |
| `burrow addon restore postgres <app> --env staging` | `POST /v1/addons/restore/env/{env}` | Overwritten the default environment's live database |
| `burrow app deploy <app> --env staging` | `POST /v1/apps/{app}/deploy/env/{env}` | Replaced what is **running in production** |
| `burrow app rollback <app> --env staging` | `POST /v1/apps/{app}/rollback/env/{env}` | Rolled production back to its previous release |
| `burrow app scale <app> <n> --env staging` | `POST /v1/apps/{app}/scale/env/{env}` | Resized production — and at `0`, stopped it serving |
| `burrow app publish <app> --env staging` | `POST /v1/apps/{app}/publish/env/{env}` | Pointed the hostname at production's workload |
| `burrow app unpublish <app> --env staging` | `POST /v1/apps/{app}/unexpose/env/{env}` | Removed production's ingress and routing |
| `burrow secret set <app> KEY=VALUE --env staging` | `POST /v1/apps/{app}/secrets/env/{env}` | Written the value into **production's Secret**, which cannot be unwritten |
| `burrow secret unset <app> KEY --env staging` | `DELETE /v1/apps/{app}/secrets/{key}/env/{env}` | Taken the value production is running on out from under it |
| `burrow addon install postgres --env staging` | `POST /v1/addons/env/{env}` | Stood the instance up as the default environment's |
| `burrow addon install postgres --archive-destination b2` | `POST /v1/addons/archive-destination/{destination}` | Created the instance with **no write-ahead-log archiving**, and reported it installed |
| `burrow addon backup postgres <app> --env staging` | `POST /v1/addons/backup/env/{env}` | Dumped from the default environment's instance |
| `burrow addon backup postgres <app> --destination b2` | `POST /v1/addons/backup/destination/{destination}` | Left the dump **inside the cluster**, on a disk beside the database it insures, and recorded a completed backup |
| `burrow addon backups postgres --env staging` | `GET /v1/addons/backups/env/{env}` | Listed every environment's backups — see the read-side note below |

A call with two narrowings appends both segments in a fixed order, so `addon install postgres
--env staging --archive-destination b2` is `POST /v1/addons/env/staging/archive-destination/b2`.

The add-on removal is the one that motivated the change and the only one where **both** answers are
routes. `--delete-data` is an opt-in, so saying nothing means *keep my data* — but it was not always
so, and a control plane from before the inversion destroys the volume on every removal. There is no
request a current client can send such a control plane that means "keep", so neither disposition can
be a parameter, and a removal against one is refused with the add-on still standing.

Old clients keep working throughout: every one of these routes is **added** beside the form clients
in the field already send, which keeps its old meaning exactly (ADR-0039 §2). For the removal that
means an unnarrowed `DELETE /v1/addons/{name}` still keeps the data volume unless `delete_data=true`
says otherwise. A client older than the inversion meant "destroy" by saying nothing, and gets the
data kept instead — a narrower outcome than it asked for, named in the response as a retained volume,
which is the only direction the ambiguity can safely be resolved in.

**The reads keep the query parameter, on purpose, with one exception.** `app status`, `app logs`,
`app list`, `app reachability`, `config list` and the secret **key** listing still carry `?env=`. A read
answered one scope out misinforms whoever is reading it and changes nothing — and against a control
plane too old for named environments there is only one environment, so what comes back is the only
data there is rather than another environment's, labelled with a name the server never learned. What
makes that safe is that the read is not the last line of defence: every write it might lead to is in
the table above and is refused on its own. Refusing the reads too would take away the commands an
operator reaches for while working out what is wrong.

The exception is `addon backups`, which is in the table. Its answer is not a display, it is an
**argument**: it is the picker for `addon restore`, and the id chosen from it is handed to a call
that overwrites a live database. A backup id is an opaque string, so nothing downstream can tell that
it came from the wrong environment's list — the list has to be refused rather than answered wide.

**A parameter that cannot outrun its own route needs none of this**, whatever it narrows: it can only
reach a control plane that shipped no earlier than it did. That covers `skip_final_backup` and
`backup_destination` on the removal; `env` and `destination` on `addon backup-instance` and
`addon restore-instance`, whose routes and parameters are all v0.14.0-rc.1; and — the one most worth
naming, because its absence from the table above otherwise looks like an oversight — `env` on
`config set` and `config unset`, whose route arrived in the same v0.7.0 that added named
environments. They stay parameters. Around 45 others are safe for the same reason.

**An unrecognised request field is now refused rather than dropped.** burrowd reads a request body
with unknown fields disallowed, so a field a control plane is too old to know produces the same kind
of structured, version-naming refusal an unknown route does — code `unknown_field`, with nothing
done — instead of a request half-carried-out and reported as a success. That is what catches the
*next* narrowing added as a body field, before anyone has to notice it. It protects from this release
forward and repairs nothing already deployed, which is why the routes above exist as well. An older
client is unaffected: it sends fewer fields, never unknown ones.

**Upgrade limit, worth stating plainly:** the shipped startup gate permits a re-run of the same
version or **exactly one minor step forward**, and refuses skips, downgrades, and cross-major
in-place moves. [ADR-0055](adr/0055-multi-version-upgrades.md), which decides multi-minor
forward jumps in one step, is 🟡 **Proposed and not built**. Upgrading across several minors
means installing each intervening minor in turn.

---

## Logs and observability

Two different surfaces, and picking the wrong one is a common mistake.

- **`burrow app logs <app>` / `burrow-agent logs <app>`** reads live Pod logs through the
  Kubernetes API, concatenated across the app's Pods. The **only** flag is `--tail`. There is
  **no `--follow`, no `--since`, no `--previous`, and no `--container`** — streaming is not
  supported, and the response is fully buffered. Log lines carry a pod name, the UTC instant
  the cluster recorded the line, and the application's own message with that instant stripped
  off. A zero timestamp means no time could be read for that line, not that the field is unused.
- **`burrow addon logs` / `burrow-agent logs-query`** queries the durable logs add-on (or a
  connected Loki) in LogsQL or LogQL. This is the surface for "what happened an hour ago".
  Each record carries the message, the store's timestamp, and the pod that emitted it — from
  `kubernetes_pod_name` on VictoriaLogs, from the `pod` stream label on Loki. A record from a
  collector that writes neither reports no pod rather than a wrong one.

For metrics, `--metrics-port` on a deploy annotates the Pod so the metrics add-on's scraper
finds `/metrics`; queries go through `burrow addon metrics` / `burrow-agent metrics-query`.

---

## What Burrow does not do

Consolidated, so a reader can stop looking:

- **No file-mounted config.** A secret key can be mounted as a file (`secret mount`, above) and is
  the only thing that puts a volume on an app Pod; config vars are environment variables only.
- **No persistent storage for apps.** No PVC, no StatefulSet — the only supported app workload
  kind is Deployment. Add-ons get volumes; apps do not.
- **No resource requests or limits, probes, security context, node selectors, tolerations,
  priority classes, or runtime classes on app Pods**, and no surface to set them. The
  ADR-0061 mutator seam exists for an embedder to compile one in, not for a user to configure.
- **No DaemonSet, StatefulSet, or CronJob app workloads.** Jobs exist only as the one-off
  runner and as internal build/backup machinery.
- **No scheduled anything.** No cron, no scheduled backups, no scheduled restarts.
- **No TCP/UDP exposure, no path-based routing, no multiple hostnames per app, no custom
  ingress annotations, no DNS-01 certificates.**
- **No backup retention, pruning, offsite copy, or point-in-time recovery.**
- **No automatic rollback**, and no readiness gate on deploy.
- **No log streaming or follow.**
- **No secret set from the agent channel**, by design.
- **No blue/green or canary deploys, no traffic splitting, no maintenance windows.**
- **No dashboard.** The CLIs and the control-plane API are the whole interface.
- **No multi-tenancy.** The control plane is single-tenant, which several security decisions
  (notably the privileged build container) rely on.

---

## Decided but not built

Accepted or proposed decisions with no implementation, or with only part of one. An ADR alone is
not a capability, and a partly built one is not two-thirds of a capability — the rows below say what
is built and what is not, and link the issue tracking the rest where there is one.

| Decision | ADR | Status of the code |
| --- | --- | --- |
| Detect the cluster's existing ingress controller and bind to its IngressClass | [0042](adr/0042-use-existing-ingress-controller.md) | Not built — the IngressClass and the HTTP-01 solver class are the literal `"nginx"`. |
| A deploy with a port creates a ClusterIP Service on its own | [0041](adr/0041-flatten-path-to-a-reachable-app.md) | Not built — no `--port` on `deploy`; the Service is created by `publish`. |
| One publish operation chaining Service, Ingress, TLS, DNS, and a cert wait | [0041](adr/0041-flatten-path-to-a-reachable-app.md) | Built — `publish` does the Service, the Ingress, the DNS record where a provider is configured, a DNS-and-plain-HTTP pre-flight, the certificate, and the wait, and reports the converged verdict. It is the same operation on both surfaces (`burrow app publish`, `burrow-agent publish`). The `--wait` on `reachability` and `domain add` remain as the primitives beneath it. |
| Multi-minor forward database upgrades in one step | [0055](adr/0055-multi-version-upgrades.md) | Proposed, not built — the gate allows one minor step. |
| Scheduled backups with retention | [0032](adr/0032-postgres-backups.md) | Not built. |
| Audit-log retention | [0027](adr/0027-audit-log.md) | Not built; deferred in the ADR. |
| The environment forcing function on the local-handle axis | [0047](adr/0047-agent-environment-safety.md) | Not built (specified for the since-removed MCP layer); the burrowd-registry axis is built. |
| Registry onboarding via the developer's code-provider registry | [0046](adr/0046-registry-onboarding.md) | Proposed, held deliberately; only the in-cluster registry shipped, via ADR-0054. |
| Acting on a Burrow Cloud target | [0078](adr/0078-the-cli-points-at-a-target.md) §1 | Mostly built. Signing in is built (`burrow auth login`, cloud ADR-0028's RFC 8628 device flow with PKCE), and the application commands act through a selected cloud target over HTTPS with the stored credential, as do the reads the managed control plane answers (`env list`, `guard list`, `cluster config list`, `audit`, `failures`, `addon list`, `addon logs`, `addon metrics`, `addon backup-health`) and the two changes it has been decided a tenant may make: `addon attach` — the route the platform provisions a tenant's database through ([cloud #215](https://github.com/burrow-cloud/cloud/issues/215)) — and `addon sql`, which the fleet answers (cloud ADR-0039) against the tenant's own data, closing the last case where a tenant's `burrow-agent` could do what their `burrow` refused ([cloud #208](https://github.com/burrow-cloud/cloud/issues/208)). What still refuses while a cloud target is selected, rather than silently using the ambient kubeconfig, is the surface that acts on a cluster with a kubeconfig (`config registry ...`, `env add`), the operations whose availability to a tenant is an open product question (`guard set`, `cluster config set`, `addon install` / `remove` / `connect` / `detach` / `backup` / `restore`, `config provider add`, `app domain ...`), the reads that describe the operator's own cluster (`cluster`, `cluster capacity`, `config provider list`), and the backup listing whose rows belong to the platform rather than the tenant (`addon backups`, [cloud #302](https://github.com/burrow-cloud/cloud/issues/302)); `burrow auth` is exempt, and the cluster-lifecycle commands are exempt from *that* refusal but carry their own — they refuse unless a cluster is named, by `--context` or by a cluster target (cloud ADR-0038 §1). What is not decided is whether that surface should follow a selected CLUSTER target — today it follows the kube context and ignores the target ([#429](https://github.com/burrow-cloud/burrow/issues/429)). |
| An app-runtime API and capability envelopes | [0050](adr/0050-app-runtime-api-and-capability-envelopes.md) | Not built; a captured direction, deferred. |
| Per-app connection pooling, read replicas, major-version upgrades, or TLS to the database | [0031](adr/0031-postgres-addon.md) | Not built; named as "not yet" in the ADR. |
| Object storage as a provider type, so a backup can leave the cluster | [0063](adr/0063-object-storage-provider.md) | Partly built. The destination registration is built: the `s3` provider type and object-storage capability, the credential pair as two keys in `burrow-credentials`, the configuration-time probe write/delete, the recorded globally-unique bucket, lifecycle-versus-retention reconciliation, and `bucket.create` at `confirm` with bucket deletion absent from both CLIs. The backup WRITE path is built too: the dump is shipped to the store and read back before the row says `completed`, retries are for a store that will not answer and never for one that answered and refused, and `burrow addon backup-health postgres` reports destination reachability, the age of the last successful backup, the age of the last one that left the cluster, and the last failure. What is left of §7 is the ALERT: physical backups are now scheduled (ADR-0066 §2), but an instance with no destination and every logical dump still are not, so there is no threshold that would be right for all of them and none is asserted. [#331](https://github.com/burrow-cloud/burrow/issues/331) |
| A final backup before `--delete-data` | [0064](adr/0064-addon-removal-keeps-its-data.md) §5 | Not built — it waits on an object-storage provider ([ADR-0063](adr/0063-object-storage-provider.md)); until then the retained backup claim is the only copy. The rest of ADR-0064 is built: removal keeps the data PVC and names it, `--delete-data` is operator-CLI-only and carries §2's typed confirmation, the backup claim always survives, and `addon list` reports retained volumes (§6). [#334](https://github.com/burrow-cloud/burrow/issues/334) |
| The Postgres add-on runs on CloudNativePG, with the operator owning WAL archiving, schedules, retention, and point-in-time recovery | [0066](adr/0066-postgres-on-cloudnativepg.md) | Built, and unproven where it counts — §1 through §5 exist and no restore from a real object store has ever been run. The OPERATOR is: `burrow cluster postgres install` applies the pinned CloudNativePG release, skips a controller that is already running, and states the cluster-admin requirement; `burrow cluster` reports whether the CRDs are served, whether a controller is actually running, and which release it is beside the one Burrow targets. §1 IS THE MECHANISM, AND IT IS THE ONLY ONE: `burrow addon install postgres` creates one `Cluster` per environment (single replica, `ghcr.io/cloudnative-pg/postgresql:17.10-minimal-trixie`, `burrow_admin` declared as a managed role, and a managed service carrying the instance's own name onto the primary so every existing consumer resolves it unchanged), refuses by name on a cluster with no controller running, and reports readiness from the `Cluster`'s ready-instance count. The ADR-0031 Deployment is gone from the tree, and with it the flag that used to select between them. REMOVAL keeps the data by ADR-0064 §1's default even though CloudNativePG's own default is to take it — the operator stamps the `Cluster` on the claims it composes, so the claims are disowned and labelled as retained volumes before the `Cluster` is deleted, and `--delete-data` deletes them by name rather than trusting a garbage collector that would leave an already-disowned claim behind. It refuses rather than assumes when the `Cluster` cannot be read, removes an instance whose `Cluster` is already gone but whose claim is not, and leaves another environment's instance and backup claim untouched. §2 AND §3 ARE THE BACKUPS: `burrow cluster postgres install` also applies the pinned pgBackRest CNPG-I plugin (Apache-2.0 plugin over an MIT engine, NOT barman, and skipped with a stated reason on a cluster without cert-manager, which its manifest needs), and `burrow cluster` reports it. With an object-storage provider registered, `burrow addon install postgres` writes a credential Secret, a `Stanza` naming that environment's OWN repository path, and a daily `ScheduledBackup`, and adds the plugin to the `Cluster` as its write-ahead-log archiver — so the archive runs continuously and one environment's repository is not addressable from another's. `burrow addon backup-instance postgres` creates a `Backup` object on demand and reads `.status`; a failed object and an archive the store will not accept record DIFFERENT closed reasons, because they are different problems. The row only says completed once the backup's manifest has been read back out of the store at the key the repository says it is at (ADR-0063 §7). RETENTION IS BURROW'S WINDOW: the provider's `--retention-days` is written into the repository as a number of days, so pgBackRest expires against the same window the bucket's lifecycle rules are reconciled against rather than a second one of its own; archive retention is deliberately left unset, since expiring WAL ahead of the backups needing it destroys point-in-time recovery silently. An instance installed with no provider registered archives nowhere and is byte-for-byte the `Cluster` it was; re-running the install after registering one attaches the plugin to it (appended to `spec.plugins`, never replacing entries somebody else put there). An install on a cluster with a destination but no plugin is NOT refused: the database installs and the result states that this instance archives nothing and names the fix, because refusing would take the database away to protect a backup on a cluster where the plugin may not be installable yet. An instance keeps the repository it was created against — an install resolving a different bucket is refused rather than re-pointing the stanza. A physical backup checks the instance's own `Stanza` against the destination it resolved before creating anything, so a backup is never verified against the wrong bucket. §4 IS THE RESTORE, and it is built and unproven in different measures. `burrow addon restore-instance postgres` takes a physical backup of the instance's current state FIRST and abandons the restore with nothing changed if it does not reach the store (ADR-0064 §5's ordering on the one operation that destroys a database on purpose; `--skip-safety-backup` overrides it for an instance too broken to back up), removes the instance and its data claim, creates a `Cluster` with a `bootstrap.recovery` from the repository UNDER THE SAME NAME — so the additional managed Service, the superuser Secret, every `DATABASE_URL`, `addon remove` and `addon backup-instance` all resolve exactly what they did — waits for it to serve, and then re-provisions each attached app through the same seam `addon attach` writes and restarts it, because a recovered instance carries the login roles as they were at the recovery point. The claim cannot be retained instead: CloudNativePG reattaches a `Cluster` to a claim left under its name rather than recovering, so keeping it would silently cancel the restore. Exactly one recovery target is required (`--backup`, `--to-time`, `--latest`) and none is assumed. It is instance-scoped and reported as such: `addon.restore_instance` is its own guardrail at `confirm`, the held message and the terminal notice name EVERY affected app rather than counting them, the instance's name is typed back on a terminal (ADR-0064 §2's shape) and refused off one without `--acknowledge-data-loss`, the audit row records the instance, the target, the safety backup and the affected apps, and the verb is absent from `burrow-agent` (ADR-0065 §2 tier 1). NO RESTORE FROM A REAL OBJECT-STORAGE DESTINATION HAS BEEN RUN — everything above is exercised against fakes, the composed `Cluster` is checked against CloudNativePG 1.30.0's published CRD schema and the plugin's own restore example at the pinned release, and neither is evidence that the data comes back. ADR-0066 §3 requires that real restore before the path is relied on. `burrow addon restore <addon> <app>` keeps its single-app `pg_dump` meaning (§4) and REFUSES a physical backup by name rather than applying an instance-wide rewind to one app; `restore-instance` refuses a logical dump by name in the same way, so the two point at each other. Backup size is not reported for a physical backup either: `Backup.status` carries none, the plugin method produces no `VolumeSnapshot` to join `restoreSize` from, and sizing a pgBackRest backup means summing objects under its repository path. §5 is built: `burrow addon backup-health postgres` reports the backup-age signal from what Burrow itself observed, which is the part of the record that is deliberately independent of the mechanism and stays correct across the swap. §6's migration is NOT built and will not be: no tenant Postgres existed under the old mechanism, so the path deleted rather than converted. [#338](https://github.com/burrow-cloud/burrow/issues/338) |

The rows above are summaries. Per-ADR implementation tracking — the code as it stands, the sections
each issue covers, and an acceptance checklist — lives in the issues labelled
[`adr`](https://github.com/burrow-cloud/burrow/labels/adr); this file stays the answer to "can Burrow
do X today?" rather than a second tracker.

One ADR runs the other way and is worth knowing:
[ADR-0062](adr/0062-remove-the-mcp-server.md) is done — the MCP server is gone from the tree,
with `burrow mcp` surviving for one release as a hidden stub that errors and names
`burrow agent claude install`.

---

## Where to look next

- [README](../README.md) — the authoritative shipped / in-progress / planned version table.
- [docs/ARCHITECTURE.md](ARCHITECTURE.md) — how the layers fit together at runtime.
- [docs/HARDENING.md](HARDENING.md) — the agent trust surface and the scoped credential.
- [docs/QUICKSTART.md](QUICKSTART.md) — a validated end-to-end walkthrough.
- [docs/adr/](adr/README.md) — why each decision is shaped as it is.
