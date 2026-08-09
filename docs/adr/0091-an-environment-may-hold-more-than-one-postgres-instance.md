# ADR-0091: An environment may hold more than one Postgres instance

## Status

🟡 Proposed

## TL;DR

[ADR-0067](0067-one-database-instance-per-environment.md) §1 puts one Postgres instance in each
environment. Right default, wrong maximum. Make it a default.

- **Second instance is asked for by name.** `addon install postgres --instance analytics`. No flag,
  no change: the environment's one instance, exactly as today.
- **Instance names stop being derived.** `AddonInstanceName(type, env)` has no answer for a second
  one, and a third component brings back the ambiguity [cloud ADR-0029](https://github.com/burrow-cloud/cloud/blob/main/docs/adr/0029-database-names-use-a-generated-id.md)
  removed: `burrow-postgres-staging-x` is both `(staging, x)` and `(staging-x, default)`.
- **The registry is the mapping.** Each environment's first instance keeps the name it already has,
  so nothing moves. Every later one gets a generated ID, and the operator never types it.
- **Attach picks one.** `--instance` selects it; without it, the environment's default. An app may
  hold several attachments, and a second one must name its own variable — `DATABASE_URL` is taken,
  and Burrow refuses rather than inventing `DATABASE_URL_2`.
- **Per-instance, not per-environment**, for everything that names an instance: the backup claim,
  `addon config`, `addon sql`, the provisioning seam.
- **Provisioning is the operator's; attaching is the agent's.** An instance is a pod and a volume.
- **ADR-0067's fix survives whole.** An instance still belongs to exactly one environment.

Supersedes [ADR-0067](0067-one-database-instance-per-environment.md) §1's ceiling only; §1's
per-environment isolation, and §§2–5, stand unchanged. Decides the instance selector
[ADR-0082](0082-an-addon-instance-is-configured-after-it-exists.md) §1 deferred, and delivers the
opt-in [ADR-0031](0031-postgres-addon.md) deferred and ADR-0067 then made unreachable. Answers the
same naming question as [cloud ADR-0029](https://github.com/burrow-cloud/cloud/blob/main/docs/adr/0029-database-names-use-a-generated-id.md),
the same way.

## Context

### What exists today

- **Exactly one Postgres instance per environment.** `AddonInstanceName(type, env)` is a pure
  function of those two values (`controlplane/addons.go`), and twenty-two non-test call sites derive
  the instance from it — install, remove, attach, detach, `addon config`, `addon sql`, the physical
  backup and restore, the dependency check, both CLI binaries, and the fakes.
- **The registry stores the instance under that name.** `addons.name` is the primary key
  (migration `00004`), and the environment is a column beside it (`00016`), so the row records the
  environment rather than being found by it.
- **An attachment is keyed by `(addon, app, environment)`** (`addon_attachments`, migration
  `00029`), so one app has at most one database per environment.
- **The variable is already a choice.** `addon attach postgres web --as PG_DSN` names the variable
  the connection string is written under, and a name something else already holds is refused with a
  message naming the conflict.
- **Backups are per environment.** `BackupVolumeName(type, env)` gives each environment one claim.
- **A guardrail can already name an instance.** [ADR-0085](0085-a-guardrail-can-name-the-app-it-guards.md)
  scopes a disposition by the instance name, and `addon install`, `addon remove` and `addon detach`
  all evaluate against it.

### What is foreclosed

Two shapes have no expression, and neither is exotic.

**Several instances in one environment.** A tenant running six services who wants a database server
per service — separate blast radius, separate resource ceilings, separately upgradable — gets one
server with six databases in it. [ADR-0031](0031-postgres-addon.md) §Rejected declined a dedicated
instance per app *as a default* and deferred it "to an explicit opt-in for users who need it". That
opt-in was never built, and ADR-0067 §1 then made it unreachable, because it fixed the collision it
was written for with a sentence that also set a maximum.

**An app attached to more than one instance.** An app that legitimately reads from two databases
cannot say so. The reason the issue gives for this — that `attach` writes a single `DATABASE_URL` —
is no longer the blocker: the variable is a choice. What blocks it is the attachment key, which has
room for one row per app per environment.

### Why ADR-0067 landed where it did

For a real bug, and its fix is not what this record touches. `EnsureAppDatabase(ctx, app)` carried no
environment, so `web` in staging and `web` in production resolved to one database — and because
provisioning is idempotent, the second attach adopted the first one's data and rotated its password
rather than failing. Staging would write to production.

One instance per environment closed that. Making it also a maximum was a consequence of the sentence
used to express it rather than a decision anyone weighed, and this record separates the two.

### What this record resolves

Four things, and they are the four the ceiling was hiding:

1. **How a second instance is named**, now that the name cannot be derived from `(type,
   environment)`.
2. **What `attach` means** when several instances exist — which one is the default, how another is
   asked for, and what variable a second attachment writes.
3. **Which of today's per-environment names are really per-instance**, so relaxing the ceiling does
   not reintroduce ADR-0067's collision one level down.
4. **Who may create one**, given that an instance is a pod and a volume.

### Why the naming question is the hard one

`AddonInstanceName` composes `burrow-<type>[-<env>]`, and the obvious extension is
`burrow-<type>-<env>-<label>`. It does not survive: an environment name and an instance label are
both DNS-1123 labels drawn from the same alphabet, so `burrow-postgres-staging-x` is the instance
`x` in environment `staging` and also the default instance of an environment called `staging-x`.
That is exactly the composed-name ambiguity cloud ADR-0029 removed from the managed product, where
its consequence was one tenant reaching another's database.

The separator trick `BackupVolumeName` uses — a dot, which an instance name cannot contain — is not
available here. A Postgres instance is a CloudNativePG `Cluster`, and the operator composes its
Services as `<cluster>-rw`, `<cluster>-ro` and `<cluster>-r`. A Service name is a DNS-1123 *label*,
which admits no dot, so the instance name cannot carry one either.

## Decision

### 1. The environment's one instance stays the default, and a second is asked for by name

```
burrow addon install postgres                          # the environment's instance, as today
burrow addon install postgres --instance analytics     # a second one beside it
```

Without `--instance`, every add-on command means what it means today: the environment's default
instance. A single-instance operator never types the flag, and no existing command changes shape.

**The shared instance stays the default because an instance is a pod and a volume**, and most apps
do not need their own. ADR-0031's density argument is unchanged by this record; what changes is that
the argument is now a default with an exit rather than a wall.

**`--instance` names a label, not a resource.** It is what a person types and what every listing
shows. What the cluster calls the instance is §2's business, and the two are deliberately not the
same value.

### 2. An instance name is looked up, not derived

The registry row is the mapping between an instance's label and its name in the cluster, and it is
the only mapping. Nothing recovers an environment or a label by splitting a name.

- **Each environment's first instance keeps the name it has today** — `burrow-postgres` for the
  default environment, `burrow-postgres-staging` for `staging`. The name is written into the row at
  creation, so an install that predates this record gains a row describing the instance, the volume
  and the superuser Secret it already has. Nothing moves, which is ADR-0067 §3's exemption surviving
  intact.
- **Every additional instance gets a short generated ID** from `[a-z0-9]`, named
  `burrow-postgres-<id>`. The ID is generated, not derived: not a hash of the label, not an encoding
  of the environment. Uniqueness is enforced by the registry rather than assumed from entropy.
- **`AddonInstanceName` stops being a lookup and becomes a name generator** used at creation time
  only. Every consumer resolves the instance from the registry.

**The alphabet is forced rather than chosen**, for cloud ADR-0029's reason: the same ID has to be
legal in a Kubernetes object name and in whatever else Burrow composes from it, and lowercase
alphanumeric is the intersection.

**This is the clause that costs something, and it is worth naming plainly.** Twenty-two call sites
derive a name from a pure function today, and two of them are in the CLI binaries, which compose the
name client-side to put it in a message and have no registry handle at all. Turning a derivation
into a lookup is the bulk of the implementation and a prerequisite for every other section here.

**It is the same answer the managed product already reached**, which is the point. The OSS naming
has the shape cloud ADR-0029 diagnosed, and two repositories answering one question differently is a
cost paid at every boundary between them.

### 3. Attach names an instance, and a second attachment names its own variable

```
burrow addon attach postgres web                          # the environment's default instance
burrow addon attach postgres web --instance analytics --as ANALYTICS_URL
```

- **Without `--instance`, attach uses the environment's default instance**, so today's command means
  today's thing.
- **An app may hold several attachments.** The attachment record is keyed by `(addon, app,
  environment, instance)` rather than by `(addon, app, environment)`.
- **A second attachment must name a free variable.** The first attachment defaults to
  `DATABASE_URL`; a second cannot, because the name is taken, and the refusal that says so by name
  already exists. Burrow does not fall back to a derived second name.
- **Detach names the instance too**, and detaching one attachment leaves the other's variable, role
  and database untouched.

**Burrow does not invent `DATABASE_URL_2`.** A generated second name is a name the application does
not read, so the attach would report success and the app would find nothing — the failure arriving
later, at a connection, rather than at the command that caused it. A refusal naming the conflict is
the more useful answer, and it is what the same path already does when a requested variable is
occupied.

### 4. What was per environment and is really per instance moves

Relaxing the ceiling without this would reproduce ADR-0067's collision one level down.

- **The backup claim is per instance.** `BackupVolumeName(type, env)` gives one claim per
  environment, so two instances in one environment both holding a database called `web` would write
  the same path on one disk — the registry rows saying which instance each dump came from while
  nothing on the volume did. That is issue #339's shape with the environment held constant.
- **`addon config` takes the instance.** ADR-0082 §1 already decided the selector is a **flag**
  rather than a positional, because a positional would be ambiguous against the setting subcommands,
  and declined to add it while it could only ever take one value. This is the record that gives it
  more than one, so the flag lands here.
- **The provisioning seam takes the instance**, alongside the environment it already takes:
  `DatabaseProvisioner`, `AppDatabaseLister` and `DatabaseQuerier`. The environment stays required —
  it is what says which namespace an app's Secret is in and which environment's data this is — and
  the instance is what selects the server. Neither is optional, for ADR-0067 §1's reason: a
  signature that can omit it is a signature that will omit it.
- **Guardrails need no change.** ADR-0085 already scopes a disposition by instance name, so denying
  `addon.remove` for one instance while leaving another alone already works and already means the
  right thing.

### 5. Creating an instance is the operator's; attaching to one is the agent's

**`addon install --instance` is operator-only**, absent from `burrow-agent`, which is where `addon
install` already sits. It provisions hardware: a pod and a volume nobody approved, and the ease of
removing it does not make the spend reversible. That is [ADR-0065](0065-what-belongs-on-the-agent-surface.md)'s
criterion and the same answer ADR-0082 §4 reaches for a shape change.

**`addon attach --instance` stays on the agent surface.** Attaching to an instance that already
exists provisions nothing and destroys nothing, which is why attach is ungated today; naming which
existing instance to attach to does not change either property.

The refusal names the verb and that a person runs it (ADR-0065 §7). An agent that can say *"this
environment has one Postgres instance, and a person can add another with `burrow addon install
postgres --instance analytics`"* is doing the useful half of the work.

### 6. An instance still belongs to exactly one environment

This record relaxes how many instances an environment may hold. It relaxes nothing about what an
instance isolates.

- An instance belongs to **one** environment. Sharing one across environments stays unsupported
  (ADR-0067 §5), so nothing has to defend against it and there is still only one isolation model.
- A database is reachable only from its own environment's instance, which is ADR-0067 §1's actual
  fix and is untouched.
- Databases keep their simple names. Isolation comes from the instance, not from a naming convention
  inside a shared server — the same sentence, now true of more instances.

## Consequences

- **A microservices layout gets separate blast radius**, and a failure-domain need has an expression
  for the first time since ADR-0031 deferred it.
- **The managed cloud's tier model gets its seam.** A shared instance on a free tier and a dedicated
  one as a paid upgrade is the same question this record answers — which instance does this attach
  point at — so the two products are not solving it twice.
- **Instance names in the cluster become opaque for every instance after the first**, and this is
  the real cost. Somebody reading `kubectl get cluster` sees `burrow-postgres-k9f3m2` and cannot
  tell what it is for without the registry. It is cloud ADR-0029's debt, and it is repaid the same
  way: every surface that shows an instance resolves it back to its label, and `addon list` is where
  a person holding a name from a log finds out what it is.
- **A pure function becomes a lookup, in twenty-two places.** Two of them are in the CLI binaries and
  will have to ask burrowd rather than composing the name themselves. Nothing about this is subtle,
  and all of it is a prerequisite.
- **A second instance is a second pod and a second volume**, and nothing creates one on its own.
  ADR-0082 §5's refusal to schedule a shape change applies unchanged to creating an instance.
- **The two repositories should land the same shape.** Cloud ADR-0029 is accepted and its generated
  ID is not built — `internal/tenantdb/names.go` still composes `t-<tenant>-<app>` — so whichever
  implementation goes first sets the pattern the other follows.
- **The default is unchanged for everybody who does not want this.** No existing command grows a
  required flag, no name in an existing install moves, and an operator who never types `--instance`
  cannot tell this record was accepted.

## Rejected alternatives

**Keep the ceiling.** The status quo, and its defence is real: one instance per environment is the
right default, it is cheap, and nobody has hit the wall yet. Rejected because the wall is not a
default — a tenant who needs a database per service has no way to say so at any price, the opt-in
ADR-0031 promised stays unbuilt, and a paid dedicated-instance tier in the managed product has no
seam to hang on. A maximum that nobody decided is worse than either answer chosen deliberately.

**A legible composed name with a globally reserved label** — `burrow-postgres-analytics`, with the
registry refusing a duplicate label anywhere and refusing an environment named `analytics`, the way
`backups` is already reserved. Genuinely attractive: the name says what the instance is for, which
is exactly what the generated ID gives up, and the reservation machinery exists. Rejected because it
makes creating an environment fail on account of an unrelated add-on instance, couples two naming
spaces that have no reason to know about each other, and keeps the property that a name has parts
which can be got wrong. The ambiguity would be prevented by a check rather than by construction, and
a check is a thing that can be forgotten on the next path that composes a name.

**Environment-qualified database names on one shared instance** — several logical databases in one
server instead of several servers. ADR-0067 §Rejected already declined this and its reasoning is
unchanged: it isolates the data and not the server, so a runaway table fills the shared volume, a
load test exhausts the shared `max_connections`, and a major-version upgrade cannot be rehearsed at
all. It is also a different question from this one. This record adds instances; it does not relax
what an instance isolates.

**An instance per app, by default.** The strongest isolation available, and rejected twice already —
by ADR-0031 on density and by ADR-0067 §Rejected for the same reason. Nothing here changes it: apps
are many, and the per-app boundary is a database. What changes is that "I want one anyway" is now
sayable.

**Derive the second variable name** — `DATABASE_URL_2`, or `<INSTANCE>_DATABASE_URL`. Saves the
operator a flag and hands the application a name nobody told it to read. The attach reports success,
the app finds nothing, and the failure surfaces at a connection instead of at the command. The
existing refusal that names the occupied variable is a better answer and is already built.

**Let the agent create an instance when an app needs one.** The natural extension of an agent that
can attach, and it is ADR-0065's line: attaching spends nothing, and provisioning spends money on
infrastructure nobody approved. Naming the command for a person to run is the useful half, and it is
the whole of what the agent gets.
