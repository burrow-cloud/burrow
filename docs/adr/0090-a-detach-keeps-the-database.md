# ADR-0090: A detach keeps the database

## Status

✅ Accepted

## TL;DR

`addon detach` drop an app's database. Make it keep it, and give destroying its own verb.

- **Detach today is `DROP DATABASE`.** Behind a confirm, but confirmed once and the data is gone
  with no way back.
- **Detach is not a destructive operation by name or by shape.** It is the inverse of attach, and
  attach creates a *connection*. Undoing a connection should not undo the data behind it.
- **The same argument [ADR-0064](0064-addon-removal-keeps-its-data.md) already accepted**, for the
  bigger verb. It stopped one level short.
- **Detach becomes: revoke the credential, keep the database.** The app loses its `DATABASE_URL`, its
  role is dropped, and the data stays.
- **`--delete-data` destroys it**, operator CLI only, absent from the agent surface — the shape
  [ADR-0064](0064-addon-removal-keeps-its-data.md) §2 already established.
- **Re-attaching adopts what is there.** Detach then attach gets the data back, which is what makes
  detach a reversible operation rather than an apology.
- **Saying so is the hard part.** A detach that keeps data while the prompt says it destroys it is
  worse than either behaviour on its own.

Supersedes [ADR-0031](0031-postgres-addon.md)'s teardown clause for `addon detach`; the rest of
ADR-0031 stands. Extends [ADR-0064](0064-addon-removal-keeps-its-data.md)'s reasoning to the verb it
did not reach.

## Context

**What exists today.** [ADR-0031](0031-postgres-addon.md) specifies detach in one line:

> **`addon detach postgres <app>`** removes the `DATABASE_URL` key and, behind a **confirm**
> guardrail (it destroys data), `DROP DATABASE`/`DROP ROLE` for that app.

That is what the code does, and every surface says so consistently: the guardrail is described as
*"detach an app from an add-on, destroying its data (e.g. drop its Postgres database)"*, the
confirmation prompt says *"drops its database and role"*, and `docs/CAPABILITIES.md` agrees. Nothing
here is undocumented or accidental.

**What breaks.** The verb and its blast radius do not match, and the mismatch is only visible after
the fact.

`attach` creates a database, a role, and a `DATABASE_URL`. Read as English, `detach` undoes the
*attachment* — the connection between an app and an add-on. It reads like `unpublish`, which stops
serving an app and leaves the app running. Instead it is the one verb in the add-on surface that
destroys a tenant's data on a single confirmation, and the confirmation is the same
"are you sure" an operator has already clicked through for `addon install` and `app run`.

The cases where this bites are ordinary rather than exotic:

- **Moving an app between instances.** Detach from the shared instance, attach to a dedicated one.
  The natural order of those two words destroys the data the move exists to preserve.
- **Taking an app out of service temporarily.** Detach, and the data is not waiting when it comes
  back.
- **Cleaning up a `DATABASE_URL` that is wrong.** The credential is the problem; the rows are not.

**What [ADR-0064](0064-addon-removal-keeps-its-data.md) already decided, one level up.** That record
looked at `addon remove` — *every* attached app's database — and changed the default to keep the
volume, with an explicit `--delete-data` for the destructive case, absent from the agent surface
entirely. Its argument was that the command's name did not say what it did, and that someone
removing an add-on intending to reinstall it cleanly loses everything.

Every word of that applies to detach at a smaller scale. It stopped where it did because it was
about one verb, not because detach was examined and found different.

**What this record resolves.** Whether detach destroys data, and what does if it does not.

**What made this urgent rather than merely tidy.** Moving provisioning onto CloudNativePG's
`Database` and `DatabaseRole` objects ([#520](https://github.com/burrow-cloud/burrow/issues/520))
makes `databaseReclaimPolicy: retain` the natural way to express an object's deletion, so the
refactor *drifts* toward keeping the data whether or not anyone decided it should. A change of this
kind must be a decision rather than a side effect — which is why the implementation deliberately
preserves today's destructive detach until this record is accepted.

## Decision

### 1. `addon detach` keeps the database

Detach revokes the app's access and leaves the data:

- the app's `DATABASE_URL` key is removed from its Secret,
- the app's **role is dropped**, so the credential it held stops working,
- the **database remains**, with its rows.

Dropping the role is what makes this a real detachment rather than a rename. The app cannot reach
the data afterwards; nothing that was issued to it still authenticates. What survives is the data,
not the access.

### 2. `--delete-data` destroys it, on the operator's CLI only

`burrow addon detach postgres web --delete-data` drops the database as well. It is the same shape
[ADR-0064](0064-addon-removal-keeps-its-data.md) §2 established for removal, and for the same
reason: destroying data should be something a person asks for in those words, not something they
arrive at by confirming a prompt.

The flag is **absent from `burrow-agent`** — tier 1 under
[ADR-0065](0065-what-belongs-on-the-agent-surface.md) §2, where `--delete-data` on removal already
sits. An agent can detach an app. It cannot destroy a database, and it cannot express the request.

### 3. Re-attaching adopts the database that is there

`attach` after a `detach` finds the existing database and connects a new role to it, rather than
refusing or creating a second one. Without this §1 is a storage leak with better manners: the data
is kept and unreachable forever.

This is what makes the pair reversible, and reversibility is the whole claim. It also settles the
move-between-instances case: detach, attach elsewhere, and the data is where it was left.

### 4. The guardrail changes what it guards, and stays

`addon.detach` keeps its `confirm` default. What it now guards is **losing an app's access to its
data**, which is disruptive and worth a confirmation, rather than destroying the data, which is no
longer what the verb does.

Its description changes to say so. A guardrail whose text describes a consequence the operation no
longer has is worse than no text: it trains an operator to discount the words.

`--delete-data` is not gated by `addon.detach` — it is not reachable from the agent surface at all,
and a guardrail suggesting otherwise would imply an operator could open it.

### 5. Every surface that says detach destroys data is corrected in the same change

The CLI long help, the confirmation prompt, the guardrail description, and `docs/CAPABILITIES.md`.
This is the load-bearing clause rather than a tidy-up: **a detach that quietly keeps data while its
prompt says it destroys it is worse than either behaviour alone.**

Somebody scrubbing an application's data before handing over a cluster, or satisfying a deletion
request, would read the prompt, confirm it, and believe the data was gone. That failure is silent
and arrives late. It is the reason this record does not permit the new behaviour to ship ahead of
the words that describe it.

## Consequences

- Detach becomes reversible, and the ordinary workflows above stop being traps.
- A detached database occupies storage nobody is using until someone removes it. That is the cost,
  and it is the same one [ADR-0064](0064-addon-removal-keeps-its-data.md) accepted: recoverable
  waste beats unrecoverable loss.
- **An operator needs a way to find them.** A database with no app attached is invisible in a way it
  was not before, because previously it could not exist. Listing what an instance holds is the
  natural home for it, and `addon sql` can already answer the question today.
- Anyone relying on detach to destroy data has to add `--delete-data`. The behaviour change is
  toward safety, so the failure mode of not noticing is retained storage rather than lost rows.
- An agent's detach stops being a destructive act. Under
  [ADR-0065](0065-what-belongs-on-the-agent-surface.md) §1 the verb now fails neither test, so
  whether `addon.detach` should relax from `confirm` is a fair question — deliberately not answered
  here, because it is a policy default rather than a property of the operation.

## Alternatives considered

**Leave it as it is, and rely on the confirmation.** The status quo, and its defence is that the
prompt does say what happens. Rejected on the same ground
[ADR-0064](0064-addon-removal-keeps-its-data.md) rejected it: a confirmation is a poor substitute
for a name that means what it does, because an operator confirms many things and reads few of them.
The word `detach` will keep meaning "disconnect" no matter what the prompt says.

**Rename the verb instead** — make it `addon drop` or `addon destroy`, and let it keep destroying.
Honest, and it makes the common safe operation unavailable: there would still be no way to
disconnect an app from its database without losing the data, which is the thing people actually
want.

**Keep the database and keep the role too**, dropping only the `DATABASE_URL` key. Smaller, and it
leaves a live credential for a database the app is no longer attached to — sitting in the instance,
valid, belonging to nothing. Detach would not be a security boundary, and the next person to read
the role list would have to reconstruct why it is there.

**Put the data behind a retention window** — dropped after N days unless re-attached. Bounds the
storage cost and adds a clock to a system that otherwise has none here, plus a scheduled destructive
job whose failure mode is deleting data nobody asked to delete. Worth revisiting if retained
databases actually accumulate; not worth building for a cost nobody has measured yet.
