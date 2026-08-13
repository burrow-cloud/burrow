# ADR-0098: A config write is guarded, because it rolls the app

## Status

✅ Accepted

## TL;DR

Setting or removing an app's config var restarts that app. No guardrail covered it, so an agent did
it freely and an operator had no way to say otherwise. Add one.

- **New code `app.config`.** Covers both `config set` and `config unset`.
- **Default: `confirm`.** Same reasoning as running a one-off command — arbitrary change, app rolls,
  a human should see it first.
- **One code, not two.** Splitting set from unset buys nothing anyone wants.
- **Per app, per environment.** `guard set --env prod --name burrowd-cloud app.config deny` protects
  one app and touches nothing else.
- **`--no-restart` is not a way past it.** Value still lands in the store; the next deploy carries it.
- **"Non-secret" was never the question.** It describes the value. The write restarts the app.
- **Audited.** Decision and execution both recorded, key names only, never values.

Extends the guardrail catalogue defined by ADR-0006 and ADR-0020, and applies ADR-0097's rule that a
guardrail holds the agent and nobody else. Supersedes nothing.

## Context

### What exists today

Every Burrow application has a **config store**: non-secret configuration, held by the control plane
and sourced into the app's containers as environment variables. Two verbs write it, and both are on
the agent's surface as well as the operator's:

```
burrow app config set web LOG_LEVEL=debug
burrow app config unset web LOG_LEVEL
```

A write does two things. It persists the value, and — unless the caller passes `--no-restart` — it
**re-applies the running workload**, so the Deployment rolls: the app's pods are replaced with pods
carrying the new environment. That second half is the point of the verb. Without it a config change
would sit in the store doing nothing until somebody deployed.

Alongside the store sits the **guardrail policy**: a table of operation codes, each with a
disposition of `allow`, `confirm` or `deny`, evaluated in the control plane before the operation
runs. `confirm` returns a structured hold that the agent surfaces to a human, who re-runs the same
command with `--confirm`. `deny` refuses outright and no confirmation opens it. A disposition holds
the **agent**; a person and a CI machine are allowed everything, because their Kubernetes access
already decides what they can do to the same Deployment.

Seven codes cover the app verbs: `app.deploy`, `app.rollback`, `app.run`, `app.delete`,
`app.autoscale`, `app.scale_to_zero`, `app.expose_public`. A config write has none. The control
plane said why, in a comment on the function that performs it:

```go
// Config vars are non-secret, so there is no guardrail.
```

### What breaks

**Non-secret is true and it is not the question.** It describes what the value *is*. The guardrail
question is what writing it *does*, and what writing it does is restart the application. An
availability-affecting operation is availability-affecting whether or not the value it carries would
embarrass anybody who read it.

**So the one mutating app verb with no disposition of any kind is a verb that rolls production.** An
agent may perform it as often as it likes, on any app, in any environment. There is no hold to relay
to a human and nothing in the audit trail saying it happened, because the trail records guarded
operations and this was not one.

**An operator who notices cannot do anything about it.** The lever does not exist:

```
$ burrow guard set app.config set --env prod --name burrowd-cloud deny
set guardrail: unknown guardrail "app.config set"
```

**The case that surfaced it is not hypothetical.** Burrow's own managed control plane is deployed as
an ordinary Burrow application, and its operator checklist names eight verbs to deny for it. Six
exist. The two that do not are the config write and the config removal — and its configuration
includes a service-account namespace that the binary checks against its cluster credential at
startup. A mismatch means it refuses to serve. A single config write by an agent therefore rolls it
into a state it does not come back from, and the thing that would normally repair a broken app is
the thing that just went down.

**`--no-restart` is not the escape hatch it looks like.** It skips the immediate roll; the value
still lands in the store, and the next deploy — which `app.deploy` allows by default — puts it in
front of the app anyway. A gate that covered only the rolling form would be a gate the flag walks
around.

### What this record resolves

Three things, in order: whether a config write is guarded at all; whether setting and removing are
one code or two; and what the default disposition is.

## Decision

### 1. One guardrail code, `app.config`, covering both directions

A single code gates both `config set` and `config unset`.

Separating them would buy the ability to allow removing a variable while holding its creation.
Nobody wants that. Both verbs write the same store and roll the same app, and an application that
comes back missing the variable it reads at startup is in exactly the place an application that came
back with a wrong value is. A second code would be a second thing to configure, a second thing to
forget, and a way to protect an app against half of one operation.

The code gates **whether the write happens**, not what is written. Burrow does not inspect the key
or the value and does not branch on either — see §5.

### 2. The default disposition is `confirm`

An unconfigured `app.config` holds the agent's write for confirmation and returns a hold naming the
key, the app, and whether the app will roll. The human approves, and the agent re-runs the same
command with `--confirm`.

`confirm` rather than `allow`, because `allow` is what the system does today and is precisely the gap
this record was opened about. A default of `allow` would ship a setting that protects only the
operators who already know they need it, and the way an operator finds out they needed it is by
losing an app to a config write.

`confirm` rather than `deny`, because writing configuration is the ordinary loop the product exists
for. An agent deploying an application sets its configuration as part of doing so, and a default that
refuses that outright would make the common path run through an operator every time.

The comparison that decides it is the one-off command runner, `app.run`, which is `confirm` for the
same shape of reason: the change is arbitrary, its effect on the running app is real, and — the test
that matters — **the confirmation can be an informed one**. "Set `DATABASE_HOST` to `db-2` on `web`
in prod, which rolls the app" is a sentence a human can read and act on. Where a confirmation cannot
be informed, holding for one is theatre; here it can.

### 3. It is scopable per environment and per app

`app.config` is declared environment-scopable and app-scopable, like the other `app.*` codes. Both
are declarations rather than inferences from the code's name.

That is what makes the `confirm` default affordable and what makes the case in the Context
expressible:

```
burrow guard set --env dev app.config allow                      # a sandbox, no friction
burrow guard set --env prod --name burrowd-cloud app.config deny # the one app that cannot survive a roll
```

The second is the shape an operator actually wants: protection on the app whose configuration is
checked at startup, and nothing changed for every other app in the same environment.

### 4. `--no-restart` does not skip the guardrail

The guardrail is evaluated for every config write, rolling or not. The hold's wording differs — it
says the running app is not rolled and that the change lands on the next deploy — but the disposition
is the same one, because the value reaches the app either way.

### 5. Burrow does not classify keys or values

There is no notion of a protected variable, no key allowlist, no pattern matching on names, and no
inspection of values. Burrow does not know which of an app's variables is load-bearing: the one that
takes the app down is a property of the app's own code, not of the string.

### 6. The decision is recorded in the audit trail

A config write records the guardrail decision and, when it proceeds, its execution — the same two
rows every other guarded operation leaves. The trail carries two operation names, `config_set` and
`config_unset`, so a reader can see which way the store moved, under the one guardrail code, because
the question a disposition answers has one answer.

The rows record the environment, the config **key**, and whether the workload was left alone. They
never record the value. A config var is non-secret by convention rather than by enforcement, and the
audit log is the worst place for a mistaken one to survive.

### 7. Reading config stays ungated

`config list` is unchanged and unguarded. It returns values, which is a deliberate property of the
non-secret store, and it changes nothing about a running app.

## Consequences

**An operator gains the lever the managed deployment needs.** `app.config` can be denied for one app
in one environment, and that app's configuration then changes only by a human hand.

**An agent's ordinary loop gains a stop.** An agent setting three variables on a new app is held
three times unless the environment's disposition says otherwise. The intended relief is the gradient:
`allow` in development, `confirm` or `deny` in production. An install that finds the friction wrong
everywhere can set it globally, and that is a deliberate act rather than an oversight.

**Behaviour changes on upgrade for every install that has never configured this code**, which is all
of them, since the code did not exist. A config write that used to proceed silently now comes back as
a hold. That is the intent of the record rather than a side effect, but it is a visible change in
what an agent can do without a human, and it lands the moment the control plane is upgraded.

**A person is unaffected.** The write is held for the agent; an operator at a terminal proceeds
without confirming, because refusing them would be undone with `kubectl` a second later.

**Both clients grow a `--confirm` flag** on `config set` and `config unset`, and the control plane
grows a `confirm` field on the set request and a `confirm` query parameter on the removal, which has
no body to carry one.

**A newer client that sends `confirm` to an older control plane is refused, not half-obeyed.** The
control plane rejects request fields it does not recognise, so the caller gets a structured refusal
naming the version rather than a write that quietly ignored the confirmation. Nothing unsafe results
from it: a control plane that old has no `app.config` guardrail to satisfy in the first place.

**The audit trail grows two operation names.** A reviewer asking what an agent changed about an app's
configuration has rows to read, where before there were none.

## Rejected alternatives

**Leave config writes ungated.** The status quo, defended by the comment in the code: config vars are
non-secret. Rejected because the premise answers a different question than the one being asked.
Nothing about secrecy bears on the fact that the write re-applies the workload, and the verb's blast
radius is the app going down, not the value being seen.

**Two codes, one for set and one for unset.** More expressive, and expressiveness is not free: it
doubles what an operator has to configure to protect an app, and the extra thing it can express —
removal allowed, creation held — is not a policy anybody has wanted. An operator who sets only one
of the two has protected the app against half an operation, which reads as protection and is not.

**Default `allow`.** The code would exist, the operator would have a lever, and no existing workflow
would change. Rejected because the failure this record is about is a config write nobody had thought
about, on an install where nobody had configured anything. A default of `allow` protects exactly the
operators who already knew, and the ones who did not find out by losing an application.

**Default `deny`.** Safest, and wrong for the verb. Setting configuration is part of deploying an
application, which is the loop the product exists to run. Denying it by default would send the
ordinary case through a human every time, and the reasonable response — relaxing the guardrail
globally to get work done — leaves production more exposed than a `confirm` default ever would.

**Gate only the rolling write, and leave `--no-restart` alone.** Tempting, because the roll is the
justification. Rejected because the flag would then be the way around the guardrail: the value still
reaches the app on the next deploy, and `app.deploy` is allowed by default, so the pair of calls
achieves exactly what the single held call would have.

**Gate on which key is written — a protected-variable list.** The most targeted-sounding option:
hold `DATABASE_URL` and let `LOG_LEVEL` through. Rejected because Burrow cannot tell the difference.
Which variable takes an app down is a fact about the app's own code, and a classifier that is wrong
in the permissive direction is a gate people trust and should not.

**Fold config writes into `app.deploy`.** They both end in a rolled workload, so one disposition
could cover both. Rejected because they are different questions with different answers: an operator
freezing deploys in production usually still wants configuration changeable, and an operator
protecting one app's configuration is not asking to stop its releases. Sharing a disposition would
make neither statement possible on its own.
