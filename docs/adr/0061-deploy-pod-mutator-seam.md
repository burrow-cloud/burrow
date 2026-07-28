# ADR-0061: A pod-mutator seam on the deploy path, mirroring the build one

## Status

✅ Accepted

## TL;DR

When Burrow deploys an app, it builds the Kubernetes object that will run it. That object is fixed —
nothing outside this package can adjust it before it is created.

That is a problem on any cluster with rules of its own. Plenty of real clusters reserve their nodes
with taints — a GPU pool, a spot-instance pool, a pool dedicated to one team — and a pod may only land
there if it carries the matching toleration. Others require every workload to name a sandboxed
runtime, or expect a priority class, a topology constraint, or an image-pull secret. Burrow's app pod
carries none of these and offers no way to add them, so on such a cluster a deploy simply never
schedules.

Burrow already solved exactly this for **builds**: `WithBuildPodMutator` (ADR-0053 §6) lets whoever
embeds the engine adjust the build pod before it is created. This adds the matching adjustment point
for **deploys**.

Realizes the extension model of [ADR-0045](0045-oss-enterprise-boundary.md); mirrors
[ADR-0053](0053-in-cluster-build-from-source.md) §6 on the deploy path. Supersedes nothing.

## Context

[ADR-0045](0045-oss-enterprise-boundary.md) establishes that this repo is a
control-plane **engine** — something other software embeds and configures, not only a binary run
as-is. Extension happens through documented seams rather than by forking.
[ADR-0053](0053-in-cluster-build-from-source.md) §6 put that into practice for the build executor, and
`BuildAdapter.WithBuildPodMutator` is the concrete hook: the build Job's pod spec can be adjusted
after it is constructed and before it is created.

The deploy path has no equivalent. `kube.Adapter` builds the app's Deployment with a `corev1.PodSpec`
literal and creates it; the adapter's only options are `WithNamespace` and `WithAddonNamespace`. Every
Burrow deploy therefore produces the same pod shape, and nothing can influence it.

**That shape is not universally deployable.** A pod is admitted and scheduled subject to the cluster's
own constraints, and several common ones can only be satisfied by fields on the pod itself:

- **Tainted node pools.** Reserving nodes with taints is ordinary practice — a GPU pool, spot
  capacity, a pool held for one team, or nodes running a sandboxed container runtime. A taint is
  satisfied only by a matching **toleration** on the pod. Burrow's app pods carry none, so on a cluster
  whose only schedulable capacity is tainted, a deploy stays `Pending` forever with no way for the
  operator to fix it.
- **A mandated runtime.** Some clusters require workloads to run under a specific `runtimeClassName` —
  a sandboxed runtime such as gVisor or Kata, or a hardware-specific one. That is a pod-spec field.
- **Scheduling and priority policy.** `priorityClassName`, `topologySpreadConstraints`, a
  `nodeSelector` for architecture or zone, or an `imagePullSecret` for a private base registry are all
  routine operator requirements expressed on the pod spec.

None of these should be built into the engine. They are properties of *a* cluster, not of Burrow, and
hard-coding any of them would impose one operator's topology on everyone else. But the engine should
not make them impossible either, which is what a fixed literal does today.

The asymmetry with the build path is the anomaly. The same operator, on the same cluster, can already
adjust the build pod and cannot adjust the app pod — and the app pod is the one that runs indefinitely.

The alternatives available without a seam are both poor. Forking the deploy path duplicates the code
that turns a deploy request into a Deployment — the most churn-prone path in the engine — and
guarantees drift. Patching the Deployment after creation is worse than untidy: it is a **race**. The
API server admits the object, the scheduler places pods, and the kubelet may start a container before
the patch applies. Where the missing field is a sandboxed runtime, that window is a workload running
unsandboxed — the precise thing the runtime was required for.

## Decision

### 1. `Adapter.WithPodMutator` — the same shape as the build seam

The deploy adapter gains:

```go
func (a *Adapter) WithPodMutator(fn func(*corev1.PodSpec)) *Adapter
```

The hook is applied to the Deployment's pod template spec **after it is constructed and before the
object is sent to the API server**, exactly as `WithBuildPodMutator` is for the build Job. It returns
the adapter for chaining, matching the existing option style.

Deliberately the same signature, the same nil-means-unchanged default, and the same naming as the
build seam: a reader who understands one understands the other, and there is no second concept to
learn.

### 2. The mutator applies on every path that writes the pod template

Create and update alike. A hook applied only on create would be silently dropped by the first rollout,
leaving a long-running workload without the toleration or runtime class it was deployed with — a
regression that appears later, under load, and presents as a scheduling problem rather than a missing
hook.

### 3. A nil mutator leaves current behaviour byte-for-byte unchanged

The default is nil, and with no mutator wired the constructed Deployment is identical to what it is
today. This is a test obligation, not an aspiration, in the same way ADR-0053 §6's seam is tested.

## Consequences

- **The engine gains an extension point it does not itself use.** That is the ADR-0045 bargain and the
  ADR-0053 §6 precedent: the alternative is downstream forks of the deploy path, which serve no one.
  The cost is one option method and one call site.
- **Clusters that were previously undeployable become deployable** without Burrow having to learn about
  taints, runtime classes, or priority policy — the operator supplies what their cluster requires.
- **The hook is trusted, in-process, and unvalidated.** A mutator can set anything on the pod spec,
  including breaking it. This is the same trust the build seam extends, and it is appropriate: the
  mutator is compiled into the same binary by whoever operates that binary, not supplied at runtime.
- **A non-idempotent mutator will drift**, since §2 applies it on updates as well as creates.
  Appending to a slice without checking is the obvious trap, and worth stating in the doc comment: the
  build seam's single-shot Job never has to think about it, and a reader may carry that assumption
  across.
- **The deploy path gains a place where behaviour can diverge invisibly.** Two installs on the same
  version can now produce different pods. That is the point, but it makes the mutator the first thing
  to inspect when one install's pods differ from another's.

## Rejected alternatives

- **Fork the deploy path downstream.** Duplicates the most churn-prone code in the engine and
  guarantees divergence, which is what ADR-0045's seam model exists to avoid.
- **Patch the Deployment after creating it.** A race with the scheduler and the kubelet: pods can start
  before the patch lands. Where the missing field is a sandboxed runtime or a scheduling constraint,
  that window is a workload running somewhere it was never meant to. A correctness problem, not an
  aesthetic one.
- **A mutating admission webhook.** Correct in principle and heavier in practice — another deployed
  component, its own certificate lifecycle, and a failure mode where the webhook being down blocks or
  silently skips every deploy. It would also impose infrastructure on installs that need none of it.
  Reasonable if the mutation ever had to apply to objects this engine does not create; it does not.
- **Configuration fields on the engine for tolerations, runtime class, and the rest.** Every such field
  is one more thing to specify, validate, document, and keep in step with the Kubernetes pod API, and
  the list has no natural end — each new cluster requirement becomes a new field and a new release. A
  single hook covers the whole pod spec and does not grow.
- **A broader "Deployment mutator" over the whole object rather than the pod spec.** Wider than the
  need, and it would let a caller change replica counts, selectors, and labels the engine relies on.
  The pod spec is where these requirements actually live, and it matches the build seam.
