# ADR-0101: A pod mutator can refuse the pod

## Status

✅ Accepted

## TL;DR

[ADR-0100](0100-a-pod-mutator-learns-which-apps-pod-it-is-shaping.md) told the app hook *which* app it
is shaping. It still cannot say **"do not deploy this one"**.

- **The hook returns nothing.** An operator whose policy lookup fails has two moves: guess, or write
  something broken enough that the cluster refuses it.
- **Both are bad.** A guess puts an app on a runtime nobody chose. Poisoning the spec does stop the
  deploy — but the person debugging it sees a complaint about a malformed name, not the real reason.
- **Fix: the hook returns an error**, and that error stops the write and reaches the caller intact.
- **Nothing is built and then abandoned.** The object is assembled in memory and dropped; no request
  reaches the API server.
- **A refusal is not a conflict**, so the retry loop does not run it again. The hook is called once.
- **Costs a source break.** The widened spelling shipped one release ago and nobody has adopted it.
  A compile error an embedder sees immediately beats a fourth name for one hook.

Refines [ADR-0100](0100-a-pod-mutator-learns-which-apps-pod-it-is-shaping.md) §1's signature and
leaves its §2 identity rule, §3 compatibility wrapper and §4 platform-hook promise standing. Keeps
[ADR-0061](0061-deploy-pod-mutator-seam.md) §3's byte-for-byte guarantee. Supersedes nothing.

## Context

[ADR-0100](0100-a-pod-mutator-learns-which-apps-pod-it-is-shaping.md) widened the deploy pod-mutator
seam so an operator can shape one app's pod differently from another's:

```go
func (a *Adapter) WithAppPodMutator(fn func(PodIdentity, *corev1.PodSpec)) *Adapter
```

**What is true today.** That signature returns nothing. Whatever the mutator concludes, the adapter
carries on and writes the pod template it has. The hook can change a pod; it cannot decline one.

**What breaks.** The motivating consumer selects a sandboxing runtime per app by reading a stored
record. That read can fail — the store is unreachable, the row is unreadable, the identity resolves to
nothing. The consumer's own decision, recorded on their side, is that a failed read must **fail the
deploy** rather than fall back to a default, because an app silently running on a runtime nobody chose
is a worse outcome than an app that did not deploy. A void hook cannot express that, so an operator
holding that policy has exactly two options:

1. **Guess.** Apply the default and let the deploy succeed. This is the outcome the policy exists to
   prevent, and it fails *open* — the app runs, on the wrong isolation boundary, and nothing says so.
2. **Poison the spec.** Write a value invalid enough that the API server rejects the object — a
   `runtimeClassName` that is not a DNS subdomain, say. This does fail closed, and it was implemented
   and works.

**Why option 2 is only half of failing closed.** The deploy stops, which is the property that matters,
and it stops for a reason the operator cannot read. What surfaces is an admission error about a
malformed name containing a string the embedder chose as a smuggled message. Anyone debugging it must
already know that a garbage `runtimeClassName` is this system's way of saying a policy lookup failed.
It also spends the API server's validation as an error channel, so a later tightening or loosening of
that validation silently changes whether the refusal works at all.

**The narrower framing.** A seam that can only express *change this* forces every "should not exist"
decision to be encoded as a change ugly enough to be rejected downstream. That is a general property
of void mutation hooks and it is worth fixing at the seam rather than at each consumer.

**What has to be resolved.** Whether the hook gains an error return, and what that costs an embedder
who has already adopted the released spelling.

## Decision

### 1. The app hook returns an error

```go
func (a *Adapter) WithAppPodMutator(fn func(PodIdentity, *corev1.PodSpec) error) *Adapter
```

A mutator that returns nil behaves exactly as before. A mutator that returns a non-nil error **stops
the write**, and that error reaches the caller of `ApplyWorkload` or `RunJob` unwrapped enough that
`errors.Is` recovers what the embedder returned. The message is the embedder's own, so the operator
reads the actual reason rather than a downstream symptom of it.

The hook remains trusted and in-process, as [ADR-0061](0061-deploy-pod-mutator-seam.md) established.
This adds a way for it to decline; it does not add validation of what it does.

### 2. Nothing reaches the API server on a refusal

Both invocation sites build their object in memory and then apply it. The error is returned from the
builder, the assembled object is dropped, and **no request is issued**. A refusal is not a create that
is then deleted, nor an object that briefly exists; it is a deploy that did not happen.

This matters because the alternative it replaces — poisoning the spec — *did* issue the request and
relied on the server to reject it. Refusing locally means the behaviour no longer depends on what any
particular cluster's admission chain happens to enforce.

### 3. A refusal is not a conflict, so it is not retried

The write path retries on optimistic-concurrency conflicts. A mutator's refusal is not one, and the
retry helper returns it rather than looping. **The hook is called exactly once per write**, which is a
property worth pinning: a mutator with a side effect — a metric, a log line, a rate-limited lookup —
would otherwise fire a variable number of times for one deploy, and the count would depend on
unrelated contention.

### 4. `WithPodMutator` and `WithPlatformPodMutator` do not change

[ADR-0100](0100-a-pod-mutator-learns-which-apps-pod-it-is-shaping.md) §3's compatibility wrapper keeps
its exact `func(*corev1.PodSpec)` signature and now stores a function that returns nil. An operator
who wired the original hook is unaffected, and
[ADR-0061](0061-deploy-pod-mutator-seam.md) §3 still holds: an adapter with no mutator, and an adapter
whose mutator returns nil, author byte-for-byte the same pod template as before either seam existed.

`WithPlatformPodMutator` is untouched. [ADR-0077](0077-placement-policy-for-pods-burrow-does-not-author.md)
§2 promises its signature and [ADR-0100](0100-a-pod-mutator-learns-which-apps-pod-it-is-shaping.md) §4
restated it. Its pods belong to no app, so it has nothing to refuse on an app's behalf.

### 5. The break is taken on the widened spelling rather than adding a fourth name

`WithAppPodMutator` has been released exactly once, and no known consumer has adopted it. Adding a
fifth spelling of one seam to preserve a name nobody calls would leave the repository with four ways
to register a pod mutator, three of them discouraged.

**A source break here is loud and immediate**: an embedder finds out at compile time, in their own
repository, with the type in the message. That is the acceptable kind of break, and it is the reason
this decision differs from
[ADR-0100](0100-a-pod-mutator-learns-which-apps-pod-it-is-shaping.md) §3's — which retained
`WithPodMutator` precisely because that one *had* real adopters and a wrapper cost nothing. Retention
was right there and is wrong here; the two are not analogous and should not be read as a precedent for
each other.

### 6. The signature pin is edited deliberately, and its location is corrected

[ADR-0100](0100-a-pod-mutator-learns-which-apps-pod-it-is-shaping.md) §5 said a third line pinning
`WithAppPodMutator` would be added to the guard that pins the other two hooks. In the implementation it
went into its own file instead, `controlplane/kube/app_pod_identity_test.go`, with its own comment
explaining that it pins for the same reason. **That is where it lives**, and §5's wording should be read
as describing the pin's purpose rather than its address.

This record changes that pin, because it is the pin for the signature being changed. It is edited with
a comment recording that the edit was intended — **a pin edited quietly is a pin defeated**, and the
next reader has to be able to tell a deliberate change from an accommodation. The two lines in
`placement_test.go` covering `WithPodMutator` and `WithPlatformPodMutator` are untouched and still
compile, which is the evidence that §4's promise was kept.

## Consequences

**An embedder on the widened hook must add a return.** The fix is `return nil`, the compiler names
every site, and there is one known consumer, unreleased. An embedder on the original
`WithPodMutator` changes nothing at all.

**Operators gain a way to fail a deploy, and the responsibility for using it.** A mutator that returns
an error on a transient condition converts a blip into a failed deploy. That is the embedder's call to
make deliberately — this record supplies the mechanism and takes no view on when it should fire.

**The engine still wires no mutator**, and `docs/CAPABILITIES.md`'s statement to that effect stays
true. This widens an extension point; it does not give the engine a use for one.

**A refusal is not observable to the engine's own metrics as a distinct outcome.** It surfaces as a
failed apply, the same as any other error from that path. Distinguishing "the operator declined" from
"the write failed" would need a typed error and is not decided here.

## Rejected alternatives

**Leave the hook void; let the operator poison the spec.** Rejected on the grounds in the Context: it
does fail closed, but the reason is unreadable at the point of failure, and it spends downstream
validation as an error channel — so a change to that validation silently changes whether refusal works.
It also depends on the API server rejecting what was sent, which makes the behaviour a property of the
cluster rather than of this code.

**Add a separate "should this pod exist" predicate alongside the mutator.** Two hooks over one decision,
called at the same moment with the same inputs, and an operator must remember to wire both or get the
failure this record is about. One hook that can decline expresses the same thing with no second thing
to forget.

**Keep the released signature and add a differently-named erroring variant.** This is §5's rejected
half. It buys a name nobody calls, at the price of a fourth registration spelling for one seam. The
break it avoids is a compile error an embedder sees immediately in their own build, which is the
failure mode a pre-1.0 module should prefer.

**Let the mutator panic to abort.** A panic is not a refusal; it is an unhandled fault in a trusted
in-process hook, and recovering it at the seam would convert every genuine bug in an operator's mutator
into a silently-swallowed deploy failure.
