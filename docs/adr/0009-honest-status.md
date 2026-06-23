# ADR-0009: Honest status — docs never describe unbuilt behavior as done

## Status

Accepted.

## Context

Burrow is being designed in the open, ahead of the code. The docs, ADRs, README, and
roadmap describe a system that is mostly not built yet. There is a strong, constant pull —
especially in a README or a pitch — to write about intended behavior in the present tense,
as if it already works. That is how projects end up with documentation that quietly lies:
a reader cannot tell what they can rely on today from what is merely planned.

Because an autonomous agent is a primary reader of Burrow's surface (tool descriptions,
results), dishonest status is not just a marketing smell — it actively misleads a caller
that will *act* on what it reads.

## Decision

**Everything in the docs is a goal until it ships. Never describe unbuilt behavior as
done.** Documentation, READMEs, ADRs, tool descriptions, and marketing distinguish, at all
times, between what is shipped and what is planned.

Concretely:

- **The README carries a version status table** — `🚧 in progress` / `✅ shipped` /
  `planned` — that never lags the code. The version under active work is in progress;
  shipped versions are marked shipped (linked to their release); later versions are
  planned.
- **Unbuilt behavior is written in a planned/future voice**, never the present tense of a
  working feature. "Burrow will build from a git reference" — not "Burrow builds from a git
  reference" — until it does.
- **ADRs record decisions, not status** ([docs/adr/README.md](README.md)). An accepted ADR
  describing unbuilt behavior is normal and correct; it does not claim the code exists.
  Status lives in the README table, the roadmap, and the plan — not in ADR prose.
- **Tool descriptions and structured results describe only what the tool actually does
  today.** A stubbed or partial operation says so honestly rather than implying success.

## Consequences

- A reader (human or agent) can always tell what they can rely on now from what is coming.
- The README status table is a maintained artifact, updated as part of each release — when
  a version ships, its row flips to shipped and links to the release in the same change set
  that cuts it.
- Doc reviews include a status-honesty check: any present-tense claim must correspond to
  shipped, tested behavior.
- This discipline is also a CLAUDE.md convention so every contributor and agent applies it;
  this ADR is the decision behind that convention.

## Rejected alternatives

- **Write aspirationally in the present tense and fix the docs later.** Rejected: it
  produces documentation that misleads exactly the readers — including autonomous agents —
  who most need to trust it, and "later" rarely comes before someone is burned.
- **Track status inside ADRs ("implemented / not yet").** Rejected: it conflates the
  decision record with the build tracker, and ADRs are immutable while status changes —
  the two must not share a home ([docs/adr/README.md](README.md)).
