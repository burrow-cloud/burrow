# ADR-0023: Provider credentials — a registry of vendor tokens in one scoped Secret

## Status

Accepted. Refines the credential-storage detail of
[ADR-0018](0018-reaching-an-app-at-a-url.md) (which described the DNS token as "injected into
burrowd via the pod spec"). This ADR replaces that mechanism for all vendor credentials. The
credential **transport** rule here (token written kubeconfig-direct, never reaching burrowd) is
superseded by [ADR-0030](0030-credentials-through-the-control-plane.md); the storage model (one
scoped `burrow-credentials` Secret, the non-secret registry, call-time reads) stands.

## Context

v0.2 needs to call a third-party API on the user's behalf — first DigitalOcean or Cloudflare
for DNS, later cloud providers for compute, registries, and more. Each needs a credential
(an API token). Three forces shape how Burrow holds it:

1. **It's bigger than DNS, and bigger than one vendor.** A user commonly splits services —
   compute on DigitalOcean, DNS on Cloudflare. So a credential is per-**provider** (a.k.a.
   vendor), and a capability (DNS) is *served by* a chosen provider. The mechanism must be a
   general provider-credential registry, not a one-off DO DNS token.
2. **Adding or rotating a token must not restart burrowd.** Injecting the token as an
   environment variable from a Secret (the way `BURROW_DATABASE_URL` is wired) would force a
   pod restart on every add or rotation, and a Deployment patch per provider.
3. **burrowd stays least-privilege.** Its Role is deliberately free of broad `secrets` access;
   whatever we add must be the minimum.

## Decision

**Store every vendor token in one Kubernetes Secret; keep the structure in the database;
burrowd reads the token through the API at call time.**

- **One `burrow-credentials` Secret** (control-plane namespace) holds all tokens — one key per
  provider (e.g. `digitalocean`, `cloudflare`). It is an opaque bag of key→token; it carries
  no schema.
- **The database is the registry.** A `providers` table (goose-migrated, ADR-0013) records
  which providers are configured, their type, their capabilities, and which Secret key holds
  the token. The *meaning and structure* live here, where they version cleanly — never in an
  implicit file or env layout.
- **burrowd reads a token via the Kubernetes secrets API** at the moment it makes a provider
  call (so a rotated token is picked up with no restart). Its only new permission is a Role in
  the control-plane namespace granting `get` on `secrets` **restricted to the one object**:

  ```yaml
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["burrow-credentials"]
    verbs: ["get"]
  ```

  That is the tightest grant Kubernetes allows: burrowd can read exactly `burrow-credentials`
  in its own namespace — not the database secret, not the API-token secret, no other secret,
  and it cannot even `list`. burrowd never **writes** the Secret.

- **`burrow provider add <type> [--name <name>] --token-stdin`** is a setup command: with the
  developer's kubeconfig it upserts the token as a key in `burrow-credentials`, and registers
  the provider in the database through burrowd's API. This is the setup-vs-operation split of
  [ADR-0017](0017-private-registry-authentication.md): the human's kubeconfig writes the
  Secret; burrowd only reads it. The command **validates the token** with a test API call
  before saving, so a bad token fails immediately rather than at first use.

- **Capabilities bind to a provider explicitly** for now: an operation that needs a capability
  (e.g. `burrow domain add <host>` needs DNS) takes an explicit `--provider <name>`.
  Auto-detection (ask each DNS-capable provider whether it owns the zone) is a later
  refinement.

`burrow install` creates the (initially empty) `burrow-credentials` Secret and adds the scoped
`get` Role, so the mechanism is present from install on.

## Consequences

- burrowd gains exactly **one** new permission — `get` on a single named Secret in its own
  namespace. No app-namespace secrets, no `list`/`watch`, no writes. A k8s admin reading the
  Role sees precisely "burrowd may read one credentials object."
- **No restart on add or rotation:** burrowd reads the token fresh from the API each call; a
  rotated token is updated in the Secret (by the human's kubeconfig) and used immediately.
- **DNS is the first consumer.** A capability seam (a DNS-provider interface — create/update/
  delete records — with DigitalOcean and Cloudflare adapters and a fake) sits on top of the
  registry. Future compute/registry providers plug into the same registry.
- **Refines ADR-0018:** the DNS credential is no longer "injected via the pod spec"; it is a
  key in `burrow-credentials` read via the scoped grant, with the registry in the database.
- A `providers` table and a small provider/credential service in the control plane; the
  `provider` CLI command group; and burrowd's credential reader (cache optional; per-call read
  is fine — provider calls are infrequent).
- **Future caveat:** this is read-only for **static API tokens**. An OAuth/refresh-token
  provider, where the issuer hands back a new token burrowd must persist, would need write
  access to the Secret — out of scope here; revisit if such a provider is added.

## Rejected alternatives

- **Inject the token as an environment variable from a Secret.** Rejected: forces a burrowd
  restart on every add and rotation, and a Deployment patch per provider.
- **Mount `burrow-credentials` as a volume (kubelet-delivered files, no RBAC).** Considered —
  it needs no `get` grant because the kubelet, not burrowd, reads the Secret. Rejected: it
  reads as circumventing the standard secrets API, and the on-disk file layout becomes an
  implicit, hard-to-version schema. The database registry is the cleaner versioned contract,
  and the `resourceNames`-scoped `get` grant is minimal regardless.
- **Hardcode DigitalOcean as the only DNS provider.** Rejected: users split compute and DNS
  across vendors (DO + Cloudflare); the provider/capability model supports that from the start.
- **Store tokens in the database, not a Secret.** Rejected: secrets belong in Kubernetes
  Secrets (RBAC-gated, encrypted at rest where configured, kept out of the app database). The
  database holds only the non-secret registry.
