# ADR-0100: A pod mutator learns which app's pod it is shaping

## Status

✅ Accepted

## TL;DR

An operator can shape every app's pod, or none. It cannot shape one app's pod differently from
another's, because the hook is never told which app it is looking at.

- **The hook takes a bare pod spec.** `func(*corev1.PodSpec)`. No app name, no namespace, nothing
  that says whose pod this is.
- **It is wired once, for the whole process.** So an operator wanting a per-app answer has nowhere to
  put the question.
- **Fix: a second spelling of the same hook that is handed the app's identity** — name, namespace,
  and whether this is the long-running workload or a one-off run.
- **The old spelling keeps working, untouched.** Same field underneath, so nothing that wired the old
  one needs an edit and no pod template changes by a byte.
- **The identity says who, never how to look them up.** No tenant, no environment name, no account.
  An embedder keys its own records however it likes; this hook does not learn that shape and cannot
  come to depend on it.
- **The platform hook does not move.** Its pods belong to no app.

Refines [ADR-0061](0061-deploy-pod-mutator-seam.md) §1's signature and leaves its §2 and §3 standing.
Keeps [ADR-0073](0073-placement-policy-reaches-every-authored-pod.md) §2's split by whose image runs, and
[ADR-0077](0077-placement-policy-for-pods-burrow-does-not-author.md) §2's promise that the platform hook keeps its type.
Supersedes nothing.

## Context

[ADR-0061](0061-deploy-pod-mutator-seam.md) §1 gave the deploy adapter a seam an operator can use to
shape the pods Burrow authors for apps:

```go
func (a *Adapter) WithPodMutator(fn func(*corev1.PodSpec)) *Adapter
```

The signature was chosen deliberately. §1 says so in as many words: *"Deliberately the same signature
… as the build seam: a reader who understands one understands the other, and there is no second
concept to learn."* That reasoning was sound for what the seam was for. An operator adding a node
selector, a toleration, a sandboxing runtime or a security context applies one policy to every app it
runs, and a hook that is handed only the pod expresses exactly that and nothing more.

**What is true today.** The hook is registered once against an `Adapter`, and the `Adapter` is built
once per process. Nothing in this repository wires one — `docs/CAPABILITIES.md` states that as a
property, and it is accurate: the only adapter construction outside tests is in `cmd/burrowd/main.go`
and it wires no mutator. The seam exists for an embedder, which is the ADR-0045 bargain
[ADR-0061](0061-deploy-pod-mutator-seam.md) names outright: *"the engine gains an extension point it
does not itself use."*

**What breaks.** An operator now needs a policy that differs *between* the apps it runs, and the seam
cannot carry one. Concretely: choosing a sandboxing runtime per app rather than per install, so that
one app can be moved onto a stronger isolation boundary while its neighbours stay where they are. The
hook is handed a `*corev1.PodSpec` and asked to decide, and a `*corev1.PodSpec` at that moment does
not say whose pod it is. The pod's labels are not yet a reliable answer at every invocation site, and
reading the container image to work out which app this is would be a re-derivation of a
classification the engine already holds — the exact shape
[ADR-0073](0073-placement-policy-reaches-every-authored-pod.md) §2 rejects, for the reason it gives: *"a wrong branch puts
tenant code on the platform pool."*

The engine, meanwhile, has the answer in hand. Both places the stored mutator is invoked are inside
functions whose argument already carries the app: `buildDeployment` receives a
`controlplane.WorkloadSpec` and `runJob` receives a `controlplane.RunSpec`, and both types have an
`App` field that is already used a few lines earlier to build labels and the container name. The
adapter's namespace is in scope at both. Nothing needs to be threaded anywhere; the identity is
already there and is being discarded at the last statement.

**What has to be resolved.** Whether the seam is widened, and if so what it is widened *with* — the
narrowest identity that answers the question, rather than the most convenient object that happens to
be nearby.

There is one more force worth naming, because it shaped the answer more than anything else. The only
embedder of this seam keys its own per-app records on a tuple this repository has no concept of. It
would be easy, and wrong, to widen the hook with the fields that embedder happens to need today. Then
the public seam would encode one consumer's storage layout, and the next embedder — or the same one
after a schema change — would need the seam changed again. A seam that has to move whenever a
consumer re-keys its database is not an extension point.

## Decision

### 1. A second spelling of the seam, carrying the app's identity

The deploy adapter gains:

```go
func (a *Adapter) WithAppPodMutator(fn func(PodIdentity, *corev1.PodSpec)) *Adapter
```

It is the same seam as [ADR-0061](0061-deploy-pod-mutator-seam.md) §1, told who it is shaping. §2 of
that record is unchanged: the mutator applies on **every** write, so a deploy, a rollback and a config
reapply all produce the same pod template. §3 is unchanged: an adapter with no mutator wired authors
byte-for-byte the pod template it authored before this seam existed.

### 2. `PodIdentity` says who, and deliberately not how to find them

```go
type PodIdentity struct {
	App       string
	Namespace string
	Workload  WorkloadRole
}
```

`App` is the application's name, the `App` field of the spec at both invocation sites. `Namespace` is
the namespace the adapter is operating in. `Workload` distinguishes the long-running workload from a
one-off run, which both sites know statically.

**It carries identity and not context.** No tenant, no environment name, no account, no organisation,
no request metadata. This repository has no concept of any of them, and putting one here would mean
this seam had a view on how an embedder organises its customers.

This is the constraint that makes the seam durable, so it is stated as part of the decision rather
than left as a note: **`PodIdentity` must never grow a field whose purpose is to be a key into an
embedder's records.** An embedder that needs to resolve `(App, Namespace)` into something it stores
does that resolution on its own side, against its own records, where a schema change costs it a
migration and costs this repository nothing. A seam that hands over a lookup key has quietly taken on
the consumer's storage layout as part of its public API, and every re-keying on that side becomes a
breaking change on this one.

`Namespace` is a fact about the cluster, which is why it belongs here and a tenant identifier does
not. It happens to be sufficient for an embedder that derives its namespaces from its own records —
but that sufficiency is the embedder's business, not this seam's promise.

### 3. `WithPodMutator` keeps its exact signature and is retained

```go
func (a *Adapter) WithPodMutator(fn func(*corev1.PodSpec)) *Adapter
```

is unchanged and continues to work. It stores into the **same field**, wrapping the caller's function
so the identity is discarded:

```go
a.WithAppPodMutator(func(_ PodIdentity, spec *corev1.PodSpec) { fn(spec) })
```

One stored field, one invocation at each site, so there is no precedence question to answer and no
second mechanism to reason about — the last wiring wins, exactly as re-registering the old hook always
has. An operator applying one policy to every app keeps the simpler signature and the shorter
sentence in §1 of [ADR-0061](0061-deploy-pod-mutator-seam.md) stays true for them.

It is marked `Deprecated:` so tooling and documentation point at the widened spelling, which is a
signpost and not a removal date. Removing it is a separate decision that would need its own record.

### 4. The platform hook does not move

`WithPlatformPodMutator` keeps its `func(*corev1.PodSpec)` signature and its reach.
[ADR-0077](0077-placement-policy-for-pods-burrow-does-not-author.md) §2 promises exactly that — *"A third seam, not a widened
second one"* — and this record does not disturb it.

The promise is worth keeping on its merits and not only because it was made. The pods that hook
reaches are the add-on instance, the log and metrics collectors, and the backup and restore jobs.
**None of them belongs to an app.** Giving both hooks one identity type would mean a struct whose
`App` field is empty at half its call sites, and an identity that is absent exactly where a reader
would go looking for it is worse than no identity at all. If the platform hook ever does need to know
what it is shaping, the answer will be an add-on instance or a collector kind — a different type,
reached by a different decision.

### 5. The compile-time pin stays, and covers the new method too

`placement_test.go` pins both hooks' signatures with method expressions, so that widening either one
to serve both kinds of pod stops compiling. That guard is the reason this record chose a new method
over an edited one, and it survives this change untouched: both pinned lines still hold.

A third line is added pinning `WithAppPodMutator`, so the new spelling is guarded the same way the
other two are. The guard's comment is updated to say that the app hook now has two spellings over one
field — a pin whose comment describes a world that no longer exists is a pin the next reader will
distrust and then weaken.

## Consequences

**Nothing breaks.** No existing call compiles differently, in this repository or outside it. The six
in-repo test wirings, the signature pin, and any embedder that wired the original hook are all
untouched. An adapter with no mutator wired authors the same bytes it did before.

**There are two spellings of one seam.** This is a real cost and the honest name for it is a wart.
[ADR-0073](0073-placement-policy-reaches-every-authored-pod.md)'s argument for two hooks was that they cover two different
*sets of pods*; these two cover the same set and differ only in what they are handed. What keeps it
tolerable is that there is only one *mechanism* underneath — one field, one invocation, one applied
policy — so a reader who finds either spelling finds the same behaviour, and the deprecation marker
tells them which to write.

**The engine still wires neither.** `docs/CAPABILITIES.md`'s statement that nothing in this repository
wires a mutator remains true and should stay true. This record widens an extension point; it does not
give the engine a use for one.

**An embedder gains the ability to differ between apps, and the responsibility that comes with it.**
A hook that can treat two apps differently can also treat them differently *by mistake*, and the
failure lands on a pod template rather than at a compile error. That risk is the embedder's to manage;
what this record does is make it possible to take it deliberately instead of impossible to take at
all.

**A future identity need is a new field, not a new hook.** `PodIdentity` being a struct rather than
positional parameters means adding a fact about the pod later is source-compatible — subject to §2's
constraint, which rules out the kind of field most likely to be asked for.

## Rejected alternatives

**Change `WithPodMutator`'s signature outright, with a renamed shim for the old one.** Go has no
overloading, so the shim must be a differently-named method — which is this record's decision with the
names swapped, keeping the good name for the widened hook. It breaks every outside embedder at compile
time. That is the *loud* kind of break rather than the silent kind, and pre-1.0 the repository has
said elsewhere that breaking changes are acceptable. It was rejected because the break buys only a
name: the behaviour, the mechanism and the migration are identical either way, and it would also
require editing the signature pin, which reads as defeating a guard that exists to catch precisely
this change.

**A per-app adapter view — `WithApp(...)` returning a copy whose mutator reads the app off the
receiver.** Rejected because the existing `WithNamespace` demonstrates the failure mode. It returns
the **receiver**, not a copy, when the requested namespace equals the one it already holds — and for
an embedder whose apps deploy to a default environment that is the common path, not an edge. A
"view" that is silently the shared object would let one deploy set a mutator that another deploy
reads, and the wrong app's pod would be shaped with no error and no failing test. A second
copy-on-write field would inherit the same trap. It also leans on `Adapter` being safe to shallow-copy,
which it is not in general: its controller placement holds a map, a slice and a pointer, and a struct
copy aliases all three.

**Let the hook work out the app for itself, from a label or the container image.** This is the shape
[ADR-0073](0073-placement-policy-reaches-every-authored-pod.md) §2 already rejected: *"one hook could serve that only by
keying off an image or label to reconstruct a classification the engine already has, and a wrong
branch puts tenant code on the platform pool."* The engine holds `spec.App` at both invocation sites.
Making the operator re-derive what the caller already knows converts a fact into a guess.

**Hand the mutator the whole `WorkloadSpec`.** Rejected on two counts. The two invocation sites do not
share a type — one has a `WorkloadSpec` and the other a `RunSpec` — so the run path would need a
synthetic `WorkloadSpec` invented for a Job, which is the move
[ADR-0077](0077-placement-policy-for-pods-burrow-does-not-author.md) §2 rejects for the controller path because *"it invents a
pod that never exists."* And `WorkloadSpec` carries the app's environment, its secret file mounts and
its secret env keys; putting all of that in front of a hook that needs a name and a namespace widens
what the seam exposes for no gain.

**Carry the environment name in `PodIdentity`.** Rejected. Neither `WorkloadSpec` nor `RunSpec` has an
environment field today, so it would mean threading one through the engine to serve a consumer's
storage key — which is §2's constraint exactly. An embedder that needs an environment resolves it from
`(App, Namespace)` against its own records.
