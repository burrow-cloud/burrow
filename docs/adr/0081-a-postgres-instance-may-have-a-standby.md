# ADR-0081: A Postgres instance may have a standby

## Status

🟡 Proposed

## TL;DR

Every Burrow Postgres instance is one pod. Nothing can ask for a second, so nothing can survive
losing the first without a reschedule.

- **The instance count is settable at install**, defaulting to **one**.
- **A second instance is both things at once.** CloudNativePG runs it as a hot standby: it takes over
  when the primary dies *and* it serves reads. There is no separate replica feature to build.
- **A read address appears only when a standby exists**, and points at standbys only. Adding one
  restarts the attached apps, which costs nothing: using a replica means changing the app's code to
  send reads there, and that is a deploy anyway.
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

### 2. A read address exists only when there is a standby to read from

`attach` writes a second connection string beside `DATABASE_URL` **only when the instance has a
standby**, pointing at the cluster's **`-ro`** service — the one that selects standbys and nothing
else.

**`-ro` rather than `-r`, and the conditional is what makes it the right choice.** `-r` selects any
instance, so a read may land on the primary; `-ro` guarantees it does not, which is what splitting
reads is for. `-ro` resolves to nothing at a single instance — which would be a trap if the address
were always present, and is not one when the address only exists where it works.

**Adding a standby restarts the attached apps**, so they pick the new variable up. That is acceptable
because of *when* it happens: an app does not benefit from a replica by existing near one. Somebody
has to change the application's code to route read-only queries down the second connection, and that
is a deploy. A restart at the moment a standby is provisioned costs nothing that the code change was
not already going to cost.

**An address that is always there would be worse.** It reads as a thing to use, so a developer wires
reads to it, and at one instance it is either the primary wearing a second name — a variable that
does nothing, quietly — or it points at no endpoint at all. Neither teaches the truth, which is that a
read replica is something you provision and then write code for.

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

**Scale-down remains unbuilt, and §2 makes it sharper rather than easier.** Removing a standby
removes the only endpoint the read address resolves to, so an app configured to use it breaks — not
at the moment of the scale-down, but at its next query down that connection. Whatever eventually
builds it has to decide whether the address is withdrawn (and the apps restarted again) or left
pointing at nothing. Better unbuilt than shipped without that answer.

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

**A read address present at every instance count**, pointing at `-r` so it resolves to the primary
when there is no standby. It buys one property — an app written once, unchanged when a standby
appears — and pays for it twice. The variable reads as a thing to use while doing nothing at a single
instance, and it forces `-r` over `-ro`, so even at two instances a read may land on the primary and
the feature does not fully work.

The property it buys is also worth less than it looks. An app does not use a replica by having its
address; somebody has to route read-only queries to it, which is a code change and a deploy. Since a
restart is already being paid at that moment, avoiding one at provisioning time buys nothing.
