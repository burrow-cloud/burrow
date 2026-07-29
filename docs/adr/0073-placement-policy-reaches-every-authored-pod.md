# ADR-0073: Placement policy reaches every pod the engine authors, split by whose image runs

## Status

✅ Accepted

## TL;DR

[ADR-0061](0061-deploy-pod-mutator-seam.md) gave the operator a hook on the app's Deployment, because
a pod built as a fixed literal cannot schedule on a cluster whose capacity is tainted or whose
workloads must name a sandboxed runtime. The reasoning was never specific to Deployments — it is
about **any pod the engine authors** — but the seam was built for one of them.

Burrow authors six pod paths. Two have a hook. **Four have nothing**: the backup and restore Jobs,
the Postgres add-on, and the metrics collector. On the cluster ADR-0061 was written for, those four
sit `Pending` forever, and the failure is quiet — a backup that never ran looks like a backup that is
still running.

This record makes the rule general: **an authored pod the operator cannot reach is a pod that cannot
be deployed**, so every one of them is reachable.

It does **not** put them all behind one hook. Placement policy keys off *whose image is running* — a
cluster may well want the tenant's own image sandboxed on tenant nodes and Burrow's own images
somewhere else entirely — so there are two hooks: the existing one for pods running the **app's**
image, and a second for pods running **Burrow's**. Collapsing them would force an operator to
tell the two apart by inspecting a pod spec, which is exactly the fragile thing a seam should spare
them.

And it states plainly what the seam is not: **wiring nothing sandboxes nothing.** Burrow does not
enforce isolation; it makes the operator's isolation reachable.

Generalizes [ADR-0061](0061-deploy-pod-mutator-seam.md) and follows the same model as
[ADR-0053](0053-in-cluster-build-from-source.md) §6. Realizes
[ADR-0045](0045-oss-enterprise-boundary.md)'s seam bargain. Supersedes nothing.

## Context

### What exists today

The engine authors six kinds of pod. Their placement fields — `tolerations`, `runtimeClassName`,
`nodeSelector` — are as follows:

| pod | image | operator hook | placement fields set |
| --- | --- | --- | --- |
| app Deployment | the app's | `WithPodMutator` (ADR-0061) | none, until a mutator sets them |
| one-off run Job | the app's | `WithPodMutator` | none, until a mutator sets them |
| build Job | Burrow's builder | `WithBuildPodMutator` (ADR-0053 §6) | none, until a mutator sets them |
| backup **and** restore Jobs | Burrow's | **none** | **none, ever** |
| Postgres add-on Deployment | Postgres | **none** | **none, ever** |
| metrics collector Deployment | vmagent | **none** | **none, ever** |
| log collector DaemonSet | Fluent Bit | **none** | a blanket `Operator: Exists` toleration |

The run Job joined the first group only recently, and only because someone went looking. The bottom
four are not a decision — they are the paths nobody has revisited since ADR-0061.

### What breaks

**On the exact cluster ADR-0061 was written for, four authored pods cannot schedule.** ADR-0061's
Context describes a cluster whose only capacity is tainted; a pod without the matching toleration
stays `Pending` indefinitely. That reasoning applies unchanged to a backup Job. The operator who
followed ADR-0061 and wired a mutator has working deploys and a backup that never runs.

**And each of the four fails quietly, in its own way.** A Pending Job is not a failed Job: `Failed`
and `Succeeded` both stay zero, so a waiter burns its full timeout and reports a timeout rather than
an unschedulable pod. A Pending add-on Deployment reports zero ready replicas, which reads like a
slow start. Nothing in either message names the missing toleration.

**The worst case is the restore.** It shares its builder with the backup path, so an operator can
hold backups that were written before the pool was tainted and discover at restore time — during an
incident — that the restore Job cannot schedule.

**The log collector's blanket toleration is right for the wrong reason.** A DaemonSet collecting node
logs must run on every node, including tainted ones, so `Operator: Exists` is correct. But it is
hard-coded rather than chosen, and it is the only placement field anywhere in the bottom four — it
looks like precedent for hard-coding when it is the one case where the engine legitimately knows the
answer.

### What this record resolves

Which authored pods carry operator placement policy, how many hooks that takes, and what the seam
does not promise.

## Decision

### 1. Every pod the engine authors is reachable by operator placement policy

A pod the operator cannot adjust is a pod that cannot run on a cluster with rules of its own. That is
ADR-0061's argument, and nothing in it was about Deployments — it was about the gap between a fixed
literal and a real cluster's admission and scheduling constraints. Any authored pod has that gap.

So the rule is general, and it is a standing obligation rather than a one-time change: **a new
authored pod path arrives with a hook, or it arrives undeployable.** This is the part worth writing
down, because four paths reached today's tree by nobody asking the question.

### 2. Two hooks, split by whose image runs

The reach divides in the place an operator's policy actually divides:

- **`WithPodMutator`** — pods running the **app's own image**: the Deployment's pod template and the
  one-off run Job. Same workload, same image, same namespace, same environment.
- **`WithPlatformPodMutator`** — pods running **Burrow's own images** on Burrow's behalf: the backup
  and restore Jobs, the Postgres add-on, the metrics collector, the log collector.

`WithBuildPodMutator` (ADR-0053 §6) stays as it is, a third hook for a third case: Burrow's builder
image over the app's source, with its own security context ([ADR-0056](0056-build-security-context-for-the-oss-builder.md))
that no other path shares.

**Two rather than one, because the two sets take genuinely different policy.** A managed operator
wants the tenant's image under a sandboxed runtime on tenant-only nodes, and their own Postgres and
collectors somewhere the tenant's code is not — different `runtimeClassName`, different taint,
frequently a different pool. One hook could only serve that by having the operator inspect the pod
spec and guess which kind it is, keying off a container image or a label. That is a classification
the engine already knows and would be throwing away, and a mutator that guesses wrong puts the
tenant's code on the platform pool.

**Two rather than six.** A hook per path would track the engine's internal decomposition, which is
not a distinction the operator has any policy about. The split is by trust, and there are two kinds
of trust here.

### 3. The log collector keeps its blanket toleration, and the hook runs anyway

`Operator: Exists` on the log-collector DaemonSet stays. A collector that skips tainted nodes loses
exactly those nodes' logs, silently, and the engine genuinely does know that a node-log collector
belongs on every node. This is the one case where a hard-coded placement field is a decision rather
than an omission.

The platform mutator still applies to it. An operator may need to add a `runtimeClassName` or a pull
secret, and there is no reason for one path to be unreachable. A mutator that removes the blanket
toleration gets what it asked for — §5.

### 4. A nil mutator leaves every authored object byte-for-byte unchanged

ADR-0061 §3's obligation, extended to every new site, and a test obligation rather than an intention.
An install that wires nothing sees no difference on any path.

### 5. The seam is not an isolation guarantee, and must not be read as one

Burrow does not sandbox anything. It authors pods and applies the operator's hook; if no hook is
wired, no pod carries a `runtimeClassName` and every workload runs under whatever the cluster's
default runtime is.

This is worth stating explicitly because the failure that prompted this record — a tenant's one-off
command running outside the sandbox every other tenant workload got — is easy to describe as "the
engine now sandboxes runs." It does not. It makes the operator's sandboxing **reach** runs. The
operator who wires nothing has exactly what they had before, which §4 guarantees, and an operator
relying on Burrow for isolation is relying on something that was never offered.

Where isolation is a product requirement, the enforcement belongs to whoever operates the cluster —
admission policy, a default runtime class, a namespace-level control — not to a hook the same binary
can decline to wire.

### 6. The hooks are applied after construction, on every write

Unchanged from ADR-0061 §2, and stated here because it now applies to Jobs and DaemonSets rather than
one Deployment. The mutator runs over the **fully-constructed** pod spec, so it can key its decision
off the containers the engine composed, and it runs on updates as well as creates, so a rollout does
not drop what the operator supplied.

Two obligations follow for the mutator author, and both are new to the platform hook: it must be
**idempotent**, since it runs on every write and appending to a slice will drift; and it must
**tolerate pod specs it did not expect** — a Job pod arrives with `RestartPolicy: Never` already set,
and a DaemonSet pod arrives with a toleration, and overwriting either produces an object the API
server rejects or a collector that stops collecting.

## Consequences

- **Four pod paths become deployable on a constrained cluster**, and the operator supplies the policy
  rather than the engine guessing it.
- **The engine gains a second unused seam.** It wires neither hook itself. That is the
  [ADR-0045](0045-oss-enterprise-boundary.md) bargain — the alternative is a downstream fork of the
  add-on and backup paths — but it is a real cost, and it is now two costs.
- **An operator must wire two hooks to cover their cluster, and wiring one is a silent partial.**
  Someone who wires only the app hook has deployable apps and undeployable backups, which is today's
  situation with an extra step available and not taken. The hook that is not wired fails the same
  quiet way §"What breaks" describes.
- **The platform hook is more dangerous than the app one.** It reaches stateful workloads — the
  Postgres add-on holds tenant data — and a mutator that moves that pod to a pool where its volume
  cannot attach breaks the add-on rather than one deploy. The trust model is unchanged from ADR-0061
  (compiled in by whoever operates the binary), but the blast radius is not.
- **`runtimeClassName` remains unenforced.** §5's honesty has a price: someone will read this record
  as closing the sandbox gap, and it does not. Closing it is the operator's, and on the managed
  product it should be admission policy rather than a hook.
- **The classification is now load-bearing.** Which hook a path gets is decided in this repository,
  and a new path assigned to the wrong one gives the operator policy they did not intend — the app
  hook on a Burrow-image pod would put Burrow's own workload on the tenant pool. It is a small
  decision made once per path, and it should be stated in the doc comment where a reader will meet it.
- **Backup and restore share a builder, so they share a policy.** They cannot be given different
  placement without splitting that builder, which nothing yet requires. Worth knowing before someone
  wants a restore on a larger node than the backup ran on.

## Rejected alternatives

- **One hook over every authored pod.** One concept, and the operator branches inside it. Rejected in
  §2: the branch would key off a container image or a label to recover a classification the engine
  already has, and a wrong branch puts the tenant's image on the platform pool. Throwing away
  information at a seam and asking the caller to reconstruct it is how a seam becomes a footgun.
- **A hook per pod path — six seams.** Maximum precision, and no classification decision for this
  repository to get wrong. Rejected because it exposes the engine's internal decomposition as public
  API, so splitting or merging an internal path becomes a breaking change, and because the operator
  has no per-path policy — the distinction they care about is the tenant boundary.
- **Configuration fields on the engine** — a tolerations list, a runtime class — rather than hooks.
  ADR-0061 rejected this for the deploy path and the reasoning is unchanged: the list has no natural
  end, and each cluster requirement becomes a field, a validation, and a release.
- **A mutating admission webhook covering everything the engine creates.** The one option that would
  also cover pods this engine does *not* author, and would make isolation genuinely enforceable
  rather than merely reachable — §5's gap closed properly. Rejected here for ADR-0061's reasons
  (another deployed component, its own certificate lifecycle, a down-webhook failure mode blocking
  every deploy) and because it is infrastructure imposed on every install to serve the ones that need
  it. It remains the right answer for an operator who needs *enforcement*, and §5 says so — this
  record makes policy reachable, not mandatory, and those are different jobs.
- **Hard-code the placement fields the managed product needs.** Fastest, and it would close the
  observed gap today. Rejected because it imposes one operator's topology on every install, which is
  the thing ADR-0061 was written to avoid, and because the OSS project has its own reason to want
  this — the tainted-pool operator is not hypothetical and is not us.
- **Leave the four paths alone and fix them when someone reports it.** Defensible for the collectors,
  which are optional. Rejected because of the restore path: the report arrives during an incident,
  from someone who cannot restore a database, and "nobody has complained" is not evidence about a
  path that is exercised rarely and urgently.
