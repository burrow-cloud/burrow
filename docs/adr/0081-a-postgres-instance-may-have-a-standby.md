# ADR-0081: A Postgres instance may have a standby

## Status

✅ Accepted

## TL;DR

A **Postgres instance** is one Postgres server — one per environment
([ADR-0067](0067-one-database-instance-per-environment.md)), holding a database per attached app.
Today it always runs as a **single pod**, so losing that pod takes the instance down until it is
rescheduled.

**This record is about the shape of one instance, not about how many exist.** Running a second,
independent Postgres — one per service, say — is a different axis, deferred by
[ADR-0031](0031-postgres-addon.md) and untouched here.

- **An instance may have standbys.** It is always created without them; adding one is
  [ADR-0082](0082-an-addon-instance-can-be-rescaled.md)'s `burrow addon config`.
- **A standby is both things at once.** CloudNativePG runs it as a hot standby: it takes over
  when the primary dies *and* it serves reads. There is no separate replica feature to build.
- **A read address appears only when a standby exists**, and points at standbys only. Adding one
  restarts the attached apps, which costs nothing: using a replica means changing the app's code to
  send reads there, and that is a deploy anyway.
- **Raising it is an operator's call, not an agent's**, because it provisions hardware.

Extends [ADR-0066](0066-postgres-on-cloudnativepg.md), which put the add-on on CloudNativePG and
hard-coded a single instance. Prerequisite for cloud ADR-0030's move of `burrowd-cloud`'s database
onto the add-on. Supersedes nothing.

## Context

**Two things are called an instance, and only one of them is this record's subject.**

- A **Postgres instance** — the add-on sense, the word `AddonInstanceName` already uses — is a
  Postgres server: one CloudNativePG `Cluster`, one per environment
  ([ADR-0067](0067-one-database-instance-per-environment.md)), holding a database and a login role
  per attached app ([ADR-0031](0031-postgres-addon.md)).
- CloudNativePG's own `spec.instances` counts the **pods inside** one such cluster — a primary and
  its standbys.

This record settles the second and says nothing about the first. To avoid the collision it uses
**standby** for the replica and reserves **instance** for the server, matching ADR-0067 and the
codebase's own `AddonInstanceName`.

`controlplane/kube/cnpg_cluster.go` writes `"instances": int64(1)`, and nothing exposes it. So every
Postgres server Burrow manages runs as a single pod, whatever it holds.

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

So a standby delivers failover **and** a readable replica together. What does not come with
it is an app knowing how to reach the replica — which is a connection string, not a feature.

## Decision

### 1. An instance is created without standbys, always

`burrow addon install postgres` takes no shape flag. It creates a Postgres instance with a primary
and nothing else, which is what it does today.

**There is no `--standbys` at install**, and leaving it out is the decision rather than an omission.
Somebody installing Postgres is standing up a database for an app that does not exist yet, with no
traffic and no way to know whether it will ever matter. Asking them then produces the answer "no"
from everyone, including the people who will need one — so the flag would be answered wrong by
exactly the population it exists for, and would sit in the help text of the most-run command teaching
a question nobody can answer at that moment.

**Install creates an instance. Changing one is a different verb**
([ADR-0082](0082-an-addon-instance-can-be-rescaled.md)), reached when the need is real and the
answer is knowable.

The word standby is used throughout rather than replica or instance: CloudNativePG runs it hot, so
replica understates it, and instance already means the server.

### 2. A read address exists only when there is a standby to read from

`attach` writes a second connection string beside `DATABASE_URL` **only when the instance has a
standby**, pointing at the cluster's **`-ro`** service — the one that selects standbys and nothing
else.

**`-ro` rather than `-r`, and the conditional is what makes it the right choice.** `-r` selects any
instance, so a read may land on the primary; `-ro` guarantees it does not, which is what splitting
reads is for. `-ro` resolves to nothing on a standby-less instance — which would be a trap if the address
were always present, and is not one when the address only exists where it works.

**Adding a standby restarts the attached apps**, so they pick the new variable up. That is acceptable
because of *when* it happens: an app does not benefit from a replica by existing near one. Somebody
has to change the application's code to route read-only queries down the second connection, and that
is a deploy. A restart at the moment a standby is provisioned costs nothing that the code change was
not already going to cost.

**An address that is always there would be worse.** It reads as a thing to use, so a developer wires
reads to it, and without a standby it is either the primary wearing a second name — a variable that
does nothing, quietly — or it points at no endpoint at all. Neither teaches the truth, which is that a
read replica is something you provision and then write code for.

### 3. Adding a standby is an operator's call

Adding a standby **provisions hardware**, so it sits where [ADR-0082](0082-an-addon-instance-can-be-rescaled.md)
§4 puts it: absent from `burrow-agent`, reported by `guard`, and refused with a message naming the
command a human runs.

The criterion is the blast radius of the effect. An agent that deploys an image spends nothing; an
agent that doubles a database's footprint spends money on infrastructure nobody approved, and the
ease of reversing the change does not make the spend reversible.

### 4. Backups are unchanged, and this is not an alternative to them

A standby is a live copy that faithfully replicates a mistake. `DROP TABLE` on the primary is
`DROP TABLE` on the standby, in the time it takes to stream.

So [ADR-0063](0063-object-storage-provider.md)'s backups to object storage are unaffected by this
record and remain the only thing that recovers from a destructive change. **A standby is availability;
a backup is recoverability**, and they fail apart. Anything that presents a replica as a reason to
relax about backups is wrong, and the docs must not.

## Consequences

**Cloud ADR-0030's move stops being a downgrade.** `burrowd-cloud`'s database can move onto the add-on
with the standby it already has, and the platform's own registry gets Burrow's backup path
without losing its standby.

**Cost becomes something an operator chooses per instance**, when they choose it. Nobody pays for a
standby they did not ask for, and nobody is asked at a moment when the answer is unknowable.

**A standby is a second thing that can be unhealthy.** `burrow addon list` and the ADR-0074
ledger surface an instance's readiness; with two, "the database is fine" becomes a question with a
more interesting answer — a cluster serving from its primary with a standby that has fallen behind is
degraded in a way one instance cannot be, and nothing here surfaces replication lag.

**§2 hands [ADR-0082](0082-an-addon-instance-can-be-rescaled.md) its hardest case.** Removing a standby
removes the only endpoint the read address resolves to, so an app configured to use it breaks — not
at the moment of the scale-down, but at its next query down that connection. Whether the address is
withdrawn (and the apps restarted again) or left pointing at nothing is a real decision, and it is
that record's to make.

## Rejected alternatives

**Leaving every instance standby-less permanently and accepting the downgrade for cloud ADR-0030.**
Honest, and it means the platform's own registry — every account and credential hash — runs as a
single pod so that a capability can stay unbuilt. The field is small; the thing it protects is not.

**An operational limit under [ADR-0068](0068-operational-limits-are-configuration.md).** Those are *bounds a human sets* — ceilings on what may be asked for. A standby count
is not a bound, it is a property of one instance, and two environments can legitimately want
different values. Modelling it as a limit would make it cluster- or environment-wide and force
the wrong answer on somebody.

**A tier that implies it** — "production instances get a standby". Attractive for the managed product
and premature for the open-source one, where there is no tier concept and an operator's needs are
their own. The managed product can build tiers on top of this capability; the capability should not
presuppose them.

**A read address present whether or not a standby exists**, pointing at `-r` so it resolves to the primary
when there is no standby. It buys one property — an app written once, unchanged when a standby
appears — and pays for it twice. The variable reads as a thing to use while doing nothing on a
standby-less instance, and it forces `-r` over `-ro`, so even with a standby a read may land on the primary and
the feature does not fully work.

The property it buys is also worth less than it looks. An app does not use a replica by having its
address; somebody has to route read-only queries to it, which is a code change and a deploy. Since a
restart is already being paid at that moment, avoiding one at provisioning time buys nothing.
