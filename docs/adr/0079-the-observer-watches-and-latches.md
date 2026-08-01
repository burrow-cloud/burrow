# ADR-0079: The observer watches, and latches a transition before recording it

## Status

🟡 Proposed

## TL;DR

[ADR-0074](0074-burrow-observes-what-it-manages.md) §3 says burrowd gains a **watch**. What shipped is
a sweep every sixty seconds. The sweep cannot see anything shorter than itself, and the thresholds it
feeds are already finer than that.

- **Watch the objects, do not poll them.** A failure is recorded when it happens, not up to a minute
  later.
- **A watch on its own is worse than a sweep.** Kubernetes status flaps; recording every edge would
  fill the ledger with noise and make "when did it start" meaningless.
- **So a transition is latched before it is recorded** — it must persist for a dwell time before it
  opens a row, and clear for one before it closes it. The dwell is **per reason**, because the
  reasons differ in what a moment of it means.
- **A gap is still a gap.** Watches disconnect, and the coverage record must show that as plainly as
  a restart.

Supersedes [ADR-0074](0074-burrow-observes-what-it-manages.md) §3's mechanism only. §4's transition
model, §5's concurrent reasons, §6's intent diffing, §7's separation from the audit log and §9's
observe-never-remediate are unchanged and are what this record exists to serve better.

## Context

ADR-0074 §3 decided burrowd would gain a watch over the objects the registry says it owns. The
implementation that landed sweeps instead: `DefaultObserveInterval` is one minute
(`controlplane/observer.go:21`), and each pass enumerates what Burrow manages and compares it against
the cluster.

The sweep was chosen for reasons that are still good ones. It reads through the same seams every
other surface reads through, so a ledger row and a `burrow app status` answer cannot contradict each
other. It is deterministic against a fake clock and a fake client, which is what
[ADR-0010](0010-testing-strategy.md) asks of anything in the core. And it was simple enough to be
correct on the first attempt, which for the thing that records reliability is worth something.

What it cannot do is see anything shorter than itself, and that limit has since acquired a number.
`status.unschedulable_grace` (`controlplane/limits.go:68`) defaults to **thirty seconds** — the
period a pod may be unschedulable before the status surface reports it. The observer samples every
**sixty**. So the ledger cannot resolve the threshold the platform already applies, and lowering the
grace below the sweep interval silently does nothing.

More generally, a sampled observer reports a *sample*, and the questions ADR-0074 exists to answer —
when did this start, is it still happening, has it happened before — are answered by a sampled
observer to within one sampling interval, always, with no way to tell from the row how much of the
imprecision is real.

**But a watch has the opposite failure, and it is worse.** Kubernetes status is not a sequence of
meaningful transitions; it is a stream of edges, many of which mean nothing. A pod goes NotReady and
Ready again in two seconds during an ordinary rolling update. A scheduler takes four seconds to place
a pod that was briefly unschedulable because a node was still joining. A container that will start
fine reports `ContainerCreating` while its image layers unpack.

An observer that recorded every edge would produce exactly the outcome ADR-0074 §4 rejected in
choosing a ledger over an event stream: thousands of rows describing nothing, and a `first_seen`
column that answers "when did the last flap begin" rather than "when did this start". A watch without
a latch is a faster way to be wrong.

## Decision

### 1. The observer watches

burrowd establishes watches over the objects the registry says it owns, rather than enumerating them
on a timer. A failure is recorded when the cluster reports it.

**What is watched is still bounded by the registry, not a namespace or a label.** ADR-0074 §3's
actual argument survives intact and is unaffected by the mechanism: a label selector can only find
things that exist, and §6's interesting failure is the thing that does not.

**It remains read-only against the cluster** (ADR-0074 §9), and it remains the first thing in Burrow
that runs when nobody asked it to.

**The read seams stay shared.** The watch feeds the same evidence functions the status surface uses,
so a ledger row and a `burrow app status` answer are still derived from one place. That property was
the sweep's best argument and it is not the sweep's to keep.

### 2. A transition is latched on both edges

A condition must **persist for a dwell time** before it opens a ledger row, and must **clear for a
dwell time** before that row is resolved.

Both edges, not one. Latching only the opening edge would let a flapping object open one row and
close it repeatedly, which is the same noise arriving through a different door — and it would make
the occurrence count in ADR-0074 §4 count flaps rather than failures.

The latch is what makes the transition model truthful rather than merely well-shaped. `first_seen`
becomes "when this started being real", not "when this was first glimpsed".

### 3. The dwell is per reason, and defaults differ

A single dwell value would be wrong for most of the vocabulary, because the reasons differ in what a
moment of them means:

| Reason | Dwell | Why |
| --- | --- | --- |
| `OOMKilled` | **None** | It already happened. The kernel killed the process; there is nothing to wait to see |
| `CrashLoopBackOff` | **None** | The backoff *is* the dwell. Kubernetes only reports it after repeated failures |
| `Unschedulable` | **`status.unschedulable_grace`** | A scheduler may simply be slow, or a node may be joining. Already a configured value, already thirty seconds |
| `ImagePullBackOff`, `ErrImagePull` | Short | A registry hiccup that resolves itself is not worth a row |
| `ProgressDeadlineExceeded`, `DeadlineExceeded` | **None** | A deadline is itself a dwell that already elapsed |

The pattern: **a reason that is already the outcome of waiting gets no further wait.** Adding one
would double-count the patience Kubernetes has already spent, and delay a row that is certainly real.

Dwell values are operational limits under `status.` ([ADR-0068](0068-operational-limits-are-configuration.md)),
so an operator whose autoscaler takes ninety seconds to provision a node can say so, in one place, and
have both the status surface and the ledger honour it. `status.unschedulable_grace` is not a new
setting joining this scheme; it is the first occupant of a scheme it was already implying.

### 4. A dropped watch is a gap, and says so

Watches disconnect. A client re-establishes and re-lists, and between those two moments it saw
nothing — which is indistinguishable, in an empty ledger, from a period in which nothing broke.

That is the failure ADR-0074's consequences name explicitly and refuse to have. So the coverage record
already built for the sweep continues to serve, with a disconnect treated as it treats a restart:
coverage ends when the watch drops and resumes when the re-list completes.

**A re-list is not free of consequence either.** It reports current state, not what happened while
disconnected, so a failure that started and resolved inside the gap is invisible. Coverage must show
the gap rather than the observer pretending continuity across it.

## Consequences

**Resolution stops being bounded by a sampling interval.** A failure is recorded when it happens plus
its dwell, which is a number chosen deliberately per reason rather than an artefact of how often a
loop runs. `status.unschedulable_grace` becomes meaningful at values below a minute, which it is not
today.

**Determinism in tests gets harder, and must not be given up.** A sweep on an injected clock is
trivially deterministic; a watch is event-driven and arrives when it arrives. The dwell timers must be
driven by the injected clock rather than by wall time, and the watch itself must be substitutable, or
the observer becomes the one part of the core that cannot be tested the way
[ADR-0010](0010-testing-strategy.md) requires. This is the real cost of the decision.

**burrowd holds more state.** A watch keeps an object cache proportional to what Burrow manages, and
the latch keeps a pending-transition set alongside it. Both are bounded by the managed set rather than
by usage, but neither was there before.

**A latched observer is deliberately slower than the truth.** For a dwelled reason, the ledger lags
the cluster by the dwell. That is the point — the lag is what removes the noise — but it means the
ledger is not the place to look for what is happening *this second*. ADR-0074 §1 already says current
state is derived live and never cached; this is the same boundary, restated from the other side.

**A flap that never exceeds its dwell is invisible.** An app that goes unready for ten seconds every
few minutes, forever, produces no rows. That is correct for the ledger's purpose and is worth naming,
because it is a real pathology the ledger will not surface — and it is the one thing the discarded
sweep would eventually have caught by luck.

## Rejected alternatives

**Keeping the sweep and shortening the interval.** The obvious cheap fix, and it degrades in the wrong
direction: a sweep short enough to resolve a thirty-second grace runs at least twice as often, and
each pass enumerates everything Burrow manages against the cluster. Cost scales with the managed set
*and* with the frequency, to approximate something a watch reports exactly.

**A watch with no latch.** Faster, simpler, and it produces the event stream ADR-0074 §4 rejected on
its merits — thousands of rows for one problem, and a `first_seen` that means "the most recent flap".
The record chose a ledger over an event stream deliberately; a latchless watch reintroduces the
stream and calls it a ledger.

**One dwell value for every reason.** Simpler to configure and wrong in both directions at once: long
enough for `Unschedulable` delays an `OOMKilled` row that is certainly real, and short enough for
`OOMKilled` records scheduler latency as a failure.

**A watch for detection with a periodic sweep for reconciliation.** Genuinely tempting, since §6's
intent-versus-cluster diffing is a comparison rather than an event, and a watch does not naturally
express "this object is absent". It is not rejected on merit — §6's diffing may well keep a periodic
pass — but it is **out of scope here**, because folding it in would decide §6's mechanism as a side
effect of deciding §3's. One record, one mechanism.

**Debouncing in the reader instead of the writer.** Store every edge and collapse it at query time.
It keeps the writer simple, preserves full fidelity, and makes retention worse rather than better —
ADR-0074 §4 bounds retention because unbounded growth in the control plane's own database is an
outage. Storing the noise in order to hide it later is the wrong end to solve it from.
