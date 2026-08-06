# ADR-0088: An app deploys only from its own repository

## Status

🟡 Proposed

## TL;DR

App remember which repository it deploy from. Agent move tag freely, cannot change repository.

- **Deploy today take any image string.** Nothing check where it come from, so one ordinary deploy
  call point an app at any image on the internet.
- **App gain a pinned repository.** Deploy compare `imageRepository(ref)` against it and refuse a
  mismatch. Tag and digest still free.
- **Auto-deploy already work this way** — `autodeploy_poller.go` only ever move the tag inside the
  running release's repository. This bring the manual path level with it.
- **Constrain WHAT, not WHETHER.** Guardrail say if an operation may run; this say what it may point
  at. `app.deploy` stay `allow`, and the agent keep shipping.
- **Pin set on first deploy**, changed only by a human through an operator-only verb. Retagging to a
  new registry is real, so the escape exist and is deliberate.
- **Not a supply-chain control.** Bad code, built honestly, still ship. Say so plainly.

Extends [ADR-0007](0007-explicit-deploy-by-image-reference.md)'s explicit deploy by image reference.
Levels the manual path with [ADR-0052](0052-pull-based-passive-deploy.md) and
[ADR-0058](0058-auto-deploy-is-opt-in.md). Bounds what the credential
[ADR-0038](0038-scoped-agent-credential.md) mints can reach. Supersedes nothing.

## Context

**What exists today.** `burrow app deploy <app> --image <ref>` accepts any string that parses as an
image reference. The control plane resolves it, records a release, and rolls it out. Nothing compares
the reference against what the app deployed last time, or against anything else.

**What breaks.** Deploying is not a privileged verb. `app.deploy` is allowed by default
([ADR-0020](0020-guardrails-as-configurable-policy.md)), and deliberately so — an agent that cannot
ship is not worth having. But the verb is broader than the intent behind allowing it. "Let the agent
deploy this app" is what an operator means; "let the agent run any container image in the world under
this app's identity, with its Secrets and its ServiceAccount" is what they grant.

The gap is widest where it matters most. An app carrying a database DSN, an API token and a private
key through `envFrom` hands all three to whatever the deploy pointed at. Repointing it is one
ordinary call, indistinguishable at the API from a legitimate release, and the agent's scoped
credential ([ADR-0038](0038-scoped-agent-credential.md)) is sufficient for it — that credential can
reach burrowd and nothing else, so the guardrails are the *only* boundary it has.

**The obvious answer is the wrong one.** Setting `app.deploy` to `confirm` or `deny` on the sensitive
apps closes it, and costs the thing the product exists for. An agent that must ask before every
deploy is an agent that has stopped being useful, and the failure this project actually protects
against is not a stolen credential — anyone holding the operator's kubeconfig bypasses burrowd
entirely with `kubectl` and never trips a guardrail. It is **an agent doing something wrong with the
credential it legitimately has**: a mistake, a bad tool call, a prompt injection.

For that failure, blocking the operation is the crude fix. Narrowing it is the proportionate one.

**The shape already exists in the product.** Auto-deploy never had this problem, because it cannot
express it: `autodeploy_poller.go` takes `imageRepository(rel.Image)` from the running release and
only ever moves the **tag** inside that repository. The passive path has been repository-pinned by
construction since [ADR-0052](0052-pull-based-passive-deploy.md). The explicit path — the one
[ADR-0007](0007-explicit-deploy-by-image-reference.md) calls the spine — is the loose one.

**What this record resolves.** That an app deploys only from the repository it belongs to, on every
path, and how that pin is set and changed.

## Decision

### 1. An app records the repository it deploys from

The app carries a **repository**: a pullable image reference with tag and digest stripped, the value
`imageRepository` already computes (`controlplane/autodeploy.go`). It is a property of the app, beside
its environment and its namespace — not a guardrail disposition, and not policy.

A deploy compares `imageRepository(--image)` against it. Equal, the deploy proceeds and the tag or
digest is whatever the caller asked for. Different, the deploy is refused before anything is created.

Tag and digest are deliberately unconstrained. Moving between versions is the operation; moving
between repositories is the thing being prevented.

### 2. The pin is set on the first deploy, and never inferred again

The first deploy of an app records its repository. Every later deploy is checked against that
recorded value — never against the previous release, which would let the pin walk one deploy at a
time to anywhere.

This is the whole security property, and it is worth stating as a rule rather than leaving it to be
noticed: **the pin is compared against, not derived from, the running release.** Deriving it each
time gives an attacker a ratchet, and gives an honest mistake a way to become permanent.

### 3. Changing it is a human operation, on its own verb

An app's repository can be changed. Retagging to a different registry, moving an organisation, and
migrating off a registry that is going away are all real, and an unchangeable pin would be a trap
rather than a control.

It changes through an operator-only verb, not through `deploy`. A deploy that could set the pin as a
side effect of carrying a new reference is the same as having no pin.

Under [ADR-0065](0065-what-belongs-on-the-agent-surface.md) §6, which requires a new capability to
state its tier: **tier 1 — absent from the agent binary.** Not denied by default, absent. A capability
whose entire purpose is to bound what the agent may deploy cannot be one the agent can call; a
disposition an operator could loosen would put the boundary back inside the thing it bounds.

### 4. Existing apps adopt it without a flag day

An app deployed before this exists has no recorded repository. On its next deploy the repository is
recorded from that deploy, exactly as a first deploy would, and the pin applies from then on.

The alternative — refusing to deploy an unpinned app until somebody sets one — would break every
running install on upgrade to protect against something none of them has yet experienced. The cost is
that one deploy per existing app is unchecked, which is the same exposure those apps have today.

### 5. It says what it refuses, and how to proceed

A refusal names the app's repository, the repository that was asked for, and the operator verb that
changes it. Both references are already non-secret and both are already in the audit log.

The agent must be able to distinguish this from a transient failure and from a guardrail hold — it is
neither. Retrying will not help, and confirming is not offered. It is a structured refusal
([ADR-0039](0039-cli-control-plane-version-skew.md)'s shape), reported so that an agent relays it to a human
rather than working around it.

## Consequences

**The blast radius of a deploy becomes the artifacts the operator already builds.** Repointing an app
now requires push access to that app's own repository, which is a registry credential the agent does
not hold and cannot obtain through burrowd. That is a real boundary rather than a policy one.

**`app.deploy` can stay allowed, honestly.** The reason to raise it to `confirm` on a sensitive app
was the arbitrary-image case, and that is what this removes. The productive loop is unchanged.

**A downgrade is still possible, and is not addressed here.** The pin constrains the repository, not
the tag, so an older image from the same repository — including a known-bad one — still deploys. That
is a much smaller surface than an arbitrary reference and a genuinely different problem: rollback is a
legitimate operation with its own guardrail. Digest pinning would close it and would prevent shipping
a new version at all, which is the opposite of the intent.

**A force-pushed tag defeats it.** If a tag in the pinned repository is overwritten, the pin is
satisfied and the content is not what was reviewed. The answer to that is immutable tags at the
registry, or digest-addressed releases, and neither is in this record.

**It is not a supply-chain control, and must never be described as one.** Code that is compromised
before it is built, or a build pipeline that is compromised, produces an image in the right repository
that this record admits without complaint. Saying otherwise would be worse than saying nothing, since
somebody would rely on it.

**One deploy per existing app is unchecked**, per §4. That is the migration cost and it is bounded.

**Two references must agree about what a repository is.** `imageRepository` normalises tag and digest
but not host aliases (`docker.io/library/x` and `x`), so an app pinned through one spelling refuses
the other. The refusal is legible, and normalising further is its own decision.

## Rejected alternatives

**Setting `app.deploy` to `confirm` or `deny` on sensitive apps.** The status quo answer, available
today, and it works. It also converts an autonomous loop into an approval queue for the apps that
matter most, which is where the loop is worth the most. It answers *whether* when the actual problem
is *what* — and it is strictly worse than this on both axes, since a confirmed deploy of an arbitrary
image is still an arbitrary image, approved by a human reading a reference they will not recognise as
wrong.

**Deriving the pin from the previous release on every deploy.** Simpler, needs no stored property, and
matches what auto-deploy already does. Rejected because it is a ratchet: each deploy re-pins from the
last, so a single bad deploy relocates the app permanently and every subsequent deploy validates
against the wrong repository. Auto-deploy gets away with it because it never changes the reference it
was given.

**A cluster-wide registry allowlist.** Coarser and easier to administer — one list, every app. It
permits every app to deploy every other app's image, which is the wrong boundary in a multi-app
install: the interesting failure is one app being pointed at something that is legitimately in the
cluster's registry but is not that app.

**Digest pinning.** Strictly stronger and it closes the downgrade and force-push cases. It also means
an app can never deploy a new version without an operator changing the pin, which is not a
constraint, it is a stop. If the goal were to freeze an app, `deny` already does that with less
machinery.

**Signature verification (cosign, policy-controller).** The rigorous answer, and it addresses the
supply-chain case this record explicitly does not. It is also a substantially larger dependency and
operational surface — key management, an admission controller, a policy language — and it is
orthogonal rather than alternative: an app that verifies signatures still benefits from not being
repointable at a signed image from somewhere else. Worth its own record if it is ever wanted.

**Doing nothing, on the grounds that an attacker with the agent credential is already inside.** The
argument that this is theatre is the strongest one against it, and it fails on the credential
boundary: [ADR-0038](0038-scoped-agent-credential.md)'s scoped credential can reach burrowd and
nothing else, so what burrowd refuses is genuinely refused — there is no `kubectl` fallback for the
holder of that credential. It is the operator's kubeconfig that makes guardrails moot, and that is a
different credential with a different threat model.
