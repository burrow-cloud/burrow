# ADR-0017: Private registry authentication via a developer-provisioned pull secret

## Status

Accepted.

## Context

Real applications live in **private** container registries; the cluster must authenticate to
pull their images. The v0.1 slice assumed public images (the smoke tests used `nginx` and a
public GHCR package), but "make the package public" is not an acceptable answer for a real
user — it is the common case to keep a registry private and supply a credential.

The credential is sensitive, and Burrow's invariants constrain where it may live:

- **The MCP server holds no cluster credentials** ([ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md)),
  and more broadly the untrusted agent path must not carry secrets — so a registry
  credential must **not** be provided over MCP or handled by the agent.
- **burrowd runs least-privilege**: its Role grants Deployments and Pods/logs but
  deliberately **no `secrets` access** (asserted by `TestRenderManifests`). Whatever we do
  must not force granting burrowd secret-write.

Kubernetes' mechanism is an **image pull secret**: a `kubernetes.io/dockerconfigjson` Secret
the kubelet uses to authenticate the pull, referenced either per-Pod or — once — on the
ServiceAccount the Pod runs under. Burrow's app Pods set no `serviceAccountName` and no
`imagePullSecrets` (`controlplane/kube/adapter.go`), so they run under the app namespace's
**default** ServiceAccount.

## Decision

Registry authentication is **human-provided, provisioned with the developer's kubeconfig,
and consumed only by the kubelet** — never by burrowd, and never over MCP.

A new setup command, **`burrow registry`**, manages the credential:

- `burrow registry login <host> -u <user> -p <token>` (or `--from-docker-config` to lift the
  entry the developer already created with `docker login <host>`) **upserts a single
  `dockerconfigjson` Secret named `burrow-registry`** in the app namespace — multiple
  registries merge into the one file — and **ensures the app namespace's default
  ServiceAccount lists it in `imagePullSecrets`**. App Pods then inherit the credential.
- `burrow registry logout <host>` removes one registry's entry (deleting the Secret and
  detaching it from the ServiceAccount when the last entry is removed).
- `burrow registry list` shows the configured registries.

Like `install` and `upgrade`, `registry` is a **setup command that acts with the developer's
ambient kubeconfig** to mutate the cluster — distinct from the agent-driven *operations*
(`deploy`, `status`, `logs`, `rollback`, `scale`) that flow through burrowd's guarded API.
This draws an explicit line: **setup uses the kubeconfig; operations go through the control
plane.**

Because the Secret is attached to the ServiceAccount, **nothing else changes**: burrowd, its
RBAC, the deploy record, and the Deployment spec are all untouched. burrowd never sees the
credential; the agent never sees it; it lives only in the cluster, created by the human.

## Consequences

- **The setup/operation boundary becomes explicit and is now a stated rule**, not an
  accident of which commands happened to shell out to the kubeconfig.
- burrowd's least-privilege Role is preserved exactly — no `secrets` verb, no new control-plane
  state, no Deployment-spec change. The feature is purely client-side plus the kubelet.
- Attaching to the **default** ServiceAccount means every Pod in the app namespace using that
  SA inherits the pull secret. The app namespace is Burrow-managed, so this is intended; a
  namespace shared with unrelated workloads would share the credential across its Pods.
- Multiple registries are supported in one merged `dockerconfigjson`.
- `burrow registry` requires the developer's kubeconfig to permit creating Secrets and
  patching the default ServiceAccount in the app namespace — the same class of access
  `install` already needs. A scoped self-host user may need it granted.
- The agent may later be shown **read-only** status ("registry `ghcr.io` is configured") so
  it can reason about pull failures, without ever seeing the credential.

## Rejected alternatives

- **Provide the credential over MCP / through the agent.** Rejected: violates
  [ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md) and the principle that the
  untrusted agent path carries no secrets.
- **Have burrowd create the pull Secret and set per-Deployment `imagePullSecrets`.** Rejected:
  it requires granting burrowd `secrets` write (breaking the least-privilege Role the tests
  enforce) and adding control-plane state for "which secret applies to which app."
  ServiceAccount attachment needs neither.
- **Always reference a conventional pull-secret name on every Deployment, configured or not.**
  Rejected: a missing pull secret yields noisy "unable to retrieve pull secret" Pod warnings
  for public-only users. SA attachment only takes effect once a credential is actually
  provisioned.
- **Auto-sync the developer's whole `~/.docker/config.json`.** Rejected as the default
  (over-broad — copies unrelated registry credentials into the cluster); offered narrowly via
  `--from-docker-config <host>` to lift just the one entry the user authenticated for.
