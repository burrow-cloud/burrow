# Capabilities

**What this answers: "can Burrow do X?" — without reading the source and guessing.**

Burrow's capabilities are not all reachable from the code path you would expect them to be
on. Two real examples, both of which have produced the wrong conclusion:

- **Pulling from a private registry.** `burrow config registry login` writes a
  `burrow-registry` `dockerconfigjson` Secret into the app namespace and patches it onto that
  namespace's `default` ServiceAccount, so app Pods inherit it
  ([ADR-0017](adr/0017-private-registry-authentication.md)). None of that appears in
  `controlplane/kube/adapter.go` — the app Deployment carries no `imagePullSecrets` and no
  `serviceAccountName` — so reading the deploy path suggests private images are unsupported.
  They are supported.
- **A database.** `burrow addon install postgres` stands up an instance for one environment,
  with a database and a role per app ([ADR-0031](adr/0031-postgres-addon.md),
  [ADR-0067](adr/0067-one-database-instance-per-environment.md) §1) and `pg_dump`/`pg_restore`
  backups ([ADR-0032](adr/0032-postgres-backups.md)). Nothing in the deploy path mentions a
  database; the connection string arrives through the app's Secret at attach time.

Reading one code path and finding nothing is not evidence a capability is absent. Nor is an
ADR evidence a capability is present — Burrow's ADRs record decisions, not implementation
status ([ADR-0009](adr/0009-honest-status.md)), and several accepted decisions are not built.
This file is the place both questions get answered together.

## How to read this

- **Scope: the code on `main`.** Every capability listed in a table below exists in the code.
  For which *release* carries it, the [README](../README.md) version table is authoritative.
- **Decided but not built** is called out explicitly, inline where an ADR would otherwise
  mislead, and collected in [Decided but not built](#decided-but-not-built).
- **Limits are stated next to the capability, not in a footnote.** A reference that lists only
  strengths causes exactly the wrong conclusions this file exists to prevent. See also
  [What Burrow does not do](#what-burrow-does-not-do).
- Two CLIs appear throughout: `burrow`, the operator CLI, and `burrow-agent`, the scoped
  channel an agent drives ([ADR-0049](adr/0049-burrow-agent-scoped-cli-control-channel.md)).
  Where a verb exists on only one of them, the table says so.

---

## Deploying and running an app

| Capability | Command | What it does | ADR |
| --- | --- | --- | --- |
| Deploy by image reference | `burrow app deploy <app> --image <ref>` | Creates or updates one `apps/v1` Deployment named after the app. This is the spine — every other path (build, auto-deploy, rollback) ends here. | [0007](adr/0007-explicit-deploy-by-image-reference.md), [0011](adr/0011-kubernetes-integration.md) |
| Override the entrypoint | `burrow app deploy <app> --image <ref> -- ./worker --queue emails` | Sets the container's `command`. There is no separate `args`. | [0007](adr/0007-explicit-deploy-by-image-reference.md) |
| Set replicas | `--replicas N` on deploy, or `burrow app scale <app> <n>` | `scale` issues a replicas-only patch and records no release. `--replicas 0` on a deploy means "keep the current count". | [0020](adr/0020-guardrails-as-configurable-policy.md) |
| Autoscale | `burrow app autoscale <app> --min --max --cpu [--memory]` | Creates an `autoscaling/v2` HorizontalPodAutoscaler targeting the Deployment. Defaults: min 1, max 10, CPU 80%, memory off. `burrow app autoscale <app> off` deletes it. | [0020](adr/0020-guardrails-as-configurable-policy.md) |
| Roll back | `burrow app rollback <app>` | Re-applies the release the current one supersedes — **exactly one step back**; there is no "roll back to release X". | [0007](adr/0007-explicit-deploy-by-image-reference.md) |
| Release history | `burrow app history <app>` | Lists releases newest-first from the control-plane database. | [0012](adr/0012-in-cluster-postgres.md) |
| Status | `burrow app status <app>`, `burrow app list` | Live workload state: desired/ready/updated replicas, availability, and — when the app is blocked — an actionable issue naming the fix, plus a machine-usable reason from a closed set: `ImagePullBackOff`, `ErrImagePull`, `Unschedulable`, `VolumeUnavailable`, `CrashLoopBackOff`, `CreateContainerConfigError`, `OOMKilled`, `ProgressDeadlineExceeded`, `DeadlineExceeded`. Conditions that resolve on their own (`ContainerCreating`, `PodInitializing`) are deliberately not reported. A crash loop carries a bounded tail of the container's own output; a missing config or secret **key** is named, a value never is. | [0011](adr/0011-kubernetes-integration.md), [0074](adr/0074-burrow-observes-what-it-manages.md) |
| Run a one-off command | `burrow app run <app> -- ./migrate` | Runs the command as a `batch/v1` Job from the app's **currently deployed image**, in the app's namespace, with the app's config and per-app Secret injected. Waits synchronously, captures the exit code and combined output. | [0048](adr/0048-one-off-command-runner.md) |
| Run a command around a deploy | `burrow app hook set <app> --on pre-deploy -- ./migrate`, plus `hook list` / `hook unset` | Stores a command per (app, environment, phase). `pre-deploy` runs on **every** deploy path, automated ones included, from the image **being deployed**, before anything reaches the cluster; its failure aborts the deploy. `pre-rollback` is unset by default and runs from the image being rolled back **away from**. See [Lifecycle hooks](#lifecycle-hooks). | [0072](adr/0072-deploy-and-run-lifecycle-hooks.md) |
| Auto-deploy on a new tag | `burrow app auto-deploy <app> <patch\|minor\|major\|off>` | burrowd polls the registry (~5 min, jittered) and fires the same guarded deploy for an in-level upgrade. Outbound-only, so it works on a NAT'd cluster. | [0052](adr/0052-pull-based-passive-deploy.md), [0058](adr/0058-auto-deploy-is-opt-in.md) |
| Delete an app | `burrow app delete <app>` | Removes the workload, its Service and Ingress, and its release history. Guarded by `app.delete`, **denied by default** — relax the guardrail (ideally per environment) before the verb will run on either CLI. | [0024](adr/0024-cli-command-taxonomy.md), [0065](adr/0065-what-belongs-on-the-agent-surface.md) |

Every verb above except `auto-deploy` and `hook` also exists on `burrow-agent` (`deploy`, `scale`,
`autoscale`, `rollback`, `run`, `delete`, `apps`, `status`, `history`). Setting the auto-deploy
level and setting a lifecycle hook are deliberately operator actions: both are standing authority
for something that happens with nobody watching.

### What the app Pod actually looks like

The Pod template is a fixed literal in Go. It sets exactly this: one container (named after
the app) with `image`, `command`, `env`, and `envFrom`; the labels
`app.kubernetes.io/name` and `app.kubernetes.io/managed-by`; a `burrow.cloud/release`
annotation so every release rolls the workload; and `prometheus.io/*` scrape annotations when
`--metrics-port` is given.

Nothing else. **The app Pod has no `Volumes` and no `VolumeMounts`**, and no resource
requests or limits, no liveness/readiness/startup probes, no `securityContext`, no
`serviceAccountName`, no `imagePullSecrets`, no `nodeSelector` and no tolerations. There is no
CLI or API surface for any of them. Pods therefore run under the namespace's `default`
ServiceAccount — which is how the private-registry pull secret reaches them (see
[Registries](#registries-and-credentials)).

The one escape hatch is `Adapter.WithPodMutator` ([ADR-0061](adr/0061-deploy-pod-mutator-seam.md)),
a `func(*corev1.PodSpec)` applied on both create and update. It is a **compile-time seam for an
embedder**: nothing in this repository wires one, and it is not reachable from the CLI or the
API. A nil mutator leaves the Deployment byte-for-byte unchanged, which is pinned by a test.

It also covers the **one-off command Job** of `burrow app run`
([ADR-0048](adr/0048-one-off-command-runner.md)), which runs the app's own image in the app's
namespace with the app's environment and therefore faces the same admission and scheduling
constraints as the app itself.

Pods running **Burrow's own images** take a second hook, `Adapter.WithPlatformPodMutator`
([ADR-0073](adr/0073-placement-policy-reaches-every-authored-pod.md)): the add-on instance
Deployment, the log-collector DaemonSet, the metrics-collector Deployment, and the backup and
restore Jobs. Two hooks rather than one because the two sets take different placement policy — an
operator may want the tenant's image on tenant-only nodes and their own Postgres and collectors
somewhere else — and one hook could serve that only by having the operator guess which kind of pod
it was handed. The build Job has a third hook of its own, `WithBuildPodMutator`.

Both hooks are equally unwired here, and a nil mutator leaves every one of those objects
byte-for-byte unchanged, pinned per path by a test. **Wiring nothing sandboxes nothing**: this is a
seam that makes an operator's isolation reachable, not isolation Burrow enforces. Enforcement is
admission policy on the cluster, not a hook the same binary can decline to wire.

Pods Burrow **causes to exist but does not author** take a third seam,
`Adapter.WithControllerPodPlacement` ([ADR-0077](adr/0077-placement-policy-for-pods-burrow-does-not-author.md)).
Where a third-party controller composes the pod from a custom resource Burrow creates, there is no
pod spec to hand a `func(*corev1.PodSpec)`, so this one is a **value** — `kube.PodPlacement`: node
selector, tolerations, node/pod affinity, topology spread — translated into the fields that
controller offers. Policy the target has no field for is **refused when it is wired**, naming the
JSON path, because a CRD's structural schema prunes unknown fields silently and an operator who is
not told their policy was dropped believes it is in force. The zero value writes nothing at all.
Nothing in this repository wires it, and no resource is composed through it yet — the Postgres
add-on becomes a CloudNativePG `Cluster` in a separate change
([ADR-0066](adr/0066-postgres-on-cloudnativepg.md)).

### Lifecycle hooks

A hook is a command Burrow runs at a named moment, stored per app, environment, and phase
([ADR-0072](adr/0072-deploy-and-run-lifecycle-hooks.md)). It exists because auto-deploy ships an
image with **nobody present**: a user who enables it and changes their schema otherwise has no
supported way to migrate. **Unset means nothing runs**, which is exactly how Burrow behaved before
hooks existed.

| Phase | When it runs | From which image | Default |
| --- | --- | --- | --- |
| `pre-deploy` | Before **any** deploy's image reaches the cluster — an explicit `burrow app deploy`, a build that ends in a deploy, and an unattended auto-deploy alike | the image **being deployed**, so the migration ships with the code that needs it | unset |
| `pre-rollback` | Before a rollback puts the older image back | the image being rolled back **away from**, because that is where the code that knows how to undo its own migration lives | unset, and leaving it unset is correct for anyone who migrates forward only |

**A rollback fires `pre-rollback` and never `pre-deploy`**, even though a rollback is mechanically a
deploy of an older image. Rolling back B to A, A's migration tool does not know B's migration
exists, so running it would step back one of *A's own* migrations instead — worse than doing
nothing. This is pinned by a test.

The command runs as a `batch/v1` Job in the app's namespace, from the named image, with the app's
config and per-app Secret injected exactly as `burrow app run` does — the same machinery, the same
ten-minute bound, the same pod mutator. **A failed hook aborts the operation it preceded**: the new
image does not roll out, the running version keeps serving, and the failure comes back as the
deploy's own failure (HTTP 422, `code: hook_failed`) carrying the phase, the exit code, and the
command's captured output. The Job is left for an hour so a failure can be inspected. A hook that
does not finish inside the run window, or whose pod cannot start, is a failure of the same kind.

Hooks are **serialized per app and environment**: two pushes in quick succession queue rather than
running two migration Jobs against one database. The lock is in-process, which is sound because
burrowd runs a single replica.

Limits worth knowing: setting a hook is an **operator action** — `burrow app hook` is not on the
`burrow-agent` surface, because a pre-deploy hook is standing authority for a command that runs on
every deploy. **Burrow does not understand migrations** — it runs your command, and versioning,
ordering, and idempotency stay your tool's job. A hook shares `burrow app run`'s **ten-minute**
bound, so a migration slower than that is reported as a failure. The audit log records the phase,
the command, the image, and the exit code, and **never the command's output**. And `post-deploy` —
the third phase [ADR-0072](adr/0072-deploy-and-run-lifecycle-hooks.md) §4 names — is **not built**:
setting it is refused rather than silently stored (see
[Decided but not built](#decided-but-not-built)).

### Deploy does not wait for the rollout

`deploy` returns once the Kubernetes API server accepts the Deployment write. A release marked
`deployed` means "the object was accepted", not "the Pods are healthy". There is no readiness
gate, no rollout timeout in Burrow, and **no automatic rollback on a failed rollout** — a bad
release is surfaced afterwards by `burrow app status` (which reports the pull error or the
stalled progress condition) and rolled back by an explicit `burrow app rollback`.

Release history is unbounded: releases are never pruned, only deleted wholesale when the app is.

---

## Getting an image built

Two build paths, plus the option of bringing an image you built yourself
([ADR-0008](adr/0008-two-build-paths.md)).

| Path | Command | What it does | Limits |
| --- | --- | --- | --- |
| Client-side | `burrow app deploy <app> --image <ref> --build <dir>` | Shells out to `docker build -t <ref> <dir>` then `docker push <ref>`, then deploys. | Needs a working Docker daemon and a prior `docker login` on the operator's machine. `--image` is required. Burrow inspects nothing — the directory must build under `docker build`. |
| In-cluster from a git ref | `burrow app build <app> --source <repo> --ref <sha-or-tag> [--image <target>]` | burrowd clones the ref and builds in a Job in the dedicated `burrow-builds` namespace, then the built image rejoins the guarded deploy path with its digest pinned. Only the git reference crosses the control channel, never code ([ADR-0004](adr/0004-code-never-over-mcp.md)). | See below. |

The in-cluster build ([ADR-0053](adr/0053-in-cluster-build-from-source.md)), concretely: an
`alpine/git:2.45.2` init container does a depth-1 fetch and checkout; the build container is
`ghcr.io/burrow-cloud/burrow-builder` and picks **buildah when a Dockerfile is present** and
the **Cloud Native Buildpacks lifecycle when it is not**. One attempt (`backoffLimit: 0`),
250m CPU / 512Mi requested and 2 CPU / 2Gi limited, a 30-minute wait, and a
`ttlSecondsAfterFinished` on the finished Job set from `build.job_retention` (three days by default
— see [Operational limits](#operational-limits); a success is deleted immediately, a failure is
left in place for diagnosis). Job names are content-derived, so re-running the same
repo/ref/target reuses a succeeded or active build. A capacity pre-flight refuses the build
when no node has room.

Sharp edges on that path:

- The **build container runs `privileged`** with `seccompProfile: Unconfined` and a writable
  root filesystem ([ADR-0059](adr/0059-oss-build-container-runs-privileged.md), superseding
  [ADR-0056](adr/0056-build-security-context-for-the-oss-builder.md)) — building a container
  inside a container needs it, and the trust argument is that the cluster and the source both
  belong to the same single tenant. The clone init container keeps the restricted floor.
- The **Buildpacks (no-Dockerfile) path cannot push to the plain-HTTP in-cluster registry.**
  It fails fast and says so; push to an external registry with `--image` instead.
- **Private git works, over HTTPS only.** A source-provider credential
  ([ADR-0057](adr/0057-source-provider-credentials.md)) is
  mounted as a git credential helper and a registry auth file, keyed by host —
  `github.com`/`ghcr.io` and `gitlab.com`/`registry.gitlab.com` only. An `ssh://` or `git@`
  remote gets no credential, and self-hosted GitHub Enterprise or GitLab hosts are not mapped,
  so both clone anonymously.
- `burrow-agent build` requires `--image`; the operator CLI can omit it.

---

## Reaching an app at a URL

| Capability | Command | What it does | ADR |
| --- | --- | --- | --- |
| Publish at a hostname | `burrow app publish <app> --host <fqdn> --port <n> [--tls]` | Creates a ClusterIP Service (`80` → the container port) and an Ingress for that one host. With `--tls`, annotates it for cert-manager and names a `<app>-tls` Secret. Both `--host` and `--port` are required. | [0018](adr/0018-reaching-an-app-at-a-url.md), [0041](adr/0041-flatten-path-to-a-reachable-app.md) |
| Remove routing | `burrow app unpublish <app>` | Deletes that Service and Ingress. Leaves the Deployment, the TLS Secret, and any DNS record alone. Not guardrailed. | [0024](adr/0024-cli-command-taxonomy.md) |
| Install the routing substrate | `burrow cluster ingress install --email <you>` | Installs ingress-nginx `controller-v1.11.3` (cloud manifest, so a `type=LoadBalancer` Service) and cert-manager `v1.16.2` if absent, then a Let's Encrypt `ClusterIssuer` (default name `letsencrypt`; `--staging` selects the staging directory). Detect-and-skip per component. | [0022](adr/0022-routing-backend-and-supported-kubernetes.md), [0042](adr/0042-use-existing-ingress-controller.md), [0043](adr/0043-public-reachability-is-a-loadbalancer.md) |
| Public DNS records | `burrow app domain add <host> --address <ip>` / `--app <app>`; `burrow app domain remove <host>` | Creates, updates, or deletes an `A` or `CNAME` record at a configured provider. `A` when the address parses as IPv4, `CNAME` otherwise. Idempotent. | [0018](adr/0018-reaching-an-app-at-a-url.md), [0023](adr/0023-provider-credentials.md) |
| Diagnose reachability | `burrow app reachability <app> [--wait --timeout]` | Walks the chain and reports the first broken link in `blocked_on`: `deployment` → `workload` → `ingress` → `ingress controller` → `tls certificate` → `dns` → reachable. Resolves the host over public DNS and compares against the controller-assigned address. `--wait` polls (3s, 3-minute default). | [0018](adr/0018-reaching-an-app-at-a-url.md), [0041](adr/0041-flatten-path-to-a-reachable-app.md) |

**DNS providers actually implemented: DigitalOcean and Cloudflare.** Both are hand-rolled HTTP
clients supporting `A` and `CNAME` records, with a longest-suffix zone match and a 3600s TTL.
Any other provider type returns "not implemented". `burrow config provider types` also lists
`github` and `gitlab`, which are *source* providers for the build path, not DNS.

TLS is **HTTP-01 only**; there is no DNS-01 solver, even though a DNS provider token is
already present. Certificates are issued through cert-manager, not by Burrow.

Limits worth knowing before you plan around this surface:

- **The IngressClass is the hardcoded literal `"nginx"`**, and so is the cert-manager HTTP-01
  solver class. [ADR-0042](adr/0042-use-existing-ingress-controller.md) decides that Burrow
  should detect and bind to the cluster's existing controller; **that detection is not built**.
  Capability probing recognizes an ingress-nginx controller only, and `burrow cluster
  bootstrap` disables k3s's Traefik precisely so ingress-nginx owns ingress.
- **One hostname per app.** The Ingress is named after the app and carries a single rule with a
  single host and a `/` prefix path; publishing again replaces it. No path-based routing, no
  multiple hosts, no multiple backends.
- **HTTP(S) only.** No TCP or UDP exposure, no websocket configuration, and no way to set a
  custom ingress annotation — the only annotation Burrow ever writes on an app Ingress is
  `cert-manager.io/cluster-issuer`, and only with `--tls`.
- **There is no HTTP-01 pre-flight.** `publish --tls` against a host whose DNS does not yet
  point at the cluster will create the Ingress and open an ACME order that cannot complete;
  the only signal is `reachability` reporting `blocked_on: "tls certificate"`. Prerequisite
  checking covers the ingress controller and cert-manager being present, not the DNS path.
- **`publish` does not touch DNS and does not wait for a certificate.** Those are `domain add`
  and `reachability --wait`. [ADR-0041](adr/0041-flatten-path-to-a-reachable-app.md) decides a
  single operation that chains Service, Ingress, TLS, DNS and cert-wait, and that a deploy with
  a port should create a Service on its own; **neither is built** — there is no `--port` on
  `deploy`, and the only app Service in the code is created by publish.
- `reachability` never probes the app itself. "Reachable" means every link in the chain is in
  place, not that the app answered.

---

## Configuration and secrets

Two separate stores with deliberately different transport ([ADR-0028](adr/0028-app-config-and-secrets.md),
[ADR-0029](adr/0029-secrets-through-the-control-plane.md)). `deploy` takes no environment
arguments; both stores are sourced at deploy time.

| | `burrow app config` | `burrow app secret` |
| --- | --- | --- |
| Commands | `set <app> KEY=VALUE`, `unset`, `list` | `set <app> KEY=VALUE`, `unset`, `list` |
| Stored in | the control-plane Postgres (`app_env`) | a Kubernetes Secret, `burrow-app-<app>-secrets`, in the app's namespace |
| Reaches the Pod as | individual `env` entries inlined in the Pod template | `envFrom` a `secretRef` on that one Secret (`optional: true`) |
| `list` shows | keys **and values** | **keys only** |
| Scope | **app-global** — the same values apply in every environment | **per-environment**, because the Secret lives in the environment's namespace |
| On set/unset | re-applies the workload so the change rolls; `--no-restart` skips it | patches a `burrow.cloud/restarted-at` annotation so the change rolls; `--no-restart` skips it |
| With no running release | persisted, applied at the next deploy | persisted, applied at the next deploy |
| On `burrow-agent` | `config set` / `config unset` / `config` — **yes** | `secret unset` and key listing only — **`secret set` does not exist on the agent binary** |

Secret values traverse the control-plane API, are written straight into the Kubernetes Secret,
and are never written to Postgres, never logged, and never audited (the audit record carries
sorted **key names** only). The same Secret is what `burrow app run` injects, so a one-off
command sees `DATABASE_URL` and everything else exactly as the app does.

**A Secret cannot be mounted as a file.** The deploy path builds a Pod with no `Volumes` and
no `VolumeMounts` at all, and there is no `--file` or `--mount` flag. Config and secrets reach
a container as environment variables or not at all. An app that needs a credential on disk
must write it out itself at startup.

Two more limits: keys must match `^[A-Za-z_][A-Za-z0-9_]*$`, and **Burrow enforces no size
limit** on a value — the effective ceiling is Kubernetes' own Secret size limit, unenforced
and unsurfaced. Neither store is guardrailed.

---

## Registries and credentials

| Capability | Command | What it does | ADR |
| --- | --- | --- | --- |
| Private registry pull | `burrow config registry login <host> -u <user> --password-stdin` | Writes a `kubernetes.io/dockerconfigjson` Secret named **`burrow-registry`** into the app namespace and patches it onto that namespace's **`default` ServiceAccount**, so app Pods inherit it. Multi-host: repeated logins merge into one `auths` map. | [0017](adr/0017-private-registry-authentication.md) |
| | `burrow config registry logout <host>` | Removes one host; removing the last one deletes the Secret and detaches it from the ServiceAccount. | [0017](adr/0017-private-registry-authentication.md) |
| | `burrow config registry list` | Lists configured hosts. Credentials are never printed. | [0017](adr/0017-private-registry-authentication.md) |
| Vendor tokens | `burrow config provider add <cloudflare\|digitalocean\|github\|gitlab> [--name]` | Reads the token hidden from a TTY or from stdin, sends it over the control-plane API, and burrowd writes it into the `burrow-credentials` Secret. DNS tokens are **verified against the vendor before anything is written**; `github`/`gitlab` tokens are not. | [0023](adr/0023-provider-credentials.md), [0030](adr/0030-credentials-through-the-control-plane.md), [0057](adr/0057-source-provider-credentials.md) |
| Object-storage destination | `burrow config provider add s3 --endpoint <url> --access-key-id <id> [--region] [--bucket \| --create-bucket] [--retention-days N] [--confirm]` | Registers an S3-compatible backup destination. The credential is a **pair**: the id is a flag, the secret access key is read hidden from a TTY or from stdin, and both land as **two keys in the same `burrow-credentials` Secret**. Before anything is written it creates or verifies the bucket, **writes and deletes a probe object**, and reconciles the bucket's lifecycle rules against `--retention-days`. Once registered, `burrow addon backup postgres <app>` writes there and verifies the object before recording the backup. | [0063](adr/0063-object-storage-provider.md), [0023](adr/0023-provider-credentials.md) |
| In-cluster registry | `burrow cluster registry install --host <fqdn>` | Deploys Zot (`ghcr.io/project-zot/zot-linux-amd64:v2.1.2`), a 5Gi PVC, a ClusterIP Service on 5000, and an nginx Ingress with cert-manager TLS and basic auth. Wires burrowd's default build push target, and installs the generated pull credential through the `registry login` path. `uninstall` reverses it. | [0054](adr/0054-install-is-control-plane-only.md) |

Three things about this area that are easy to get wrong:

- **`burrow config registry` and `burrow cluster registry` are different things.** The first is
  external *pull* credentials; the second is an in-cluster registry to *push* to.
- **The registry login runs with the operator's kubeconfig, not through burrowd.** burrowd
  never sees the credential and cannot read that Secret. It is written **per namespace**, so an
  additional environment needs its own login (`--app-namespace`); there is no `--env` flag on
  the command.
- **burrowd never contacts the registry to pull** ([ADR-0040](adr/0040-burrowd-never-contacts-the-registry.md)) —
  the kubelet does, using the inherited pull secret. burrowd's one outbound registry call is
  tag listing for auto-deploy, and **that call is anonymous**: a private repository returns 401
  and never auto-deploys. That gap is known and open.

[ADR-0046](adr/0046-registry-onboarding.md) (auto-wiring the developer's existing code-provider
registry) is 🟡 Proposed and deliberately unbuilt; only its in-cluster-registry half shipped,
via ADR-0054.

### The object-storage provider is a backup destination, and only that

`burrow config provider add s3` registers somewhere **outside the cluster** for a backup to go
([ADR-0063](adr/0063-object-storage-provider.md)), and with one registered `burrow addon backup
postgres <app>` writes there — see [Backups](#backups) for what "wrote there" is allowed to mean.

What registration does, and why each part is there:

- **It is addressed by endpoint, not by vendor.** `--endpoint` is the S3-compatible API URL and
  the vendor is whoever answers it, so there is no vendor list to maintain. S3 compatibility is
  a spectrum rather than a standard, so a vendor is supported when it has been *tested*, not
  when it claims compatibility.
- **The credential is a pair, held as two keys in the one `burrow-credentials` Secret.** Not a
  second Secret — that would have meant widening burrowd's `resourceNames: ["burrow-credentials"]`
  grant, which is the tightest grant Kubernetes offers. The `providers` row records the two key
  *names* plus the non-secret endpoint, region and bucket, so the destination can be inspected
  without reading a Secret at all.
- **It verifies the destination by writing and deleting a probe object.** A wrong key or a
  typo'd endpoint fails *there*, while somebody is watching — not at the first scheduled backup,
  silently.
- **The bucket is recorded, never inferred.** `--create-bucket` has Burrow create its own with a
  readable prefix and a random component (bucket namespaces are global per vendor, so a fixed
  name is both likely taken and guessable) and record the name it created. `--bucket` points it
  at one that already exists, which is verified rather than assumed. Burrow only ever writes to
  the bucket it recorded.
- **It reconciles bucket lifecycle against backup retention, and refuses disagreement.** A rule
  that expires objects sooner than a retained backup needs them leaves a backup set that lists
  fine and cannot be restored — discovered during recovery. `--retention-days` declares how long
  backups must stay restorable; with no window declared, nothing prunes Burrow's backups, so any
  age-expiring rule is refused. Where the lifecycle configuration **cannot be read** — the vendor
  does not serve the API, or the credential may not read it — the check is reported as
  **unknown**, never as verified.
- **Creating a bucket is `confirm`-guarded (`bucket.create`); deleting one is not a Burrow
  operation at all.** Deletion's blast radius is every backup the platform holds, and a bucket
  name lives in a global namespace, so a mistaken argument could reach outside the cluster
  entirely. It is absent from both CLIs and `burrow-agent`'s `guard` reports it as such, naming
  the vendor's own tool as what performs it.

**Scope this credential to one bucket at the vendor wherever the vendor permits it.** It grants
write access to the store that will hold every backup, which makes it the most consequential key
in `burrow-credentials` — more so than a DNS token, whose worst case is a record you can put
back. The Secret's RBAC is the same scoped grant as every other provider's; the tighter control
available is at the vendor, and it is worth taking.

And what it deliberately does **not** do, which is as much of the decision as the feature:
no object browser, no `cp`/`sync`/`ls` of arbitrary prefixes, no presigned URLs, and no bucket
policy, IAM, replication or cross-region surface. The vendor's own CLI is better at all of those.
A capability enters here only when a Burrow feature requires it.

---

## Add-ons

A curated, compiled-in catalog of permissively licensed backing services
([ADR-0025](adr/0025-building-block-addons.md)). All four install into the `burrow-addons`
namespace as a single-replica `Recreate` Deployment plus a ClusterIP Service, with a
ReadWriteOnce PVC when they need storage. **There are exactly four.**

| Add-on | Image | Port | Storage | Extra workload |
| --- | --- | --- | --- | --- |
| `logs` | `victoriametrics/victoria-logs:v1.51.0` | 9428 | 10Gi PVC | a Fluent Bit DaemonSet (`fluent/fluent-bit:3.2.10`) reading `/var/log`, tolerating all taints |
| `metrics` | `victoriametrics/victoria-metrics:v1.115.0` | 8428 | 10Gi PVC, retention per `addon.metric_retention` (default 744h — the month it was) | a vmagent Deployment (`victoriametrics/vmagent:v1.115.0`) scraping the app **and** add-on namespaces |
| `cache` | `valkey/valkey:8.0` | 6379 | **none — ephemeral** | — |
| `postgres` | `postgres:17-alpine` | 5432 | 10Gi PVC | an always-on `quay.io/prometheuscommunity/postgres-exporter:v0.20.1` sidecar on port 9187 |

`burrow addon install <type>` takes **no tuning flags** — no `--size`, no `--storage-class`,
no `--retention`, no version override. (`--env` selects which environment the instance serves, not
how it is built.) Sizes and images are compile-time constants; the metrics add-on's retention is the
one tunable, set cluster-wide with `burrow cluster config set addon.metric_retention <duration>`
before the instance is installed ([Operational limits](#operational-limits)). No PVC sets a
`storageClassName`, so every volume lands on the cluster default
StorageClass. `burrow addon remove <type>` deletes the Deployment, Service, and collectors and
**keeps the data PVC**: reinstalling the add-on lands on the same claim and picks the data back
up, so for `postgres` the databases, roles, and role passwords survive and attached apps
reconnect on their existing `DATABASE_URL`. Destroying the volume is the separate, explicit
`--delete-data`. The removal output names the volume it kept and how to reclaim it; the
confirmation the `addon.remove` guardrail holds names the affected apps.

**`--delete-data` has a second gate the guardrail does not provide.** On a terminal it prints a
warning naming the data volume, its namespace, the attached apps whose databases are in it, and
the backup volume that survives — then requires the add-on's name to be **typed back** before
anything is removed; anything else typed, an empty line, or EOF aborts and nothing is deleted.
With no terminal to type into it **refuses** rather than proceeding, unless
`--acknowledge-data-loss` is passed, so a script that destroys databases had to say so in the
script. `--confirm` satisfies the `addon.remove` guardrail and is deliberately not that
acknowledgement. The enumeration behind the notice is best-effort: a control plane that will not
answer degrades it to the volume-concrete message rather than making a wedged add-on unremovable.
The prompt is a legibility device for humans, **not a security control** — what keeps an agent
away from this is that the whole verb is absent from `burrow-agent`
([ADR-0064](adr/0064-addon-removal-keeps-its-data.md) §2).

**`--delete-data` takes a final backup first, and removes nothing if it fails.** Where an
object-storage provider is registered, burrowd dumps every attached database to that store
*before* it destroys anything, and a backup that does not arrive aborts the whole removal with
the volume, the workload and the registry row all intact — the error naming the app and the
closed reason (`Unschedulable`, `StoreRejected`, …) rather than a timeout. "Arrived" means the
`Backup` row says `completed` at an object-store destination, which [ADR-0063](adr/0063-object-storage-provider.md)
§7 only allows once the object was written *and* read back; an in-cluster dump is never accepted
as the final backup, because it shares a failure domain with the volume about to be destroyed.
With several stores registered burrowd refuses to guess and `--backup-destination` names one.
`--skip-final-backup` destroys the data without one and says so in the output — it exists because
an add-on is often removed *because* it is wedged, and a wedged instance cannot be dumped
([ADR-0064](adr/0064-addon-removal-keeps-its-data.md) §5). With **no** object-storage provider
registered the behaviour is what it always was, and the output says no off-cluster copy was
taken. An instance that will not say which databases it holds is refused rather than destroyed
blind: `--skip-final-backup` is the way past that too.

**Removal is operator CLI only** — the whole verb, not just `--delete-data`, is absent from
`burrow-agent` like `detach` and `restore`. Because there is one add-on instance per type per
cluster and every app's database lives on the one shared `postgres`, removing it is removing
*the* add-on and takes every attached app down at once, which is
[ADR-0065](adr/0065-what-belongs-on-the-agent-surface.md) §2 tier 1: not compiled into the agent
binary at all.

**A retained volume stays visible after the removal that created it.** `burrow addon list` reports
the claims an earlier removal left behind in a section of their own, below the add-on table: the
claim name, the add-on it belonged to, whether it holds the add-on's `data` or its `backup` dumps,
its size, and its namespace — plus both ways out, reinstalling to get the data back or
`kubectl delete pvc` to get the storage back. In `--json` they are a separate `retained_volumes`
array alongside `addons`, so a retained claim is never readable as a running add-on. Nothing
reclaims them automatically and nothing should
([ADR-0064](adr/0064-addon-removal-keeps-its-data.md) §6).

Retained claims are found by reading the **cluster**, not the registry: a removed add-on leaves no
registry row, which is exactly why its volume used to be invisible. A claim is Burrow's because of
the labels it was created with (`app.kubernetes.io/managed-by=burrow` plus `burrow.cloud/addon`
naming the type), never because of its name, so a claim of your own in `burrow-addons` is not
reported. A claim is *retained* when no installed add-on of its type owns it — which is what keeps
a live add-on's own volume out of the section.

The listing reports **size, not cost.** The claim knows its capacity; the price per GiB belongs to
the provider, and ADR-0064 leaves that choice open. Reporting cost would need a per-provider price
per storage class and region, obtained from the provider's API or a table Burrow would have to keep
current — a confident wrong number about money is worse than an honest one about bytes.

One accepted decision changes the `postgres` row above and is **not built.**
[ADR-0066](adr/0066-postgres-on-cloudnativepg.md) replaces the mechanism with a CloudNativePG
`Cluster` custom resource, handing WAL archiving, scheduled backups, retention and point-in-time
recovery to the operator; the add-on is still the single-replica `postgres:17-alpine` Deployment
in the table. [ADR-0067](adr/0067-one-database-instance-per-environment.md) is built in full: one
instance **per environment** (§1), and the first environment a registered one named `prod` mapped
to the existing app namespace (§2–§3).

| Capability | Command | What it does |
| --- | --- | --- |
| Install | `burrow addon install <type> [--env]` | As above, for one environment — each gets its own instance (ADR-0067 §1). `metrics` additionally needs RBAC the CLI stages client-side first. |
| List | `burrow addon list` / `burrow-agent addons` | Type, mode (`installed`/`connected`), backend, endpoint, capabilities. This is how an app is pointed at `cache` — read the endpoint and set it as config. `burrow addon list` additionally reports the volumes an earlier removal kept, in their own section (`retained_volumes` in `--json`). |
| Attach an app | `burrow addon attach postgres <app> [--env]` | **Postgres only.** On the named environment's instance, creates role `app_<app>` and database `<app>` owned by it, revokes `CONNECT` from `PUBLIC`, grants it to the role, and writes the generated `DATABASE_URL` into the app's Secret in that environment's namespace, then restarts the workload there. Re-attaching rotates the password. The URL is never returned, logged, or audited. |
| Detach | `burrow addon detach postgres <app> [--env]` | Removes `DATABASE_URL`, then `DROP DATABASE … WITH (FORCE)` and `DROP ROLE` **on that environment's instance**. Destructive; confirm-gated. |
| Remove | `burrow addon remove <name> [--delete-data] [--skip-final-backup] [--backup-destination <provider>]` | Tears the add-on's workload down and **keeps its data volume** unless `--delete-data` is passed. Confirm-gated by `addon.remove`; the held message names the volume, the attached apps for `postgres`, and whether a final backup is taken first. `--delete-data` additionally requires the add-on's name typed back on a terminal, and refuses off one without `--acknowledge-data-loss`. With an object-storage provider registered it takes a final backup of every attached database first and removes **nothing** if it fails; `--skip-final-backup` destroys the data without one and announces it (ADR-0064 §5). **Operator CLI only** — absent from `burrow-agent` entirely (ADR-0065 §2). |
| Connect an existing backend | `burrow addon connect <loki\|prometheus> --endpoint <url> [--auth]` | Registers a backend you already run — **deploys nothing**. Only `loki` (logs) and `prometheus` (metrics) are connectable. Operator CLI only. |
| Query logs | `burrow addon logs [query] [--limit]` / `burrow-agent logs-query` | LogsQL against VictoriaLogs, or LogQL against Loki. Limit clamps to 200 when out of range or unset, capped at 1000. |
| Query metrics | `burrow addon metrics <query>` / `burrow-agent metrics-query` | PromQL **instant** query against VictoriaMetrics or Prometheus. |

**Add-on instances are per environment, and every add-on operation names one.**
`burrow addon install postgres --env staging` stands up a second instance (`burrow-postgres-staging`)
beside `prod`'s `burrow-postgres`, with its own volume and its own superuser credential; attach,
detach, backup and restore all act on the named environment's instance
([ADR-0067](adr/0067-one-database-instance-per-environment.md) §1). Databases keep their simple
names, so `web` in staging and `web` in production are two databases on two servers — the isolation
is the instance, not a naming convention. An operation that names no environment while more than one
is registered is refused rather than defaulted (ADR-0047 §1), and the provisioning seam takes the
environment non-optionally: there is no value meaning "whichever instance is there".

An install predating this keeps everything it had: the default environment (`prod`) resolves to
`burrow-postgres`, the same pod, volume, and password, so nothing migrates. The unqualified instance
name belongs to whichever environment is the default, so renaming that environment from `default`
to `prod` (§2) renamed no instance. Sharing one instance across environments is not supported and
cannot be expressed (ADR-0067 §5); a user who wants one server runs one environment.

The Postgres exporter is always on and reports connection and transaction health plus
`pg_stat_statements` slow-query stats, and the metrics scraper discovers the add-on namespace,
so installing the two in either order works ([ADR-0051](adr/0051-postgres-always-exports-metrics.md)).
Its stated limit holds: an **already-installed** Postgres add-on only gains the exporter on
reinstall.

Limits: only Postgres supports `attach`/`detach` — cache, logs, and metrics are wired by
reading the endpoint from `addon list` and setting it as app config. Log queries take **no
time range** (the Loki adapter hardcodes the last hour). Metrics queries are **instant only**;
a range query exists in the engine but no CLI, agent verb, or API route reaches it. Add-on
readiness is judged from the store Deployment alone, so a broken Fluent Bit DaemonSet or
vmagent still reports ready.

---

## Backups

Logical `pg_dump` / `pg_restore` backups for the Postgres add-on
([ADR-0032](adr/0032-postgres-backups.md)).

| Capability | Command | What it does |
| --- | --- | --- |
| Back up an app's database | `burrow addon backup postgres <app> [--destination <provider>]` | Runs `pg_dump -Fc` in a one-shot Job (`postgres:17-alpine`), writing `/backups/<app>/<id>.dump` on the PVC. With an object-storage provider registered, the same Job then writes that dump to the store and **reads it back** before the backup is recorded as completed. Waits up to 10 minutes; records id, path, destination, object key, size, and status in the control-plane database. |
| List backups | `burrow addon backups postgres [<app>]` | Reads the control-plane database, newest first, with a `WHERE` column saying which backups left the cluster. |
| Backup health | `burrow addon backup-health postgres [<app>]` | Reports what Burrow itself observed: how long ago the last backup completed, how long ago the last one **left the cluster**, the last failure with its closed reason, how many rows are still pending, and whether each registered object-storage destination answers **right now**. Read-only, and computed from Burrow's own `Backup` rows rather than from any backup engine's status fields. |
| Restore | `burrow addon restore postgres <app> --backup <id> --confirm` | Runs `pg_restore --clean --if-exists` into the app's database, overwriting live contents. Confirm-gated by the `addon.restore` guardrail. **Operator CLI only** — absent from `burrow-agent`. |

The limits are as important as the capability:

- **With no object-storage provider registered, the dump never leaves the cluster.** It lands on
  a `burrow-postgres-backups` PVC, 10Gi, ReadWriteOnce, on the default StorageClass, in the same
  `burrow-addons` namespace as the database it came from — so it shares a failure domain with its
  source. That is recorded on the row (`destination: cluster`) rather than left to be inferred,
  and the listing shows it, because a set of in-cluster dumps should not be able to read as a
  backup strategy. Registering a destination (`burrow config provider add s3`) is what fixes it.
- **The PVC and the store are a tier, not alternatives.** `pg_dump` always writes to the volume,
  and `pg_restore` reads from it: [ADR-0066](adr/0066-postgres-on-cloudnativepg.md) §4 keeps the
  single-app logical restore deliberately, and it is the thing that reads the volume. The object
  store is what takes the backup out of the database's failure domain.
- **A completed backup means the bytes reached the destination, and nothing weaker.** The write
  is signed with the dump's own SHA-256, so the endpoint validates the bytes it stored and
  refuses a truncated transfer; the object is then **read back** and its length compared before
  anything is recorded. A write that does not complete is retried (four attempts, exponential
  backoff) because a transient network failure is the common case and a retried-and-succeeded
  backup is not an incident — but a destination that ANSWERS and refuses is not retried, since a
  revoked credential does not become a valid one by being asked again. Every path that is not a
  verified object records `failed` with a reason from a closed set (`StoreUnreachable`,
  `StoreRejected`, `ObjectNotReadable`, `DumpFailed`, `NotRecorded`) or an
  [ADR-0074](adr/0074-burrow-observes-what-it-manages.md) §2 `IssueReason` when the Job never started. **No
  row ever says `completed` for bytes that did not arrive.**
- **The destination credential reaches the pod only through a Job-owned Secret**, mounted
  read-only, never as a Job env var or a command-line argument, and it is deleted on every path
  out of the backup — success or failure. The vendor's own error text goes to the Job's pod log,
  never into the `Backup` row: a vendor error body is the one place an access key id is known to
  be echoed back.
- **Backup age is REPORTED, and nothing alerts on it.** `burrow addon backup-health postgres`
  answers [ADR-0063](adr/0063-object-storage-provider.md) §7's question on demand — destination
  reachability, the age of the last successful backup, the age of the last one that left the
  cluster, the last failure — from Burrow's own rows, which is what
  [ADR-0066](adr/0066-postgres-on-cloudnativepg.md) §5 requires, since a backup engine's own status
  fields can report stale values rather than absent ones. What does not exist is a **threshold**:
  nothing schedules a backup yet, so any "no successful backup in N hours" would be a number Burrow
  invented and then alerted on. The surface reports the ages; the alert waits for the scheduling
  that gives a threshold a meaning.
- **There is no scheduling.** No CronJob exists anywhere in the tree, and the control plane is
  not even granted `cronjobs` RBAC. Every backup is an explicit command.
- **There is no retention or pruning.** No delete-backup command, no "keep last N", no
  expiry — dumps and their database rows accumulate until the PVC fills.
- **There is no point-in-time recovery.** No WAL archiving, no `archive_command`, no
  `pg_basebackup`. Recovery granularity is "whenever someone last ran a backup".
- **The backup PVC outlives everything, deliberately.** Removing the Postgres add-on keeps it,
  and so does `burrow addon remove postgres --delete-data`: dumps outliving the database they
  came from is the point of taking them, and it is what makes destroying the data survivable.
  Their records stay listed, and the removal output names the volume so the storage is not a
  surprise. Reclaiming it is a manual `kubectl delete pvc`.
- **`--delete-data` takes a final backup first where one can be taken.** With an object-storage
  provider registered, every attached database is dumped to that store before anything is
  destroyed, and a backup that does not reach it aborts the removal with nothing deleted
  ([ADR-0064](adr/0064-addon-removal-keeps-its-data.md) §5). With none registered the behaviour is
  unchanged and the only copy that survives is whatever the retained backup PVC already held —
  which the output says. `--skip-final-backup` is the override for an instance too broken to dump,
  and it announces itself. This is **not a backup regime**: a dump taken at teardown is the
  least-exercised path in the product, running against an instance that may already be unhealthy.
- A failed backup or restore Job is left in place for diagnosis rather than reaped.

Scheduled backups with retention are decided as a follow-on in ADR-0032 and are **not built**.
[ADR-0066](adr/0066-postgres-on-cloudnativepg.md) decides they arrive by a different route
entirely — CloudNativePG doing the archiving, scheduling, retention and point-in-time recovery,
with Burrow creating a custom resource instead of orchestrating `pg_dump` — and it is **not
built** either: every command in the table above is the ADR-0032 Job path.

---

## Environments

Two distinct things share the name, and conflating them is the usual source of confusion
([ADR-0035](adr/0035-environments.md), [ADR-0036](adr/0036-environment-selection.md)).

- A **local handle** in `~/.burrow/config` — a named pointer to a kube context, a control-plane
  namespace, an app namespace, a burrowd environment name, and the scoped agent kubeconfig.
  Client-side state; no cluster contact.
- A **registered environment** in burrowd — a row mapping a name to the Kubernetes namespace
  its apps deploy into. Install creates one, named **`prod`**, mapped to the app namespace
  (`burrow-apps`) — not to `burrow-apps-prod`
  ([ADR-0067](adr/0067-one-database-instance-per-environment.md) §2–§3). `prod` and the retired
  `default` are both reserved names for `burrow env add`.

| Command | What it does |
| --- | --- |
| `burrow env` / `env list` | Lists local handles and marks the active one. `--discover` probes every kube context for an installed burrowd and registers a handle for each. |
| `burrow env add <name>` | Creates the namespace and burrowd's Role/RoleBinding in it, registers the environment with burrowd, and records a local handle. Namespace defaults to `<app-namespace>-<name>`. |
| `burrow env use <name>` / `env follow` | Pins the active environment, or clears the pin so it follows the current kube context. |
| `burrow env rename <old> <new>` | Renames a local handle. |
| `burrow env remove <name>` | Deletes the **local handle only** — and the minted agent kubeconfig under `~/.burrow/agents/`. It does not unregister the environment in burrowd. |
| `burrow-agent environments` | Lists what the agent can see. Read-only, local. |

Most operate verbs take `--env <name>`. Mutating operations resolve it through a forcing
function ([ADR-0047](adr/0047-agent-environment-safety.md)): when more than one environment is
registered and no environment is named, the operation is **refused** with a structured error
listing the alternatives, rather than falling back to the ambient default. Read-only
operations are exempt by design. An unreachable target names the other environments but never
switches.

The name is a guardrail decision rather than naming taste.
[ADR-0065](adr/0065-what-belongs-on-the-agent-surface.md) makes `app.delete` and `dns.delete`
deny-by-default and expects the operator to relax them per environment; an environment called
`default` invites `guard set --env default app.delete allow` as the obvious way to stop the
friction, and production has then been relaxed without the word ever being typed. A consequence
worth stating: someone whose only cluster is genuinely a sandbox gets an environment called `prod`
with production-grade defaults and will find them strict. Install says so in its output. The default
environment's guardrail policy **is** the global policy, so `guard set --env prod app.delete deny`
and `guard set app.delete deny` are the same write; an environment added later diverges from that
baseline with its own `--env` row.

An install predating ADR-0067 gains `prod` on its first start under a version that carries it,
pointing at the namespace its apps are already in. Its stored environment name moves from `default` to
`prod` in one migration; **nothing in the cluster moves or is renamed** — apps stay in `burrow-apps`
and the Postgres instance stays `burrow-postgres`, with the same volume and superuser credential.

Limits: `env remove` and `env add` are asymmetric, as noted above — unregistering an
environment in burrowd is not reachable from either CLI. ADR-0047 also specifies the same
forcing function on the *local handle* axis; that half is **not built** (it was specified for
the MCP layer, since removed). Per-environment guardrails are real but partial — see below.

The Postgres collision that used to come with a second environment is closed: stateful add-ons are
per-environment, so two environments' apps of the same name have separate databases on separate
instances ([ADR-0067](adr/0067-one-database-instance-per-environment.md) §1, under
[Add-ons](#add-ons)).

---

## Guardrails

Guardrails are policy evaluated in the control plane, between the agent and the cluster
([ADR-0006](adr/0006-guardrails-in-the-control-plane.md),
[ADR-0020](adr/0020-guardrails-as-configurable-policy.md)). Three dispositions:

- **`allow`** — proceeds.
- **`confirm`** — the operation does **not** run; it returns a structured "needs confirmation"
  result (HTTP 422) until the same call is repeated with `--confirm`. `burrow-agent` surfaces
  this as `held_for_confirmation` with exit code 2 and never self-confirms.
- **`deny`** — refused; `--confirm` cannot bypass it. An unset or invalid disposition also
  reads as deny.

These are all fourteen, in listing order, with their defaults:

| Code | Gates | Default | Env-scopable |
| --- | --- | --- | --- |
| `app.deploy` | deploying a new release | `allow` | yes |
| `app.scale_to_zero` | scaling an app to zero | `confirm` | yes |
| `app.expose_public` | exposing an app at a public hostname | `confirm` | yes |
| `dns.write` | creating or updating a public DNS record | `confirm` | no |
| `dns.delete` | deleting a public DNS record | `deny` | no |
| `addon.install` | installing an add-on | `confirm` | no |
| `addon.remove` | removing an add-on | `confirm` | no |
| `addon.detach` | detaching an app from an add-on, destroying its data | `confirm` | no |
| `addon.restore` | restoring a database over its live contents | `confirm` | no |
| `app.delete` | deleting an app entirely | `deny` | yes |
| `app.rollback` | rolling back to the previous release | `allow` | yes |
| `app.autoscale` | configuring autoscaling | `allow` | yes |
| `app.run` | running a one-off command in the app's image | `confirm` | yes |
| `bucket.create` | creating a bucket at an object-storage provider | `confirm` | no |

`burrow guard list [--env <name>]` shows the effective disposition and, for a named
environment, whether it came from the environment, the global policy, or the built-in default.
`burrow guard set [--env <name>] <code> <allow\|confirm\|deny>` persists an override in the
control-plane database. `burrow-agent guard` can **read** the policy and cannot set it —
structurally, the verb does not exist on that binary.

Both `guard` surfaces report a **second kind of limit** alongside the dispositions: the
capabilities absent from the `burrow-agent` binary, each with what it is, why it is held back,
and the operator command that performs it
([ADR-0065](adr/0065-what-belongs-on-the-agent-surface.md) §7). The two groups are separate keys
in `--json` — `guardrails` and `absent_capabilities` — because they are different answers: a
`deny` is a limit an operator can move with `guard set`, an absent capability is not on the
binary at all. See [The agent surface](#the-agent-surface).

Limits:

- **`app.delete` and `dns.delete` are denied, and a deny is not a hold** — no `--confirm`
  opens one, on either CLI ([ADR-0065](adr/0065-what-belongs-on-the-agent-surface.md) §3).
  Deleting an app destroys its release history and deleting a DNS record takes an application
  off the internet, so neither rests on a prompt someone might not read. **That default is a
  floor, not a fixed setting.** The expected shape is a per-environment gradient — `burrow guard
  set --env dev app.delete allow`, `--env staging app.delete confirm`, production left denied —
  and the refusal message says so, because relaxing one guardrail globally to unblock a sandbox
  relaxes production with it. `burrow app delete` is how a human deletes an app once the
  guardrail permits it.
- **Which guardrails can be scoped to an environment is a property each one declares**
  ([ADR-0068](adr/0068-operational-limits-are-configuration.md) §5), not one read off the `app.`
  prefix as it used to be. The app-level codes are scopable because the operations they gate always
  name an environment. The cluster-level codes (`dns.*`, `addon.*`, `bucket.create`) are not,
  because their dispositions are looked up with no environment at all; setting one with `--env` is
  rejected. `dns.delete`'s deny is therefore still all-or-nothing: an operator who wants the agent
  managing DNS in development but not production must pick one answer for both. Widening it is now
  a change to that declaration plus the lookup at its call site rather than a rename.
- **`app.replica_ceiling` is no longer a guardrail.** It is an operational limit — see
  [Operational limits](#operational-limits) below. Exceeding it is a validation failure, not a
  disposition, and any stored disposition for it was dropped by the schema migration.
- **Several mutating operations are not guardrailed at all**: `app config set/unset`,
  `app secret set/unset`, `app unpublish`, `addon attach`, `addon backup`, `addon connect`,
  `config provider add`, `app auto-deploy`, `env add/remove`, and `guard set` itself. Some are
  deliberate (config and secrets destroy nothing; attach provisions rather than destroys);
  `unpublish` taking an app offline without a gate is worth knowing.
- Removing an environment does not cascade to its guardrail overrides; they are orphaned and
  would apply again if the name were reused.

---

## Operational limits

A limit is a **bound a human sets**, and it is deliberately not a guardrail
([ADR-0068](adr/0068-operational-limits-are-configuration.md)). A guardrail answers *what happens
when this is attempted*; a limit answers *where the line is*. Exceeding one is a **validation
failure**: there is no disposition on it, nothing to relax, and no `--confirm` that opens it. The
refusal names the limit, the tier the effective bound came from, and the command that raises it.

| Limit | Bounds | Default | Tier |
| --- | --- | --- | --- |
| `app.replica_ceiling` | the largest replica count a deploy, a scale, or an autoscaler's maximum may ask for | `50` (1 – 2147483647) | environment or cluster |
| `build.job_retention` | how long a finished in-cluster build Job and its pods are kept before Kubernetes reaps them | `72h0m0s` (1m – 8760h) | cluster |
| `addon.metric_retention` | how long the metrics add-on keeps samples, applied when its instance is created | `744h0m0s` (24h – 87600h) | cluster |
| `status.unschedulable_grace` | how long a pod must have been unschedulable before Burrow reports it as an Issue | `30s` (0s – 1h) | cluster |

Two operator-owned tiers, resolved in the order guardrail dispositions already use — the
environment's value, then the cluster's, then the built-in default:

- `burrow cluster config list [--env <name>]` shows every limit with its effective value, the tier
  that value came from, and the built-in default it reverts to.
- `burrow cluster config set <limit> <value>` sets the **cluster** value, which applies everywhere.
- `burrow cluster config set --env <name> <limit> <value>` sets that **environment's** value, which
  wins there. `--env prod` writes the cluster value: `prod` is the environment install created and
  the baseline the others diverge from ([ADR-0067](adr/0067-one-database-instance-per-environment.md)
  §2), exactly as for `guard set`.

**The whole surface is absent from `burrow-agent`** (ADR-0068 §4, ADR-0065 tier 1), asserted by the
surface-guard test, for the reason `guard set` is: a bound the agent can raise is not a bound. The
agent gets the refusal and the operator command to relay, not the lever. Reading a limit is harmless
and useful, but ADR-0068 leaves the shape of an agent-side read undecided, so there is none yet.

Limits:

- **Three of the four are cluster-only**, and deliberately: how long a build's logs are worth
  keeping, how long metric samples are kept, and how long a pod may sit unschedulable are all facts
  about the cluster. An operator who tuned the last of them per environment would be encoding the
  same scheduler fact twice and would eventually disagree with themselves.
- **`status.unschedulable_grace` is ONE value, everywhere.** The live status surface, the Job
  waiters behind `app run` / build / backup / restore, and the failure ledger
  ([ADR-0074](adr/0074-burrow-observes-what-it-manages.md)) all judge the same pod through the same
  inspection, so a change to it moves all of them together. Two surfaces disagreeing here would not
  be a tuning difference — they would hold different definitions of "failure".
- **Two of them apply at creation, not retroactively.** `build.job_retention` is written onto each
  build Job as `ttlSecondsAfterFinished`, and `addon.metric_retention` onto the metrics add-on's
  container args, so a change reaches the next build and the next install of the add-on. A running
  metrics instance keeps the retention it was created with until it is removed and reinstalled.
  `status.unschedulable_grace` has no such lag: it is read on every inspection.
- **A limit reads live.** burrowd reads the values on each operation rather than at startup, so a
  `cluster config set` takes effect on the next deploy, build, or status call with no restart. If
  the database cannot be read, every limit falls back to its built-in default and burrowd logs that
  it did — an unavailable database must not become a failed deploy.
- **There is no `unset`.** A value returns to the default by being set back to it explicitly, the
  same as for a guardrail disposition.
- **Values are validated on the way in**, against the limit's kind and permitted range, and stored
  in canonical form. A value that later stops parsing (a hand-edited row) is skipped on read and the
  next tier applies, so a bad row cannot wedge a deploy.
- Removing an environment does not cascade to its limit values, exactly as it does not cascade to
  its guardrail overrides.

---

## Audit log

An append-only record in the control-plane Postgres ([ADR-0027](adr/0027-audit-log.md)). A
guarded operation writes two rows: the decision (`allowed`, `held`, or `denied`) and then the
execution (`executed` or `failed`).

Each row carries the timestamp, operation, target, an allow-listed `args` map, the guardrail
code and disposition, the outcome, a result string, the caller, the principal, and the
client version. Operations recorded: `deploy`, `scale`, `autoscale`, `rollback`, `app_delete`,
`expose`, `dns_write`, `dns_delete`, `addon_install`, `addon_remove`, `addon_attach`,
`addon_detach`, `addon_backup`, `addon_restore`, `run`.

Read it with `burrow audit` or `burrow-agent audit`, filtered by `--app`, `--operation`,
`--outcome`, and `--limit` (default 200, newest first).

Limits: **`args` never contains a secret value** — where environment data is involved the row
records sorted key *names* only. There is **no time-range filter and no environment filter**;
the environment appears inside `args` but is not queryable. There is **no retention or
pruning**, so the table grows without bound. There is no update or delete path by design.
Writes are best-effort: a failed append is logged and swallowed rather than failing the
operation it describes. `guard set`, `addon connect`, `unexpose`, and provider registration are
**not** audited.

---

## The agent surface

`burrow-agent` is a separate binary from `burrow`: capability-reduced, credential-free,
JSON-first, and invoked directly by the agent
([ADR-0049](adr/0049-burrow-agent-scoped-cli-control-channel.md)). Its surface is closed and
pinned by tests that fail if a verb is added or removed.

**Read-only:** `apps`, `status`, `history`, `next-tag`, `logs`, `config`, `secret` (keys),
`reachability`, `cluster`, `cluster capacity`, `addons`, `backups`, `logs-query`,
`metrics-query`, `guard`, `audit`, `providers`, `environments`.

**Mutating** (each returns an outcome envelope — `executed` / `held_for_confirmation` /
`denied` / `error`, exit codes 0/2/3/1): `deploy`, `build`, `rollback`, `scale`, `autoscale`,
`run`, `expose`, `unexpose`, `domain add`, `domain remove`, `addon install`, `addon attach`,
`addon backup`, `config set`, `config unset`, `secret unset`, `delete`.

**Absent from the agent binary entirely** — these are operator actions on the `burrow` CLI:
`cluster install`, `cluster upgrade`, `cluster bootstrap`, `cluster ingress install`,
`cluster registry install`, `cluster postgres install`, `cluster config set`, `join`, `env add`,
`guard set`, `app secret set`, `app auto-deploy`, `app hook set`, `addon remove`, `addon remove --delete-data`,
`addon connect`, `addon detach`,
`addon restore`, `config provider add`, `config registry login`, `agent <tool> install`,
`app publish`/`unpublish` under those names, and the client-side `--build` deploy path.
**Bucket deletion** is in that list too and is the one entry Burrow does not carry on *either*
CLI ([ADR-0063](adr/0063-object-storage-provider.md) §5): it destroys every backup the platform
holds, and a bucket name lives in a global namespace, so a mistaken argument can reach outside
the cluster entirely. `guard` reports it with the vendor's own tool as the answer to "who can".

Those verbs are not prose alone. They are entries in a capability catalogue
(`internal/agentsurface`), which is also the table the surface-guard test reads as its closed
allow-list: every capability appears once, tagged with the surface that carries it, so the two
readers cannot disagree about where a verb sits. That is what lets `guard` report each absent
capability with what it is and who can run it, below.

What qualifies a verb for this surface is
[ADR-0065](adr/0065-what-belongs-on-the-agent-surface.md): a capability belongs unless its effect
reaches beyond the app the agent was asked about (**scope** — disqualifying outright, so the verb
is not compiled in) or a human cannot restore the prior state afterwards (**reversibility** — the
operator decides, so it ships as a guardrail denied by default). `addon remove` is the scope case:
add-ons are one instance per type per environment and every app in an environment has its database
on that environment's `postgres`, so it removes *the* add-on for an environment and takes every
attached app in it with it. The surface-guard test
asserts its absence rather than leaving it to be a property of the current command tree.

The reversibility tier ships too: `app.delete` and `dns.delete` are `deny` by default, so
`burrow-agent delete` and `burrow-agent domain remove` exist, are refused, and say what would
change the answer (see [Guardrails](#guardrails)).

**An absent capability is legible rather than a dead end** (ADR-0065 §7). `burrow guard list` and
`burrow-agent guard` both report the absent capabilities alongside the dispositions: what each one
is, why it is not on the agent surface, who can perform it ("the burrow operator CLI, run by a
human with the cluster's admin kubeconfig"), and the exact command that person runs. So an agent
asked to remove an add-on relays "that is not something I can do, and here is who can" instead of
`unknown command`. That matters beyond politeness: ADR-0065 §5 notes a dead end is what invites an
agent to route around the control channel entirely and reach for `kubectl` or a shell, which is
the failure [ADR-0021](adr/0021-guardrails-require-control-plane-only-agent-access.md) says Burrow
cannot close from the inside.

`burrow-agent` derives the report by subtracting the command paths it actually registers from the
catalogue, so a verb taken out of the binary becomes legible in `guard` with no second edit. The
report enumerates the agent surface to anything that can read it, which ADR-0065 §7 accepts
outright: the CLI is open source and `--help` already reveals it, so the read carries no access
control.

The narrowing is structural, not advisory: the binary lacks the verb, its
kubeconfig lacks the permission, and the control plane gates the operation anyway.
`burrow agent claude install` wires it up for Claude Code — the only tool name with a built-in
recipe — by merging permission rules into `~/.claude/settings.json` and an orientation block
into `~/.claude/CLAUDE.md`, idempotently, backing up any file it edits. Any other tool prints
the rules to apply by hand.

For the scoped credential minted at install, what each of the four credentials that reach a
cluster can and cannot do, `BURROW_AGENT_REQUIRE_SCOPED=1` fail-closed behaviour, and the
multi-user join, see **[docs/HARDENING.md](HARDENING.md)** — that document is the source of
truth and this one does not restate it.

---

## Cluster lifecycle

| Capability | Command | What it does | ADR |
| --- | --- | --- | --- |
| Install the control plane | `burrow cluster install <kube-context>` | Provisions the control-plane, app, add-on, and build namespaces; the `burrowd` ServiceAccount and its narrowly scoped namespace Roles; the API-token and database Secrets; an empty `burrow-credentials` Secret; the `burrow-agent` ServiceAccount, its Role (proxy to burrowd, read the API token — nothing else) and long-lived token Secret; one read-only ClusterRole for capability detection; **an in-cluster Postgres (`postgres:18`, 1 replica, 1Gi PVC)**; and the `burrowd` Deployment and Service. Ensures metrics-server as a baseline. | [0012](adr/0012-in-cluster-postgres.md), [0038](adr/0038-scoped-agent-credential.md), [0054](adr/0054-install-is-control-plane-only.md), [0060](adr/0060-cluster-lifecycle-command-group.md) |
| Provision a VPS | `burrow cluster bootstrap` (on the VPS) | Installs k3s (Traefik disabled, servicelb kept, so the node IP is a free LoadBalancer) plus the control plane, then prints a `burrow join <token>`. Refuses under ~1900 MiB RAM without `--yes`. **Burrow never SSHes anywhere** — this is run once, over your own SSH session. | [0044](adr/0044-single-vps-k3s-cluster.md) |
| Join from a laptop | `burrow join <token>` | Merges the admin context into the ambient kubeconfig and writes the scoped agent kubeconfig under `~/.burrow/agents/`. Idempotent. | [0044](adr/0044-single-vps-k3s-cluster.md) |
| Upgrade | `burrow cluster upgrade` | Re-renders the install manifest with a new burrowd image, preserving the API token, database password, and namespaces read back from the cluster. Postgres and its PVC are untouched. Migrations are applied by burrowd at startup. | [0013](adr/0013-database-migrations-and-upgrade-policy.md) |
| Install the PostgreSQL operator | `burrow cluster postgres install` | Applies the pinned CloudNativePG release (`1.30.0`) from its own upstream artifact, skipping when a controller is already running and re-applying when the CRDs are there without one. Needs **cluster-admin** (it installs cluster-scoped CRDs), which is why it is an operator step and absent from the agent. It installs the operator only: the Postgres add-on still runs its own Deployment. | [0066](adr/0066-postgres-on-cloudnativepg.md) |
| Inspect the cluster | `burrow cluster` | Ingress classes, default StorageClass, LoadBalancer support and whether it is free or billable, cert-manager, metrics-server, the CloudNativePG operator (present / running / its release beside the one Burrow targets), cloud provider, configured DNS providers. | [0034](adr/0034-agent-native-onboarding.md), [0043](adr/0043-public-reachability-is-a-loadbalancer.md) |
| Scheduling headroom | `burrow cluster capacity` | Per-node and cluster-total allocatable, committed and free CPU/memory and pod counts, top consumers, and whether a build would fit. Computed from node allocatable versus summed Pod requests — no metrics-server needed. | [0054](adr/0054-install-is-control-plane-only.md) |
| Version | `burrow version` | The CLI version, the installed control plane's image tag, and the latest published release. | [0016](adr/0016-cli-distribution-and-upgrade-lifecycle.md), [0039](adr/0039-cli-control-plane-version-skew.md) |

`install` is idempotent and multi-user safe: it applies server-side with `kubectl` nowhere in
the path, and if Burrow is already installed it does **not** re-mint secrets — it performs a
local join, writing only `~/.burrow`. Additive cluster components are deliberately separate
opt-in commands rather than `--with-*` flags ([ADR-0054](adr/0054-install-is-control-plane-only.md)).

The CLI reaches burrowd through the **Kubernetes API-server service proxy**, authenticated by
your kubeconfig, with Burrow's own token sent only in the `X-Burrow-Token` header
([ADR-0014](adr/0014-self-host-connectivity-via-kubeconfig.md),
[ADR-0015](adr/0015-token-header-only-x-burrow-token.md)) — so no inbound endpoint is needed. A
direct `--control-plane <url> --token <t>` is also supported.

Version skew ([ADR-0039](adr/0039-cli-control-plane-version-skew.md)): every request carries
`X-Burrow-Client-Version`. burrowd serves any client within one minor and never hard-blocks on
a version difference alone; a client more than one minor behind gets HTTP 426 naming the
upgrade command, and an unknown route returns a structured error saying to upgrade.

**Upgrade limit, worth stating plainly:** the shipped startup gate permits a re-run of the same
version or **exactly one minor step forward**, and refuses skips, downgrades, and cross-major
in-place moves. [ADR-0055](adr/0055-multi-version-upgrades.md), which decides multi-minor
forward jumps in one step, is 🟡 **Proposed and not built**. Upgrading across several minors
means installing each intervening minor in turn.

---

## Logs and observability

Two different surfaces, and picking the wrong one is a common mistake.

- **`burrow app logs <app>` / `burrow-agent logs <app>`** reads live Pod logs through the
  Kubernetes API, concatenated across the app's Pods. The **only** flag is `--tail`. There is
  **no `--follow`, no `--since`, no `--previous`, and no `--container`** — streaming is not
  supported, and the response is fully buffered. Log lines carry a pod name and a message;
  the timestamp field on the type is never populated on this path.
- **`burrow addon logs` / `burrow-agent logs-query`** queries the durable logs add-on (or a
  connected Loki) in LogsQL or LogQL. This is the surface for "what happened an hour ago".

For metrics, `--metrics-port` on a deploy annotates the Pod so the metrics add-on's scraper
finds `/metrics`; queries go through `burrow addon metrics` / `burrow-agent metrics-query`.

---

## What Burrow does not do

Consolidated, so a reader can stop looking:

- **No file-mounted config or secrets, and no volumes on app Pods at all.** Environment
  variables are the only injection mechanism.
- **No persistent storage for apps.** No PVC, no StatefulSet — the only supported app workload
  kind is Deployment. Add-ons get volumes; apps do not.
- **No resource requests or limits, probes, security context, node selectors, tolerations,
  priority classes, or runtime classes on app Pods**, and no surface to set them. The
  ADR-0061 mutator seam exists for an embedder to compile one in, not for a user to configure.
- **No DaemonSet, StatefulSet, or CronJob app workloads.** Jobs exist only as the one-off
  runner and as internal build/backup machinery.
- **No scheduled anything.** No cron, no scheduled backups, no scheduled restarts.
- **No TCP/UDP exposure, no path-based routing, no multiple hostnames per app, no custom
  ingress annotations, no DNS-01 certificates.**
- **No backup retention, pruning, offsite copy, or point-in-time recovery.**
- **No automatic rollback**, and no readiness gate on deploy.
- **No log streaming or follow.**
- **No secret set from the agent channel**, by design.
- **No blue/green or canary deploys, no traffic splitting, no maintenance windows.**
- **No dashboard.** The CLIs and the control-plane API are the whole interface.
- **No multi-tenancy.** The control plane is single-tenant, which several security decisions
  (notably the privileged build container) rely on.

---

## Decided but not built

Accepted or proposed decisions with no implementation, or with only part of one. An ADR alone is
not a capability, and a partly built one is not two-thirds of a capability — the rows below say what
is built and what is not, and link the issue tracking the rest where there is one.

| Decision | ADR | Status of the code |
| --- | --- | --- |
| Detect the cluster's existing ingress controller and bind to its IngressClass | [0042](adr/0042-use-existing-ingress-controller.md) | Not built — the IngressClass and the HTTP-01 solver class are the literal `"nginx"`. |
| A deploy with a port creates a ClusterIP Service on its own | [0041](adr/0041-flatten-path-to-a-reachable-app.md) | Not built — no `--port` on `deploy`; the Service is created by `publish`. |
| One publish operation chaining Service, Ingress, TLS, DNS, and a cert wait | [0041](adr/0041-flatten-path-to-a-reachable-app.md) | Partial — `publish` does Service, Ingress, and the TLS request. DNS and waiting are separate commands. |
| Multi-minor forward database upgrades in one step | [0055](adr/0055-multi-version-upgrades.md) | Proposed, not built — the gate allows one minor step. |
| A `post-deploy` hook, told the outcome and the reason, running whether the deploy succeeded or failed | [0072](adr/0072-deploy-and-run-lifecycle-hooks.md) §4–§7 | Partly built. The phases that run **before** an image moves are built — `pre-deploy` and `pre-rollback`, one command with the phase named, serialized per app and environment, with a failed hook aborting the operation (see [Lifecycle hooks](#lifecycle-hooks)). `post-deploy` is not: nothing waits for a rollout to settle, so nothing can report how it went, and setting the phase is refused rather than stored as a command that never runs. That also means §6's "Burrow reports, the hook decides" has no reporter yet — a deploy that ships and then crashloops is still silent. [#386](https://github.com/burrow-cloud/burrow/issues/386) |
| Scheduled backups with retention | [0032](adr/0032-postgres-backups.md) | Not built. |
| Audit-log retention | [0027](adr/0027-audit-log.md) | Not built; deferred in the ADR. |
| The environment forcing function on the local-handle axis | [0047](adr/0047-agent-environment-safety.md) | Not built (specified for the since-removed MCP layer); the burrowd-registry axis is built. |
| Registry onboarding via the developer's code-provider registry | [0046](adr/0046-registry-onboarding.md) | Proposed, held deliberately; only the in-cluster registry shipped, via ADR-0054. |
| An app-runtime API and capability envelopes | [0050](adr/0050-app-runtime-api-and-capability-envelopes.md) | Not built; a captured direction, deferred. |
| Per-app connection pooling, read replicas, major-version upgrades, or TLS to the database | [0031](adr/0031-postgres-addon.md) | Not built; named as "not yet" in the ADR. |
| Object storage as a provider type, so a backup can leave the cluster | [0063](adr/0063-object-storage-provider.md) | Partly built. The destination registration is built: the `s3` provider type and object-storage capability, the credential pair as two keys in `burrow-credentials`, the configuration-time probe write/delete, the recorded globally-unique bucket, lifecycle-versus-retention reconciliation, and `bucket.create` at `confirm` with bucket deletion absent from both CLIs. The backup WRITE path is built too: the dump is shipped to the store and read back before the row says `completed`, retries are for a store that will not answer and never for one that answered and refused, and `burrow addon backup-health postgres` reports destination reachability, the age of the last successful backup, the age of the last one that left the cluster, and the last failure. What is left of §7 is the ALERT: there is no threshold to alert against, because nothing schedules a backup yet. [#331](https://github.com/burrow-cloud/burrow/issues/331) |
| A final backup before `--delete-data` | [0064](adr/0064-addon-removal-keeps-its-data.md) §5 | Not built — it waits on an object-storage provider ([ADR-0063](adr/0063-object-storage-provider.md)); until then the retained backup claim is the only copy. The rest of ADR-0064 is built: removal keeps the data PVC and names it, `--delete-data` is operator-CLI-only and carries §2's typed confirmation, the backup claim always survives, and `addon list` reports retained volumes (§6). [#334](https://github.com/burrow-cloud/burrow/issues/334) |
| The Postgres add-on runs on CloudNativePG, with the operator owning WAL archiving, schedules, retention, and point-in-time recovery | [0066](adr/0066-postgres-on-cloudnativepg.md) | Partly built. The OPERATOR is: `burrow cluster postgres install` applies the pinned CloudNativePG release, skips a controller that is already running, and states the cluster-admin requirement; `burrow cluster` reports whether the CRDs are served, whether a controller is actually running, and which release it is beside the one Burrow targets. The MECHANISM is not built — still a single-replica `postgres:17-alpine` Deployment with Burrow-orchestrated `pg_dump` / `pg_restore` Jobs, no operator, no custom resources, no WAL archiving. §5 is built: `burrow addon backup-health postgres` reports the backup-age signal from what Burrow itself observed, which is the part of the record that is deliberately independent of the mechanism and stays correct across the swap. [#338](https://github.com/burrow-cloud/burrow/issues/338) |

The rows above are summaries. Per-ADR implementation tracking — the code as it stands, the sections
each issue covers, and an acceptance checklist — lives in the issues labelled
[`adr`](https://github.com/burrow-cloud/burrow/labels/adr); this file stays the answer to "can Burrow
do X today?" rather than a second tracker.

One ADR runs the other way and is worth knowing:
[ADR-0062](adr/0062-remove-the-mcp-server.md) is done — the MCP server is gone from the tree,
with `burrow mcp` surviving for one release as a hidden stub that errors and names
`burrow agent claude install`.

---

## Where to look next

- [README](../README.md) — the authoritative shipped / in-progress / planned version table.
- [docs/ARCHITECTURE.md](ARCHITECTURE.md) — how the layers fit together at runtime.
- [docs/HARDENING.md](HARDENING.md) — the agent trust surface and the scoped credential.
- [docs/QUICKSTART.md](QUICKSTART.md) — a validated end-to-end walkthrough.
- [docs/adr/](adr/README.md) — why each decision is shaped as it is.
