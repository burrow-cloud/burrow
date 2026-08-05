# ADR-0087: Running SQL against an attached database

## Status

✅ Accepted

## TL;DR

`burrow addon sql` run a statement against an app's database, give back rows not text. Denied by
default.

- **Burrow has no way to query a database it provisioned.** Only route today: `app run` with `psql`,
  which need `psql` in the app image and hand back a text blob.
- **burrowd run the statement**, using the credential it already minted for that app. No port
  forward, no laptop reach the database.
- **Result is columns and rows**, so an agent can read it. CLI draw a table, `--json` give the rows.
- **Relational add-ons only.** `cache`, `logs`, `metrics` take no statement — they already have their
  own query verbs. Query surfaces belong to the add-on type.
- **New guardrail `addon.sql`, denied by default** — [ADR-0065](0065-what-belongs-on-the-agent-surface.md)
  §3 tier 2. Operator open it per environment.
- **Burrow never decide read versus write.** Telling them apart need a parser and a model of every
  function; a wrong answer let a write through a gate named read. So one code, one default: deny.
- **Bounded**: statement timeout, row cap, one connection. A query cannot sit on the instance.
- **Statement text land in the audit log**, so a literal in a query is recorded. Named as a cost.

Extends [ADR-0020](0020-guardrails-as-configurable-policy.md)'s guardrails and states its tier under
[ADR-0065](0065-what-belongs-on-the-agent-surface.md) §6. Builds on
[ADR-0031](0031-postgres-addon.md)'s database-per-app and [ADR-0066](0066-postgres-on-cloudnativepg.md)'s
instances. Sibling to [ADR-0048](0048-one-off-command-runner.md), which made the same honesty argument about
`app run`. Supersedes nothing.

## Context

**What exists today.** Burrow provisions Postgres. `addon install` creates an instance,
`addon attach` creates a database and a role for an app and writes a `DATABASE_URL` into the app's
Secret ([ADR-0031](0031-postgres-addon.md)). Burrow backs it up, restores it, reports its health, and
rewrites the DSN after a recovery. It cannot read a single row out of it.

**What breaks.** The only way to run a query is:

```sh
burrow app run web -- psql "$DATABASE_URL" -c 'select * from users limit 10'
```

That is a real answer and it is the wrong shape in three ways. It needs `psql` **in the application
image**, which is a production image and has no reason to carry a database client. It returns
`app run`'s combined stdout and stderr — a **text blob**, so the one Burrow verb whose entire output
is tabular data is also the only one an agent cannot parse. And it runs the statement **inside the
app's runtime**, so a database that exists but whose app will not start is unreachable, which is
exactly the state somebody most wants to query it in.

There is no port-forward and no proxy. Nothing lets a laptop open a connection to the instance, and
that is deliberate — the database is reachable from inside the cluster, and adding a tunnel would
make the operator's kubeconfig the credential for tenant data.

**What this record resolves.** A first-class way to run a statement and get rows back, and what
gates it.

**The forcing question, and the reason this record is short on cleverness.** The obvious design is
two dispositions: let reads through, hold writes. It does not survive contact with SQL. A statement's
effect is not a property of its leading keyword:

- `WITH deleted AS (DELETE FROM users RETURNING *) SELECT * FROM deleted` is a `SELECT` that deletes.
- `SELECT do_the_thing()` is a `SELECT` that is whatever the function is, including a function that
  writes, one marked `SECURITY DEFINER` that writes as its owner, or `pg_terminate_backend`.
- `SELECT … FOR UPDATE` takes locks that block every other writer.
- `COPY … TO PROGRAM` and `DO $$ … $$` are not queries at all.

Classifying these correctly is a parser plus a semantic model of every function reachable from the
statement, re-derived whenever the schema changes. That is a SQL firewall.
[ADR-0048](0048-one-off-command-runner.md) §5 already refused to build one for `app run` and said so plainly:
*"this is a command runner, not a SQL firewall."* The same refusal applies here, and it is worth
stating that the failure mode of trying is **worse than not having the feature**: a gate labelled
"reads are allowed" that lets a `DELETE` through is more dangerous than no gate, because people
trust it.

## Decision

### 1. `burrow addon sql <addon> <app>` runs a statement against that app's database

The statement is supplied with `--statement`/`-c`, or on stdin, or from a file with `--file`. The
addon and the app together name the database, the same pair `addon attach` and `addon detach`
already take.

It targets **one app's database**, never the instance. There is no form of this command that reaches
the whole instance, `template1`, or another app's database — the database-per-app boundary
([ADR-0031](0031-postgres-addon.md)) is the boundary here too, and a statement that could cross it
would make one app's guardrail meaningless.

**Both arguments are load-bearing, which is worth stating because one of them looks redundant.**
`<addon>` names a **type** (`postgres`), not an instance and not a database: `addon install postgres`
provisions one instance per environment, and `addon attach postgres web` creates a database and a
role **per app** on it. So an instance holds as many databases as it has attached apps, and
`addon sql postgres` on its own names a server rather than a target. Dropping `<app>` would leave
the command to pick a database itself — which means connecting as a superuser and letting the
statement choose, the one shape this section rules out.

### 2. It applies to relational add-ons only, and says so when it does not

`sql` is defined for add-ons that speak it. Today that is `postgres` alone; `cache` (ValKey), `logs`
(VictoriaLogs) and `metrics` (VictoriaMetrics) are the other three installable types and none of them
takes a statement.

This is not a special case bolted on — it is the shape Burrow already has. `addon logs [query]` and
`addon metrics <query>` exist, and each speaks its own store's query language. **Query surfaces are
per-add-on-type**, and `sql` is the relational member of that set rather than a general facility that
happens to work on one type.

Asked for an add-on that is not relational, it refuses by naming the verb that add-on does take, so
the refusal teaches the shape instead of merely reporting a mismatch. A future relational add-on
gains `sql` by being relational; nothing about this command is Postgres-specific beyond the dialect
the statement is written in, which is the caller's concern rather than Burrow's.

### 3. burrowd runs it, with the credential it already minted

burrowd generated the DSN it wrote into the app's Secret, so it needs no new secret and no new grant
to connect. It opens a connection to the instance, runs the statement, and closes it.

Two things follow, and both are the point. **No connection to the database ever leaves the cluster**
— there is no port-forward, no tunnel, and no path that turns an operator's kubeconfig into a
credential for tenant data. And the statement runs **independently of the application**, so a
database whose app is crash-looping is still queryable, which is the case `app run` cannot serve.

It connects as the **app's own role**, not as a superuser. What the statement may touch is what the
application itself may touch, and nothing this command does raises that.

### 4. The result is columns and rows

The response carries the column names, the rows, the row count, and whether the result was truncated.
The CLI renders a table for a human; `--json` returns the rows, so an agent composes on the result
rather than parsing a rendering of it.

A statement returning no rows returns its command tag and the number of rows affected. An error
returns Postgres's own message, `SQLSTATE` included, unmodified — a database error is an outcome,
not a CLI failure, the same treatment [ADR-0048](0048-one-off-command-runner.md) §3 gives a non-zero exit code.

### 5. A new `addon.sql` guardrail, denied by default

Denied by default. Under [ADR-0065](0065-what-belongs-on-the-agent-surface.md) §6, which requires a
new capability to state its tier: this is **tier 2 — denied by default** (§3), not tier 1. The verb
is compiled into `burrow-agent` and the agent can see that it exists and that it is denied, because
an agent that knows a capability is closed asks for it rather than inventing a way around it.

The default is deny rather than confirm because **there is no upper bound on what a statement does**.
`app run`'s confirm default (ADR-0048 §4) is defensible because a human reads a command line and
recognises `npm run migrate`; a human reading a hundred-line statement is not meaningfully
approving it. Where a confirmation cannot be an informed one, holding for confirmation is theatre.

It is env-scopable, and the expected shape is a gradient set with
`guard set --env <env> addon.sql …`: allow in development, where the database is disposable and the
agent inspecting it is the whole value; confirm or deny in production.

### 6. Burrow does not classify a statement as a read or a write

There is **one** guardrail code and it gates whether the statement runs, not what it does. Burrow
does not parse the statement, does not branch on its leading keyword, and does not report it as a
read or a write.

This is the same honesty ADR-0048 §5 applies to `app run`, and the Context sets out why the
alternative is not merely expensive but actively harmful: a classifier that is wrong in the
permissive direction is a gate people trust and should not.

**The one credible path to a softer default is deferred, and named so nobody re-derives it.**
Postgres can enforce read-only itself — a statement run in a `READ ONLY` transaction is refused by
the database if it writes, with no parser and no judgement on Burrow's part. A future
`--read-only` mode with its own disposition is therefore possible in a way that classification is
not, because the enforcement belongs to the engine rather than to us. It is not in this record: it
needs its own decision about what it does to the volatile-function and lock cases, and shipping it
alongside the unrestricted form would blur which of the two the guardrail is about.

### 7. Every run is bounded

A **statement timeout**, so a query cannot sit on the instance holding locks. A **row cap** on what
is returned, with truncation reported rather than silent. A **single connection**, closed when the
statement finishes.

These are operational limits rather than guardrails — the distinction
[ADR-0068](0068-operational-limits-are-configuration.md) §2 draws for the replica ceiling. They are not dispositions and
`guard set` does not reach them; they exist so that this command cannot take an instance down, which
is a different concern from whether the caller was allowed to run it.

## Consequences

**Burrow can finally answer a question about data it stores.** Every other verb reports on the
platform's own state; this is the first that reads the application's. That is the point, and it is
also the reason the default is deny.

**The statement text is recorded in the audit log** ([ADR-0027](0027-audit-log.md)). That is what
makes the capability accountable, and it means a literal in a query — an email address, a token
somebody pasted into a `WHERE` clause — is written to the audit table. Anyone with audit read access
sees it. This is stated rather than mitigated: redacting literals means parsing the statement, which
is the thing §5 refuses to do.

**burrowd opens connections to tenant databases.** It already holds the credentials, so this grants
nothing new, but it is a new runtime behaviour: burrowd's own database was previously the only one it
connected to. The bounds in §6 exist because of this.

**A denied-by-default capability is invisible in practice until somebody opens it.** Most installs
will never turn it on, and the feature's value is concentrated in development environments where an
operator sets it to allow. That is the intended shape, and it means adoption will look low in a way
that is not a signal of anything.

**`app run` with `psql` still works and is not deprecated.** It does something this does not — runs
an arbitrary program in the app's runtime, including a migration tool that is not SQL. The two
overlap only where somebody was using `app run` as a query tool.

## Rejected alternatives

**Two dispositions, one for reads and one for writes.** The design everybody reaches for first, and
the Context is the argument against it: a `SELECT` can delete, a function call can be anything, and
the classifier that gets this right is a parser plus a model of every reachable function. Its failure
mode is a gate labelled safe that is not. The read-only *transaction* (§5) is the version of this
idea that works, and it works precisely because Postgres does the judging.

**A port-forward or a proxy — `burrow addon proxy`.** What a person coming from `kubectl` expects,
and it is the most useful version of this for a human with a database client. Rejected here because
it makes the operator's kubeconfig into a credential for tenant data and puts a long-lived tunnel
between a laptop and an instance, which is a materially larger surface than running one bounded
statement. It is a separate decision, not a variant of this one, and it does not serve the agent at
all — a tunnel returns a socket, not rows.

**Exec into the CloudNativePG primary and run `psql` there.** Cheap, since the operand image already
has a client. It returns text, which fails the one requirement that motivated this; it runs as the
instance's superuser, which is broader than the app's own role; and it makes the command depend on
the operand image's contents.

**A `psql` sidecar or an init container in the app.** Puts a database client in a production image to
serve an operator's occasional need, and does nothing for an app that will not start.

**Leaving it at `app run` with `psql` and documenting the recipe.** The status quo, and defensible
until the output shape is considered: Burrow's other verbs return structured results specifically so
an agent can reason over them ([ADR-0003](0003-agent-neutral-mcp-control-surface.md)), and this is the
one place where the data *is* the answer. A documented recipe that returns a text blob is a
documented gap.
