# ADR-0074: Burrow observes what it manages, and remembers when it broke

## Status

✅ Accepted

## TL;DR

Burrow can tell you an app is not available. It usually cannot tell you **why**, and it can never
tell you **when it started**.

`WorkloadStatus` carries an `Issue` field built exactly for this — an actionable explanation plus a
machine-usable reason — and it is populated in **one place**, for **one failure class**: a failed
image pull. Every other blocking condition — a pod that never scheduled, a container crashlooping, a
missing config key, an OOM kill, a volume that will not attach — reports `Available: false` with an
**empty** `Issue`. The surface built to spare users `kubectl` goes quiet on most of the failures they
actually hit.

And nothing observes. There is no watch and no informer anywhere in the tree: every status is a
question asked at the moment someone asks it. So a failure with nobody present is not recorded, it is
merely missed — a Job that cannot schedule reports a *timeout* ([#352](https://github.com/burrow-cloud/burrow/issues/352)),
an add-on that never came up reads as a slow start, and an app that crashlooped from 02:00 to 02:40
and recovered leaves no evidence that it ever happened.

This record does three things:

1. **Widens `Issue`** to the failure classes users hit, keeping the criterion the code already
   applies: blocking and human-fixable, not self-resolving. This is separable, needs no new
   machinery, and should ship first.
2. **Gives burrowd an observer** — the first thing in Burrow that runs when nobody asked it to —
   watching the workloads the registry says Burrow owns, and writing failures to a **ledger**: one
   row per object and reason, with a first-seen, a last-seen, a resolution, and a count. Not an event
   stream.
3. **Makes it readable from the CLI**, cluster-wide, including the one diagnosis `kubectl` cannot
   perform: comparing what Burrow **intended** to exist against what the cluster actually has.

Current state stays **derived and never cached**, because a cache lies during exactly the incident it
exists to help. History must be **stored**, because nothing can reconstruct it afterwards. That split
is the design; the rest follows from it.

**Failures arrive in bunches, and the record is built for that** (§5). Keying on object *and* reason
is what lets one pod be OOM-killed and unschedulable at the same time without either being lost, and
what makes a pod flapping between two reasons legible as the one bug it is. The harder case is the
reverse — one taint, twenty unschedulable pods — so the listing groups by shared reason and orders
oldest-first, since the earliest row in a cascade is usually the one worth fixing. It stops there:
**Burrow shows correlation and refuses to claim causation**, because a confidently wrong root cause
during an incident is worse than none.

That refusal is a **division of labour, not a shortfall**. Burrow does not run a model and must not
acquire one: its half is to report every failure completely, in a shape something else can reason
over; **the agent's half is to turn twenty rows into a cause and a fix.** So the ledger optimises for
completeness over tidiness — the boring rows are the ones an agent needs — and an inference engine
inside burrowd is explicitly out of scope.

It is **not** the audit log ([ADR-0027](0027-audit-log.md)) and must not merge with it: audit records
what Burrow was *asked to do* and what it *decided*; this records what the cluster *did afterwards*.
And burrowd **observes without remediating** — noticing a crashloop is a read; restarting it is a
mutation with nobody present, and that is a different record with a much higher bar.

Extends [ADR-0009](0009-honest-status.md)'s discipline from the documentation to the running system:
a surface that reports "not available" without saying why, and a history that cannot distinguish
"nothing broke" from "nobody was watching", are the same dishonesty ADR-0009 refuses in prose.
Widens the Issue vocabulary [ADR-0017](0017-private-registry-authentication.md) introduced for one
failure class. Distinct from [ADR-0026](0026-observability-query-adapters.md)'s query seam and from
[ADR-0027](0027-audit-log.md)'s audit log, and §7 says why. Answers the quiet-failure problem
[ADR-0073](0073-placement-policy-reaches-every-authored-pod.md) describes from the other side.
Supersedes nothing.

## Context

### What exists today

- **`burrow status <app>` and `burrow list` read the cluster live, per call.** Nothing is cached and
  nothing is observed. `Engine.Status` and `Engine.ListApps` both call straight through to the
  Kubernetes adapter.
- **`WorkloadStatus.Issue` and `IssueReason` are the only enrichment**, and they exist for precisely
  the purpose this record extends: prose a human can act on, plus a raw Kubernetes reason an agent can
  branch on without parsing the prose.
- **They are populated in one function, for one family of reasons.** `Adapter.pullIssue` inspects the
  app's pods for a blocking image-pull condition. `imagepull.go` states the scope in the code:
  *"These are the only reasons Burrow surfaces as an Issue."*
- **Every other blocking condition produces `Available: false` and an empty `Issue`.** Unschedulable,
  `CrashLoopBackOff`, `CreateContainerConfigError`, `OOMKilled`, a volume that will not attach: the
  status surface reports that the app is not available and says nothing about why.
- **Jobs are not in the status surface at all.** Build, run, backup and restore Jobs are waited on
  inline against a deadline, and the wait's failure mode is [#352](https://github.com/burrow-cloud/burrow/issues/352).
- **Add-ons, ingress, certificates and the collectors have no status surface.** Whether the Postgres
  add-on is serving, whether the wildcard certificate is close to expiry, whether the log collector is
  collecting — none is answerable through Burrow.
- **There is no watch and no informer anywhere in the tree.** Nothing observes; everything asks.
- **The registry records intent, not outcome.** Postgres holds apps, releases, add-ons and backups —
  what Burrow was told to make exist. After the call that wrote the row returns, nothing updates it
  from the cluster.

### What breaks

**The failures people actually hit are invisible in the surface built to explain failures.** A failed
image pull is a real problem and worth its treatment, but it is not the common one. Apps crashloop
because of a bad config value; pods stay `Pending` because a cluster has no room or because a
toleration is missing. Both of those return "not available" with no reason, and the user does the one
thing the control plane exists to spare them: they reach for `kubectl describe`.

**A failure that heals leaves no trace, so every diagnosis starts at the least useful moment.**
Kubernetes Events expire — one hour by default. An app that crashlooped from 02:00 to 02:40 and
recovered is, by morning, indistinguishable from an app that has been up for a week. "Has this
happened before?" and "when did it start?" are the first two questions in any incident and Burrow can
answer neither, about anything.

**A failure with nobody watching is reported as a timeout.** This is [#352](https://github.com/burrow-cloud/burrow/issues/352)
exactly: a Job whose pod cannot start leaves `Failed` and `Succeeded` both zero, so the waiter burns
its full deadline and reports elapsed time rather than the unschedulable pod it was actually blocked
on. And the paths where nobody is watching are the ones designed that way — auto-deploy
([ADR-0052](0052-pull-based-passive-deploy.md)) and push-deploy exist so that nobody has to be
present.

**"Everything Burrow manages" is not a queryable set.** ADR-0073's four unreachable pod paths were
found by reading source, not by asking Burrow. There is no call that returns the objects the control
plane believes it owns together with what each is currently doing — which is why a backup that never
ran is indistinguishable from a backup still running.

**The most valuable diagnosis is the one `kubectl` structurally cannot make.** An app registered in
Burrow whose Deployment is *absent* — deleted by hand, evicted and never rescheduled, or never
created because a write failed — appears in `kubectl get deploy` as nothing at all. The evidence is
an absence, and only the side that knows what was intended can see it.

### What this record resolves

Whether Burrow observes the things it manages or merely answers questions about them; what counts as
a failure and in what vocabulary; where the record of one lives and for how long; and how it is read
without reaching for `kubectl`.

## Decision

### 1. Current state is derived; history is observed and stored

Two surfaces, because they have different sources of truth.

**Current state is read from the cluster on demand and never cached.** `burrow status` stays a live
read. A cached status is a second copy of a fact that changes without telling anyone, and it is
staleest during an incident — the one moment it would be consulted. It would let Burrow report a
healthy app that is, at that moment, down.

**History cannot be derived and therefore must be recorded.** Nothing reconstructs a crashloop that
ended, because the cluster does not keep the evidence. If Burrow is to answer "when did this start",
something has to have been watching at the time.

Everything below follows from that split, and the split is the reason this is one record rather than
two: widening the live surface and building the ledger are the same problem seen at two timescales.

### 2. Widen the Issue vocabulary to the failures users hit

`IssueReason` is already the right shape — a closed set of machine-usable reasons, paired with prose.
It has one member family. Add the other blocking classes: **unschedulable** (naming what could not be
satisfied — taint, resource, node selector), **`CrashLoopBackOff`** (with the exit code and the tail
of the previous container's log), **`CreateContainerConfigError`** (naming the missing config or
secret **key**), **`OOMKilled`** (naming the limit), **volume attach or bind failure**, and
**deadline exceeded** for Jobs.

The inclusion criterion already exists — it is stated in `imagepull.go` rather than in any record —
and it is promoted here unchanged, because it is what keeps the vocabulary from becoming noise: **a
reason is an Issue when it is blocking and human-fixable, and not when it resolves on its own.**
`ContainerCreating` and `PodInitializing` stay excluded on exactly that ground.

**This is separable from the rest of the record and should ship first.** It needs no new component, no
schema and no background loop, and it removes most of the reasons a user reaches for `kubectl` today.
The ledger is the larger and later half; the vocabulary is what makes the ledger worth writing rows
into, and it is useful on its own.

### 3. burrowd observes the workloads the registry says it owns

burrowd gains a watch over the objects Burrow manages, and this is the significant change: it is the
first thing in Burrow that runs **when nobody asked it to**.

**What it watches is bounded by the registry, not by a namespace or a label.** Burrow watches what it
believes it owns. That is what makes §6 expressible — a label selector can only find things that
exist, and the interesting failure is the thing that does not.

**It is read-only against the cluster, and it must stay that way** — see §9.

### 4. A failure is a transition, recorded once, with a lifetime

Not an event stream. **One row per (object, reason)**, carrying:

- `first_seen` — when this failure began. The answer to "when did it start".
- `last_seen` — when it was last observed. The answer to "is it still happening".
- `resolved_at` — null while active. The answer to "did it recover on its own".
- an **occurrence count** — a pod that has restarted four hundred times is one row that says four
  hundred, not four hundred rows.

An event stream has higher fidelity and is unreadable at exactly the moment it matters: an incident
produces thousands of rows describing one problem, and the person reading has to reconstruct the
transition themselves. A ledger answers the two questions people actually ask in a single row.

**Retention is bounded** — resolved failures are pruned after a fixed window. This lives in the
control plane's own database, and unbounded growth there is not a tidiness problem, it is an outage.

### 5. Many failures at once is the normal case, not the exception

Failures do not arrive one at a time, and a surface designed around the single-failure case is
useless during the incidents it was built for.

**Concurrency on one object is why the key is `(object, reason)` and not a status field.** A pod can
be OOM-killed *and* unschedulable; those are two rows with independent first-seen, resolution and
count, and either may end without the other. A single status per object would silently drop the
second, which is the failure this record is about. It also makes alternation legible: a pod flapping
between `CrashLoopBackOff` and `OOMKilled` produces two rows whose counts both climb, and **together
they name the bug** — the memory limit — where either alone points somewhere unhelpful.

**One cause routinely produces many rows, and that is the harder problem.** A taint added to a node
pool makes every backup Job, the add-on and the collector unschedulable in the same minute —
[ADR-0073](0073-placement-policy-reaches-every-authored-pod.md)'s scenario exactly. A database that
goes down crashloops every app that depends on it. The ledger will hold *N* rows for one problem, and
a listing that prints them flat is a wall of red at the moment someone can least afford to read one.

**So the listing groups by shared reason and orders oldest-first.** The same reason appearing across
many objects inside one window is the signature of a common cause, and the earliest `first_seen` in a
cascade is the likeliest thing to actually fix. A burst is itself information: thirty rows in one
minute is a cluster-level event, not thirty application problems, and should read as one.

**Grouping is presentation; the rows are the record.** Resolving the taint resolves each row on its
own schedule, so they stay separate underneath — and an agent reading the API gets the rows, not the
grouping, so it can correlate on its own terms rather than inheriting a human-facing heuristic.

**Burrow presents correlation and does not claim causation.** It will not assert that the Postgres
add-on being down *caused* an app's crashloop, however obvious that is to a reader, because it cannot
verify the dependency and **a confidently wrong root cause sends someone down the wrong path during
an incident** — worse than offering none. What it can do honestly is place the facts beside each
other: same reason, same window, same node, and the ordering that says which came first. This is §2's
restraint applied to presentation — report the blocking thing that is known, not the plausible thing
that is inferred.

**And forming a cause and a fix is the agent's job, not Burrow's.** This is a division of labour, not
a limitation Burrow should work around: **Burrow does not run a model, and must not acquire one to do
this.** Its half is to report every failure it observes, completely and in a shape something else can
reason over — a stable reason vocabulary, real timestamps, stable object identity, and no editorial.
The agent's half is to read twenty rows and conclude "the node pool was tainted at 02:14; remove the
taint or add the toleration" — synthesis across incomplete evidence, which is what a language model is
good at and a control plane is not.

Two things follow. **Completeness matters more than curation**: the ledger's job is to leave nothing
out, because a fact withheld to keep a listing tidy is a fact the agent cannot reason from, and the
agent is the consumer that benefits most from the boring rows. And **an inference engine inside
burrowd is out of scope** — it would be a worse version of something the caller already has, and
building it would undercut the reason the surface is structured for machines in the first place.

This is the same boundary the rest of the control plane draws. Burrow does not decide *what* to
deploy either; it makes deploying safe, legible and reversible, and leaves the judgement to whoever
is driving.

### 6. What Burrow intended, compared with what the cluster has

The observer diffs the registry against the cluster and records the discrepancies as failures in
their own right:

- a registered app with **no Deployment**,
- an add-on registered as installed with **no running pod**,
- a backup row still `pending` whose **Job no longer exists**,
- an exposure whose **Ingress is gone**, or whose certificate never issued.

None of these is visible from the cluster side, because in each case the evidence is an absence. This
is the diagnosis that genuinely requires a control plane, and it is the strongest single argument for
this record over "just read the events".

### 7. This is not the audit log, and they must not be merged

[ADR-0027](0027-audit-log.md)'s audit log records **what Burrow was asked to do and what it decided**
— the operation, the guardrail disposition, whether it was allowed, held, denied or executed. It is a
record of **intent**, it is append-only, and its value depends on being complete.

This records **what the cluster did afterwards** — outcome, mostly unrequested by anyone.

They are read together during an incident, and that is an argument for a shared clock and a shared
object identity, **not** a shared table. Merging them would subject a security-relevant append-only
record to the retention pruning §4 requires, and would make "the audit log is complete" false.

### 8. `burrow status` widens; a new listing answers the cluster-wide question

`burrow status <app>` stays what it is — one app, live — and gains the widened Issue plus a short
recent-failure history for that app.

The new thing is a **cluster-wide listing**: every object Burrow manages, its current failures by
default, with a `--since` window for history and a filter by kind. This is the command that replaces
"reach for `kubectl`", and it has to be cluster-wide to do that, because the question a user has at
that moment is "what is broken", not "is *this* broken".

The recommended name is `burrow failures`, with `--all` to include resolved ones. `burrow health`
reads better as a heading and worse as a thing that lists rows, and it invites a single
green/red verdict for a cluster, which is not a claim Burrow should make. The naming should be
revisited alongside the wider command-organization question rather than settled definitively here.

### 9. burrowd observes; it does not remediate

The observer is read-only against the cluster and takes no corrective action.

This is stated because the next step is obvious and wrong-by-default: once burrowd notices a
crashloop, restarting it looks like a small addition. It is not — it is a **mutation performed with
nobody present**, which is the exact shape every guardrail in Burrow exists to gate. And the failures
most tempting to remediate are the ones where the automatic remedy is usually incorrect: restarting an
OOM-killed pod without changing its limit reproduces the OOM, and rescheduling an unschedulable pod
does not create capacity.

If remediation is wanted, it is its own record, with its own guardrail dispositions.

**No secret value ever enters a ledger row.** A `CreateContainerConfigError` names a missing **key**,
which is safe and is the actionable part. A log tail captured for a crashloop is application output
and may contain anything, so it is bounded, and the record should say plainly that it is application
output rather than implying Burrow has sanitised it.

## Consequences

- **The most common failures become self-explaining**, and the reach for `kubectl describe` stops
  being the second step of every diagnosis. This is the reliability-legibility argument in concrete
  form: a control plane that cannot say why it is unhealthy is not operable.
- **"When did it start" becomes answerable**, for the first time, about anything.
- **burrowd becomes a process with a background loop and a persistent watch.** It was previously a
  request/response server, and it is not one any more. A restart interrupts observation.
- **A gap in the ledger is indistinguishable from health unless it is made visible.** If the observer
  was down from 02:00 to 03:00, an empty ledger for that hour reads as "nothing broke". The ledger
  must record its own observation coverage, so that a stale or interrupted observer is visible as
  such. A reliability surface that fails silently would be a particularly poor joke.
- **The ledger's most important consumer is an agent, not a person**, so the schema is designed for
  one: a closed reason vocabulary that can be branched on without parsing prose, real timestamps, and
  stable object identity. A surface tuned for a human reader — collapsed rows, summarised text, a
  tidy verdict — would be the wrong artifact, and §5 makes completeness the priority for that reason.
- **Burrow will look less clever than a tool that guesses a root cause**, and that is a deliberate
  trade. The comparison to make is not against a control plane that prints a diagnosis, but against
  that control plane's wrong diagnoses at 3am.
- **Grouping by shared reason is a heuristic and will sometimes be wrong.** Two unrelated apps
  crashlooping for unrelated bugs in the same minute will be shown together. That is an acceptable
  cost of never asserting causation — the grouping is a hint about where to look, not a claim, and
  the rows underneath stay individually addressable — but the surface must present it as such rather
  than as a diagnosis.
- **Something has to watch the watcher.** The observer's own failure is silent by exactly the argument
  this record makes about everything else, and the answer cannot be another observer.
- **Real API-server load.** A watch over every managed pod in a large cluster is not free, and the
  cost scales with what Burrow manages rather than with usage.
- **The ledger is per-cluster, because burrowd is.** A fleet-wide view is a later composition and this
  record does not attempt it.
- **§2 delivers most of the value for a fraction of the cost**, which is a real risk to §3 ever
  getting built. That is an acceptable outcome for the OSS project — the widened vocabulary is a
  genuine improvement standing alone — but it should be an explicit decision if it happens, not a
  drift.
- **The managed product will want this per tenant, and will want alerting on it.** This record stops
  at *listable* deliberately; alerting has a different failure model and belongs elsewhere.

## Rejected alternatives

- **Widen the Issue vocabulary and stop there — §2 alone.** Good value for a small cost, which is
  exactly why §2 is separable and sequenced first. Rejected as the *whole* answer because it cannot
  answer "when did this start" or "has this happened before": those need something to have been
  watching, and no amount of on-demand polling produces history retroactively.
- **Kubernetes Events as the record.** They already exist, are already structured, and need no
  storage. Rejected because they expire (one hour by default), so the history is not there to read;
  because reading them is precisely the "reach for `kubectl`" this record removes; and because they
  cannot express §6 — an Event is emitted by a controller about an object that exists, and the
  interesting failure is an object that does not.
- **Ship metrics and alerting instead**, or serve failures through
  [ADR-0026](0026-observability-query-adapters.md)'s query seam — the collectors and the seam already
  exist. Rejected as an answer to *this* question. Metrics answer "how much, over time"; "which of
  the things I own is broken, and why" is a state question, and expressing it as metrics yields an
  alert rule per failure class and a dashboard dependency for something a CLI should answer in one
  line. The seam is the wrong home for a further reason: it queries **backends the user connected**,
  about the **app's** telemetry, and may not be installed at all — while this is the control plane's
  own knowledge about its own objects, and must be available on an install with no observability
  add-on. They compose well — the ledger is a good metric source — but neither replaces the other.
- **Cache current state in Postgres and serve `status` from it.** Fast, and one source for both
  surfaces. Rejected in §1: the cache is most stale during an incident, and a control plane that
  reports a healthy app that is currently down is worse than one that is merely slow.
- **An append-only event stream instead of a transition ledger.** Higher fidelity; flap counts become
  derivable rather than stored. Rejected in §4: unreadable at the moment it matters, and it turns
  retention into a volume problem inside the control plane's database.
- **Put failures in the audit log.** One timeline, one table, one query. Rejected in §7: it would
  subject an append-only security record to retention pruning and break the completeness property
  that gives the audit log its value.
- **Infer root cause from a dependency graph** — Burrow knows which apps use which add-on, so it
  could say "these six crashloops are the Postgres outage". Genuinely useful when right, and the
  information is partly there. Rejected in §5: the graph is incomplete (it knows registered add-on
  bindings, not that app A calls app B), so the inference would be confident and sometimes wrong, and
  an incident is the worst possible place to be led somewhere plausible. Grouping by shared reason
  and time gets most of the benefit while only ever asserting things Burrow observed. The deeper
  reason is §5's: synthesising a cause from partial evidence is the agent's half of the work, and
  Burrow does not run a model. A dependency graph good enough to support causation is its own record.
- **Observe and remediate.** Restart the crashlooper; reschedule the Pending pod. Rejected in §9 as a
  separate decision with a much higher bar — it is a mutation with nobody present, and the remedies
  are usually wrong for the failures that most invite them.
- **A Kubernetes controller with CRDs, storing status on custom resources.** Idiomatic, and the
  cluster becomes the source of truth. Rejected because Burrow deliberately keeps control-plane state
  in Postgres rather than in the cluster; because CRD status subresources are current-state only, so
  the history problem is unsolved; and because it would put the record of a failure inside the system
  that is failing.
