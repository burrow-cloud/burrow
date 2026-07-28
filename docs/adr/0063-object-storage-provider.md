# ADR-0063: Object storage is a provider type, scoped to being a backup destination

## Status

🟡 Proposed

## TL;DR

Backups currently land on a disk **in the same cluster as the database they came from**
([ADR-0032](0032-postgres-backups.md)). If the cluster, the node, or the volume goes, both go
together. A backup that shares a failure domain with its source is not really a backup.

Fixing that needs somewhere off-cluster to put them, which means object storage, which means a
credential and a bucket. This adds **object storage as a provider type** on the existing credential
registry ([ADR-0023](0023-provider-credentials.md)) — not as a new mechanism, just a new kind of
thing that registry already knows how to hold.

**The important part of this record is the scope, not the feature.** Every object-storage vendor
already ships a capable CLI, and one of them is enormous. Burrow is not competing with those and
must not grow into one. What it does is narrower and is the part those CLIs cannot do: put the
credential where the backup engine will look for it, create a bucket shaped correctly for the job,
and refuse the one configuration that silently destroys recoverability — a bucket lifecycle rule
that deletes objects a restore still needs. Everything else about a bucket stays the vendor's
business.

Extends [ADR-0023](0023-provider-credentials.md) (the provider registry and the one credential
Secret). Serves [ADR-0032](0032-postgres-backups.md) (backups) and
[ADR-0025](0025-building-block-addons.md) (add-ons that may want durable storage). Supersedes
nothing.

## Context

[ADR-0032](0032-postgres-backups.md) gives Burrow logical backups: `pg_dump -Fc` through a one-shot
Job, `pg_restore` to recover, recorded in the registry. It writes them to a PersistentVolumeClaim in
the add-on namespace — **the same cluster as the database**. That was the right first step and it is
not a durable end state. The dumps survive a bad migration; they do not survive losing the cluster,
and they do not survive losing the volume. There is also no schedule, so a backup happens when
somebody remembers, and no retention, so the volume fills and then new backups fail too.

Every one of those gaps needs the same missing thing first: a destination that is not this cluster.

**Why this is Burrow's problem and not the user's `aws s3` invocation.** The objection deserves a
real answer, because the alternative genuinely exists — S3-compatible vendors ship CLIs, and the
S3 API is well served by tooling far better than anything this project should write.

The answer is that "get a bucket" is not the work. The work is:

- **The credential has to arrive somewhere specific.** A backup engine running in the cluster reads
  its credentials from a Kubernetes Secret, in a particular namespace, in a particular shape, named
  by a particular field of a particular custom resource. Creating a bucket does none of that, and
  the wiring is where the time goes and where the mistakes are.
- **One configuration silently destroys recoverability.** A base backup is only restorable together
  with the write-ahead log covering it. A bucket lifecycle rule that expires objects sooner than the
  oldest backup needs them leaves a backup set that looks healthy, lists fine, and cannot be
  restored — discovered during recovery, which is the worst moment to discover anything. Neither the
  vendor's CLI (which knows nothing about backups) nor the backup engine (which does not own the
  bucket policy) can see both halves. Burrow can.
- **An agent must not be able to delete the bucket.** Burrow's guardrails
  ([ADR-0006](0006-guardrails-in-the-control-plane.md), [ADR-0020](0020-guardrails-as-configurable-policy.md))
  are how an operation is made agent-safe. An operation performed with the vendor's CLI sits outside
  that entirely.
- **The bytes have meaning Burrow knows and the CLI does not.** "This app's backups, from these
  dates, this size" is a question about the registry, not about object keys.

None of that argues for Burrow becoming an object-storage client. It argues for Burrow owning the
seam between a bucket and the thing that writes to it, and nothing further.

## Decision

### 1. Object storage is a provider type, on the existing registry

`burrow provider add` gains an object-storage type. It reuses ADR-0023 exactly as built: the
credential goes into the single `burrow-credentials` Secret in the control-plane namespace, the
`providers` table records that the provider exists and which Secret key holds its credential, and
burrowd reads that key at call time through its `resourceNames`-restricted `get` on that one Secret.

**No new credential mechanism, no new Secret, no new RBAC.**

**The provider is addressed by S3-compatible endpoint**, not by vendor. Endpoint, region and bucket
are configuration; the vendor is whoever answers that endpoint. Burrow is not in the business of
maintaining a vendor list, and every vendor worth supporting speaks this API.

**One wrinkle this record must not gloss.** ADR-0023's Secret is one *opaque token* per provider,
and an S3 credential is a **pair** — an access key ID and a secret access key — plus non-secret
endpoint/region/bucket. The pair is stored as a single structured value under the provider's key,
with the non-secret parts in the `providers` row where they version cleanly and are inspectable
without reading a Secret. This is a small extension of ADR-0023's "opaque bag" and is called out
because a reader of that record will reasonably expect a bare token.

### 2. The scope is a backup destination, not an object-storage product

**What Burrow does:**

- Registers the credential and makes it reachable by the components that need it.
- Creates a bucket, when asked, with a shape appropriate to backups.
- Verifies the destination actually works — writes and deletes a probe object at configuration
  time, so a wrong key or a typo'd endpoint fails **now**, loudly, and not at the first scheduled
  backup, silently.
- Reports what Burrow put there, in Burrow's terms: which app, when, how large.

**What Burrow does not do, and this list is the decision:**

- No general object browser. No `cp`, no `sync`, no `ls` of arbitrary prefixes, no presigned URLs.
- No bucket policy, IAM, replication, or cross-region configuration surface.
- No object-storage features that exist for their own sake rather than to serve a Burrow capability.

If a user wants those, the vendor's CLI is better at them than Burrow will ever be, and pointing
there is the correct answer rather than a deficiency. **A capability enters Burrow's object-storage
surface only when a Burrow feature requires it** — not because the underlying API offers it.

### 3. Bucket lifecycle and backup retention are reconciled, and disagreement is refused

This is the invariant that justifies the feature existing.

Burrow knows how far back its backups are meant to be restorable. When it creates a bucket it does
not set an expiry that contradicts that, and when it is pointed at an existing bucket whose
lifecycle rules would expire objects a retained backup still depends on, it **refuses to proceed**
and says which rule and which backup.

The failure this prevents is silent, delayed, and total: the backup list looks correct, the restore
fails. Refusing at configuration time is the only point where it is cheap.

Where Burrow cannot read a bucket's lifecycle configuration — the credential may not be permitted
to, and some S3-compatible implementations differ here — it says so plainly rather than implying it
checked. An unverifiable invariant reported as verified is worse than one reported as unknown.

### 4. Destructive object-storage operations are guardrailed

Bucket creation and bucket deletion get guardrail codes, evaluated in the control plane like every
other guarded operation. Deletion is **denied by default**: the blast radius is every backup the
platform holds, and no agent-driven workflow has a legitimate reason to remove a bucket.

This is what makes the operation agent-safe, and it is the property the vendor's CLI cannot have.

### 5. One provider registration serves every consumer

Backups are the first consumer and the reason this exists. The registration is not backup-specific:
an add-on that needs durable storage ([ADR-0025](0025-building-block-addons.md)) uses the same
provider rather than acquiring its own credential.

Multiple providers may be registered — the registry is a table, not a singleton — so a user
migrating between vendors, or holding backups somewhere other than their primary object store, is
expressible rather than a special case.

## Consequences

- **Backups can leave the cluster**, which is the point, and the scheduling and retention gaps in
  ADR-0032 become worth closing because there is finally somewhere durable to close them against.
- **A new long-lived credential exists on the cluster.** It is in the same Secret, under the same
  restricted grant, as every other provider token — but it grants write access to the store holding
  every backup, which makes it the most consequential key in `burrow-credentials`. It should be
  scoped to one bucket at the vendor where the vendor permits that, and this is worth saying in the
  documentation rather than assuming.
- **A restore now depends on a third party being reachable.** That is the trade for surviving the
  cluster, and it is the right one, but "our backups are safe" becomes "our backups are safe if that
  vendor is up when we need them."
- **§3's reconciliation only works when Burrow can read the bucket's lifecycle configuration.**
  Where it cannot, the invariant degrades to a documented assumption, and §3 requires saying so.
- **The scope in §2 will be pressed.** "It already has a credential and a bucket, so could it
  just…" is a reasonable request that arrives repeatedly, and each instance is individually cheap.
  The list exists so that granting one is a deliberate amendment rather than a drift.
- **S3 compatibility is a spectrum, not a standard.** Endpoint-addressing means most vendors work;
  it does not mean all do, and multipart behaviour is where they differ most. A vendor is supported
  when it has been tested, not when it claims compatibility.

## Rejected alternatives

- **Tell users to run the vendor's CLI.** The honest baseline, and for actually managing object
  storage it is the better tool. Rejected because it does not do the part that matters: the
  credential still has to reach the backup engine in the right shape and place, nothing reconciles
  bucket lifecycle against backup retention, and nothing makes the operation agent-safe. It also
  leaves every user to discover the PITR-destroying lifecycle rule independently.
- **A full object-storage command surface.** Tempting because the API is right there and each
  addition is small. Rejected as the thing that turns a backup destination into a second-rate S3
  client — precisely the shape this project's users are trying to get away from, and a surface with
  no natural end.
- **Support one vendor properly rather than S3-compatible endpoints.** Simpler, and it would allow
  vendor-specific features. Rejected because it makes the vendor a dependency of the architecture
  rather than a configuration value, and because the S3 API is the one thing every candidate
  already speaks.
- **Keep backups on a PersistentVolumeClaim and rely on volume snapshots.** Cheaper, no credential,
  no third party. Rejected because it keeps the backup in the same failure domain as the database
  for the cases that matter most, and because snapshot retention is the operator's problem in a way
  that has repeatedly been discovered too late.
- **A dedicated Secret for object-storage credentials**, separate from `burrow-credentials`.
  Arguably tidier given this key is more consequential than the others. Rejected because it forks
  ADR-0023's model for one provider type and would need its own RBAC grant; the tighter answer is
  scoping the credential at the vendor, not moving it.

## Questions

- **Is the credential pair stored as structured data under one Secret key, or as two keys?** §1
  assumes the former to stay closest to ADR-0023's one-key-per-provider shape, but two keys is more
  legible to a human reading the Secret and avoids a parsing step. Either is defensible; the choice
  should be made before the first provider ships, because migrating it later is a credential
  rotation for every user.
- **Does bucket creation belong in Burrow at all, or only bucket *use*?** §2 permits creation, on
  the grounds that a correctly-shaped bucket is part of the job. The narrower alternative — require
  a bucket to exist, verify it, never create one — is a smaller surface and a defensible line, and
  it removes the need for any create/delete guardrail.
- **What does Burrow do when the object store is unreachable at backup time?** Retry, alert, fail
  the backup loudly? This record establishes the destination; it does not decide the failure
  behaviour, and that behaviour is what determines whether a silent outage becomes a silent gap in
  the backup history.
