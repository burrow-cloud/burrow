# ADR-0064: Removing an add-on keeps its data; destroying the volume is a separate, operator-only act

## Status

🟡 Proposed

## TL;DR

`burrow addon remove postgres` deletes the add-on's disk. For the Postgres add-on that is **every
attached app's database**, gone, with no way back — and nothing in the command's name or its
confirmation prompt says so. Someone removing the add-on intending to reinstall it cleanly loses all
their application data.

This changes the default: **removal tears down the add-on's workload and keeps its volume.**
Destroying the data needs an explicit `--delete-data`, and that flag exists only on the operator's
CLI — an agent cannot express it at all.

Two things make the current behaviour worse than it first appears. The **backups survive the
databases**: the dump volume is a separate claim that removal never touched, so a user is left
holding backups of databases that no longer exist. And attached apps keep their `DATABASE_URL`
Secret and keep running, now pointed at a Service that is gone.

**Supersedes [ADR-0031](0031-postgres-addon.md)'s Teardown section**, which specified that removal
deletes the PVC. Everything else in ADR-0031 — one shared instance, a database and role per app, the
provisioning model, the RBAC grant — stands unchanged. Builds on
[ADR-0020](0020-guardrails-as-configurable-policy.md) (the `addon.remove` disposition) and
[ADR-0049](0049-burrow-agent-scoped-cli-control-channel.md) §layer (a) (capabilities the agent
surface does not carry).

## Context

ADR-0031 §Teardown says `addon uninstall postgres` "removes the Deployment, Service, PVC, and the
`burrow-postgres` Secret… (also a confirm guardrail — it destroys *every* app's database)." The
consequence was named at the time and the guardrail was set accordingly. In practice that is not
enough, for three reasons found while documenting the behaviour.

**The confirmation does not carry the consequence.** The guardrail message reads `removing the
add-on "burrow-postgres"`. A `confirm` disposition only protects a user who understands what they
are confirming, and nothing in that sentence mentions a volume, an app, or a database. This is the
gap between *gated* and *informed*.

**"Remove and reinstall" is the obvious repair move.** It is what an operator reaches for when a
component is misbehaving, and it is the move most likely to be made under time pressure by someone
not reading carefully. A verb whose recovery-shaped use is also its most destructive use is badly
named regardless of how it is gated.

**The blast radius is not the add-on's.** Removing a component that a user installed should
plausibly remove that component. But this add-on holds *other things'* data — every attached app's
database lives inside the volume, and those apps did not participate in the decision.

Two further facts, both discovered rather than assumed, shape the decision:

**The backup volume already survives.** `burrow-postgres-backups` is a separate claim created on the
backup path, untouched by removal — so today's behaviour destroys the databases and keeps the dumps.
That asymmetry is accidental, and it is also *correct*: backups outliving their source is the entire
point of a backup. It is retained deliberately here.

**Keeping the volume while deleting the superuser Secret would be a trap**, and this is why the
change is not a one-line edit. The official Postgres image only honours `POSTGRES_PASSWORD` during
`initdb`, which never re-runs over an existing data directory. Keeping the volume but regenerating
the Secret on reinstall would produce a database that is present and permanently unauthenticable —
burrowd, the metrics exporter and the backup Jobs all locked out of data that is still sitting
there. The Secret and the volume therefore share a fate: kept together or destroyed together, never
split.

There is also no attachment registry to consult. `migrations/00004_addons.sql` records that an
add-on is installed, not which apps use it; attachment exists only as a key in the app's Secret and
as a database on the instance. Enumerating affected apps means asking the instance.

## Decision

### 1. Removal keeps the data volume by default

`addon remove` deletes the add-on's Deployment, Service and configuration, and **leaves its
PersistentVolumeClaim in place**. Reinstalling the add-on picks the existing volume back up, and
attached apps reconnect on their existing `DATABASE_URL` — the role passwords live in the data
directory, so they survive with it.

The command says what it kept, and how to reclaim it later. A retained volume that nobody knows
about is a surprise bill.

### 2. Destroying the volume requires `--delete-data`, and it is operator-only

The destructive path exists — a user who wants the space back must be able to have it — behind an
explicit, purpose-named flag on the **operator CLI only**.

`burrow-agent` does not carry the flag. Not gated on it, not permitted to pass it: the verb is
absent from the binary. This is ADR-0049's first layer, and it is the right one here because an
agent reads untrusted input by nature and no agent-driven workflow has a legitimate reason to
destroy an application's database. It is the same line `addon detach` and `addon restore` already
sit on, and it is asserted by a test rather than left as a property of the current command tree.

### 3. The confirmation names what will be destroyed

When `addon.remove --delete-data` is held for confirmation, the message states the volume by name
and the apps whose databases are in it. "This is destructive" is not consent; "this destroys the
databases of `web`, `api` and `worker`" is.

Enumeration is **best-effort and never blocking**. An add-on is often removed *because* it is
wedged, so an unreachable instance degrades to the volume-concrete message rather than making a
broken add-on unremovable. A removal that cannot be performed when the component is broken is a
worse failure than an under-specified prompt.

### 4. The backup volume always survives, including under `--delete-data`

Backups outlive the database they came from. That is what makes §2's destructive path survivable at
all, and it is now deliberate rather than incidental. The command reports the retained backup claim
by name, so the remaining data — and the remaining cost — is visible.

## Consequences

- **The recovery-shaped use of `remove` stops being the destructive one.** Remove-and-reinstall now
  does what it looks like it does.
- **Volumes accumulate.** A user who removes several add-ons over time holds claims they may not
  remember, costing money on a managed disk. §1's output is the only thing standing between that and
  a surprise, which makes the wording load-bearing rather than cosmetic.
- **`--delete-data` is now the single most destructive flag in the CLI**, and it is on a verb people
  reach for while debugging. Its confirmation text is the last line of defence and should be
  reviewed as such whenever it changes.
- **Enumerating attached apps means querying the instance**, so removal gains a dependency on the
  add-on being reachable — deliberately non-blocking (§3), but it is a new failure mode in a path
  that previously had none.
- **A retained volume plus a regenerated Secret would be unrecoverable**, so any future change to
  add-on credential handling must preserve the shared fate in §Context. This is the kind of
  invariant that looks like an implementation detail and is not.
- **This does not fix backup retention.** The backup claim surviving forever is correct for safety
  and unbounded in cost; nothing here prunes it.

## Rejected alternatives

- **Keep deleting the volume, and improve the confirmation text.** The smallest change, and it
  respects ADR-0031's original decision. Rejected because a prompt is a poor defence for an
  irreversible act with a blast radius beyond the component being removed — and because the users
  most likely to lose data are the ones least likely to be reading carefully, since they are
  debugging.
- **Refuse removal outright while apps are attached.** Safest-sounding, and rejected on two grounds:
  it makes a wedged add-on unremovable exactly when removal is the repair, and it introduces a
  bespoke override concept next to the `confirm` disposition that already expresses "stop and think"
  — while `guard set addon.remove deny` already expresses "never," for an operator who wants that.
- **Put `--delete-data` on the agent surface behind a `deny` guardrail.** Uniform with how other
  destructive verbs are handled, and rejected because a disposition is a row someone can change,
  while an absent verb is not. For the one operation that destroys application data irreversibly,
  structural absence is worth the asymmetry.
- **Delete the backup volume too under `--delete-data`.** Consistent — the user asked for the data
  to be gone. Rejected because it removes the only remaining path back from a mistaken invocation of
  the most destructive flag in the product, and because backups outliving their source is what a
  backup is for.
- **Record app attachment in the control-plane database** so enumeration needs no live query.
  Genuinely better, and out of scope here: it is a schema and a write path on every attach and
  detach, and this record should not be blocked on it. Worth doing separately.

## Questions

- **Should a retained volume be visible somewhere?** §1 reports it at removal time and then it is
  invisible — nothing lists orphaned add-on claims. A user who removed an add-on months ago has no
  way to find what it left behind short of `kubectl get pvc`.
- **Does `--delete-data` deserve its own guardrail code** rather than sharing `addon.remove`'s
  disposition? They are now materially different operations, and an operator who wants `remove`
  allowed but `remove --delete-data` denied cannot express that today.
