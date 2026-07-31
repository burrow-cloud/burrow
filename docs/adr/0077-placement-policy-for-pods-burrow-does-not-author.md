# ADR-0077: Placement policy for pods Burrow does not author

## Status

🟡 Proposed

## TL;DR

[ADR-0073](0073-placement-policy-reaches-every-authored-pod.md) made every pod the engine **authors**
reachable by operator placement policy, through hooks shaped as `func(*corev1.PodSpec)`. That works
because Burrow builds the pod spec and can hand it over before writing it.

[ADR-0066](0066-postgres-on-cloudnativepg.md) breaks that assumption. A CloudNativePG `Cluster` is
authored by **the operator**, not by Burrow. Burrow creates a custom resource and CNPG builds the pod
— and CNPG exposes `spec.affinity` (nodeSelector, tolerations, pod anti-affinity) and
`spec.topologySpreadConstraints`, **not a PodSpec**. There is nothing to hand to the hook.

- **The gap is not hypothetical and not small.** The Postgres add-on is the pod ADR-0073 calls the
  platform hook's "most dangerous reach" — it holds tenant data, and its placement decides whether its
  volume can attach.
- **This record generalises ADR-0073's rule** from *every pod the engine authors* to **every pod the
  engine causes to exist**, and accepts that reaching the second kind means translating policy into
  whatever surface the operator offers.
- **Translation is lossy and must fail loudly**, not silently drop what it cannot express. A hook that
  sets a `runtimeClassName` CNPG has no field for must be refused at wiring time, not ignored at
  write time.
- **A third seam, not a widened second one.** `WithPlatformPodMutator` keeps its `*corev1.PodSpec`
  signature; operator-authored placement gets its own hook over the fields an operator actually has.

Extends [ADR-0073](0073-placement-policy-reaches-every-authored-pod.md) §1 to a case it did not
contemplate. Required by [ADR-0066](0066-postgres-on-cloudnativepg.md) before the add-on becomes a
`Cluster`. Supersedes nothing.

## Context

### What exists today

- **ADR-0073 §1** states the rule: *"a new authored pod path arrives with a hook, or it arrives
  undeployable."* §2 splits the reach by whose image runs — `WithPodMutator` for the app's own image,
  `WithPlatformPodMutator` for Burrow's — and both are `func(*corev1.PodSpec)`.
- **Every pod covered by those hooks is built by Burrow.** `controlplane/kube` composes the pod spec,
  applies the hook over the fully-constructed spec, then writes the object.
- **ADR-0066 changes who builds the database pod.** Burrow will create a CNPG `Cluster` and read its
  status; the operator reconciles it into a StatefulSet and pods.
- **CNPG's placement surface is not a PodSpec.** It offers `spec.affinity` — `nodeSelector`,
  `tolerations`, node and pod affinity, `podAntiAffinityType` — and `spec.topologySpreadConstraints`.
  It does not accept an arbitrary pod template.

### What breaks

**The most consequential pod Burrow places stops taking placement policy.** On the cluster ADR-0061
was written for — one whose only capacity is tainted — an operator who wires the platform hook gets a
database that will not schedule, and the hook they wired is not consulted because there is no pod spec
to consult it with.

**And the failure is the quiet kind.** A `Cluster` whose pods cannot schedule reports zero ready
instances, which reads as a slow start. Nothing names the missing toleration, and nothing says the
hook was skipped rather than applied and found nothing to do.

**Silently narrowing the rule would be worse than the gap.** ADR-0073's §1 is a standing obligation,
and the natural reading of "authored" — Burrow literally constructs the object — quietly excuses every
future operator-managed component from it. The next operator Burrow adopts inherits the same hole
without anyone deciding to dig it.

### What this record resolves

How operator placement policy reaches a pod Burrow does not build, what happens to policy the
operator's surface cannot express, and whether that is one seam or two.

## Decision

### 1. The rule is about pods Burrow causes to exist, not pods Burrow assembles

ADR-0073 §1 is restated: **every pod the engine causes to exist is reachable by operator placement
policy.** Whether Burrow composes the pod spec or hands a custom resource to an operator that composes
it is an implementation detail of how Burrow asks for a workload — it is not a reason the operator's
cluster rules stop applying.

A component that cannot carry placement policy at all is a component Burrow should not adopt for a
workload that must schedule on a constrained cluster. That is a real constraint on future operator
choices and it is meant to be.

### 2. A third seam, over the fields an operator offers

`WithPlatformPodMutator` keeps its `func(*corev1.PodSpec)` signature and its existing reach.
Operator-authored workloads get a **separate hook** over a placement shape an operator can actually
consume: node selector, tolerations, affinity, topology spread.

**Not a widened second hook**, because the two cannot be the same type. Forcing the platform hook to
serve both would mean synthesising a fake `PodSpec`, letting the operator mutate it, and then
scraping the fields back out — inventing a pod that never exists so a signature can be preserved. That
is a translation with extra steps and a worse failure mode: fields set on the fake spec that have no
destination would vanish with nothing to notice them.

**Not a per-operator hook either.** The shape is the placement vocabulary, not CNPG's schema, so a
second operator maps onto the same seam.

### 3. What cannot be translated is refused at wiring time, not dropped at write time

An operator hook may set something the target has no field for — a `runtimeClassName`, a security
context, a volume. CNPG has no equivalent for most of them.

**Burrow refuses to start rather than writing a `Cluster` that silently lacks it.** The check happens
when the hook is wired and the resource is composed, not when a pod fails to schedule an hour later.

This is the same argument ADR-0073 §5 makes about the seam not being an isolation guarantee, applied
one level down: an operator who wires a hook and is not told it was ignored believes their policy is
in force. **A silently dropped `runtimeClassName` on a database holding tenant data is precisely the
failure that must not be quiet.**

### 4. Volume attachment bounds what placement may do

ADR-0073's Consequences already warn that the platform hook reaches stateful workloads and that moving
the Postgres pod to a pool where its volume cannot attach breaks the add-on rather than one deploy.
Under CNPG this is sharper: the operator manages the PersistentVolumeClaims and a `Cluster` whose pods
cannot reach their volumes is not a scheduling inconvenience, it is a database that will not start.

The hook is not restricted from expressing it — Burrow cannot know an operator's topology — but the
obligation is stated where a wiring author meets it, as ADR-0073 §6 states idempotency.

### 5. The managed product's answer is stated, and it is not this hook

The cloud's `PlatformPodPlacement` adds one toleration for the server node and deliberately nothing
else, because k3s local-path volumes bind to one node and any steering strands them. That reasoning is
unchanged by CNPG and is not this record's to make — but it is the concrete case this seam must be
able to express, and a design that cannot express "tolerate this taint, touch nothing else" has failed.

## Consequences

- **A third seam.** ADR-0073 already called two "a real cost, and it is now two costs". This is three,
  and the honest framing is that the third exists because a dependency changed who builds a pod, not
  because the design wanted it.
- **Operator adoption gains a criterion.** "Can placement policy reach the pods it creates" becomes a
  question asked of an operator before adopting it, alongside licence and maturity. CNPG passes;
  something else may not, and that is now a reason to decline it.
- **Refusing at wiring time can block a start-up** that previously worked, if an existing hook sets
  something untranslatable. That is the intended direction — a database that refuses to start is
  recoverable, a database silently running unplaced is discovered during an incident — but it is a new
  way to be blocked and should say exactly which field had no destination.
- **The translation is a maintenance surface.** CNPG's placement fields can change between versions,
  and a mapping that silently stops covering a field reintroduces §3's failure. It needs a test that
  fails when the target schema moves, not only when Burrow's code does.
- **ADR-0073 §1's wording needs reading with this record.** Anyone finding §1 alone will read
  "authored" narrowly, which is the gap this exists to close.

## Rejected alternatives

- **Widen `WithPlatformPodMutator` to cover both**, synthesising a `PodSpec` for operator-authored
  workloads and scraping the fields back. One seam instead of two, and existing wirings keep working
  unchanged. Rejected in §2: it invents a pod that never exists, and fields with no destination vanish
  silently — the exact failure §3 is written to prevent, reintroduced by the mechanism meant to avoid
  a third hook.
- **Accept that operator-authored pods take no placement policy**, and document it. Honest, and no new
  surface. Rejected because it makes the database undeployable on the constrained cluster ADR-0061 was
  written for, and because it narrows ADR-0073 §1 by accident — the next operator inherits the hole
  without a decision.
- **Have the operator's own configuration carry it** — let the cluster administrator set CNPG's
  affinity directly, outside Burrow. Legitimate, and it is what an operator running CNPG themselves
  would do. Rejected because Burrow creates and owns the `Cluster` object: a hand-edited `spec.affinity`
  is reconciled away on the next write, so the administrator's change is not merely bypassed, it is
  reverted.
- **A mutating admission webhook over everything Burrow causes to exist.** The one option that reaches
  operator-authored pods without translation, and ADR-0073 already named it the right answer for an
  operator who needs *enforcement*. Rejected here for ADR-0073's reasons — another deployed component,
  its own certificate lifecycle, a down-webhook failure mode blocking every deploy — and because it
  remains an option an operator can choose on top of this rather than instead of it.
- **Per-operator hooks**, one shaped for CNPG and another for whatever comes next. Maximum fidelity to
  each operator's schema. Rejected because it exposes a dependency's API shape as Burrow's public
  seam, so a CNPG upgrade becomes a breaking change for anyone who wired it.
