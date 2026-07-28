# ADR-0065: What belongs on the agent surface — three tiers, by blast radius and reversibility

## Status

✅ Accepted

## TL;DR

[ADR-0049](0049-burrow-agent-scoped-cli-control-channel.md) established that the agent gets its own
narrow CLI. It did not say **what qualifies** a command to be on it, so each addition has been
argued from scratch and the surface has drifted: the agent can `attach` but not `detach`, `backup`
but not `restore` — and yet it can `addon remove`, which takes down every app at once.

This states the criterion and applies it. Two questions decide where a capability lands:

1. **Does its blast radius exceed the app the agent was asked about?**
2. **Can a human undo it?**

That yields three tiers, each mapping onto a mechanism that already exists:

| Tier | For | Mechanism |
| --- | --- | --- |
| **Absent from the binary** | no operator should ever want an agent doing this | the verb is not compiled in |
| **Denied by default** | legitimate for some operators, wrong as a default | guardrail disposition `deny` |
| **Confirmed by default** | routine but consequential | guardrail disposition `confirm` |

Three concrete changes follow: **`addon remove` leaves the agent binary**, and **`app.delete` and
`dns.delete` become `deny` by default** instead of `confirm`.

**A denied verb is better than an absent one wherever the risk allows**, which is why the middle
tier is the default answer rather than the first. An agent that hits `unknown command` may get
creative — reach for `kubectl`, or a shell. An agent that gets `denied: app.delete` knows precisely
what happened and that a human must decide, and can see the same limit ahead of time through the
read-only `guard` command.

Refines [ADR-0049](0049-burrow-agent-scoped-cli-control-channel.md) (which surface exists) and
[ADR-0020](0020-guardrails-as-configurable-policy.md) (which supplies the dispositions). Supersedes
nothing.

## Context

The agent surface today is 30 commands, enforced as a closed allow-list in
`cmd/burrow-agent/agent_surface_guard_test.go` so nothing joins it silently. The enforcement is
sound. The membership rule is missing.

The result is a surface with a clear pattern and one exception. Additive operations are on it and
their destructive counterparts are not — `attach` without `detach`, `backup` without `restore`. Then
`addon remove` sits there anyway, and it is the single most destructive verb in the product:
add-ons are **one per type per cluster** (`InstallAddon` takes a type, and `addons.name` is its
primary key), and ADR-0031 puts every app's database on that one shared Postgres. So `addon remove`
is not "remove an add-on"; it is "remove **the** add-on", taking out every attached app at once.
There is no configuration in which its blast radius is small.

`app delete` is the other outlier, for a different reason. Its radius is correctly scoped — one app,
the one the agent was asked about — but it destroys the release history along with the workload and
routing, so there is no rollback afterwards. It is `confirm` by default, which protects a human who
reads the prompt.

**Two mechanisms already exist for keeping a capability away from an agent, and they are not
interchangeable.** Absence from the binary is absolute and unconfigurable: an operator who
legitimately wants the behaviour cannot have it, and the agent cannot discover why. A `deny`
disposition is configurable per environment, visible to the agent through the read-only `guard`
command, and — crucially — **not something the agent can change**, since `guard set` is operator
CLI only, run with the admin kubeconfig.

That last property is what makes the middle tier trustworthy, and it is worth naming as load-bearing
rather than incidental.

## Decision

### 1. The criterion

A capability qualifies for the agent surface unless it fails one of two tests:

- **Scope.** Its effect reaches beyond the app the agent was asked to work on.
- **Reversibility.** A human cannot restore the prior state afterwards.

Failing **scope** is disqualifying outright — tier 1. Failing **reversibility** alone is not; it
means the operator decides, so it defaults to denied — tier 2.

### 2. Tier 1 — absent from the binary

Reserved for capabilities where **no operator should want an agent to hold them**, so configurability
would be a liability rather than a feature. A verb in this tier is not compiled into
`burrow-agent`, and its absence is asserted by the surface-guard test rather than left as a property
of the current command tree.

**`addon remove` moves here.** It fails scope unconditionally: one shared instance, every app's data
behind it, and no legitimate agent workflow that requires removing it. `--delete-data`
([ADR-0064](0064-addon-removal-keeps-its-data.md) §2) is already in this tier.

### 3. Tier 2 — denied by default

The **default answer** for a capability that fails reversibility but not scope. Shipped as a
guardrail with disposition `deny`, which an operator may relax per environment.

Two changes to current defaults:

- **`app.delete`: `confirm` → `deny`.** Deleting an app destroys its release history, so `confirm`
  protects only an attentive reader. An operator who wants the agent tidying up its own work turns
  it on deliberately.
- **`dns.delete`: `confirm` → `deny`.** Removing a public DNS record takes an application off the
  internet, and the record may not be one Burrow created.

Both remain fully available to the human operator CLI, which these dispositions do not gate.

**A tier-2 default is a floor, not a fixed setting, and the expected usage is per environment.**
`app.*` codes are environment-scopable, so the shape an operator actually wants is a gradient —
`allow` in development, `confirm` in staging, `deny` in production — and the `deny` default is
simply where an environment sits until someone says otherwise. A safe default that is relaxed
deliberately, per environment, is the point of the tier.

**`dns.delete` cannot express that today, and this record does not pretend otherwise.**
`EnvScopable` keys on the `app.` prefix, so `dns.*`, `addon.*` and every other non-`app.` code is
cluster-wide only. Its `deny` is therefore all-or-nothing: an operator who wants the agent managing
DNS in development but not production must choose one answer for both. That is a real limitation of
this decision, it argues for widening environment scoping beyond the `app.` prefix, and it is a
separate change this record does not make.

### 4. Tier 3 — confirmed by default

Unchanged, and named so the tiers are complete: consequential but routine operations stay at
`confirm`, where the agent must surface the operation to a human and re-run with explicit approval.
`app.run` is the clearest case — arbitrary commands, but inside the app's own image and environment,
and the whole point of [ADR-0048](0048-one-off-command-runner.md).

### 5. Prefer tier 2 to tier 1 where the risk allows

Absence is the blunter instrument and should be the exception. A denied verb produces a legible
refusal the agent can relay and can anticipate via `guard`; an absent verb produces `unknown
command`, which is a dead end that invites an agent to route around the control channel entirely —
the exact failure ADR-0021 says Burrow cannot close from the inside.

Tier 1 is therefore for capabilities whose worst case is unbounded, not merely bad.

### 6. New capabilities state their tier

A command added to the agent surface names its tier and the test it passes, in review. The
surface-guard test already forces the surface to be edited deliberately; this makes the *argument*
explicit rather than reconstructed later from what happened to be allowed.

### 7. The agent can see what it cannot do, including what is absent

`guard` reports denied verbs today. It also reports the capabilities that are **absent from the
binary** and why, so an agent can tell a human "removing an add-on is not something I can do, and
here is who can" rather than reporting an unhelpful `unknown command`.

This is what makes tier 1 tolerable rather than merely safe. An absent verb is otherwise a dead end
(§5), and a dead end is what pushes an agent toward routing around the control channel. A verb that
is absent *and legible* is a refusal the agent can relay, which is the whole behaviour this record
wants.

It does enumerate the surface to anything that can read it. That is accepted: the surface is already
public in an open-source CLI, and an attacker learns nothing from it that `--help` does not give
them.

## Consequences

- **The most destructive verb leaves the agent surface**, and the remaining destructive ones are off
  by default rather than one unread prompt away.
- **An agent can create apps it cannot remove.** With `app.delete` denied, apps the agent provisioned
  and no longer needs accumulate until a human intervenes — the same shape as the retained volumes in
  ADR-0064 §6, and accepted for the same reason: an unnecessary app costs money, a wrongly deleted
  one costs data. It is a real cost and operators who feel it can flip the disposition.
- **Changing a default changes behaviour for operators who never set it**, which is exactly the
  population intended. An explicit disposition already in the policy table is untouched, so anyone
  who deliberately chose `confirm` keeps it.
- **The middle tier's integrity depends entirely on `guard set` staying off the agent surface.** If
  that ever changes, both tier-2 entries become advisory in a single step. It is also currently
  **unaudited**, so a change to it leaves no trace — worth closing on its own merits, and this record
  raises the stakes.
- **Tier 1 is unappealable.** An operator with a genuine need for agent-driven add-on removal has no
  recourse but to run the operator CLI. That is the intended trade and it will occasionally be the
  wrong one for somebody.
- **The criterion will be argued about**, which is the improvement. Today the argument happens
  implicitly and its outcome is a diff; this makes it a stated test a reviewer can apply.

## Rejected alternatives

- **Remove every destructive verb from the binary.** Simple and maximally safe, and it forecloses
  legitimate operator choices while producing dead-end failures that push a determined agent toward
  `kubectl`. The scoped credential (ADR-0038) confines it there, but a refusal it can understand is
  better than one it cannot.
- **Leave everything at `confirm` and rely on the human.** The status quo. A confirmation is a real
  control only for someone who reads it, and the cases here are exactly the ones where the reader is
  busy, mid-incident, or trusting the agent's summary of what it is about to do.
- **A single "destructive" flag on commands** rather than three tiers. Simpler to describe and it
  collapses the distinction that matters: unbounded blast radius and merely irreversible warrant
  different mechanisms, and one flag cannot express both.
- **Per-command policy instead of guardrail codes.** More precise — `addon remove` and
  `addon remove --delete-data` could differ — and rejected because it introduces a second policy
  surface next to guardrails. ADR-0064's open question about a separate code for `--delete-data` is
  the narrower version of this and the right place to settle it.
