# ADR-0089: A secret can reach an app as a file

## Status

✅ Accepted

## TL;DR

A secret can land on disk, not only in an environment variable. Burrow owns the directory it lands
in.

- **Today a secret reaches an app one way: as an environment variable.** The app's PodSpec has no
  `Volumes` and no `VolumeMounts` at all.
- **An environment variable is a bad place for a credential.** Readable at `/proc/<pid>/environ`,
  **inherited by every child process**, lands in a crash dump. A file is read by whoever opens it.
- **`burrow app secret mount <app> KEY`** projects that key into a file. The value does not move —
  same Secret, same storage, same writer. Only the projection changes.
- **Burrow owns the directory**, default `/run/secrets`. Not an arbitrary path. A mount can then
  never shadow a file in the app's own image, and the kubelet updates it in place.
- **The path arrives as `BURROW_SECRETS_DIR`**, never the value. Same shape the build Job already
  uses for its own credentials.
- **The variable stays** unless you say `--no-env`. Suppressing it costs the `envFrom` shortcut, and
  that cost is named rather than hidden.
- **A mount is app configuration, not a release property.** A rollback does not un-mount the
  credential the running code needs.
- **[ADR-0029](0029-secrets-through-the-control-plane.md) holds unchanged.** Where the value lives
  and who may set it do not change.

Extends [ADR-0028](0028-app-config-and-secrets.md)'s per-app Secret and
[ADR-0029](0029-secrets-through-the-control-plane.md)'s transport. States its tier under
[ADR-0065](0065-what-belongs-on-the-agent-surface.md) §6. Unblocks cloud
[ADR-0021](https://github.com/burrow-cloud/cloud/blob/main/docs/adr/0021-the-control-plane-is-deployed-by-the-oss-install.md).
Supersedes nothing.

## Context

**What exists today.** A secret value lives in one place: the per-app Kubernetes Secret,
`burrow-app-<name>-secrets` ([`controlplane/domain.go`](../../controlplane/domain.go)), written by
burrowd over its authenticated API ([ADR-0029](0029-secrets-through-the-control-plane.md)). It reaches
the running container exactly one way — [`controlplane/kube/adapter.go`](../../controlplane/kube/adapter.go)
sources every key in that Secret as an environment variable:

```go
envFrom := []corev1.EnvFromSource{{
	SecretRef: &corev1.SecretEnvSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: controlplane.AppSecretName(spec.App)},
		Optional:             boolPtr(true),
	},
}}
```

The `PodSpec` that container sits in sets `Containers` and nothing else. No `Volumes`. The
`WorkloadSpec` the engine hands it ([`controlplane/types.go`](../../controlplane/types.go)) carries
`App`, `Kind`, `Image`, `Env`, `Command`, `MetricsPort`, `Readiness`, `Replicas` and `ReleaseID` —
there is no field that could express a mount, and
[`controlplane/dependencies.go`](../../controlplane/dependencies.go) already says so in prose:
*"Burrow mounts no volume on a USER's workload today."* `docs/CAPABILITIES.md` says it to users:
*"A Secret cannot be mounted as a file … there is no `--file` or `--mount` flag."*

**What breaks.** An environment variable is a worse container for a credential than a file, in ways
that are concrete rather than stylistic:

- It is readable at `/proc/<pid>/environ` by anything that can see the process.
- **It is inherited by every child process** — including whatever a `burrow app run` or a shell in
  the container spawns. A file is read by whoever opens it.
- It lands in a crash dump, or in a debug endpoint that prints the environment.
- Some credentials are structurally file-shaped: a kubeconfig, a PEM private key, a service-account
  JSON. Turning those into environment variables means base64 plus a decode step at startup, which
  is a workaround wearing the shape of a design.

The case that raised it is Burrow's own. `burrowd-cloud` needs a kubeconfig for the worker fleet,
and under cloud ADR-0021 it is deployed as an ordinary Burrow app — so the only available form was
**base64 content in an environment variable**: a cluster-admin credential to an entire fleet, in
`/proc/self/environ`, inherited by children. That shipped, with the cost written down rather than
hidden.

**The mechanism is familiar everywhere else in this repository.** The build Job mounts `git-creds`
and `registry-auth` as Secret volumes; the backup shipper mounts its object-store credential; the
add-ons mount ConfigMaps. [`controlplane/kube/build.go`](../../controlplane/kube/build.go) is the
canonical shape, and it already states the principle this record generalises:

> A source-provider credential (ADR-0057) is consumed by MOUNTING, never by passing … The token
> itself lives only in the mounted Secret's data — it is never one of these env values, so it never
> appears in the Job spec or a command line.

Burrow does this for its own credentials and offers it to nobody else's.

**What this record resolves.** How a mount is expressed, where the file may land, whether the
environment variable stays, whether the agent may ask for one, and what any of it does to
[ADR-0029](0029-secrets-through-the-control-plane.md).

**The forcing question.** The obvious design is "let the caller name a path." It is the wrong
default, for two reasons that only appear once you write it down. A mount at an arbitrary path can
**shadow a file in the app's own image** — `/etc/passwd`, an interpreter, a config the framework
reads — and the resulting failure looks like a broken image rather than a mount. And a mount at an
exact path has to be a `subPath` mount, which is the one form of Secret volume the kubelet **does
not update in place**; choosing arbitrary paths silently gives up rotation without a rollout, which
is one of the two reasons to want a file in the first place. Burrow owning the directory is not a
restriction bolted on for tidiness. It is what makes both properties true.

## Decision

### 1. A secret key may be projected as a file, in a directory Burrow owns

```sh
burrow app secret mount web GOOGLE_CREDENTIALS
burrow app secret mount web TLS_KEY --filename tls.key
burrow app secret unmount web TLS_KEY
```

`mount` names a **key**, never a value — it is in the same class as `secret list` and
`secret unset`, and there is no form of it that carries a secret. The key must already exist, or the
command refuses: mounting a key that was never set produces an app that starts, finds no file, and
fails at the moment it needs the credential, which is the failure this record exists to avoid making
easy.

The file lands at `<dir>/<filename>`, where `filename` defaults to the key. Secret keys already match
`^[A-Za-z_][A-Za-z0-9_]*$` ([ADR-0028](0028-app-config-and-secrets.md)), so a key is always a legal
filename; `--filename` exists for the app that wants `tls.key` rather than `TLS_KEY`, and is
validated as a single path segment — no `/`, no `.`, no `..`.

### 2. The directory is `/run/secrets`, and it is Burrow's

One Secret volume per app, mounted read-only at `/run/secrets`, projecting only the keys that were
mounted:

```go
corev1.Volume{Name: "app-secrets", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
	SecretName:  controlplane.AppSecretName(spec.App),
	Items:       items,             // one KeyToPath per mounted key
	DefaultMode: int32Ptr(0400),
	Optional:    boolPtr(true),
}}}
```

`Items` is what keeps this honest: an unmounted key is **not** in the volume, so mounting one key
does not put every other secret the app holds on disk. `Optional: true` for the same reason
`envFrom` is optional — a workload whose Secret does not exist yet still applies.

**`/run/secrets` and not a caller-chosen path.** It is a tmpfs path no base image populates, so
nothing is shadowed and the failure mode "my app broke and the mount is why" cannot occur. It is a
whole-volume mount rather than a `subPath`, so the kubelet **updates the file in place** when the
value changes. And it is one directory, so `BURROW_SECRETS_DIR` is a real answer to "where are my
credentials" rather than a list.

`--dir` overrides the directory **per app**, for an app that insists on `/etc/app/secrets`. It is
one directory for that app, still Burrow-owned in the sense that Burrow mounts a volume over it, and
the operator who sets it accepts that whatever the image had there is hidden. It is deliberately not
a per-key flag: per-key paths are how you arrive at `subPath` and lose in-place updates.

**An app that hardcodes a path takes the path from the environment instead.** Nearly every tool that
reads a credential file accepts a path variable — `GOOGLE_APPLICATION_CREDENTIALS`, `KUBECONFIG`,
`REGISTRY_AUTH_FILE`, `GIT_CONFIG_GLOBAL` — and Burrow's own build Job already works exactly this
way. Setting one is `burrow app config set`, which already exists.

### 3. The path is in the environment; the value never is

Burrow injects `BURROW_SECRETS_DIR` when the app has at least one mount, carrying the directory and
nothing else. This is the shape the backup shipper already uses (`BURROW_SHIP_CREDENTIALS_DIR`), and
the property it preserves is the one from `build.go`: a path in the environment is not a secret in
the environment.

### 4. The variable stays, unless the caller says otherwise

Mounting a key **adds** a file. It does not remove the environment variable, because removing it
would break an app mid-rollout — the code that reads the file has to be deployed before the variable
it replaces disappears, and Burrow does not know when that happened.

`--no-env` marks a key **file-only**, and is the answer for a credential whose whole reason for being
on disk is to stay out of `/proc/self/environ`.

**Its cost, stated rather than discovered.** `envFrom` sources the Secret wholesale; there is no way
to exclude one key from it. So the first `--no-env` on an app switches its pod template from
`envFrom` to an enumerated `secretKeyRef` per remaining key. That is a real behaviour change for
that app: today, `secret set` of a **new** key reaches the pod on a restart, because `envFrom` picks
up whatever the Secret holds. With enumeration, a new key changes the pod template, so `secret set`
reapplies the workload rather than bumping the restart annotation. The reapply path already exists in
the engine. An app with no `--no-env` key keeps `envFrom` and is bit-for-bit unchanged.

### 5. A mount is app configuration, not a property of a release

Mounts are stored the way config keys are: per app, per environment, in Postgres, **keys and
filenames only, never a value** — which is the same thing Postgres already records about secrets. The
deploy, env-reapply and rollback paths all read them the same way they read `cfg` today.

**Rollback is the reason.** A release records the code; a mount records how a credential is delivered
to whatever code is running. If a mount rode the release, rolling back to a release cut before the
mount existed would **remove the file the running app needs** — a rollback, the incident escape
hatch, would take the credential with it. Config already behaves this way, and a mount is config.

### 6. Rotation updates the file; whether the process notices is the app's business

A whole-volume Secret mount is updated in place by the kubelet, typically within about a minute. So
for a mounted key, `burrow app secret set` has two effects that were previously one: the file is
replaced without any help from Burrow, and the pod is restarted so the process re-reads it.
`--no-restart` becomes genuinely useful rather than merely available — an app that re-reads the file
on each use gets rotation with no downtime at all.

Burrow does **not** claim the process picked up the change. It claims the file did.

### 7. On the agent surface, at the tier its neighbours already occupy

Under [ADR-0065](0065-what-belongs-on-the-agent-surface.md) §1, `secret mount` fails neither test.
Its scope is the one app it names, and it is reversible by `unmount` — so it is tier 3 material at
worst, and it is **not** more dangerous than the injection it replaces: mounting a key the app
already holds as an environment variable moves a credential the app can already read from one place
it can read it to another.

It ships **ungated**, because config and secret mutation are ungated today: there is no guardrail
code for `secret set`, `secret unset`, `config set` or `config unset`. Inventing one for the single
verb that makes a credential *safer* would be the wrong place to start. If that neighbourhood gains a
guardrail, `mount` and `unmount` ride that code rather than acquiring their own.

`secret set` remains absent from the agent surface, unchanged and for the unchanged reason: it is the
one verb that carries a value.

### 8. ADR-0029 holds, unchanged

Worth confirming rather than assuming, because it is the record most likely to be read as
constraining this one. It is not:

- **Where the value lives** — the per-app Kubernetes Secret, "never inlined into the Deployment,
  never written to Postgres". A mount is a `KeyToPath` reference in the pod template; the template
  gains a key name and a filename and no value. Postgres gains a filename.
- **Who may set one** — burrowd, over its authenticated API, from the human's CLI. Unchanged. There
  is still no `secret set` on the agent surface.
- **Never logged, never audited** — a mount is audited by key name, exactly as `secret set` is.

What changes is the **projection**: which of the pod's two doors the value comes through. The value
crosses no new boundary, and reaches no principal that could not already read it.

## Consequences

- An app can hold a credential it never puts in its environment, and a credential that is a file can
  be a file.
- `burrowd-cloud`'s worker kubeconfig stops being base64 in `/proc/self/environ`. Cloud
  [#50](https://github.com/burrow-cloud/cloud/issues/50) can close.
- `WorkloadSpec` gains its first field that is not about the container's code, and the app PodSpec
  gains its first `Volumes` entry. The three construction sites in `controlplane/engine.go` and the
  fake are the whole surface.
- An app that uses `--no-env` leaves the `envFrom` fast path. That app's `secret set` is a workload
  reapply rather than an annotation patch — slower, and correct.
- `/run/secrets` becomes a path Burrow owns in every app container that mounts anything. An image
  that populates it will find it hidden, which is the same statement as "Burrow owns it" and is why
  the default is a path no base image populates.
- Files are mode `0400` and owned by the container's user. An app that drops privileges after start
  and then opens the file will fail, and the fix is the app's `runAsUser`, not the mount.
- A key that is mounted and then `unset` leaves the pod with no file rather than an empty one, and
  the pod still starts. Same shape as an unset environment variable, which also simply is not there.

## Alternatives considered

**A per-key arbitrary path.** The obvious design and the one the issue reached for. Rejected on the
two grounds in the Context: it makes shadowing a file in the app's own image possible, with a failure
that does not look like a mount, and it forces `subPath`, which is the one Secret-volume form the
kubelet does not update in place. It buys compatibility with an app that hardcodes a path, and the
environment variable those apps almost all accept buys the same thing without either cost.

**Mount every key, always.** One volume, no declaration, every secret is both a variable and a file.
Simpler to build and worse to hold: it puts every credential an app has on disk whether or not
anything reads it there, which widens what a path-traversal bug or a stray `tar` reaches, and it
makes the file surface a thing that changes whenever anyone sets a secret.

**Make the mount a property of the secret rather than of the app.** `secret set KEY --as-file`, so
delivery is decided when the value is written. Rejected because the person who holds the credential
and the person who knows how the app reads it are not always the same, and because it puts a
delivery decision on the one command whose entire discipline is that it carries a value and therefore
does as little else as possible.

**Do it in the ADR-0061 pod mutator.** It already exists and can add a volume. It is an **embedder**
seam — compiled in, not configured — and using it here would mean the answer to "can my app mount a
secret" is "fork the control plane."

**Nothing, and let the app write the file itself at startup.** What every affected app does today:
read the variable, `base64 -d`, write to disk. It works, and it means the credential is in the
environment *and* on disk, in a location Burrow knows nothing about, with the decode step duplicated
in every app. This record removes a workaround rather than adding a feature.

## References

- [ADR-0028](0028-app-config-and-secrets.md) — the per-app Secret, keys-only listing, the restart
  annotation.
- [ADR-0029](0029-secrets-through-the-control-plane.md) — a value crosses the control-plane API and
  never the agent channel.
- [ADR-0065](0065-what-belongs-on-the-agent-surface.md) — the tier criterion, and §6's requirement
  that a new capability state its tier.
- [ADR-0057](0057-source-provider-credentials.md) — the build's mounted credential, the idiom this
  generalises.
- [`controlplane/kube/build.go`](../../controlplane/kube/build.go) — `KeyToPath` + `ReadOnly` + the
  path in the environment.
- burrow [#424](https://github.com/burrow-cloud/burrow/issues/424) — the issue this record answers.
- cloud [#50](https://github.com/burrow-cloud/cloud/issues/50) — the case that raised it.
