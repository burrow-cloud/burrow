# ADR-0057: Source-provider credentials — one provider token for private-git clone and its registry

## Status

🟡 Proposed

## TL;DR

The in-cluster build ([ADR-0053](0053-in-cluster-build-from-source.md)) clones the git source
with **no credentials**, so it can only clone **public** repositories. This ADR adds a
**source-provider credential**: one token keyed by provider (`github`, `gitlab`, …) that both the
build clone uses to fetch **private git** and — where that same provider hosts a registry — the
image pull uses as registry auth, since one GitHub PAT covers `github.com` clones **and** `ghcr.io`
pulls, one GitLab token covers `gitlab.com` **and** its registry. The OSS self-host product backs
the credential with a stored **fine-grained PAT** (a per-repo SSH **deploy key** as the tighter
option); the managed product backs the same **seam** with **GitHub App** installation tokens. The
token flows the guarded way — set through the control plane, written to a Secret by burrowd, never
over MCP, never logged, never in Postgres ([ADR-0029](0029-secrets-through-the-control-plane.md),
[ADR-0030](0030-credentials-through-the-control-plane.md)) — and is mounted into the clone
container via a git credential helper / `url.insteadOf`, not passed as a tool argument.

Extends [ADR-0053](0053-in-cluster-build-from-source.md) (gives its clone a private-source path);
realizes the credential transport of [ADR-0029](0029-secrets-through-the-control-plane.md) /
[ADR-0030](0030-credentials-through-the-control-plane.md); reuses the OSS/managed seam split of
[ADR-0045](0045-oss-enterprise-boundary.md) and [ADR-0053 §6](0053-in-cluster-build-from-source.md);
unifies with, but does not replace, the per-host registry login of
[ADR-0017](0017-private-registry-authentication.md) (`burrow config registry`).
This is the generalized solution to #279. Supersedes nothing.

## Context

The in-cluster build ([ADR-0053](0053-in-cluster-build-from-source.md)) clones a git reference
inside the user's cluster and builds it. The clone init container is handed only the repository URL
and the ref — **no token, no SSH key, no credential helper** — so a private `--source` fails at the
fetch:

```
fatal: could not read Username for 'https://github.com': No such device or address
```

(git falls back to an interactive credential prompt, which has no TTY in a Job.) The same message
appears for a **nonexistent** repo, because a host like GitHub returns *auth-required* rather than
leak whether a private repo exists. So today in-cluster build is **public-source-only**, which
excludes the common case: a solo developer's app lives in a **private** repo.

Fixing this means introducing a credential the clone can use. Two forces shape where it lives and
how it is supplied:

- **Burrow's invariants constrain the transport.** A credential must never cross MCP or be handled
  by the agent ([ADR-0004](0004-code-never-over-mcp.md),
  [ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md)); Burrow already has a guarded path
  for exactly this — the value traverses burrowd's authenticated API and is written into a Secret,
  never logged and never in Postgres ([ADR-0029](0029-secrets-through-the-control-plane.md),
  [ADR-0030](0030-credentials-through-the-control-plane.md)).

- **The credential need is not unique to the clone.** Getting a private image out of the same
  provider's registry already needs a credential ([ADR-0017](0017-private-registry-authentication.md)),
  and [ADR-0046](0046-registry-onboarding.md) already observed that a **code-provider token wires
  the code-provider registry**: a GitHub PAT authenticates both `github.com` git and `ghcr.io`
  pulls; a GitLab token authenticates both `gitlab.com` git and its container registry. Introducing
  a *second*, separate token for the clone when one provider token already covers both git and
  registry would be redundant setup for the user.

Two further forces decide *what kind* of token, and they pull in opposite directions for the two
products. A **PAT** is the one concept every provider shares and needs no inbound endpoint. A
**GitHub App** is the provider-blessed shape for the managed product — but it requires an inbound
OAuth **callback endpoint** that a private, NAT'd ICP does not have (the same reachability wall that
made [ADR-0052](0052-pull-based-passive-deploy.md) and
[ADR-0053](0053-in-cluster-build-from-source.md) poll rather than accept a webhook), and it either
centralizes trust in a project-owned App (contradicting Burrow's you-keep-root posture) or adds more
setup than a PAT. So the *shape* of the credential must differ by product while the *thing the build
asks for* — "a usable token for this provider and repo" — stays the same.

## Decision

### 1. A source-provider credential, keyed by provider, holding one token

Burrow adds a **source-provider credential**: a credential **keyed by provider** (`github`,
`gitlab`, …), each holding **one token**. It is referenced by **both** consumers that authenticate
to that provider:

- the **build clone** ([ADR-0053](0053-in-cluster-build-from-source.md)), to fetch a **private git**
  source; and
- **where the same provider hosts a registry**, the **image pull**, as registry auth.

One credential per provider, not one per subsystem: a single `github` token covers `github.com`
clones and `ghcr.io` pulls; a single `gitlab` token covers `gitlab.com` clones and its registry.
The user configures the provider once, and both the private-source build and the provider-registry
pull are satisfied.

### 2. PAT for OSS, GitHub App for managed — behind one seam

The credential is a **seam**: an interface whose contract is *"give me a usable token for provider
X / repo Y."* The two products implement it differently, and neither the build clone nor the
registry-auth consumer knows which is behind it:

- **OSS self-host backs the seam with a stored PAT** (§4 transport). A PAT needs **no inbound
  endpoint**, is the one concept **every** provider shares, and keeps trust with the user who holds
  the token — it fits the private/NAT'd ICP and the you-keep-root posture.

- **The managed product backs the same seam with GitHub App installation tokens.** The managed
  control plane **has** a public OAuth callback endpoint and centralization is appropriate there, so
  it uses the provider-blessed App and mints short-lived installation tokens behind the identical
  interface.

This is the **same OSS/managed seam split** as the `Builder`
([ADR-0053 §6](0053-in-cluster-build-from-source.md)) and poll-vs-webhook
([ADR-0052](0052-pull-based-passive-deploy.md)): the OSS interface describes the need, and each
product supplies the trust model it can support ([ADR-0045](0045-oss-enterprise-boundary.md)). The
OSS interface is not coupled to the managed product's App machinery.

### 3. The credential is provider-shaped, not host-shaped

The source-provider credential is keyed by **provider identity**, because a provider is exactly the
unit across which one token is valid for both git and registry. This is distinct from the per-host
registry login ([ADR-0017](0017-private-registry-authentication.md), the `burrow config registry`
credential), which is keyed by **registry host** and knows nothing about git. The two coexist; §5
governs their overlap.

### 4. Guarded transport — set through the control plane, mounted, never a tool argument

The token follows the guarded credential path of
[ADR-0029](0029-secrets-through-the-control-plane.md) /
[ADR-0030](0030-credentials-through-the-control-plane.md), unchanged:

- It is **set through the control plane** — a human setup action sends the value to burrowd's
  authenticated API, which **writes it into a Secret**. The registry records the provider key only;
  the value is **never carried over MCP**, **never logged**, **never in an error or an API response
  body**, and **never written to Postgres**. There is **no MCP tool** that sets or reads a
  source-provider token — the agent references the provider by key and asks the human, exactly as it
  does for app secrets and provider credentials ([ADR-0004](0004-code-never-over-mcp.md)).

- It is **consumed by mounting, not by passing**. For the **clone**, burrowd mounts the token into
  the clone container and configures git to use it via a **credential helper** or a
  `url.<base>.insteadOf` rewrite that injects the token for that provider's host — so the token
  never appears as a `--source` argument, a Job env var visible in the spec, or a command line. For
  the **registry**, burrowd materializes the provider token into the `dockerconfigjson` pull path
  for that provider's registry host, the same mechanism the kubelet already uses
  ([ADR-0017](0017-private-registry-authentication.md)).

The build clone thus asks the §2 seam for a token and mounts what it returns; whether that token is
a stored PAT (OSS) or a freshly minted App token (managed) is invisible to it.

### 5. Relationship to `config registry` — sharing is allowed, not forced

A source-provider credential **MAY** cover that provider's registry, unifying with
`burrow config registry` ([ADR-0017](0017-private-registry-authentication.md)): configuring the
`github` provider can wire `ghcr.io` pulls too, so the user does not separately
`config registry login ghcr.io`. But **non-provider registries keep the per-host `config registry`
login** — Docker Hub, a private company registry, or the in-cluster registry
([ADR-0053](0053-in-cluster-build-from-source.md)) have **no source provider** and are configured
per host as they are today. **Sharing is allowed, not
forced:** a user may point the build at a private-git provider and still pull the built image from a
different registry, and a user with only public source but a private registry uses `config registry`
alone. Neither command becomes a prerequisite for the other.

### 6. Scope guidance — least privilege recommended, one broad PAT the pragmatic default

Burrow guides the user toward a **least-privilege** credential without forcing it:

- **Fine-grained PAT (recommended):** a fine-grained token scoped to **read-only contents** on the
  **specific repositories** the user builds, plus **`read:packages`** where the provider registry is
  shared (§5). This bounds the token's blast radius to exactly what the build and pull need.

- **Per-repo deploy key (tightest, supported):** for git clone, an SSH **deploy key** scoped to a
  **single repository** is tighter still and is supported as the git-only credential form for users
  who want per-repo isolation. A deploy key authenticates the clone only; a shared provider registry
  still needs a token.

- **One broad classic PAT (pragmatic default):** for the solo developer who is Burrow's first user,
  a single broad PAT that covers all their repos and the provider registry is the pragmatic default
  — fewer tokens to manage, at a wider blast radius the guidance names honestly.

## Consequences

- **Private-source builds work.** In-cluster build is no longer public-only; the common case (a
  private app repo) builds, closing #279.
- **One token, two uses.** A single provider token satisfies both the private-git clone and the
  provider-registry pull, so a GHCR/GitLab user configures one credential instead of two — the
  unification [ADR-0046](0046-registry-onboarding.md) anticipated.
- **All invariants hold.** The token never crosses MCP and is never handled by the agent
  ([ADR-0004](0004-code-never-over-mcp.md)); it rides the guarded control-plane transport
  ([ADR-0029](0029-secrets-through-the-control-plane.md),
  [ADR-0030](0030-credentials-through-the-control-plane.md)); it is mounted, never passed as a tool
  argument (§4).
- **The OSS/managed seam is preserved.** OSS stores a PAT and the managed product mints App tokens
  behind one interface, with no OSS dependency on App machinery
  ([ADR-0045](0045-oss-enterprise-boundary.md)) — the same split the Builder and the poller already
  use.
- **burrowd sees the token in transit.** Acceptable on the same terms as
  [ADR-0029](0029-secrets-through-the-control-plane.md) / [ADR-0030](0030-credentials-through-the-control-plane.md):
  burrowd is the trust boundary, its Secret access is scoped, and the no-log / no-response / no-MCP
  guards are the real risk surface and are tested, not assumed.
- **A broad PAT is a real blast radius.** The pragmatic default (§6) is a token that can read all of
  the user's repos and packages; the guidance names the fine-grained PAT and the deploy key as the
  tighter options so the wide default is a deliberate choice, not an accident.
- **Distinguish the private-repo error.** With a credential path in place, the build can turn the
  raw `could not read Username` failure into an actionable message — *public repo required / repo
  not found / private repo needs a source-provider credential* — which the credential-less path
  (#279) could not disambiguate.

## Rejected alternatives

- **A GitHub App for the OSS self-host product.** Rejected: a GitHub App needs an inbound OAuth
  **callback endpoint** that Burrow's private/NAT'd ICP does not have — the same reachability wall
  that made [ADR-0052](0052-pull-based-passive-deploy.md) and
  [ADR-0053](0053-in-cluster-build-from-source.md) poll rather than accept a webhook. It also either
  **centralizes** trust in a project-owned App (contradicting the you-keep-root posture) or adds
  more setup than a PAT, and it is **GitHub-specific** where a PAT is the one concept every provider
  shares. The managed product **has** a public callback and centralization is fine there, so it uses
  a GitHub App behind the same seam (§2).

- **Separate per-subsystem tokens (one for clone, one for registry).** Rejected as redundant: one
  provider token already authenticates both `github.com` git and `ghcr.io` (and likewise GitLab), so
  requiring a second, distinct token for the clone is setup with no security gain
  ([ADR-0046](0046-registry-onboarding.md)). The source-provider credential is keyed by provider
  precisely so one token serves both (§1). Users who *want* isolation still can — a git-only deploy
  key plus a separate registry login (§5, §6).

- **Carry the token over MCP / as a `--source` argument.** Rejected: it would put a credential on
  the agent path ([ADR-0004](0004-code-never-over-mcp.md),
  [ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md)) or bake it into a Job spec where it
  is logged and inspectable. The token is set through the control plane and mounted via a credential
  helper (§4).

- **Fold the provider credential into `config registry` (host-keyed only).** Rejected: a
  registry-host key knows nothing about git and cannot express "this token also clones
  `github.com`." The provider key is the unit across which one token is valid for both git and
  registry (§3). The per-host `config registry` login stays for **non-provider** registries (§5),
  but it is not the home for a source-provider token.

- **A broad PAT as the only supported form.** Rejected: fine-grained PATs and per-repo deploy keys
  are supported and recommended so a security-conscious user can bound the blast radius; the broad
  PAT is offered as the pragmatic solo-developer default, not the only option (§6).
