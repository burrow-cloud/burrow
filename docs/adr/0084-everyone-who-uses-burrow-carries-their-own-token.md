# ADR-0084: Everyone who uses Burrow carries their own token

## Status

✅ Accepted

## TL;DR

Today one token sits in a Secret on the cluster, and everybody who can read it — every person,
every agent, every CI job — presents that same string. Burrow cannot tell them apart, so it cannot
revoke one of them, record which one acted, or refuse one of them anything.

- **`burrow auth login` mints a credential for you**, using your kubeconfig once to prove you are an
  operator of that cluster. After that, your kubeconfig is not how you talk to Burrow.
- **It is a ServiceAccount token, minted by burrowd**, in a self-contained kubeconfig holding the
  API server URL, the CA and the token — the same shape [ADR-0038](0038-scoped-agent-credential.md)
  already mints for the agent.
- **The agent gets its own**, separate from yours and revocable on its own.
- **The target decides which cluster, for every command.** No command reads the ambient kube
  context, so `kubectl config use-context` and `kubectx` drop out of Burrow work entirely.
- **Each install gets an id, and the target records it.** A kube context name is a label, not an
  identity — it can be renamed, and worse, reused for a different cluster.
- **The route does not change.** The API-server proxy stays, so a kubeconfig still has to exist —
  but it stops being something you manage.
- **The kubeconfig and the install Secret stay as break-glass**, for the day burrowd is too broken
  to mint anything.

Supersedes [ADR-0014](0014-self-host-connectivity-via-kubeconfig.md)'s use of the kubeconfig as the
route to a shared credential, and replaces the credential [ADR-0038](0038-scoped-agent-credential.md)
mints for the agent. Fills the seam [ADR-0027](0027-audit-log.md) and ADR-0038 both left open.
Makes ADR-0083's question moot — that record was closed unaccepted rather than merged, so §4 here is where the answer lives ([#439](https://github.com/burrow-cloud/burrow/pull/439)).

## Context

There appear to be several ways to authenticate to Burrow. There is really one, and it is shared.

**What actually happens on a self-hosted cluster.** `connect.KubeconfigTransport`
([`connect/connect.go`](../../connect/connect.go)) does three things in order: builds a REST config
from the kubeconfig, **reads the burrowd API token out of the install Secret**, and reaches burrowd
through the API server's service proxy carrying that token in `X-Burrow-Token`
([ADR-0015](0015-token-header-only-x-burrow-token.md)).

So the kubeconfig is not the credential. It does two other jobs:

- **It is the route.** burrowd has no address of its own, and the API-server proxy is how a laptop
  reaches a ClusterIP service. ADR-0014 says this outright — its title is *connectivity*.
- **It is the gate on the shared token.** Anyone with RBAC to `get` `burrow-credentials` in the
  `burrow` namespace holds it.

**The token is install-wide.** One string, in one Secret, for the lifetime of the install. The
person who ran `burrow install`, the second person who joined, the agent, and a CI job all present
it. It is identical in every case.

**Three things follow, and all three are already written down in the code as unfinished.**

[`controlplane/audit.go`](../../controlplane/audit.go) records a constant in the column that is
supposed to say who acted:

```go
// auditCaller is the coarse caller identity recorded until an authentication model exists
const auditCaller = "control-plane"

// auditPrincipal is the acting identity recorded until per-user identities exist
const auditPrincipal = "shared-agent"
```

Every row. [ADR-0027](0027-audit-log.md)'s audit log is an append-only record of who did what, and
the "who" is a literal. `principalFromContext` is the seam left for the value that never arrived.

**Nothing can be revoked short of rotating the install.** Someone leaves, a laptop is lost, an agent
is compromised: the only lever is to change the Secret, which logs out everyone and everything at
once.

**Tier 1 is a surface control, not an authorization control.**
[ADR-0065](0065-what-belongs-on-the-agent-surface.md) keeps the most dangerous verbs away from an
agent by leaving them out of the `burrow-agent` binary
([`internal/agentsurface/surface.go`](../../internal/agentsurface/surface.go)). Against an LLM
driving a CLI that is real and sufficient friction. It is not a boundary: the agent holds the same
token the operator does, so the operation is reachable over plain HTTP by anything that decides to
make the request. ADR-0038 narrowed what the agent's *kubeconfig* could reach and left this
untouched, because the credential burrowd checks was never the kubeconfig.

**What changed recently.** [ADR-0078](0078-the-cli-points-at-a-target.md) added targets, and
[#437](https://github.com/burrow-cloud/burrow/issues/437) made a Burrow Cloud target operable — with
a per-tenant bearer token, minted at sign-in, revocable from the console. The managed product has
the model this record proposes. Self-hosted does not, and the difference is visible as two headers,
two transports, and a set of commands that reach a cluster while others reach a target. The
confusion is real and it is downstream of one thing: **self-hosted has no per-caller identity.**

## Decision

### 1. `burrow auth login` mints a credential, and the kubeconfig is how you prove you may have one

Against a self-hosted cluster, signing in uses the kubeconfig **once**: the CLI proves to burrowd
that it can act as an operator of that cluster, and burrowd issues a token bound to that principal.
The token is stored the way the cloud credential already is, under `~/.burrow/credentials/`.

The kubeconfig keeps exactly one further job, §4.

This mirrors what a Burrow Cloud sign-in already does — a device flow proves who you are to the
managed control plane, which issues a tenant-scoped token. Same shape, different proof of identity,
because a cluster has no browser and a kubeconfig is the operator credential that already exists.

### 2. The credential is a ServiceAccount token, and burrowd mints it

Not a token format Burrow invents. A Kubernetes ServiceAccount token, written into a self-contained
kubeconfig carrying the API server URL, the cluster CA and the token —
[ADR-0038](0038-scoped-agent-credential.md) §3's mechanism, which already runs for the agent.

**The grant is the proxy subresource and nothing else.** The RBAC
`cmd/burrow/manifests/install.yaml.tmpl` already writes for the agent is the whole pattern:

```yaml
- apiGroups: [""]
  resources: ["services/proxy"]
  resourceNames: ["burrowd", "burrowd:{{.Port}}", "http:burrowd:", "https:burrowd:"]
  verbs: ["get", "create", "update", "patch", "delete"]
```

`resourceNames` pins it to the burrowd Service, and the verbs cover the HTTP methods burrowd's API
uses. Nothing else: no pods, no other services, no other namespace.

**burrowd mints, not the CLI.** Creating a ServiceAccount and a RoleBinding is an RBAC write, so a
CLI doing it itself would require every person who signs in to hold cluster-admin — which defeats
ADR-0078 §3's *"the second person to use a cluster installs nothing"*. burrowd already holds the
permissions, so it issues the credential and hands back the kubeconfig.

**Who may ask is the open question this creates**, and it is named rather than answered here: without
it, anyone who can reach burrowd can mint themselves a credential. The bootstrap is the operator's
own kubeconfig; what follows needs deciding before this ships.

**The Role stays in `install`; the ServiceAccount and binding move to sign-in.** The Role defines the
shape of access, is created once, and needs an RBAC write. The per-principal objects are made when a
principal appears.

### 3. Long-lived is accepted, as ADR-0038 accepted it

A `kubernetes.io/service-account-token` Secret, not a bound token from the TokenRequest API. Bound
tokens expire and need refreshing, and the thing that would refresh them is the credential itself.

ADR-0038 §3 made this call already and gave the reason: **the RBAC narrowness is the real control**,
and the credential is revoked by deleting the ServiceAccount or its Secret. A token that reaches
exactly one Service by name, with no other grant anywhere in the cluster, is a small thing to hold
for a long time.

### 2. Every principal is separate

| Principal | Where its token lives | Revoked by |
|---|---|---|
| A person | `~/.burrow/credentials/` | that person's row |
| The agent | `~/.burrow/agents/` | its own row, without touching the human's |
| A CI job | wherever the job keeps secrets | its own row |

The second person to use a cluster runs `burrow auth login` and gets their own, installing nothing —
which is [ADR-0078](0078-the-cli-points-at-a-target.md) §3's rule, now with two identities behind it
instead of two copies of one string.

### 4. The target decides the cluster, and nothing reads the ambient context

Every command resolves through the selected target. None reads `kubectl`'s current context.

This is the half of the problem that is not about credentials at all, and it is the half people
actually feel. Today `burrow auth switch` selects a target, `burrow auth status` lists the configured
ones, and then a set of commands ignores both and acts on whatever context was last set — so a person
runs `kubectx` *and* `burrow auth switch`, keeps them in agreement by hand, and has no way to tell
which one a given command obeyed. The output does not say. That is not a missing feature; it is one
missing property of a feature that already exists.

A target already stores a **context name** rather than a copy of a credential
([ADR-0078](0078-the-cli-points-at-a-target.md) §1), which is what makes this cheap: Burrow builds its
own client from the named context, and never asks which one is current.

**The honest limit.** The kubeconfig cannot be removed entirely, and it is worth being exact about
what each of its jobs becomes:

| What the kubeconfig does today | After this record |
| --- | --- |
| Proves who you are to burrowd | **Gone** — §1's minted token |
| Decides which cluster a command acts on | **Gone** — the target decides, by name |
| Routes to burrowd through the API-server proxy | **Stays**, unless burrowd is given its own address (§7) |
| `install`, `bootstrap`, `join`, `upgrade` | **Stays** — there is nothing to authenticate to yet |
| Break-glass when burrowd will not serve | **Stays**, deliberately (§8) |

So a kubeconfig still has to exist on the machine. What changes is that it stops being something a
person manages: no context to switch, no agreement to maintain, no second tool in the loop.

### 5. Each install has an id, and the target records it

`burrow install` generates an id for that install and keeps it. A target records the id alongside the
context name, and the CLI compares them on connect: the context name says **how to get there**, the
id says **whether you arrived**.

**A kube context name is a label, not an identity.** [`localconfig/target.go`](../../localconfig/target.go)
stores only the name, and `KubernetesTarget` sets `Name` to the context, so both fields orphan
together the moment somebody renames it. Three ways that goes wrong, and only the first is loud:

| | What happens today |
| --- | --- |
| The context is renamed | The target stops resolving. Annoying, obvious, fixable. |
| Two kubeconfigs merged by `KUBECONFIG` share a context name | First match wins, order-dependent, silent. |
| **A cluster is destroyed and recreated under the same name** | **The target points at a different cluster and says nothing.** |

The third is not a corner case. `doctl kubernetes cluster kubeconfig save` writes deterministic names
like `do-nyc3-burrow`, so tearing a cluster down and standing another one up produces a byte-identical
context name for an entirely different cluster.

That is the failure [ADR-0078](0078-the-cli-points-at-a-target.md) §1 rejected inference to avoid — a
wrong guess being "a deploy landing somewhere unintended". Storing a name rather than inferring one
narrowed the window; it did not close it, because the name is user-controlled, provider-generated and
reusable.

**The id is possible here because of §1.** A target no longer names a cluster; it names a burrowd. A
burrowd can know its own name, and a cluster cannot be asked to have a stable one.

- A **rename** is recoverable rather than fatal: the id still says what is being looked for, so the
  target can be re-pointed, or the contexts scanned until the right install answers.
- A **reuse** is caught and named: *target `prod` expects the Burrow installed as X; the cluster at
  context `do-nyc3-burrow` is running install Y.*
- **Merge ambiguity** is caught on arrival instead of never.

It pairs with the token rather than duplicating it. A token minted by one install and presented to
another is refused regardless; checking the id first means the message names the cause instead of
returning a bare 401 from a cluster the caller did not know they had reached.

The id is not a secret — it identifies an install, it does not authorise anything — so it rides the
handshake the client already performs for version skew ([ADR-0039](0039-version-skew-is-legible.md))
rather than needing a route of its own.

### 6. Identity is unified; the route is not

This record changes **who is calling** and nothing about **how the bytes arrive**.

- Self-hosted stays on the API-server proxy. burrowd gaining an address of its own is real
  infrastructure — ingress, hostname, certificate, exposure — and on a private cluster it may be
  impossible. The proxy is the path that needs nothing, and that is worth keeping.
- The two headers stay for the same reason. `X-Burrow-Token` exists because the API server consumes
  `Authorization` itself on the proxy path ([`client/transport.go`](../../client/transport.go)), so
  the header is a property of the transport rather than of the credential. A token is a token;
  which header carries it is the transport's business.

### 7. A front door for burrowd is offered, never required

Reaching zero uses of a kubeconfig means burrowd having an address of its own: ingress, a hostname, a
certificate. That is worth having — it is what lets `burrow auth login` work from a machine with no
kubeconfig at all, which is what CI is — and it is not worth requiring.

Requiring it makes a working install depend on ingress and DNS, on clusters that may be private, and
it puts burrowd on the internet. ADR-0014 chose the proxy precisely so connectivity needed nothing,
and that property is worth keeping for the person who has one cluster and no wish to expose it.

So it is opt-in: the proxy is the default and always works, and a cluster that wants token-only
access can be given a front door. The credential model is identical either way, which is the point of
separating identity from route — the front door becomes a deployment choice rather than a different
way of authenticating.

### 8. The kubeconfig and the install Secret stay as break-glass

If burrowd cannot serve, it cannot mint or check a token, and a token-only CLI has no way in. The
existing path — kubeconfig, install Secret, API-server proxy — remains as the recovery route,
documented as one rather than surviving by accident.

It is a real credential with real power, so it is named honestly: reading `burrow-credentials` is an
administrative act on the cluster, gated by cluster RBAC, and it is how you get in when the front
door is broken.

### 9. The seams that were left open get filled

`principalFromContext` returns the authenticated principal instead of `"shared-agent"`, and
`auditCaller` stops being a constant. [ADR-0027](0027-audit-log.md)'s audit log starts recording who
acted. Nothing about the schema changes — the columns were reserved for this.

Tier 1 can become an authorization boundary rather than only a surface one, since burrowd can now
tell an agent's token from an operator's. Whether it should is
[ADR-0065](0065-what-belongs-on-the-agent-surface.md)'s question, not this one; this record only
makes the answer available.

## Consequences

**The self-hoster to managed migration becomes a target switch and nothing else.** Both sides mint a
token at sign-in and carry it per request. That is [ADR-0078](0078-the-cli-points-at-a-target.md)'s
whole premise, and today it is true of the routing and false of the credential.

**Pointing at the wrong cluster becomes a refusal rather than a surprise.** The case worth naming is
a cluster rebuilt under a name a provider generates deterministically, where every visible signal —
the context name, the target name, `auth status` — reads correct.

**Guardrails start meaning what everyone already assumed they meant.** Today `enforce()` takes a
scope, a code and whether the caller confirmed — and **no caller dimension at all**, because there is
none to take: `auditPrincipal` is the constant `"shared-agent"`. So a `deny` binds the operator as
hard as it binds the agent, and the operator's only lever is to relax the policy, act, and re-deny.
That is not what "don't let the agent do this" is supposed to cost. With a principal to distinguish,
a disposition can bind the agent and leave the human alone, which is how the feature is already
described and sold.

**`kubectx` stops being part of using Burrow.** Today a person switching between a worker fleet and a
control-plane cluster runs a context switcher alongside `burrow auth switch`, because some commands
follow one and some the other. After §3 there is one switch, and it is Burrow's.

**ADR-0083 stops being a question.** It
asks which cluster `burrow guard set` acts on when a cluster target is selected — a question that
exists only because those commands reach a cluster directly rather than reaching a control plane.
Under this record they reach the target's burrowd with a token like everything else, and there is
nothing left to decide. It should be closed unaccepted rather than superseded, since building its
answer means building something this record removes.

**A token on disk is a credential on disk.** So is a kubeconfig, and the token is strictly narrower —
burrowd-only, one principal, revocable without touching anyone else. The new exposure is that a
stolen token reaches burrowd without appearing in the cluster's own audit trail, which is what
expiry and a revocation list are for. Both are part of building this, not follow-ups.

**burrowd gains state it did not have**: issued tokens, their principals, their revocations. It has a
database, so there is somewhere to put it, and it is small. It is still a new thing that must be
backed up and must survive an upgrade.

**Existing installs keep working.** The shared install token is what §4 preserves, so an install that
never runs `burrow auth login` behaves exactly as it does today. The change is opt-in per person at
first, and the old path stops being the default rather than stopping.

**This is a v0.15 direction, not a go-live one.** Nothing is broken today: cloud already works this
way and self-hosted works as designed. But every command written in the old shape between now and
then is a command to migrate, so it is the next thing rather than a later thing.

## Alternatives rejected

**Leave it as it is.** The confusion is not cosmetic. It produced ADR-0083's open question, it is why
`docs/CAPABILITIES.md` needs a paragraph explaining which commands honour a target, and it is why
[ADR-0027](0027-audit-log.md)'s audit log records a literal in its most important column. Each of
those looks like a small local problem and all three are the same missing thing.

**Give burrowd its own address and use bearer tokens over HTTPS everywhere.** The clean version, and
it makes a working install depend on ingress, DNS and a certificate. A private cluster may not be
able to satisfy any of that, and ADR-0014 chose the proxy precisely so connectivity needed nothing.
Separating identity from route means this can still happen later, for clusters that want it, without
being a precondition.

**A token format Burrow invents, checked against a table burrowd keeps.** The first draft of this
record proposed exactly that, and rejected per-principal Kubernetes ServiceAccounts on the grounds
that they make every Burrow user a cluster object and leave two identity systems.

It is the wrong trade, and §2 takes the ServiceAccount instead. Kubernetes already has issuance,
revocation and expiry; a Burrow-owned token table means building all three, and burrowd would then
hold long-lived credential material it currently does not. The "two identity systems" objection is
also weaker than it read: the managed product authenticates a tenant, a self-hosted cluster
authenticates an operator, and those are different questions that happen to both end in a bearer
token. What is shared is the *shape* — a stored credential, no ambient kubeconfig — which is what
makes the migration ADR-0078 exists for a target switch.

The decisive practical point is that **the mechanism already exists and is running**: ADR-0038 mints
precisely this for the agent today.

**Drop the kubeconfig entirely, including break-glass.** Purest, and it means an unreachable burrowd
is an unrecoverable install. Reliability is the argument for the whole product; removing the
recovery path to tidy the credential story trades the thing being sold for the thing being
explained.

**Identify an install by its API-server URL or its CA certificate instead of a generated id.** Both
are already in the kubeconfig and neither needs anything new. Rejected because both describe the
cluster rather than the Burrow in it: a CA changes when a cluster is rebuilt, which catches the
dangerous case, but neither notices burrowd being removed and reinstalled in place — a different
install, with different credentials, at the same address behind the same certificate. §1 makes the
install the thing being targeted, so the install is what should be identified.

**Wait until after multi-user and SSO are designed.** Those need this, not the other way round. A
per-principal token is what an SSO identity would be attached to.
