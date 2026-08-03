# ADR-0086: Burrow installs one kind of Postgres

## Status

✅ Accepted

## TL;DR

Burrow runs two different Postgres stacks. Add-ons get CloudNativePG — backups, failover, point-in-time recovery. burrowd's own database gets a plain Deployment on a 1Gi volume with **no backup at all**.

- **One stack.** burrowd's database becomes a CloudNativePG cluster like every other.
- **`burrow install` installs the operator**, the same way it already installs and waits for a database today.
- **Still the floor.** Not an add-on, not attachable, not visible to `burrow addon`. Outside the model, same stack.
- **Chosen at install, and only there.** `--database cnpg` is the default; `--database plain` keeps
  today's Deployment for a cluster that will not accept CustomResourceDefinitions.
- **Never falls back on its own.** A cluster that refuses the CRDs fails the install and names the
  flag. Silently handing somebody an unbacked-up database is the failure this record exists to end.
- **Costs named:** the default now creates cluster-scoped CustomResourceDefinitions, and takes longer.

Supersedes [cloud ADR-0030](https://github.com/burrow-cloud/cloud/blob/main/docs/adr/0030-the-cloud-control-planes-database-is-a-burrow-addon.md) §2's reading that the floor must be a different *kind* of database. Extends [ADR-0066](0066-the-postgres-addon-runs-on-cloudnativepg.md).

## Context

**Two Postgres stacks are in the product today.**

`cmd/burrow/manifests/install.yaml.tmpl:393` applies a plain `Deployment` named `postgres` with a 1Gi PVC, and `cmd/burrow/install.go:693` waits up to three minutes for it. That is burrowd's own database.

Every Postgres **add-on** is a CloudNativePG `Cluster` ([ADR-0066](0066-the-postgres-addon-runs-on-cloudnativepg.md)) — reconciled by an operator, backed up through the pgBackRest plugin to object storage, capable of a standby ([ADR-0081](0081-a-postgres-instance-may-have-a-standby.md)) and of point-in-time recovery.

So the same product runs Postgres two ways, and the one it runs worst is its own.

**What the floor actually holds.** Environments, the guardrail policy, release history, app records, registry credentials. On a live install it is also the database holding the `deny` dispositions that protect the multi-tenant control plane deployed beside it (cloud ADR-0021, ADR-0030).

**Its protection today is none.** One replica, one 1Gi block volume, no backup, no snapshot schedule, no CronJob. If that volume is lost, the install's entire state goes with it — including the policy that was the reason to trust the arrangement.

**Why it looked defensible.** Cloud ADR-0030 §2 says *"one database stays outside the model — open-source burrowd's own. Something has to be the floor, and that is it."* That is right, and it is about **management**: the thing that manages add-ons cannot itself be an add-on, or repairing it requires it to be working.

The mistake was reading "outside the add-on model" as "a different database stack". Those are separable, and only the first is load-bearing.

**The argument that the operator would become a prerequisite does not hold.** It was raised — that CloudNativePG would have to be installed before Burrow, so `burrow cluster postgres install` would gate `burrow install` — and it is wrong. Install already applies a database manifest and waits for it to become ready. Applying an operator's manifest first, and waiting for that too, is more steps in one command rather than a new step before it.

## Decision

### 1. `burrow install` installs CloudNativePG, and burrowd's database is a `Cluster`

The install applies the operator, waits for its controller, creates the `Cluster` for burrowd's database, and waits for it to become ready — in place of today's `Deployment` and its three-minute wait.

One stack. One set of failure modes to learn, one repair path, one thing to get right.

### 2. The choice is made at install, and the default is CloudNativePG

```
burrow install <context>                      # CloudNativePG, the default
burrow install <context> --database plain     # today's Deployment
```

`plain` exists for one reason: a cluster whose platform team will not accept cluster-scoped
CustomResourceDefinitions. That is a real constraint no amount of operator installation satisfies,
and refusing those clusters outright would be the wrong trade for a product whose first audience is
people who did not choose their cluster's rules.

**It is a choice, not a fallback.** An install that cannot create the CustomResourceDefinitions
**fails and names the flag**. It does not quietly install the plain database instead.

That distinction is the point. Silently degrading to an unbacked-up database because a permission
check failed is exactly the class of failure this record exists to remove — the operator would get a
success message, a working install, and no protection, with nothing anywhere saying which of the two
they got.

**What `plain` costs is stated at install rather than discovered:** no backups, no point-in-time
recovery, no failover, no standby. `burrow cluster` reports which of the two is running, so the
answer survives the install output scrolling away.

**Add-ons are unaffected either way.** A Postgres add-on is a CloudNativePG `Cluster` regardless
([ADR-0066](0066-the-postgres-addon-runs-on-cloudnativepg.md)), so an install on `plain` can still
run `burrow cluster postgres install` later and get add-ons. Its own database stays plain until it is
migrated.

### 3. It is still the floor, and the floor is about management rather than mechanism

burrowd's database is **not** an add-on. It does not appear in `burrow addon list`, cannot be attached to an app, cannot be removed by `addon remove`, and is not subject to add-on guardrails.

Cloud ADR-0030 §2's reasoning survives intact: the thing that manages add-ons cannot be managed as one, because repairing it would require it to be working. That remains true and is unaffected by which stack runs the database underneath.

### 4. The floor gets backups on the same path as everything else

Once it is a `Cluster`, the pgBackRest plugin covers it exactly as it covers an add-on instance — WAL archiving and base backups to a registered object-storage provider, with the same retention policy and the same restore path.

**When no provider is registered it is unbacked-up, which is what it is today**, so nothing regresses for an install that has not configured storage.

### 5. Install-time cost is stated rather than hidden

`burrow install` will create **cluster-scoped CustomResourceDefinitions** and take materially longer. Both are named in the install plan before anything is applied, the way `burrow cluster postgres install` already names its own.

## Consequences

**The most important database in an install stops being the least protected one.** That inversion is the reason for this record. A product whose argument is production-grade reliability should not ship a control plane whose own state has no backup.

**Two install shapes exist, and both must keep working.** That is the price of `--database plain`,
paid in tests and in every path that assumes which one is running. Worth it: the alternative is
refusing an install to anyone whose platform team reserves CustomResourceDefinitions, which is a
large population that cannot change its own rules.

**`burrow install` gains a cluster-scoped footprint by default.** It already creates a ClusterRole and needs cluster-admin, so this is a widening rather than a new category — but CustomResourceDefinitions are the thing a platform team is most likely to reserve. On a cluster that refuses them, install now fails where it previously succeeded. That is a real adoption cost and it lands hardest in exactly the environments noted in [#457](https://github.com/burrow-cloud/burrow/issues/457).

**Install takes longer and has more states.** Operator ready, then cluster bootstrapped, rather than one Deployment rolling out. More places to fail, and each needs a message that says which stage did.

**The repair path gains machinery, but less than it appears.** If the CloudNativePG operator is down, existing clusters keep serving — it stops reconciling, not running. So a broken operator does not take burrowd's database offline, and the data remains in a PVC that can be reached without the operator at all.

**Existing installs need a migration, and it is the hard part.** An install running the plain Deployment has live state in `postgres-data`. Moving it into a `Cluster` is a dump and restore with burrowd stopped, and getting it wrong loses the install's history. It needs its own design, and it is the reason this is a record rather than a patch.

**One less thing to explain.** "Why is Burrow's own database different from the ones it gives you" has no good answer today, and every new person asks it.

## Alternatives rejected

**Leave it, and back the volume up outside Burrow.** A DigitalOcean snapshot schedule, or an equivalent, covers the data without touching the product — and for a floor there is something right about protection that does not depend on Burrow working.

Rejected because it fixes the smaller half. Snapshots give crash-consistent copies of a running Postgres volume, not point-in-time recovery, and restoring one is a manual act nobody has practised. It also leaves the two-stack incoherence entirely, along with a second set of failure modes and the question every new reader asks.

Worth doing **anyway** as an interim, and worth keeping afterwards: the operator is one more thing that can be broken, and a volume snapshot is the recovery that needs none of it.

**Make burrowd's database an add-on properly.** Removes the special case altogether. Rejected for cloud ADR-0030 §2's original and still-correct reason: repairing the thing that manages add-ons would require it to be working.

**Install CloudNativePG only when an add-on needs it.** Keeps a minimal install for someone who never installs Postgres. Rejected because it makes the floor's protection conditional on an unrelated decision — the floor would be backed up or not depending on whether somebody happened to want a Postgres add-on.

**Detect the refusal and fall back to `plain` automatically.** Reads as helpful and is the failure mode in disguise: an install that cannot create the CustomResourceDefinitions would report success while handing over a database with no backups, and nothing afterwards would say which of the two happened. §2 fails instead and names the flag, so choosing the unprotected database is always something a person did on purpose.

**Ship a Postgres operator of our own, or manage the StatefulSet directly.** Full control, no CustomResourceDefinitions from a third party. Rejected on the same ground as [ADR-0066](0066-the-postgres-addon-runs-on-cloudnativepg.md): backup, failover and point-in-time recovery for Postgres are a large problem that CloudNativePG has already solved, and reimplementing them badly is worse than depending on it.
