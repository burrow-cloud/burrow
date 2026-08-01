# ADR-0081: A Postgres instance may have a standby, and apps get a read address either way

## Status

🟡 Proposed

## TL;DR

Every Burrow Postgres instance is one pod. Nothing can ask for a second, so nothing can survive
losing the first without a reschedule.

- **The instance count is settable at install**, defaulting to **one**.
- **A second instance is both things at once.** CloudNativePG runs it as a hot standby: it takes over
  when the primary dies *and* it serves reads. There is no separate replica feature to build.
- **Apps always get a read address**, whether there is one instance or two. Written once, it is
  correct at either count, so nothing in an app changes when a standby is added.
- **Raising it is an operator's call, not an agent's**, because it provisions hardware.

Extends [ADR-0066](0066-postgres-on-cloudnativepg.md), which put the add-on on CloudNativePG and
hard-coded a single instance. Prerequisite for cloud ADR-0030's move of `burrowd-cloud`'s database
onto the add-on. Supersedes nothing.

## Context

`controlplane/kube/cnpg_cluster.go` writes `"instances": int64(1)`, and nothing exposes it. Every
database Burrow manages is a single pod, whatever it holds.

CloudNativePG recovers a lost primary by rescheduling the pod and reattaching its volume, so this is
not a data-loss story. It is an **outage of minutes rather than a failover of seconds** — and for a
database that is the outage of everything depending on it.

**The case that forces the question** is cloud ADR-0030, which moves `burrowd-cloud`'s own database —
every tenant, credential hash, audit row and usage record — onto this add-on. That database is
currently a hand-applied `Cluster` with `instances: 2`. ADR-0030 argued the move was safe because it
is *"the same engine with Burrow's lifecycle around it"*, which is true of the engine and not of the
topology: as things stand the move would take the platform's own registry from two instances to one.

There is a second thing worth stating, because assuming otherwise leads to designing a feature that
does not need building. **A CloudNativePG standby is not a passive copy.** The operator runs it as a
hot standby and publishes three services for one cluster:

| Service | Selects |
| --- | --- |
| `<cluster>-rw` | The primary. Follows it across a failover |
| `<cluster>-ro` | Standbys only |
| `<cluster>-r` | Any instance |

So a second instance delivers failover **and** a readable replica together. What does not come with
it is an app knowing how to reach the replica — which is a connection string, not a feature.

## Decision

### 1. The instance count is set at install, and defaults to one

`burrow addon install postgres --instances <n>`, defaulting to **1**.

**One is right for the common case**, and the reason is cost rather than caution: an instance is a
pod and a persistent volume, and it is the most expensive thing an add-on provisions. Doubling that
for every app on a free tier would be a poor default paid by everyone to benefit few.

The number is validated at the point of naming, not passed through — CloudNativePG accepts values
that make no sense for the shape Burrow provisions, and a database that comes up wrong is worse than
a refusal.

**Changing it on an existing instance is not built here.** CloudNativePG scales a `Cluster` up and
down live, so this is a capability Burrow declines to expose yet rather than one that does not exist.
It is deliberately left out because the interesting part is not the scale-up — it is what a scale-down
does to a standby that a read address is currently pointing at.

### 2. Apps get a read address at any instance count

`attach` writes a second connection string beside `DATABASE_URL`, pointing at the cluster's
**`-r`** service — the one that selects *any* instance.

**Not `-ro`, and the difference is the whole point.** `-ro` selects standbys only, so at one instance
it resolves to nothing and an app using it fails. `-r` is the primary at one instance and spreads
across both at two. So the address is **correct at either count**, an app is written once, and adding
a standby later changes nothing in the app.

The cost of that choice, stated: `-r` does not *guarantee* a read misses the primary. An application
that needs reads strictly off a standby is not served by this, and would need `-ro` and the
instance-count awareness that comes with it. That is a narrower need than "let me spread reads", and
this record serves the wider one.

**Always written, even at one instance.** An address that appears only above some replica count is an
address nobody writes code against, because the code has to handle its absence anyway.

### 3. Raising it is operator-only

`--instances` is absent from `burrow-agent` and reported by `guard` as a capability the agent does
not have ([ADR-0065](0065-what-belongs-on-the-agent-surface.md) tier 1).

The criterion is the blast radius of the effect, and this one **provisions hardware**. An agent that
can deploy an image is spending nothing; an agent that can double a database's footprint is spending
money on infrastructure nobody approved. That is a different kind of act, and the fact that it is
easily reversed does not make the spend reversible.

The agent still reports it: a refusal names the flag and that a human runs it, per ADR-0065 §7's rule
that an absent capability which is legible is a refusal the agent can relay, where a dead end pushes
it off the control channel entirely.

### 4. Backups are unchanged, and this is not an alternative to them

A standby is a live copy that faithfully replicates a mistake. `DROP TABLE` on the primary is
`DROP TABLE` on the standby, in the time it takes to stream.

So [ADR-0063](0063-object-storage-provider.md)'s backups to object storage are unaffected by this
record and remain the only thing that recovers from a destructive change. **A standby is availability;
a backup is recoverability**, and they fail apart. Anything that presents a replica as a reason to
relax about backups is wrong, and the docs must not.

## Consequences

**Cloud ADR-0030's move stops being a downgrade.** `burrowd-cloud`'s database can move onto the add-on
at the instance count it already runs, and the platform's own registry gets Burrow's backup path
without losing its standby.

**Cost becomes something an operator chooses per instance**, and can get wrong in both directions —
paying double for an app that did not need it, or discovering at the worst moment that the database
holding everything was provisioned at one. Neither is preventable by design; the default handles the
common case and the flag handles the rest.

**A second instance is a second thing that can be unhealthy.** `burrow addon list` and the ADR-0074
ledger surface an instance's readiness; with two, "the database is fine" becomes a question with a
more interesting answer — a cluster serving from its primary with a standby that has fallen behind is
degraded in a way one instance cannot be, and nothing here surfaces replication lag.

**Scale-down remains unbuilt and is now the sharper question.** With a read address always present,
removing the standby it may be serving is not simply the reverse of adding one. Better to leave it
out than to ship a scale-down whose effect on in-flight reads nobody has thought about.

## Rejected alternatives

**Leaving it at one and accepting the downgrade for cloud ADR-0030.** Honest, and it means the
platform's own registry — every account and credential hash — runs at one instance so that a
configuration field can stay unwritten. The field is small; the thing it protects is not.

**An operational limit under [ADR-0068](0068-operational-limits-are-configuration.md) instead of an
install flag.** Those are *bounds a human sets* — ceilings on what may be asked for. An instance count
is not a bound, it is a property of one instance, and two apps in one environment can legitimately
want different values. Modelling it as a limit would make it cluster- or environment-wide and force
the wrong answer on somebody.

**A tier that implies it** — "production instances get two". Attractive for the managed product and
premature for the open-source one, where there is no tier concept and an operator's needs are their
own. The managed product can build tiers on top of this field; the field should not presuppose them.

**Exposing `-ro` as the read address.** Strictly better for spreading reads off the primary, and it
resolves to nothing at one instance — so every app using it would need to know the instance count,
which is the coupling §2 exists to remove. A later record can add it for the narrower need without
disturbing this one.

**Setting the read address only when there is a standby.** Saves a variable and costs the property
that makes it useful: an app cannot rely on an address that may or may not be there, so it branches,
and the branch is exactly what a single always-correct address removes.
