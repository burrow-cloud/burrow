# ADR-0097: Guardrails hold the agent, and nobody else

## Status

✅ Accepted

## TL;DR

Guardrails exist to stop an agent going off the rails. They never applied to people in any useful
sense, so stop pretending they do. Drop `--binds` from the CLI.

- **A disposition binds the agent.** Nothing else. `user` and `machine` credentials are allowed
  everything, always, with no confirmation.
- **Blocking a person was never real.** Their Kubernetes RBAC is the ceiling on what they can do, and
  anything Burrow refuses them they can do with `kubectl` a second later.
- **`--binds` goes.** It made every operator answer a question with one correct answer, and got it
  wrong by omission — a `deny` written without it silently froze the human too.
- **Credential kinds stay.** The server has to know who is asking; that is what makes "hold the
  agent" expressible at all.
- **Team-level restrictions are not Burrow's job.** Kubernetes RBAC already does that, and an
  operator wants the agent to be a subset of a person's access, which is what this gives them.

Supersedes [ADR-0094](0094-a-guardrail-can-bind-the-agent-and-leave-the-human-alone.md).

## Context

### What exists today

A guardrail disposition is one of `allow`, `confirm` or `deny`, stored against a code like
`app.delete`. Until recently a `deny` refused **every** caller — the agent, the operator at a
terminal, and a CI job alike.

That was a known defect: the whole point of a guardrail is to hold an over-eager agent, and instead it
also froze the person trying to repair the cluster. The fix added a fourth axis to the policy key, the
kind of credential the caller holds, and a `--binds` flag to set it:

```
burrow guard set app.deploy --env prod --name burrowd-cloud --binds agent deny
```

Written without `--binds`, the same command still binds everyone.

### What breaks

**The flag asks a question with one right answer.** Every real use of a guardrail means "stop the
agent". An operator who omits the flag does not get a neutral default — they get the old defect back,
silently, on the one command where they were trying to make things safer.

**It reads as an access-control system, and it is not one.** Three credential kinds and a binding flag
look like the beginnings of per-team permissions. They are not, and they must not become that: a
person's Kubernetes RBAC is already the hard ceiling on what they can do. Burrow refusing an operation
their RBAC permits buys nothing, because `kubectl` is right there.

**It confuses the person it is meant to serve.** The first question anyone asks on meeting the flag is
"which one do I want", and the answer is always the same. A setting with one correct value is not a
setting.

### What this record resolves

Whether a guardrail can bind anybody other than the agent. It cannot. The flag that implied otherwise
goes, the credential kinds that make the distinction expressible stay, and the disposition table means
one thing: what the agent may do.

## Decision

### 1. A disposition applies to the agent

Every guardrail disposition is evaluated for callers holding an **agent** credential. For `user` and
`machine` credentials the answer is always `allow`, with no confirmation, whatever the table says.

The policy key loses its credential-kind axis. `prod.burrowd-cloud.app.deploy` means what it looks
like it means, and there is no `agent:` prefixed variant to reason about.

### 2. `--binds` is removed from `guard set`

The flag is gone. A disposition is set the way it reads:

```
burrow guard set app.deploy --env prod --name burrowd-cloud deny
```

and that denies the agent while leaving every person able to deploy.

An existing policy row carrying a kind-bound key is read as an ordinary row for the same code — the
binding was only ever `agent` in practice, and that is now the meaning of every row.

### 3. Credential kinds remain

The server still records and reads whether a caller is a `user`, an `agent`, or a `machine`. That is
not an access-control axis; it is how the server knows which callers a disposition governs. It also
remains what the audit trail names, so a reader can tell an agent's deploy from a person's.

### 4. A machine is not an agent

CI credentials are allowed everything, like a person's. A machine runs a script somebody wrote and
reviewed; the risk a guardrail exists to bound is a model choosing an action nobody asked for.

An operator who wants their CI held to the same line as their agent has the tool for it already: issue
CI an agent credential.

### 5. Undispositioned codes still deny the agent

A guardrail code with no disposition denies the agent and allows everyone else. The fail-safe stays
where it matters — a verb nobody has considered is not handed to a model — and it costs a person
nothing, because a person was never going to be refused.

### 6. Team-level restriction is Kubernetes' job

Burrow does not grow per-person permissions. An operator who needs one engineer to do less than
another expresses that in Kubernetes RBAC, which binds every path to the cluster rather than only the
paths through Burrow. The agent is then a subset of what its holder can already do, which is the
shape operators expect and the only one that is actually enforceable.

## Consequences

**`burrow guard set` gets simpler**, and its output stops needing to explain who a disposition
catches. Documentation that currently distinguishes bound from unbound dispositions collapses to one
sentence: it holds the agent.

**A change freeze no longer freezes people.** Setting `app.deploy` to `deny` in an environment stops
the agent deploying there and leaves operators working. An operator who wants nobody deploying has to
say so somewhere that can enforce it, which was always true — a freeze a person can step around with
`kubectl` was never a freeze.

**`confirm` becomes an agent-only disposition.** It already was in effect: a person confirming their
own deliberate action is a question with one answer, asked of the only party who could answer it. The
CLI's own destructive-action prompts are unaffected and remain the thing that protects a person from a
slip.

**Existing installs change behaviour if they used `--binds` to bind a human.** Nothing is known to,
and the flag is recent enough that this is a stated risk rather than a migration.

**The audit trail is unchanged.** It already names the credential kind, and that is now the only place
the distinction is visible to a reader.

## Rejected alternatives

**Keep `--binds` and default it to `agent`.** The safer version of today: the common case needs no
flag, and the rare case is still expressible. Rejected because the rare case is not real. Binding a
person buys nothing their RBAC has not already decided, so the flag would exist to express a thing
nobody should do — and every operator would still have to learn what it means to decide they do not
want it.

**Keep binding for `machine` only.** CI is the one non-agent caller that runs unattended, so holding
it has some appeal. Rejected because a machine runs a reviewed script rather than choosing its own
actions, and an operator who disagrees can issue CI an agent credential today.

**Make guardrails per-person.** The natural next step once three kinds exist, and the reason to write
this down now: a team lead restricting a team member sounds like it belongs here and does not.
Kubernetes RBAC binds every path to the cluster; a Burrow-only restriction binds one and reads as
protection while `kubectl` sits open beside it.

**Leave it alone.** The flag works and the behaviour is correct when it is used. Rejected because
correctness that depends on remembering a flag is not correctness: the failure mode is silent, it
lands on the operator trying to make things safer, and it produces exactly the defect the flag was
introduced to fix.
