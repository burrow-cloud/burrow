# ADR-0099: An agent may not rewrite its own limits

## Status

🟡 Proposed

## TL;DR

A guardrail holds the agent. The agent can turn it off. Two doors, both open today, and the second
one does not even need the first.

- **It can rewrite the policy.** The route that sets a disposition never asks who is calling, so a
  credential that reaches the API can relax the table that holds it.
- **It can mint itself a different identity.** The admin bit lives on the principal, not on the
  credential kind — so an admin's agent credential can create an invitation, redeem it, and hold a
  credential of a kind that every disposition allows.
- **Both are closed the same way**: a credential of kind `agent` may not write policy and may not
  mint identity. No new model, no new axis.
- **The managed product is not affected.** Its tenants never reach either route.
- **Two comments in the tree currently claim the opposite**, in the words "binding by construction".
  They are the reason this went unnoticed.

Corrects an omission in [ADR-0097](0097-guardrails-hold-the-agent-and-nobody-else.md), which decided
what a disposition binds without saying who may change one. Supersedes nothing.

## Context

### What exists today

A guardrail disposition decides what an agent may do. The recent work made that precise: a
disposition holds callers of kind `agent`; a person and a machine are allowed everything, because a
person's Kubernetes access already decides what they can do.

That model has one unexamined assumption — that the party being held cannot change the thing holding
it.

**`PUT /v1/guard/{code}` performs no caller check.** Neither the handler nor the engine method behind
it asks which kind of credential is calling. Any credential that authenticates can set any
disposition, including relaxing one to `allow`.

**The admin bit is a property of the principal, not of the credential.** A person who is an admin has
an admin's agent credential. That credential can create an invitation and redeem it, and what comes
back is a credential of kind `user` — for which every disposition resolves to `allow`, by design.

So an agent has two independent routes out: change the rule, or change what it is.

### What breaks

Every guardrail in the product, on a self-hosted install, at the discretion of the caller it exists
to hold. `app.delete` set to `deny` refuses an agent exactly until that agent sets it to `allow`.

This is not a theoretical reach. The agent surface is a CLI the model drives directly, and both
routes are ordinary API calls with no special ceremony.

**It is worse than when it was first raised**, because a person can now hold a Burrow credential with
no cluster access at all beyond a proxy to one Service. The problem used to need a shared cluster and
several people; it now fits a two-person self-hosted install.

**And the code says otherwise.** Two comments assert that the binding holds by construction. A reader
checking this exact question is told there is nothing to check.

### What this record resolves

Who may change a disposition, and who may create an identity. Not what a disposition binds — that is
already decided — and not the separate question of whether reaching the engine should grant the
caller's permissions rather than the engine's, which is its own record.

## Decision

### 1. A credential of kind `agent` may not write policy

Setting a guardrail disposition is refused for an agent credential. That covers every shape of the
route — global, per environment, per app or add-on instance.

Reading the policy stays open to everybody. An agent that can see what binds it can explain a refusal
to its person, which is the whole reason the listing exists.

### 2. A credential of kind `agent` may not mint identity

Creating an invitation, redeeming one, and issuing a credential are refused for an agent credential.
An agent that can create a principal can create a principal that is not held.

### 3. An unknown kind is treated as an agent here, as everywhere else

On an install with no per-caller credentials nobody has a kind, including the agent, because it
reaches the engine with the shared install token. Reading unknown as a person would leave both doors
open on exactly the installs that have only an agent to hold — which is most of them.

That matches how a disposition is already resolved, and it is the fail-safe direction: the cost is
that an operator using the shared token must use their own credential to change policy, which is the
behaviour the credential work exists to produce.

### 4. The refusal says what it is

It names the operation and the reason — that policy and identity are not an agent's to change — and
does not read as a guardrail denial, because it is not one. No disposition governs it and no
`--confirm` satisfies it.

### 5. The comments that claim otherwise are corrected

Two comments state that the binding holds by construction. They are wrong today and would be wrong in
a different way after this change — the binding will hold because these two routes refuse an agent,
not by construction. Both are corrected to say what actually enforces it.

## Consequences

**An operator on a shared-token install must authenticate to change policy.** Today the install token
can do it. That is the point, and it is a real behaviour change: `burrow guard set` from a machine
that has never run `burrow auth login` will start refusing.

**The agent surface loses two capabilities it should never have had.** Nothing an agent legitimately
does involves writing policy or minting identity.

**The self-hosted install gets a guardrail model that holds.** Until this lands, no claim that a
guardrail bounds an agent is true on that install, and nothing further should be built on the premise.

**The managed product is unchanged.** Its tenants already cannot reach either route: the policy write
is absent, and the identity routes are inert because no token source is wired. This closes the gap for
self-hosted installs only.

**It does not address the larger question** of whether reaching the engine should grant the caller's
own permissions rather than the engine's. That remains open, and it is the reason a separate record
follows this one.

## Rejected alternatives

**Require `--confirm` on a policy write instead of refusing it.** Cheap and familiar, and it fails on
its own terms: a confirmation is satisfied by the caller, so an agent relaxing its own guardrail would
simply pass the flag. A hold the held party can satisfy is not a hold.

**Make it a guardrail with a disposition of its own.** Circular in the obvious way — the agent would
relax `guard.set` and proceed. A rule about who may change the rules cannot itself be one of the
rules.

**Check the admin bit rather than the credential kind.** Closer to how the identity routes already
work, and it misses the case: an admin's agent credential carries the admin bit, so an admin's agent
would keep both doors. The kind is what distinguishes the caller here, which is the same conclusion
the disposition work reached.

**Leave it and document it.** Honest, and it makes every guardrail on a self-hosted install
decorative. The product's stated purpose is bounding what an agent may do; a bound the agent can lift
is not one, and writing that down does not make it safe.
