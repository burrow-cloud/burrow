# ADR-0071: A `deny` disposition gates the operator too, and deletion becomes a two-step act

## Status

🟡 Proposed

## TL;DR

[ADR-0065](0065-what-belongs-on-the-agent-surface.md) §3 says of the two verbs it denies by default:

> Both remain fully available to the human operator CLI, which these dispositions do not gate.

**That sentence is wrong.** Guardrails are evaluated in the control plane
([ADR-0006](0006-guardrails-in-the-control-plane.md)), and both CLIs reach it through the same API.
A `deny` refuses `burrow app delete web --confirm` exactly as it refuses the agent.

So the real consequence of ADR-0065 §3, which that record did not state, is that **deleting an app
becomes two operator steps**: relax the disposition, then delete. This record confirms that is the
intended behaviour rather than an accident, and declines the alternative — a guardrail that knows who
is calling.

What *is* operator-only is the **lever**, not the verb: `guard set` is absent from `burrow-agent`, so
only a human can change the disposition. That distinction is the one ADR-0065 §3 meant and stated
imprecisely.

Corrects a factual claim in [ADR-0065](0065-what-belongs-on-the-agent-surface.md) §3 and confirms the
behaviour it produces. The tier model, the criterion, and the two default changes are unaffected.
Supersedes nothing.

## Context

### What exists today

- `Engine.DeleteApp` evaluates `GuardrailAppDelete` before deleting; `Engine.RemoveDomain` evaluates
  `GuardrailDNSDelete`. Both are **server-side**, in the control plane.
- The operator CLI and `burrow-agent` both reach those methods through the same API. Neither carries
  a bypass, and the control plane cannot presently distinguish them for this purpose.
- ADR-0065 §3 changed both defaults from `confirm` to `deny`.
- `guard set` is on the operator CLI only, and absent from the agent binary.

So after ADR-0065 §3, `burrow app delete web --confirm` is refused. `--confirm` satisfies a *hold*;
it does not satisfy a *denial*.

### What breaks

Nothing in the code — the implementation matches the decision. What broke is the record: ADR-0065 §3
told a reader that operators were unaffected, and they are not. A reader planning around that
sentence would be surprised at the moment they needed to delete something.

This is worth its own record rather than a quiet fix, because the behaviour it produces is a real
change to how an operator works and deserves to be a decision rather than a discovery.

### What this record resolves

Whether the behaviour ADR-0065 §3 actually produces is the one intended, and how the mistaken claim
is corrected given the record it appears in cannot be edited.

## Decision

### 1. The behaviour stands: a denial gates every caller

A `deny` disposition refuses the operation regardless of which CLI issued it. Deleting an app under
the default policy is therefore:

```
burrow guard set --env prod app.delete confirm
burrow app delete web --confirm
```

Two deliberate steps, the first of which is scoped and visible.

This is a better outcome than the sentence it replaces. Deletion destroys release history and cannot
be rolled back; making it a policy change followed by an act — rather than one command with a flag —
is proportionate, and the operator ends up with a policy that reflects what they actually want rather
than a one-off override.

### 2. Guardrails do not learn who is calling

The obvious way to make ADR-0065 §3's sentence true would be a caller-aware bypass: the disposition
applies to the agent, and a human passes through.

**Rejected.** It puts a new trust dimension inside the guardrail evaluator — the control plane would
have to decide, per request, whether the caller is a human, and be right. That is an authentication
question wearing a policy question's clothes, and getting it wrong means a denial that does not deny.
[ADR-0006](0006-guardrails-in-the-control-plane.md) keeps guardrails deterministic; "deny unless the
caller is a person" is not.

The separation that already exists is the correct one and needs no new machinery: **the verb is
gated for everyone, and the lever is gated by structure.** `guard set` is absent from the agent
binary, so only a human can change a disposition. An agent cannot widen its own permissions; a human
can, deliberately, and the audit log records it once
[ADR-0069](0069-audit-covers-mutation-not-only-guardrails.md) lands.

### 3. The correction lives here, not in ADR-0065

ADR-0065 is Accepted and its body is immutable but for the Status line. A wrong sentence in it is not
a typo or a dead link, so it is not repairable in place.

This record is the correction, and `docs/CAPABILITIES.md` — which documents behaviour rather than
decisions — is where a reader looking for what happens today should find it.

## Consequences

- **Deleting an app is a two-step operator act** under default policy, and someone will meet that
  friction at an inconvenient moment. That is the intended trade; the alternative is a flag that
  makes an irreversible operation a single command.
- **An operator's first instinct will be to relax the disposition globally.** ADR-0065 §3 already
  frames a tier-2 default as a floor rather than a fixed setting, and the refusal message points at
  the per-environment form — but the cheapest keystroke is still the cluster-wide one, and that is
  worth watching once there is usage to observe.
- **ADR-0065 now contains a sentence known to be false**, with the correction in a different record.
  That is the cost of immutability, and it is the right cost — but it means the index row and
  `CAPABILITIES.md` carry more weight, since they are what a reader consults first.
- **`dns.delete` is worse affected than `app.delete`.** `EnvScopable` keys on the `app.` prefix, so
  the two-step relaxation for DNS is necessarily cluster-wide.
  [ADR-0068](0068-operational-limits-are-configuration.md) §5 proposes widening that; until it lands,
  an operator removing one DNS record relaxes the guardrail for the whole cluster.

## Rejected alternatives

- **A caller-aware bypass**, making ADR-0065 §3's sentence true. Rejected in §2: it makes the
  guardrail evaluator answer an authentication question, and a mistake there is a denial that does
  not deny.
- **Revert `app.delete` to `confirm`**, since `confirm` does distinguish in practice — an agent that
  honours its instructions relays a hold to a human. Rejected because it depends on the agent
  choosing to cooperate, which is exactly the property ADR-0065 declined to rely on for an
  irreversible operation. `deny` binds regardless.
- **Amend ADR-0065 §3 in place**, treating a false sentence as a repairable defect like a dead link.
  Rejected because the distinction between fixing a link and rewriting a claim is what makes the
  immutability rule mean anything, and because the behaviour deserves a record of its own rather than
  a silent correction.
- **Say nothing and let `CAPABILITIES.md` carry it.** That file already documents the real behaviour,
  so the practical gap is closed. Rejected because the ADR is what someone reads to understand *why*,
  and leaving a known-false statement in the reasoning is how a wrong assumption propagates into the
  next decision.
