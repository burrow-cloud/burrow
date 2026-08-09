# ADR-0095: Attaching a database is held for a human

## Status

🟡 Proposed

## TL;DR

`addon attach` is the one add-on verb with no guardrail at all. Give it one: `addon.attach`, held for
confirmation by default.

- **Today nothing gates it.** `AttachAddon` evaluates no disposition. The refusals are conventions in
  prose, not a control the control plane enforces.
- **Attach is not the harmless verb the code says it is.** It creates a database and a role on a
  server every other app depends on, writes a credential into an app's Secret, and restarts the app.
  A re-attach also rotates a password nobody can read back.
- **`confirm`, not `deny`.** [ADR-0065](0065-what-belongs-on-the-agent-surface.md)'s tier 3, and
  `bucket.create`'s shape exactly: additive, part of the workflow the product exists for, and it
  spends somebody's disk.
- **Scoped to the instance, and to the environment.** The instance is where the database lands; the
  environment is where an operator wants a gradient — `--env dev allow`, confirm in production. Second
  declared exception to "addon.* is not env-scoped", on [ADR-0087](0087-running-sql-against-an-attached-database.md) §5's grounds.
- **Unbound.** It binds everybody, including the operator, until
  [ADR-0094](0094-a-guardrail-can-bind-the-agent-and-leave-the-human-alone.md) is built. Then
  `guard set --binds agent addon.attach deny` becomes the interesting setting, with no change here.
- **A confirm is cooperative.** The caller supplies it. This raises the floor; it does not hold
  against an agent that decides to pass the flag.
- **How many databases a tenant may have is a quota, not a guardrail** (§6). A disposition cannot say
  "three". That lever belongs in [ADR-0068](0068-operational-limits-are-configuration.md)'s limits and
  is a separate record.

Revises [ADR-0085](0085-a-guardrail-can-name-the-app-it-guards.md)'s clause that `addon.attach` is not
a guardrail; the rest of ADR-0085 stands, and this record uses its key composition. Applies
[ADR-0065](0065-what-belongs-on-the-agent-surface.md)'s tier test to a verb that predates it. Reads
against [ADR-0090](0090-a-detach-keeps-the-database.md), which is what makes attach reversible, and
[ADR-0091](0091-an-environment-may-hold-more-than-one-postgres-instance.md), which makes *which
instance* a real question. Supersedes nothing.

## Context

### What exists today

**`addon attach` is ungated, and deliberately so.** `Engine.AttachAddon`
([`controlplane/engine.go`](../../controlplane/engine.go)) validates, resolves the environment and the
instance, settles the variable name, refuses an occupied one, and then provisions — with no call to
`Policy.enforce` anywhere on the path. It is the only mutating add-on verb of which that is true:
`addon.install`, `addon.remove`, `addon.detach`, `addon.restore`, `addon.restore_instance` and
`addon.sql` all have codes.

Three places say why, in the same words:

- [`controlplane/guardrails.go`](../../controlplane/guardrails.go), on `GuardrailAddonDetach`:
  *"(Attach is not guarded: it provisions, it destroys nothing.)"*
- `AttachAddon`'s own doc comment: *"Attach provisions and destroys nothing, so it is allowed by
  default (no guardrail) and is safe over the agent control channel."*
- [ADR-0085](0085-a-guardrail-can-name-the-app-it-guards.md), deciding which guardrails take a
  `--name`: *"`addon.attach` is not a guardrail at all, since attaching provisions and destroys
  nothing."*

So this is a decision that was made, not an omission. It is also the older half of a pair whose other
half has since changed: when it was made, `detach` was `DROP DATABASE`, and against that, attach
really was the safe direction.

**Every person-side limit on attaching is a convention.** An operator who does not want their agent
giving apps databases has nothing to set. `guard set` refuses the code because it is not in
`knownGuardrails`. The only levers are the agent surface — attach is compiled into `burrow-agent`, and
removing it would be [ADR-0065](0065-what-belongs-on-the-agent-surface.md) tier 1, unappealable and a
dead end — and the operator's own instructions to the agent, which are prose.

### What that breaks

**"Destroys nothing" is true and is not the whole of what attach does.** Reading the path:

1. It **provisions a database and a login role** on a Postgres instance shared by every other app in
   the environment ([ADR-0031](0031-postgres-addon.md), [ADR-0067](0067-one-database-instance-per-environment.md) §1).
   The disk, the connection slots and the backup window are the instance's, and they are finite.
2. It **writes a credential into the app's Secret** and, where the instance has a standby, a second
   read address beside it ([ADR-0081](0081-a-postgres-instance-may-have-a-standby.md) §2).
3. It **restarts the app** — `rollForSecretChange`, a real rollout of a running production workload.
4. On an app that is already attached it **rotates the role's password**, and moves the variable if
   `--as` names a different one, removing the old key.

Item 4 is the one that is not reversible in any sense. The connection string is generated
server-side, never returned and never logged, so the previous password is unrecoverable the moment
`EnsureAppDatabase` runs. Burrow repairs the app it knows about by writing the new value into its
Secret; anything *else* holding that DSN — a pooler, a sibling service someone wired by hand, a
migration runner in CI — is broken by a call nobody reviewed.

**And the reversibility argument has quietly improved since it was made.**
[ADR-0090](0090-a-detach-keeps-the-database.md) made `detach` keep the database, so the undo for a
wrong attach is now a detach that leaves a database behind rather than one that destroys the data.
That is better for the app and it means the *provisioning* half of an attach is only partly undoable
by any verb the agent can reach: the credential goes, the role goes, the database and its disk stay
until a human runs `detach --delete-data`, which is operator CLI only
([ADR-0090](0090-a-detach-keeps-the-database.md) §2). An agent that attaches wrongly leaves residue a
human has to clear — the same shape as the retained volumes in
[ADR-0064](0064-addon-removal-keeps-its-data.md) §6 and the undeletable apps in
[ADR-0065](0065-what-belongs-on-the-agent-surface.md)'s consequences.

**There is nothing for a plan tier to bind to.** The managed product's plans differ in what a tenant
may have, and the first thing on that list is a database. A guardrail code is the only per-operation
dial the control plane has, and `addon.attach` is not one, so a tier cannot express even "this plan
does not include a database" without a special case outside the policy table.

### What this record resolves

Whether `addon attach` gets a guardrail, what its default disposition is, what it can be narrowed to,
whether it binds the agent alone, and — because the two are easy to conflate — which of the two
plausible limits on attaching this is, and which one it is not.

## Decision

### 1. `addon.attach` is a guardrail, held for confirmation by default

A new code, `addon.attach`, gating whether an attach proceeds. Its default disposition is `confirm`.

The argument is [ADR-0065](0065-what-belongs-on-the-agent-surface.md)'s two tests, applied to the four
effects above.

**Scope: it passes.** The credential, the restart and the variable all land on the one app the agent
was asked about. The database and role land on a shared instance, which is a reach beyond that app —
but a bounded one: a database and a role, of a size the app itself decides, on a server that already
holds one per attached app. That is not `addon remove`'s shape, where the worst case is every app's
data at once and no configuration makes it small. Tier 1 is for a worst case that is *unbounded*, and
this one has a ceiling.

**Reversibility: it mostly passes, and the exception is narrow.** A human can undo a first attach:
`detach` removes the credential and drops the role, `detach --delete-data` removes the database. The
app comes back from its restart. What a human cannot undo is a *re-attach*'s password rotation, and
the loss there is not the app's — the app is handed the new value — but any out-of-band holder's.
Failing reversibility in a narrow case does not make this tier 2; it makes the prompt matter, which
§4 handles.

So: passes scope, passes reversibility for the case that dominates. That is
[ADR-0065](0065-what-belongs-on-the-agent-surface.md) tier 3 — *consequential but routine* — and the
existing tier-3 entry is the closest precedent there is. `bucket.create` is held for confirmation
because *"creating a bucket is additive, reversible, and part of a legitimate workflow, but it costs
money at a vendor, so a human approves it."* Attach is additive, reversible, part of the workflow the
product exists for, and it spends disk on a server the operator pays for. The same sentence describes
it, so it gets the same answer.

**And a confirmation here is an informed one**, which is the test
[ADR-0087](0087-running-sql-against-an-attached-database.md) §5 set when it refused `confirm` for
`addon.sql`. The reader of an `addon.sql` prompt is approving a hundred lines of SQL they have not
read; the reader of this one is approving a single sentence — *give `web` its own database on
`burrow-postgres` in `prod`, writing the connection string into `DATABASE_URL`* — that names every
consequence it has. Where a confirmation can be understood, holding for one is a control rather than
theatre.

**Frequency is the other half of why `confirm` is affordable.** An attach happens once per app per
environment, not once per deploy. A gate at that frequency costs an operator one approval per new
service; `app.deploy`'s `allow` default exists because the same gate on the everyday verb would not
be read after the first week.

### 2. It is scoped to the add-on instance, and it is env-scopable

`addon.attach` declares `names: targetAddon` and `envScoped: true`. Both tiers, resolved by the chain
[ADR-0085](0085-a-guardrail-can-name-the-app-it-guards.md) §2 already established: the instance's key,
then the environment's, then the global one, then the default.

**The name tier is the instance, not the app.** Attach names two things, exactly as `detach` does, and
[ADR-0085](0085-a-guardrail-can-name-the-app-it-guards.md) §1 already settled which one a two-target
add-on verb scopes by: the instance, *"because that is where the data lives and where the reach
stops."* The reasoning transfers without adjustment. The thing an operator wants to protect is a
server — *"nothing new goes on the production database"* — and scoping by app would let them protect
`web` while the identical verb puts a database on the same instance for `api`, which reads as
protection and is not. It also composes with
[ADR-0091](0091-an-environment-may-hold-more-than-one-postgres-instance.md) for free: an operator with
a dedicated `analytics` instance can hold attaches to it and leave the environment's default instance
alone, because the label is already the key.

**The environment tier is the second declared exception to "`addon.*` is not env-scoped."** The rule
in `knownGuardrails` is that an add-on operation's instance name already draws the per-instance line,
so an environment tier over the top of it would be a second way to say the same thing.
[ADR-0091](0091-an-environment-may-hold-more-than-one-postgres-instance.md) weakened the premise: an
environment may now hold several instances, so *per instance* and *per environment* are no longer the
same statement. `addon.sql` was the first exception, on the grounds
[ADR-0087](0087-running-sql-against-an-attached-database.md) §5 gave — the want is a gradient across
environments, and an operator who adds a second instance should not have to repeat the disposition on
it. `addon.attach` has that want more strongly than `addon.sql` does.

It is also what makes the `confirm` default affordable rather than annoying. The friction case is a
developer whose agent is building three services in a sandbox and stopping for approval each time; the
answer is `burrow guard set --env dev addon.attach allow`, one command, leaving production held. That
is [ADR-0065](0065-what-belongs-on-the-agent-surface.md) §3's floor-not-a-fixed-setting shape, and
without the environment tier the only relief available would be a cluster-wide `allow` — relaxing
production to unblock a sandbox, which is precisely the failure `relaxHint` was written to steer
people away from.

### 3. It binds every caller, and gains the caller-kind axis when ADR-0094 is built

The disposition ships unbound. `addon.attach confirm` binds the operator and the agent alike, as every
disposition does today.

**Not because the kind-bound version is uninteresting** — it is the setting most operators will
eventually want, and the sentence *"the agent may not put databases on production; I may"* is the
whole point of [ADR-0094](0094-a-guardrail-can-bind-the-agent-and-leave-the-human-alone.md). It ships
unbound because the axis does not exist yet. `Policy.enforce` takes no context, `guard set` has no
`--binds`, and the key has no kind prefix; building the axis is ADR-0094's work and doing a fourth of
it here would land a second, narrower version of a mechanism that record already specified whole.

**Nothing about this record has to change when it arrives.** The default lives in `DefaultPolicy` as a
plain `GuardrailCode`-keyed entry, and
[ADR-0094](0094-a-guardrail-can-bind-the-agent-and-leave-the-human-alone.md) §3 step 7 has the
built-in default bind every kind, so an unbound `confirm` default is the correct default in both
worlds. What arrives with ADR-0094 is a new *setting*, not a new default:
`burrow guard set --binds agent --env prod addon.attach deny`, which is the first configuration of
this code that is a real gate rather than a raised floor.

**A default of `deny` was not chosen for the same reason.** A deny today refuses the operator too, and
`burrow addon attach` is how a person gives their own app a database — a default that breaks the
human path in order to bind the agent is the trade
[ADR-0094](0094-a-guardrail-can-bind-the-agent-and-leave-the-human-alone.md) exists to remove, and
taking it here would be adopting the defect a week before the fix.

### 4. The confirmation says which of the two things this call is doing

The held message distinguishes a first attach from a re-attach, because they have different
consequences and [ADR-0090](0090-a-detach-keeps-the-database.md) §5 is the record that says a prompt
describing a consequence the operation does not have is worse than no prompt.

A first attach names what is created and where:

> attaching `web` to the postgres instance `burrow-postgres` in environment `prod` (creates a database
> and a login role on it, writes the connection string into `DATABASE_URL`, and restarts the app)

A re-attach leads with the rotation, since that is the irreversible part and the reason to stop:

> re-attaching `web` to the postgres instance `burrow-postgres` in environment `prod` **rotates the
> password**: the app is given the new connection string in `DATABASE_URL`, and any other holder of
> the old one stops connecting

Where `--as` moves the variable, the message names the key being vacated as well, because that removal
is what leaves an app reading a name that is no longer written.

**Burrow tells the two apart by the recorded attachment**, the same row `detach` reads. An attachment
made before the variable name was recorded (issue #462) reads as a first attach and gets the
understating message. That is a real gap and it is left rather than papered over: the alternative is
inferring a prior attachment from the instance's own catalogue, which is a live query on the path of a
prompt, and a first attach wrongly described as a rotation would train the reader to discount the
words in the other direction.

### 5. What this does not protect against

Named plainly, in the manner [ADR-0043](0043-public-reachability-is-a-loadbalancer.md) and
[ADR-0047](0047-agent-environment-safety.md) named theirs, because a control whose limits are
implicit gets trusted for things it does not do.

1. **A confirmation is cooperative.** `confirm` is satisfied by the caller setting a flag. Nothing in
   the control plane verifies a human saw the prompt, and an agent that decides to pass `--confirm` is
   not stopped by this. `deny` is the disposition that holds against a misbehaving agent
   ([ADR-0020](0020-guardrails-as-configurable-policy.md)), and until ADR-0094 is built, setting it
   also stops the operator. So what ships here is a raised floor, not a boundary — which is the same
   thing every other `confirm` in the table is, and worth saying rather than assuming a reader knows.
2. **It does not bound how many databases exist.** One approval per attach, a hundred apps, a hundred
   databases, every one of them approved. §6.
3. **It does not stop an app reaching a database another way.** A DSN pasted in through
   `burrow secret set`, a connection string compiled into the image, or a pooler the app talks to
   instead are all outside this code. Attaching is Burrow's provisioning verb, not the app's only
   route to Postgres.
4. **It cannot un-rotate a password.** The confirmation is the entire protection for §4's out-of-band
   holder; once the operation proceeds, no verb restores the old credential, because nothing ever held
   it.
5. **It does not gate what the app does with the database.** Once attached, the app connects with its
   own credential on its own schedule. `addon.sql` gates statements *Burrow* runs
   ([ADR-0087](0087-running-sql-against-an-attached-database.md)); it says nothing about the app's own
   connections, and this code says nothing about either.
6. **It does not gate creating the instance.** `addon.install` does, and an operator who denies
   attaches while leaving installs at `confirm` has protected the databases and not the servers.
7. **The relaxation lever is on the operator CLI only**, which is what the whole tier depends on
   ([ADR-0065](0065-what-belongs-on-the-agent-surface.md) §3). `guard set` is not compiled into
   `burrow-agent`. That is a surface control rather than an authorization boundary and this record
   inherits the caveat rather than fixing it.

### 6. How many databases a tenant may have is a quota, and this is not it

Two different limits are easy to hear in *"limit attaching a database"*, and building one while
believing it is the other would produce a control that satisfies neither.

- **May this call proceed?** A disposition. Per operation, three values, resolved from the policy
  table, answered before anything is provisioned. That is the guardrail this record adds.
- **Has this tenant had enough?** A count. It needs a number, a live tally of what exists, and a
  refusal that names both. A disposition cannot express it: `allow`, `confirm` and `deny` are not
  numbers, and the closest a policy key gets to "three databases" is a `deny` on the fourth, which the
  policy table has no way to know is the fourth.

**A plan tier binds to the second one.** *Free gets one database, the paid tier gets five* is a
sentence about a count, and no arrangement of dispositions says it. The one thing a tier *can* say
through this guardrail is the degenerate case — a plan with no database at all is `addon.attach deny`
— and reading that as tier support would be the conflation this section exists to prevent.

**The quota's home already exists and is not here.**
[ADR-0068](0068-operational-limits-are-configuration.md) built the second mechanism precisely for
this: a `LimitCode` with a `LimitKindCount`, environment and cluster tiers, and — §2, which is the
load-bearing sentence — a breach that is a *validation failure checked before any guardrail runs*,
because a limit is a line and a guardrail is a question. A database quota is `LimitKindCount` in the
shape `LimitReplicaCeiling` already has.

This record does not add it, for two reasons. It is a different mechanism with a different failure
mode and it deserves its own argument — where the count is scoped (per instance, per environment, per
install), what happens to attachments that already exceed a lowered limit, and whether the tally is
read from the registry or from the instance. And it is not what is missing today: the ungated verb is
missing, and an install with a quota and no guardrail would still let an agent attach freely up to the
number.

## Consequences

**An attach stops without an approval, on installs that never set anything.** This is a behaviour
change for every existing install, and it is the intended population — the same trade
[ADR-0065](0065-what-belongs-on-the-agent-surface.md) §3 made when it moved `app.delete` and
`dns.delete`. An operator who wants the old behaviour sets `burrow guard set addon.attach allow`, or
the per-environment form. An operator who already has an explicit disposition for this code has none,
because the code did not exist.

**An unattended attach fails until something is configured.** Any automation that attaches — a
provisioning script, a CI job, an agent left to build an app end to end — meets a 422 with
`needs_confirmation` the first time. That is a legible refusal naming the command that relaxes it,
which is the whole reason a denied verb beats an absent one, but it is still an interruption in a flow
that used to complete.

**A client older than this change cannot satisfy the hold.** `confirm` travels in the attach request
body, so a `burrow` or `burrow-agent` built before this sends no confirmation and has no flag to set,
and a newer burrowd holds its attach. burrowd is the compatibility anchor
([ADR-0039](0039-cli-control-plane-version-skew.md) §2) and this is a case where an old client
loses a capability rather than gaining a wrong outcome: it is refused with the reason, nothing is
provisioned, and either upgrading the client or `guard set addon.attach allow` restores it. That is
the acceptable half of the compatibility trade — the unacceptable half is a request whose meaning
changes silently, which this is not.

**The guardrail table grows a fifteenth row, and the add-on codes finally cover the add-on verbs.**
Every mutating add-on operation is now gated by something an operator can inspect with `guard list`
and set with `guard set`. The pattern *"additive add-on verbs are ungated"* stops being a rule a
reader has to infer from which codes happen to exist.

**Two accepted records now describe attach in words that are no longer accurate.**
[ADR-0085](0085-a-guardrail-can-name-the-app-it-guards.md)'s *"`addon.attach` is not a guardrail at
all"* and [ADR-0031](0031-postgres-addon.md)'s *"attach is safe to expose to the agent precisely
because the agent supplies only an app name"* are both superseded in that clause and stand otherwise.
The code comments and `docs/CAPABILITIES.md` are corrected as part of building this rather than
afterwards, because a doc that says a verb is ungated when it is gated is the same defect ADR-0009
names, pointing the other way.

**A confirm on the additive verb makes the pair legible.** Attach and detach now both hold: one for
creating access, one for ending it. An operator reading `guard list` sees a symmetric pair rather than
a verb whose absence they have to notice.

**ADR-0094 acquires a fourth code worth binding.** The six mutating `app.*` codes were the motivating
set; `addon.attach` joins them as an obvious candidate for `--binds agent` on a production instance,
and it arrives already scoped to the instance the binding would name.

**A fifteenth code is a fifteenth thing to configure wrongly.** Specifically: setting it on the app's
name rather than the instance's, which resolves to no override at all and reads in `guard list` as
unset. That is [ADR-0085](0085-a-guardrail-can-name-the-app-it-guards.md)'s existing trap rather than
a new one — `guard set --name` refuses a name that is not an instance for `addon.*` codes — and it is
worth naming because attach is the add-on verb whose command line puts the *app* name last.

## Rejected alternatives

**Leave attach ungated.** The status quo, and the position three current records take. It rests on
*"attach destroys nothing"*, which was written when detach did, and which never covered the password
rotation, the restart, or the disk. It also leaves an operator with no lever at all short of the agent
binary, and leaves a plan tier nothing to bind to.

**Deny by default.** The strongest reading of *"make the limit real"*, and it is the wrong week for
it: a deny today binds the operator, and `burrow addon attach` is how a person gives their own app a
database. It also fails [ADR-0065](0065-what-belongs-on-the-agent-surface.md)'s test on the merits —
attach passes scope and mostly passes reversibility, which is tier 3, and tier 2 is for verbs a human
cannot undo. The deny becomes the right setting the moment it can bind the agent alone, and §3 leaves
that one `guard set` away.

**Allow by default, so the code exists without changing behaviour.** Tempting, and it is a guardrail
in name only: an operator who never sets it gets no protection, which is today with an extra row in
`guard list`. A default is the answer for everyone who has not thought about the question, and the
answer to *"should a machine be able to create a database on my production server unsupervised"* is
not yes.

**Remove `attach` from the agent surface.** [ADR-0065](0065-what-belongs-on-the-agent-surface.md) §5
prefers a denied verb to an absent one wherever the risk allows, and the risk allows it easily here.
Attaching is also the verb the agent most legitimately needs: an agent that can deploy an app but
cannot give it a database writes the connection string by hand into `secret set`, which is worse in
every respect.

**Scope it to the app instead of the instance.** The more natural reading — `attach <addon> <app>`
puts the app last, and the credential lands on the app. Rejected for
[ADR-0085](0085-a-guardrail-can-name-the-app-it-guards.md) §1's reason, which does not weaken here:
protection an operator sets on `web` while the same verb reaches the same instance for `api` reads as
protection and is not. It would also give the add-on codes two different name meanings, so
`guard set --name` would resolve to an app for one `addon.*` code and an instance for the rest.

**Scope it to the app *and* the instance — a fourth tier.** It expresses both wants exactly, and it is
a fourth axis on a key that just gained a third, for a code whose per-app want ("don't let the agent
rewrite this app's `DATABASE_URL`") is already covered: the occupied-name refusal (issue #462) will not
let an attach overwrite a variable the attachment does not own.

**Keep it out of the environment tier, like the other `addon.*` codes.** Consistent, and it makes the
`confirm` default expensive: the only relief from a sandbox's friction would be a cluster-wide
`allow`. [ADR-0091](0091-an-environment-may-hold-more-than-one-postgres-instance.md) also removed the
premise the rule rested on — per-instance and per-environment stopped being the same statement once an
environment could hold several instances.

**Bind it to `agent` from the start.** It is the setting that matters, and the axis does not exist:
[ADR-0094](0094-a-guardrail-can-bind-the-agent-and-leave-the-human-alone.md) is accepted and unbuilt,
`enforce` takes no context, and the key has no kind segment. Building a narrow version of it for one
code would produce a second mechanism to reconcile with the one ADR-0094 already specified in full.

**A separate code for a re-attach**, so the rotation could be denied while first attaches were
allowed. It is the `--delete-data` question ADR-0064 left open, one verb over, and the answer is worse
here: whether a call is a re-attach is not something the caller asks for, it is something the control
plane discovers, so the disposition would depend on state the operator setting it cannot see. §4 puts
the distinction in the prompt, where the reader can act on it.

**Build the quota instead.** It is the limit a plan tier binds to (§6) and it is not the gap: an
install with a database quota and no guardrail still lets an agent attach freely up to the number, with
no operator approval anywhere. The quota is a real and separate want, in a mechanism that already
exists ([ADR-0068](0068-operational-limits-are-configuration.md)), and it deserves the record this one
is not.

**Build both here.** One record, two mechanisms, and the argument for each would be read as the
argument for the other — which is the specific confusion §6 exists to prevent.
