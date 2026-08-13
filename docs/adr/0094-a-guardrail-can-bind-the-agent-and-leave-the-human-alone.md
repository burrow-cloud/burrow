# ADR-0094: A guardrail can bind the agent and leave the human alone

## Status

⛔ Superseded by [ADR-0097](0097-guardrails-hold-the-agent-and-nobody-else.md)

## TL;DR

A `deny` refuses everybody today, including the operator holding the cluster's admin credential. A
disposition may now say which kind of credential it binds.

- **The kind rides the context, and `enforce` takes one.** `Policy.enforce` gains a
  `context.Context` and reads the caller through the seam the audit trail already reads. One read,
  not seventeen.
- **The disposition stays one word.** `allow`, `confirm`, `deny`, unchanged everywhere. The kind is
  a **fourth narrowing axis on the policy key**, alongside environment and name:
  `agent:prod.burrowd-cloud.app.deploy`.
- **Target narrows before caller.** Within a tier, the kind-bound key answers first; the unbound key
  is the tier's answer for everyone else. The deny-when-unset default binds every kind, so the
  fall-through always terminates closed.
- **An install nobody signed in to behaves exactly as it does today.** No caller, no kind, and an
  unknown kind matches only unbound keys. `guard set --binds` is **refused** on an install with no
  credentials rather than silently binding nothing.
- **An admin's agent is still bound**, because the key is the credential's **kind**, not the
  principal. Same person, two credentials, two answers.
- **Not RBAC.** One axis, three values, set at issuance. No principal ever appears in a policy key.

Answers the question [ADR-0084](0084-everyone-who-uses-burrow-carries-their-own-token.md) §9
reserved, using the credential kind that record's §3 defines. Makes real the promise
[ADR-0065](0065-what-belongs-on-the-agent-surface.md) §3 already prints and
[ADR-0020](0020-guardrails-as-configurable-policy.md) requires — a gate that holds against an
over-eager agent without holding against the person repairing the cluster. Reuses the key
composition [ADR-0085](0085-a-guardrail-can-name-the-app-it-guards.md) §2 established. Supersedes
nothing.

## Context

**What exists today.** A guardrail resolves to a disposition from what the operation *targets* and
nothing else. [`controlplane/guardrails.go`](../../controlplane/guardrails.go):

```go
func (p Policy) enforce(scope GuardrailScope, op string, code GuardrailCode, confirmed bool, what string, requested, limit int32) error
```

`GuardrailScope` is an environment and a name. There is no caller dimension, and until recently
there was nothing to put in one: `principalFromContext` returned the constant `"shared-agent"`
because every person, every agent and every CI job presented one install-wide token.

**What that breaks.** `deny` refuses the operation for whoever asked. Not "refuses the agent" —
refuses. Three accepted records describe behaviour the enforcement path cannot perform:

- [ADR-0065](0065-what-belongs-on-the-agent-surface.md) §3, on the tier-2 `deny` defaults for
  `app.delete` and `dns.delete`: *"Both remain fully available to the human operator CLI, which
  these dispositions do not gate."* They are not, and it does.
- [ADR-0020](0020-guardrails-as-configurable-policy.md) requires a gate that holds *"even against an
  over-eager or misbehaving agent"* and names `deny` as that gate, with *"the human's lever to relax
  it is `guard set`"*. The lever is real, and it is the wrong shape: relax the policy, act, re-deny
  — by hand, during whatever incident made the operation necessary, with the guardrail off for
  everyone in between.
- [`docs/HARDENING.md`](../HARDENING.md) is the one that says so plainly, because it had to:
  *"Per-caller authorization — a guardrail that binds the agent and leaves the human alone — is
  future work."*

**The case waiting on it.** The managed control plane runs as an ordinary Burrow app on its own
cluster, which is what makes it upgradeable by the same machinery as everything else, and also what
puts `app.deploy`, `app.rollback`, `app.run`, `app.delete`, `app.autoscale` and `app.scale_to_zero`
within reach of an agent. Denying those six on that one app is exactly the protection wanted: a
compromised agent cannot repoint the platform at an image of its choosing. The managed product's own
record decided **not to apply the denial at all**, because applying it today would refuse the
operator at the moment they are repairing the platform — and an operator who cannot repair the
control plane is a worse outcome than an agent that can redeploy it. So the protection does not
exist, and it does not exist for a reason that lives here rather than there.

**What is now available.** Three changes landed since:

- [#543](https://github.com/burrow-cloud/burrow/pull/543) added the `principals` and `credentials`
  tables and the `CredentialAuthorizer` seam. A credential's `Kind` is `user`, `agent` or `machine`
  ([`controlplane/principal.go`](../../controlplane/principal.go)), **recorded at issuance and read
  from the stored row on every request, never from the request**. The wire has nowhere to put one
  and `TestTheClaimWillNotTakeAKindFromTheRequest` asserts it at the boundary.
- [#545](https://github.com/burrow-cloud/burrow/pull/545) made `burrow auth login` mint a person's
  own credential, and turned `principalFromContext` into a real resolver.
- [#553](https://github.com/burrow-cloud/burrow/pull/553) made a sign-in issue a **pair** — the
  person's credential and their agent's, one principal, two rows, two revocations — and named the
  limit this record has to resolve: *an admin's agent credential is an admin's credential*.

So the value is already at the boundary and already read. `callerFromRequest`
([`controlplane/audit.go`](../../controlplane/audit.go)) resolves the caller's kind for every
audited row; [`controlplane/caller.go`](../../controlplane/caller.go) carries it on the request
context and its own header says what it is for: *"a guardrail that binds one kind of caller and not
another."* The audit trail can tell an operator from their agent after the fact. The decision cannot
tell them apart at the moment it matters.

**What this record resolves.** How the caller's kind reaches the decision, what a disposition means
once it can, and what happens on an install where nobody has signed in.

## Decision

### 1. The kind reaches the decision on the context, and `enforce` takes one

`Policy.enforce` gains a `context.Context` as its first parameter, and resolves the caller's kind
through the existing seam:

```go
func (p Policy) enforce(ctx context.Context, scope GuardrailScope, op string, code GuardrailCode, confirmed bool, what string, requested, limit int32) error
```

`evaluateGuardrail`, `evaluateDeploy`, `evaluateReplicas`, `evaluateAutoscale`, `disposition` and
`dispositionSource` take one for the same reason. A context is already in hand at all seventeen
places that evaluate a guardrail — every one of them sits inside an `Engine` method that passes the
same `ctx` to `recordDecision` on the next line — so this is a parameter added, not a value
threaded.

**The kind is not a field on `GuardrailScope`.** A scope *"names what one operation targets"*, and
who is asking is not what is targeted; putting the two in one struct makes a category error that
every future reader has to re-derive. The mechanical objection is worse than the aesthetic one:
seventeen call sites construct a `GuardrailScope` literal, so a kind field is seventeen chances to
forget one, and a forgotten one does not fail — it resolves to the empty kind and quietly stops
binding. That is [ADR-0085](0085-a-guardrail-can-name-the-app-it-guards.md) §3's trap exactly, one
axis over: a capability that looks configured and is not.

Reading it from the context puts the read in **one place**, which is what keeps ADR-0084 §3's
property — the kind comes from the stored row, never from the request — a property of the system and
not of seventeen call sites agreeing.

The resolver is a package var beside `principalFromContext`, for the same reason that one is: a test
or the managed product substitutes it without touching a call site.

### 2. The disposition stays one word; the kind is a fourth axis on the key

A disposition is `allow`, `confirm` or `deny` and remains exactly that. `Disposition.Valid()` is
unchanged, the API type is unchanged, and the audit log's `disposition` column keeps holding one
word.

What gains a dimension is the **key** a disposition is stored under. The policy key is already a
composed string that nothing ever parses — every path builds the key it wants and looks it up, which
is why ADR-0085 could add a third tier to a `TEXT PRIMARY KEY` table with no migration. This record
adds a fourth segment on the same terms: an optional **kind prefix**, colon-separated.

| Key | Binds |
| --- | --- |
| `app.delete` | every caller, in the default environment and any environment without its own |
| `staging.app.delete` | every caller in `staging` |
| `prod.burrowd-cloud.app.deploy` | every caller, for that one app |
| `agent:app.delete` | `agent` credentials, cluster-wide |
| `agent:prod.burrowd-cloud.app.deploy` | `agent` credentials, for that one app |

The colon is unambiguous by construction. Environment names, application names and add-on instance
names are DNS-1123 labels and guardrail codes are dotted lowercase identifiers, so **no colon can
appear in any other segment** — a kind prefix cannot collide with anything, in either direction, and
no key needs escaping.

The operator surface is a flag rather than a compound value:

```
burrow guard set [--env <env>] [--name <name>] [--binds user|agent|machine] <code> <allow|confirm|deny>
```

`--binds` takes one of the three kinds ADR-0084 §3 defines and nothing else — the set is closed,
recorded at issuance, and validated at `guard set` against `CredentialKind.Valid()`.

**Why not make the disposition a mapping from kind to disposition.** It is the more expressive
shape, and it is more expressive in the direction that hurts. Every code would carry three values at
every tier, most of them unset, and an unset entry in a mapping is precisely the ambiguity a
guardrail cannot afford: does `{agent: deny}` mean the human is allowed, or that the human is
unspecified? It changes the wire type, the stored value, `Disposition.Valid()` and every consumer of
the audit column, and it turns `guard list` from a list into a matrix. The key axis costs none of
that, because the machinery for narrowing on an axis already exists twice over and behaves the way
operators have already learned. The expressiveness that matters — different answers for different
kinds — is preserved: two rows at one tier, one bound and one not, say it exactly.

### 3. The target narrows before the caller, and an unbound key is the tier's answer for everyone else

For a caller of kind `K`, most specific first:

1. `K:<env>.<name>.<code>`
2. `<env>.<name>.<code>`
3. `K:<env>.<code>`
4. `<env>.<code>`
5. `K:<code>`
6. `<code>`
7. the built-in default — `deny`

The name tier is consulted only for a code that declares one name bounds its effect, and the
environment tier only for one that declares itself env-scopable, both exactly as today. `prod` and
the empty environment continue to read the global tier rather than an environment of their own.

**The kind narrows within a tier, not across tiers.** A cluster-wide `agent:app.delete deny` does
not beat a per-app disposition set deliberately for one app; ADR-0085 §1 established that the
narrowest *target* wins, and inverting it for the caller axis would make a wide setting silently
override a narrow one. What the operator targeted is the more specific statement; who is asking
refines it.

**A key that does not bind the caller's kind is not an answer** — resolution simply continues to the
next lookup. This falls out of the ordering rather than being a rule of its own, and it is the
property that keeps a narrow binding from punching a hole through a wider deny: `agent:` on one app
tightens that app for agents and leaves every other tier reading exactly as it did. Because step 7
binds every kind, a caller who matches nothing is denied, which is where an unset guardrail has
always landed.

The axis relaxes as well as it tightens, like every other axis. A global `app.delete deny` with
`user:dev.app.delete allow` under it is a human-only relaxation in one environment, and it is the
shape [ADR-0065](0065-what-belongs-on-the-agent-surface.md) §3 already describes as the point of a
deny default being a floor rather than a fixed setting.

### 4. An install nobody has signed in to behaves exactly as it does today

A request carrying the shared install token puts no `Caller` on the context — `ContextWithCaller`
returns the context unchanged when there is no principal — so there is no kind to resolve. **An
unknown kind matches only the unbound keys**, steps 2, 4, 6 and 7.

Every guardrail such an install has configured therefore resolves to the same disposition it
resolves to today, for the same reason it does today: the disposition was set without a binding, and
an unbound disposition binds everyone. Nothing about the shared-token path changes, which is what
ADR-0084's *"existing installs keep working"* requires.

The failure this leaves is that a kind-bound disposition on such an install would bind **nothing**,
because nothing is ever that kind. A protection that silently protects nothing is the worst
available outcome, so it is refused where it is asked for rather than discovered later:
**`guard set --binds` fails on an install with no credentials**, naming the cause and the fix — the
install has no per-caller credentials, so a disposition bound to a kind would bind nobody; run
`burrow auth login` first. The unbound `deny` remains available and remains the honest answer for a
shared-token install: blunt, and it holds.

**An unknown kind is not treated as `agent`.** It is the tempting safe-by-default reading and it is
backwards. On a shared-token install *every* caller is unknown, so the operator would be treated as
their own agent on every request — reinstating, for every install that has not signed in, exactly
the defect this record exists to remove.

### 5. An admin's agent is bound, because the key is the credential's kind

A sign-in issues two credentials to one principal: the person's, `kind=user`, and their agent's,
`kind=agent`. Anything keyed on the principal cannot separate them — this is what
[#553](https://github.com/burrow-cloud/burrow/pull/553) named and left, and it is why that record's
limit is real: an admin's agent credential is an admin's credential.

Keying on the **kind** separates them, and nothing else about the caller is read. The resolution
above touches `Caller.Kind` and nothing more: not `PrincipalID`, not `PrincipalName`, not the admin
bit — which `Caller` deliberately does not carry, so that *"may this caller do this"* has exactly one
answering seam. `agent:prod.burrowd-cloud.app.deploy deny` binds the admin's agent as firmly as
anybody else's, while the same person's `user` credential resolves at the next step down. One
person, two credentials, two answers, and the difference is which token was presented rather than
who presented it.

This is also why the record does not exempt admins. An admin exemption keyed on the principal
exempts that admin's agent, which is the agent most worth binding.

### 6. A refusal says who is not bound, and `guard list` answers for the caller who asked

A refusal already names the thing a disposition was set for and the command that would relax it.
Where the disposition that answered was kind-bound, the message says so, and says what the way out
is:

> deploying `burrowd-cloud` is denied by the current guardrail policy for the app
> `burrowd-cloud` — this disposition binds `agent` credentials; an operator can run it with their
> own

That last clause is the load-bearing one, because the reader is usually an agent relaying to a
human. Today the only relayable next step is *ask someone to relax the policy*; after this it is
*ask someone to run it*, which leaves the guardrail in place while the work gets done.

`guard list` resolves for the **kind of the caller asking**, so an agent reading the policy sees
what binds the agent. [ADR-0065](0065-what-belongs-on-the-agent-surface.md) §7 requires the agent to
be able to see what it cannot do, and a listing that showed the human's answer to the agent would be
worse than no listing: the agent would plan an operation the policy has already refused. Rows whose
disposition came from a kind-bound key report which kind, so a human reading the same listing can
see the binding rather than infer it from a surprise.

No new audit column is needed. Every audited row already records the caller's kind alongside the
principal and the disposition, so the trail already distinguishes the two decisions.

### 7. What this does not become

One axis, with three values, fixed at issuance. That is the whole of it.

- **Not per-principal policy.** No principal name or id ever appears in a policy key. A policy keyed
  on a person is an access-control list, and an access-control list needs a grant model, an
  ownership model and an escalation-prevention story — none of which any of this needs, and all of
  which would be decided badly by acquiring them one key at a time.
- **Not RBAC, and not a replacement for the authorizer.** Whether a caller may issue a credential or
  grant the admin bit is answered by the `CredentialAuthorizer` seam and stays there. A guardrail
  answers whether an operation is permitted by policy; the two are different questions and keeping
  them apart is what lets an identity provider later replace one of them whole.
- **Not a change to the agent surface.** `guard set` is not compiled into `burrow-agent` and does
  not become so, so an agent cannot set — or unset — its own binding. That control is still a
  surface control rather than an authorization boundary, and this record does not pretend otherwise;
  it makes the *disposition* a boundary, which is the half that was missing.

## Consequences

**The protection the managed control plane needs becomes applicable.** The six mutating codes can be
denied for `agent` credentials on that one app, and the operator repairing it is unaffected. That is
the specific thing blocked today, and it unblocks without a special case: it is an ordinary
disposition on an ordinary key.

**Two accepted records stop describing behaviour the code cannot perform.** ADR-0065 §3's *"remain
fully available to the human operator CLI"* becomes true when the disposition is bound, and
`docs/HARDENING.md`'s honest limitation can be rewritten as a capability with instructions. Both
should be updated as part of building this rather than after it, because a record that says a thing
is future work when it has shipped is the same defect in the other direction.

**`deny` becomes usable where it was too blunt to reach for.** The everyday cost of a deny today is
that it costs the human something too, so operators reach for `confirm` — the cooperative gate — in
places that warrant the hard one. A deny that leaves the human alone is a deny an operator will
actually set.

**More capabilities can move from tier 1 to tier 2.** ADR-0065 §5 prefers a verb that is present and
denied over a verb that is absent, because an agent that can see a closed door asks about it instead
of inventing a way around it. Some verbs are absent from the binary today only because a deny would
also have bound the operator. Which ones move is ADR-0065's question and stays there; this record
widens where the preference is available.

**A fourth axis is a fourth thing to get wrong.** Six lookups instead of three, and a policy that can
now be misconfigured in a way that looks configured — a binding on a kind the install does not issue,
or a binding at the wrong tier. §4 refuses the first at set time and §6 makes the second visible in
the listing, and neither eliminates the class. The mitigation that matters is that the default is
unchanged: a disposition set without `--binds` behaves as every disposition behaves today.

**The kind is only as trustworthy as issuance.** Everything here rests on the kind being recorded
when the token is minted and read from the stored row. A path that ever issued an `agent` credential
as `user`, or let a request assert its kind, would silently unbind every agent-bound deny on the
install. That property has a boundary test today and needs one at every future issuance path.

**Nothing changes for an install that has not signed anybody in**, which is most self-hosted installs
at the time this is written. The feature arrives with `burrow auth login` rather than before it,
and the shared-token path stays exactly as it is.

## Rejected alternatives

**Put the kind on `GuardrailScope`.** Rejected in §1: it conflates what an operation targets with who
is asking, and it turns one read into seventeen opportunities to omit the value, where an omission
resolves to "binds nobody" without failing.

**Pass a `Caller` to `enforce`.** Same seventeen call sites, and no call site has a `Caller` in hand
— each would begin with `CallerFromContext(ctx)`, which is the read this record puts in one place
instead.

**Make `Disposition` a mapping from kind to disposition.** Rejected in §2: an unset entry per kind
per code per tier is ambiguity where a guardrail can least afford it, and it changes the wire type,
the stored value and every consumer of the audit column to buy expressiveness the key axis already
provides.

**Encode the binding in the value — `guard set app.deploy deny:agent`.** The disposition is a single
word in the API, the database, the audit column and `Disposition.Valid()`. A flag keeps it one; a
compound value makes four places learn a grammar.

**Let the request declare its kind** (a header, a flag, a claim). Refused outright, and already
refused in code: a caller-declared kind makes a deny cooperative, and ADR-0020 requires it to hold
against a misbehaving agent. `TestTheClaimWillNotTakeAKindFromTheRequest` exists to keep it refused.

**Key the policy on the principal instead of the kind.** Cannot separate a person from their own
agent — they share a principal, which is what #553 recorded — and it turns the policy into an access
control list. §5, §7.

**Exempt admins from denies.** The admin bit is deliberately absent from `Caller`, so that
authorization has one answering seam; and an admin exemption exempts that admin's agent, which is
the agent most worth binding.

**Treat an unknown kind as `agent`.** Rejected in §4: it makes every operator on a shared-token
install their own agent, reintroducing the defect for the majority of installs.

**Fall through to `allow` when a disposition does not bind the caller's kind.** A narrow binding
would then punch a hole through every wider deny — set `agent:` on one app and the human gets `allow`
for it regardless of what the environment or the global policy says. Continuing the lookup is both
safer and simpler.

**Leave the human's lever as "relax, act, re-deny".** That is today. It requires the operator to
disable the protection for everyone, perform the work, and remember to restore it, by hand, usually
during an incident. A gate whose correct use depends on an interrupted human remembering step three
is not a gate.

**Solve it on the agent surface alone** — keep the dangerous verbs out of `burrow-agent` and call
that the boundary. ADR-0065 §2 already declines this, and ADR-0084's Context says why: the agent
holds a token and the operation is reachable over plain HTTP by anything that decides to make the
call. A surface control is real friction against an LLM driving a CLI; it is not a boundary, and it
is not what an operator setting `deny` believes they are getting.
