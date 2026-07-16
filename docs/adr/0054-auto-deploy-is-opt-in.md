# ADR-0054: Auto-deploy is opt-in — the default level is off

## Status

✅ Accepted

## TL;DR

Auto-deploy is **opt-in**: an app with no explicitly-set level is `off`, and the pull-based
watcher never polls or moves it until an operator sets a level. This revises
[ADR-0052](0052-pull-based-passive-deploy.md) §2/§5, which made `minor` the default and turned
auto-deploy **on** for every app; everything else in ADR-0052 — the level semantics, the guarded
deploy path, the surfaced available upgrade, the rollback safety stop, and the conservative
cadence — is unchanged. Supersedes ADR-0052 only on the default: the new default is `off`.

## Context

ADR-0052 decided that auto-deploy is on by default at `minor`, applying to every app "new or
already deployed." That choice has a sharp edge on upgrade, reported as issue #270: when a cluster
is upgraded to a version that carries the poller, a **pre-existing** app that never opted into
auto-deploy immediately begins being polled. Because the poller lists the registry anonymously and
has no read credential of its own yet ([ADR-0040](0040-burrowd-never-contacts-the-registry.md); the
poller's read-auth story is tracked separately as issue #279), a private repository answers `401`,
and the watcher logs that failure every interval — recurring noise for an app the operator never
asked to auto-deploy.

The deeper problem is not the log line but the surprise: an on-by-default watcher means installing
or upgrading Burrow silently changes what may deploy to an app without the operator choosing it.
"What deploys unattended" is exactly the class of decision Burrow keeps in human hands
([ADR-0038](0038-scoped-agent-credential.md) withholds it from the agent for the same reason). A
default that turns it on for every existing app, at upgrade time, with no opt-in, is the wrong side
of that line — and it contradicts ADR-0052's own title, which calls the watcher "opt-in."

## Decision

**The default auto-deploy level is `off`.** An app with no stored level row reads `off`, so:

- The watcher's per-app reconcile reads the level first and returns before contacting the registry,
  so an app that never opted in is **never polled** — no tag listing, no `401`, no log noise.
- Turning auto-deploy **on** is a deliberate operator action: `burrow app auto-deploy <app>
  <patch|minor|major>` (per environment, [ADR-0052](0052-pull-based-passive-deploy.md) §6). Until
  that is set, only the explicit guarded deploy ships a release — the canonical path
  ([ADR-0007](0007-explicit-deploy-by-image-reference.md)).

This changes only the default. When an operator opts in, every other ADR-0052 behavior holds
unchanged: upgrades-only movement within the level's cap, an above-cap tag surfaced as an available
upgrade rather than taken, the rollback/downgrade safety stop that sets `off` with a reason, the
same guarded rollout/record/rollback/audit, and the conservative jittered cadence.

## Consequences

- **No app is auto-deployed without an explicit opt-in.** Upgrading a cluster to a poller-carrying
  version is inert for existing apps: nothing is polled or moved until an operator sets a level.
  This removes the post-upgrade `401` noise (#270) at its source, since an off app is skipped before
  any registry call.
- **Opt-in is one command.** `burrow app auto-deploy <app> minor` turns it on with the same semantics
  ADR-0052 specified; the CLI help and docs now describe auto-deploy as opt-in.
- **The release-notes migration ADR-0052 anticipated is no longer needed** — existing apps do not
  begin auto-taking minors on the day the poller ships, because they stay `off` until opted in.
- **The poller's read-credential story is unchanged and still open** (issue #279): a genuinely
  opted-in private-repository app still lists anonymously and may `401`. That failure is now logged
  once per distinct error rather than every interval, so a standing fault is not spammy; wiring read
  auth remains future work tied to provider credentials.

## Rejected alternatives

- **Keep on-by-default (ADR-0052 as written).** Rejected: it changes what may deploy to a
  pre-existing app at upgrade time without the operator choosing it, and produces recurring registry
  errors for apps that never opted in — the surprise #270 reports.
- **Default off only for pre-existing apps, on for newly-created ones.** Rejected as a hidden,
  time-dependent rule: "was this app created before or after the feature shipped" is invisible state
  the operator cannot see or reason about. A single, uniform default (`off`, opt-in) is predictable;
  new and old apps behave identically.
- **Suppress the poller's registry errors instead of changing the default.** Rejected as treating the
  symptom: silencing the `401` would still leave every existing app being polled and potentially
  auto-deployed without opting in. The log de-duplication is kept as a minor improvement for the
  genuinely opted-in case, but it is not the fix.
