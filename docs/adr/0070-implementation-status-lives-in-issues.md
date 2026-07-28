# ADR-0070: Implementation status lives in issues, created on acceptance

## Status

🟡 Proposed

## TL;DR

ADRs record decisions, never whether the code exists ([ADR-0009](0009-honest-status.md)). That leaves
a real question — *is this built?* — and the answer has been kept in **three hand-maintained prose
documents**: `ROADMAP.md`'s "Decided, not yet built", `CAPABILITIES.md`'s equivalent table, and
`PLAN.md`'s sequencing. They drift independently, and every drift found in the 2026-07-27 sweep was
a human forgetting to edit a sentence — the README claiming a Proposed ADR had shipped,
`CAPABILITIES.md` calling an accepted ADR Proposed.

**Implementation status moves to GitHub issues labelled `adr`, opened when an ADR is accepted.** An
issue closes itself when a pull request says `Closes #N`. A sentence does not, and that is the whole
argument.

Three rules make it work: **one issue per implementable unit** rather than per ADR, because partial
implementation is what prose handles worst; **the issue links to the ADR and never the reverse**,
since an accepted record cannot be edited and one accruing pointers to its own implementation is
tracking status again; and the prose is **trimmed, not deleted**, so the repository still describes
itself to someone reading it offline.

Refines the `docs/adr/README.md` convention that sends decided-but-unbuilt work to `ROADMAP.md` and
`PLAN.md`, and serves [ADR-0009](0009-honest-status.md), whose standard the prose trackers kept
failing. Supersedes nothing.

## Context

### What exists today

- **`docs/adr/README.md`** instructs: "track decided-but-unbuilt work in `docs/ROADMAP.md` and
  `docs/PLAN.md`, or as a skipped/failing test that names the ADR."
- **`ROADMAP.md`** has a "Decided, not yet built" section — prose entries naming the ADR and what the
  code does instead.
- **`CAPABILITIES.md`** has a "Decided but not built" table covering the same ground.
- **`PLAN.md`** sequences themes, referring to the same decisions again.
- The repository has issues and labels, used for bugs and features, not for ADR implementation.

### What breaks

**The same fact is written three times and maintained zero times.** Each document is updated by
someone remembering to, and each has a different audience, so they diverge quietly rather than
visibly.

A sweep on 2026-07-27 found, among others: the README's shipped list including a decision that was
still Proposed and refused at runtime; `CAPABILITIES.md` describing an ADR as "Proposed but
implemented" after it had been accepted; and `ROADMAP.md` and `PLAN.md` describing a released version
as unreleased. None of these were disagreements about the code. All were prose left behind by a
change.

**And the failure is silent.** A stale sentence looks exactly like a current one. Nothing fails, no
test breaks, and the reader most likely to be misled — someone new, or an agent reading the tree — is
the one least able to tell.

**Partial implementation is worse still.** ADR-0064 shipped four sections of six; ADR-0065 shipped
one change of three. In prose that becomes a judgement call about whether an entry says "built" or
"not built", and both are wrong.

### What this record resolves

Where the answer to "is this built?" lives, who updates it, and what remains in the repository.

## Decision

### 1. An accepted ADR gets issues, labelled `adr`

On acceptance — not before (§4) — the implementable work becomes GitHub issues carrying the label
`adr`. The open set under that label **is** the decided-but-unbuilt list.

The mechanism is the point: a pull request that says `Closes #N` updates the status by merging.
Nobody has to remember, which is the failure mode every prose tracker shares.

### 2. One issue per implementable unit, not per ADR

ADR-0064 needed three; ADR-0065 needed two. An ADR is a decision and may contain several separable
pieces of work, with different sizes and different blockers.

This is also what makes partial implementation representable. Under one-issue-per-ADR, a record with
four of six sections built is either "open" or "closed" and neither is true.

### 3. The issue links to the ADR; the ADR never links to the issue

An accepted ADR is immutable but for its Status line, so it cannot gain the link afterwards — and it
should not gain it beforehand either. A record that points at its own implementation is tracking
status by another name, which is exactly what ADR-0009 forbids.

The reference runs one way: an issue names its ADR and section.

### 4. Issues come after acceptance

A draft can change materially in review, and does — sections have been rewritten between draft and
acceptance more than once. An issue opened against a draft describes work that is not yet decided,
and someone may start it.

There is a second reason, and it is the more important one: **an open issue creates momentum.** If
the work is already tracked, accepting the record starts to feel like a formality rather than the
point at which errors are caught. The gate is worth more than the head start.

### 5. Sequencing uses issue dependencies, not prose

Where one ADR's work blocks another's, that is recorded with GitHub's `blocked_by` relationship
rather than a sentence. It is queryable, it survives editing, and it is visible to whoever picks up
the work rather than only to whoever wrote the note.

### 6. The prose is trimmed, not deleted

`ROADMAP.md` keeps a "Decided, not yet built" section: themes, sequencing, and one line per decision
naming its issues. `CAPABILITIES.md` keeps its table, shortened to a decision summary and an issue
reference.

**A repository should describe itself to someone reading it offline**, and to an agent reading the
tree without network access. Moving everything to issues would trade one failure — prose that goes
stale — for another: a repository that cannot answer a basic question about itself without an API
call. Themes stay in-repo; per-unit granularity moves out.

### 7. A bug does not wait for an ADR

A defect gets an issue immediately, whatever decision may later be recorded about it. ADR-0067's
provisioner collision — two environments silently sharing one database — was a defect from the moment
it was found, and would have deserved an issue before any topology decision existed.

## Consequences

- **The decided-but-unbuilt list stops drifting**, because it is no longer a list anyone maintains.
- **Some of the repository's self-description moves to GitHub.** §6 keeps the important half in-repo,
  but the detail now lives somewhere a clone does not carry. That is a real loss for offline reading
  and for anyone who dislikes depending on a forge, and it is the price of the mechanism.
- **Issues can still go stale**, just differently: one whose ADR is later superseded needs closing by
  hand, and nothing prompts that. The failure moves rather than disappearing entirely.
- **Acceptance gains a step.** Accepting an ADR now means opening its issues too, and skipping that
  step leaves work invisible — the same class of omission as forgetting to update the prose, though
  a smaller one because it happens once rather than continuously.
- **The label becomes load-bearing.** An issue that should carry `adr` and does not is absent from
  the list, and nothing notices.
- **Sequencing is now real data.** `blocked_by` can be queried and displayed, where the prose version
  was a sentence someone had to find and believe.

## Rejected alternatives

- **Keep the prose trackers and be more disciplined.** No new dependency, everything stays in the
  repository, and it is what the existing convention already asks for. Rejected because discipline is
  precisely what failed — repeatedly, silently, and in three places at once. A process that works
  only when nobody forgets is a process that does not work.
- **A single prose tracker instead of three.** Fixes the divergence between documents without moving
  anything out of the repository, and is genuinely better than today. Rejected because it does not
  fix the underlying problem: one document still goes stale, just alone, and it still cannot close
  itself when the work merges.
- **Track status in the ADR itself**, as a Status line richer than Proposed/Accepted. The most
  discoverable option — the answer would sit with the decision. Rejected because it makes accepted
  ADRs mutable in exactly the way [ADR-0009](0009-honest-status.md) and the immutability convention
  forbid, and because a record that changes as code lands is no longer a record of a decision.
- **A skipped or failing test naming the ADR**, which the current convention already offers as an
  option. Elegant for a single behaviour, and it lives in the repository. Rejected as the general
  answer because most decisions are not expressible as one failing assertion, and a permanently
  skipped test is its own kind of noise.
- **Move everything to issues and delete the prose.** Cleaner, no duplication at all. Rejected by §6:
  a repository that cannot answer "what is decided but not built" without network access has traded
  a maintenance problem for an accessibility one.
