# ADR-0040: Burrowd never contacts the registry; Kubernetes resolves and pulls

## Status

✅ Accepted

## TL;DR

Burrowd never authenticates to or contacts a container registry. It applies the workload by
image reference and lets the kubelet resolve and pull the image with the `imagePullSecret`
([ADR-0017](0017-private-registry-authentication.md)). The pre-deploy `registry.Resolve` (a
`remote.Head` to the registry) is removed: the digest it produced was recorded on the release
but never used to deploy or roll back, it contradicted ADR-0017's "the credential is consumed
only by the kubelet, never by burrowd," and it made private-image deploys fail with a 500
because burrowd has no registry credential (and, by ADR-0017, no secrets access at all). The
release's image is identified by its reference; the resolved digest, if wanted, is read back
from Kubernetes, never resolved by burrowd.

Extends [ADR-0017](0017-private-registry-authentication.md); builds on
[ADR-0002](0002-four-layer-architecture.md) and [ADR-0004](0004-code-never-over-mcp.md);
realizes [ADR-0006](0006-guardrails-in-the-control-plane.md) (pull failures surface as
structured status). Supersedes nothing.

## Context

[ADR-0017](0017-private-registry-authentication.md) decided that a private-registry credential
is provisioned with the developer's kubeconfig as a `dockerconfigjson` pull secret
(`burrow-registry`, multiple registries merged into one), delivered to app Pods via the app
namespace's default ServiceAccount, and **consumed only by the kubelet — never by burrowd, and
never over MCP.** It also records that burrowd runs least-privilege with deliberately **no
`secrets` access**.

Independently, the deploy path grew a step that quietly steps outside that boundary:
`engine.Deploy` calls `registry.Resolve(image)`, which does a `remote.Head` against the
registry (using go-containerregistry's default keychain) to fetch the image's content digest.
That digest is written to the release record — but the workload applied to Kubernetes always
uses the image **reference**, never the digest, and rollback redeploys by reference too. So
burrowd contacts the registry solely to populate an audit field it never acts on.

For a **public** image the `remote.Head` succeeds anonymously, so this went unnoticed. For a
**private** image it needs a credential burrowd does not have and, per ADR-0017, is not allowed
to have — so it returns `401`, which `engine.Deploy` surfaces as a generic error and the API
returns as **HTTP 500**. The result: a private image can be *pulled* by the kubelet (the pull
secret is set) but can never be *deployed*, because burrowd fails to resolve it first. The
resolver is a latent contradiction of ADR-0017 and the direct cause of the failure.

## Decision

**Burrowd never contacts a container registry, for any reason. It hands Kubernetes the image
reference and the kubelet does the resolving and pulling.**

### 1. Remove the resolver

Delete the pre-deploy `registry.Resolve` call, the `Registry` seam and its resolver
implementation and fake, and the go-containerregistry dependency (its only user). `engine.Deploy`
applies the workload by image reference directly; there is no registry round-trip in the control
plane on the deploy path.

### 2. The pull secret is the only registry credential, consumed only by the kubelet

Unchanged from [ADR-0017](0017-private-registry-authentication.md): `burrow config registry
login` writes the merged `dockerconfigjson` secret and the app runs under a ServiceAccount that
references it; the kubelet authenticates the pull. Multiple registries live in the one secret's
`auths` map. burrowd neither reads nor references the credential.

### 3. The release is identified by reference; the digest is read back, not resolved

The release records the image **reference**. If the immutable digest is wanted for the record,
it is read back from Kubernetes (the kubelet reports the pulled digest in
`Pod.status.containerStatuses[].imageID`) — obtained from cluster state burrowd may already
read, never by burrowd authenticating to the registry. It is a recorded/observed value, never
an input to deploy or rollback.

### 4. Image and pull failures surface as structured status, not a synchronous pre-check

Removing the resolve removes the synchronous "image not found" error at deploy time. A missing,
mistyped, or unauthorized image instead surfaces asynchronously as a Pod `ImagePullBackOff`,
which burrowd already turns into an actionable `issue` on the workload status
([ADR-0006](0006-guardrails-in-the-control-plane.md)). That message distinguishes the causes it
can (no credential for the registry versus image not found) and names the fix.

### 5. Pod delivery of the secret (noted, not decided here)

How the Pod obtains the secret — the current ServiceAccount `imagePullSecrets` patch
(namespace-wide, zero burrowd involvement) versus burrowd stamping `imagePullSecrets:
[burrow-registry]` on each app Pod by its well-known name (per-app, avoids mutating the shared
default ServiceAccount) — is orthogonal to this decision. This ADR keeps the ServiceAccount
patch; a per-app variant is a compatible refinement and does not require burrowd to hold or read
the credential either way.

## Consequences

- burrowd needs no registry credential and no `secrets` access — the deploy path is now
  consistent with ADR-0017's least-privilege rather than contradicting it.
- Private-image deploys work with only the pull secret in place; the 500 disappears by removing
  code, not by adding credential plumbing.
- One fewer dependency (go-containerregistry), a smaller graph.
- The control plane loses a synchronous image-existence check at deploy time; that feedback
  moves to the asynchronous, already-built status `issue` path. Deploy returns once the workload
  is applied, and the agent confirms health via status — the model it already follows.
- The release's stored digest becomes an observed value (or is left unset) rather than a
  resolved one; nothing that deploys or rolls back depended on it.

## Rejected alternatives

- **Give burrowd the registry credential** (grant it `secrets` access and build a keychain from
  the `burrow-registry` secret so it can resolve private images). Directly contradicts
  [ADR-0017](0017-private-registry-authentication.md): the credential is consumed only by the
  kubelet, and burrowd deliberately has no secrets access. It also re-establishes burrowd as a
  registry client, the opposite of delegating to Kubernetes. Rejected.
- **Keep resolving with the default keychain.** Works only for public images and fails silently
  for private ones — the current bug. Rejected.
- **Deploy by digest** (resolve the tag and pin the digest into the Deployment for immutability).
  Requires exactly the registry round-trip and credential this ADR removes, and the disciplined
  unique-tag guidance already gives immutable-enough references. Deferred; not worth burrowd
  becoming a registry client.
