# ADR-0083: A cluster command acts on the target you chose

## Status

📝 Proposed

## TL;DR

`burrow auth switch` picks where Burrow is. Some commands honour that and some ignore it, and which
ones do is invisible until one acts on the wrong cluster.

- **A cluster command follows the selected target**, the same as an app command already does.
- **A pinned environment still does not redirect it.** That was the original reason for the split and
  it survives — a target names a cluster, a pin names an environment inside one, and only the second
  is dangerous to follow.
- **`--context` and `--kubeconfig` still win**, because naming a cluster on the command line is an
  answer, not an accident.
- **No target selected changes nothing.**

Closes the question [ADR-0078](0078-the-cli-points-at-a-target.md) §4 left open. Supersedes nothing.

## Context

Two sets of commands resolve where to connect, and they do it differently.

Per-app commands go through the target model: they read the active target, and it decides the
cluster ([ADR-0078](0078-the-cli-points-at-a-target.md) §1). Cluster and policy commands — `guard`,
`cluster …`, `env add`, `addon …`, `audit`, `failures`, `domain`, `provider`, `config registry
login` — do not. They connect with the raw `--context` or the kubeconfig's current context and never
consult the target at all.

That split is deliberate, and the reason is in the code
([`cmd/burrow/main.go`](../../cmd/burrow/main.go)):

> Commands that do not target an app (install, env add, guard, audit, addon) use it so a pinned
> handle never silently redirects a cluster-setup or policy command.

**That reasoning is correct and is about something else.** A pinned environment handle narrows a
command to one environment inside a cluster. A cluster-wide operation must not inherit that
narrowing — `burrow guard set` sets policy for the cluster, and a pin left over from an afternoon's
work on `staging` has no business changing what it means. Targets arrived later, in ADR-0078, and
inherited the same code path without anyone asking whether the reason still applied. It does not: a
target names a *cluster*, which is exactly the thing these commands need to know.

**What broke it.** Until recently, selecting a Burrow Cloud target refused everything, so nothing
could go wrong quietly — a person either operated a cluster or was told they could not.
[#437](https://github.com/burrow-cloud/burrow/issues/437) changed that: the application commands now
genuinely act through a selected cloud target over HTTPS, while the cluster commands still reach the
ambient kubeconfig.

The result is a mixed mode, and it is worse than either half was alone:

```
burrow auth login              # the managed product
burrow deploy                  # goes to the managed product
burrow guard set app.delete deny   # goes to whatever kubectl's current context is
```

Nothing errors. Both commands report success. The second one changed policy on a cluster the person
had not thought about in a week.

The cloud half of that is being refused outright
([#429](https://github.com/burrow-cloud/burrow/issues/429)), because a command that reaches a cluster
cannot act on a tenant that has none. This record decides the half that refusal does not touch: with
a **cluster** target selected, which cluster does `burrow guard set` act on?

## Decision

### 1. A cluster command resolves through the selected target

When a Kubernetes target is active, it decides the cluster for every command, not only the per-app
ones. `burrow guard set`, `burrow addon create`, `burrow env add`, `burrow audit` and the rest act on
the cluster the target names.

This is what `burrow auth switch` already appears to promise. ADR-0078 §1 describes a target as
"where the control plane is", with no exceptions listed, and `burrow auth status` names one active
target rather than one for some commands and another for others.

### 2. A pinned environment still does not redirect a cluster command

The original protection is kept, because the original reasoning was never wrong. A target and a pin
are different things:

| | What it names | Whether a cluster command follows it |
|---|---|---|
| Target | A cluster | **Yes** — that is the question being asked |
| Pinned handle | An environment inside a cluster | **No** — unchanged |

A cluster command that inherited a pin would silently narrow a cluster-wide effect. A cluster command
that follows a target lands on the cluster the person selected, by name, on purpose.

### 3. An explicit flag still wins

`--context` and `--kubeconfig` override the target, as they do today. Someone who names a cluster on
the command line has answered the question, and the answer is theirs.

Per [#429](https://github.com/burrow-cloud/burrow/issues/429)'s refusal half, those flags are refused
by name against a **cloud** target rather than silently ignored — a Burrow Cloud tenant has no
cluster for a `--context` to select.

### 4. No target selected keeps today's behaviour exactly

The ambient kubeconfig, unchanged. That is the pre-ADR-0078 path, it is what a self-hoster who never
runs `burrow auth login` uses, and nothing here touches it.

### 5. `install` stays exempt

[ADR-0078](0078-the-cli-points-at-a-target.md) §3: install continues to act on a kubeconfig context,
because installing into Burrow Cloud is not a thing that can be asked for, and because the second
person to use a cluster installs nothing. Install is how a cluster becomes targetable in the first
place, so it cannot require a target.

## Consequences

**Someone with a target selected sees their cluster commands move.** A person who has been running
`burrow auth switch` for app work while quietly relying on `kubectl config current-context` for
`burrow addon create` will find those two agree where they used to diverge. That is the fix, and it
is still a behaviour change arriving in an upgrade rather than in response to anything they did.
`burrow auth status` and the target named on every mutating command
([ADR-0078](0078-the-cli-points-at-a-target.md) §1) are what make it visible.

**The mixed mode is what this removes.** After this, the answer to "where did that command go" is one
answer for every command, which is the property worth having — not because following the target is
obviously better in isolation, but because *two rules that look like one* is the shape that produces
the surprise.

**`--context` remains the escape hatch**, and it is a real one: operating on a cluster other than the
selected target stays a single flag away, without switching back and forth.

## Alternatives rejected

**Leave it as it is and document the split.** This is roughly what `docs/CAPABILITIES.md` does today,
and a documented trap is still a trap. The person most likely to be caught is the one who has not
read the capability table — a new self-hoster with one cluster and one target, for whom the two
happen to agree until the day they do not.

**Refuse cluster commands whenever any target is selected**, forcing an explicit `--context` every
time. Safe, and it makes the common case worse than it was: a person with one cluster and one target
would type the flag on every invocation to reach the only cluster they have. The cost lands hardest
on the setup where nothing is ambiguous.

**Follow the pin too, for consistency.** Consistency is the wrong goal here, and the code comment
this record quotes says why: a cluster-wide policy change narrowed by a leftover pin is a silent
scoping error. The asymmetry is load-bearing, so it is stated rather than removed.

**Decide it inside [#429](https://github.com/burrow-cloud/burrow/issues/429)'s refusal.** Refusing a
cloud target is a safety fix with no design question in it, and it should not wait on this. Routing a
cluster target is a change to what a command means, and it should not ride along inside a fix. The
two are separated deliberately, which is why #429's first half was built without this record and this
record does not depend on it.
