# Hardening: make the control plane the agent's only path to the cluster

Burrow's guardrails ([ADR-0006](adr/0006-guardrails-in-the-control-plane.md),
[ADR-0020](adr/0020-guardrails-as-configurable-policy.md)) gate dangerous operations —
refusing or holding-for-confirmation things like scaling to zero or oversized scale-ups. But
they only govern operations that go **through the control plane**. They are a real boundary
only when the control plane is your agent's **only** path to the cluster
([ADR-0021](adr/0021-guardrails-require-control-plane-only-agent-access.md)).

## The scoped agent credential is the boundary

At `burrow install` Burrow mints a **scoped, burrowd-only credential** for the agent
([ADR-0038](adr/0038-scoped-agent-credential.md)): a `burrow-agent` ServiceAccount with a narrow
Role granting only what the client needs to reach burrowd (proxy access to the burrowd Service and
`get` on the API-token Secret) and nothing else — no pods, no other secrets, no other namespaces,
no cluster-wide read. It writes a self-contained kubeconfig for that credential under
`~/.burrow/agents/` (never into `~/.kube/config`). The human keeps their own admin kubeconfig for
privileged setup and governance (`install`, `upgrade`, `guard set`, `env add`, registry/provider
credentials).

`burrow-mcp` and the CLI operate path (`deploy`, `status`, `logs`, `rollback`, `scale`, …) default
to that scoped kubeconfig, so **the agent's reachable credential is confined to the control plane**:
even a shelled-out `kubectl` pointed at it is denied everything except reaching burrowd, and the
guardrails become binding by construction rather than resting only on the shell-denies below. The
kubeconfig is the real trust boundary — so the highest-value hardening step is to make the scoped
credential the *only* kubeconfig the agent can reach (point its `KUBECONFIG` at the scoped file, or
run it in a container/VM that carries only that credential).

### Trust surfaces

Four credentials reach this cluster, and they are deliberately unequal. The table maps each surface
to the credential it carries, what that lets it do, and what it cannot:

| Surface | Credential it uses | What it can do | What it cannot do |
|---|---|---|---|
| You, via `kubectl` | your admin kubeconfig (`~/.kube/config`) | Everything on the cluster: any resource, any namespace, cluster-scoped objects, exec, delete, RBAC. No Burrow guardrails. | Nothing restricts it. Full cluster admin, and it is what installs Burrow. |
| You, via `burrow` (setup and governance): `install`, `upgrade`, `cluster ingress install`, `config registry`, `config provider`, `env add`, `env list --discover`, `guard set`, `addon`, `domain`, `audit` | your admin kubeconfig | Install/upgrade Burrow, write its namespaces/RBAC/secrets, set the guardrail policy, configure registry and DNS-provider credentials, install add-ons, manage DNS, read the audit log. | These are admin operations. `guard set` lives here on purpose: only the human, with admin, changes guardrails. |
| You, via `burrow` (operate an app): `app deploy`/`status`/`logs`/`scale`/`rollback`/`autoscale`, `app config`/`secret`, `publish` | the scoped agent kubeconfig (falls back to admin if none) | Operate apps through burrowd, with every action guardrail-checked and audited. | Reach the cluster around burrowd; the guardrails gate what is allowed. You still have kubectl for raw access. |
| Your agent, via `burrow-mcp` | only the scoped kubeconfig (`~/.burrow/agents/<env>`), granting exactly: proxy to the `burrowd` Service, and `get` the `burrowd-api-token` Secret | The `burrow_*` MCP tools only (deploy, status, logs, scale, rollback, autoscale, config, secret list/unset, expose, addons, domains, reachability, metrics/logs query, guard read-only, audit read), every mutating tool guardrailed and audited. | Anything else on the cluster: no arbitrary kubectl, no other Secrets, no other namespaces, no cluster-scoped reads, no exec, and it cannot change guardrails. It cannot leave burrowd. |

**Two independent layers.** The scoped credential is the wall that keeps the agent from going around
burrowd (touching the cluster directly). The guardrails are the policy for what is allowed through
burrowd. Different mechanisms; you need both.

**One honest limitation, stated plainly.** Inside burrowd, authorization today is a single shared API
token the agent can read, so the scoped credential confines the agent to burrowd but does not by
itself enforce which burrowd operation it may call. The guardrails and the MCP tool surface do that
(and the guard tool is read-only). Per-principal authorization inside burrowd, so an agent identity
is denied specific endpoints regardless of tooling, is future work, and the `principal` seam added in
ADR-0038 is the groundwork for it. A hardening-conscious operator should lean on the guardrails plus
environment isolation, not assume the scope alone is a per-operation boundary.

### Joining an already-installed cluster (multi-user)

A second person on an already-installed cluster does not re-install: `burrow install <context>`
detects the existing control plane and performs a **local join** — it reads the existing
`burrow-agent` credential and writes only their own `~/.burrow` scoped kubeconfig, making no cluster
changes. `burrow env list --discover` and `burrow upgrade` do the same backfill for handles and for clusters
installed before the scoped credential existed.

The join reads the `burrow-agent-token` Secret in the control-plane namespace, so a joining user
needs `get` on exactly that Secret. Burrow **does not** widen the default RBAC to grant it: by
default the joining user must have kubeconfig access sufficient to read that one Secret (e.g. the
admin who installed, or anyone already granted it), otherwise the join fails with a clear, actionable
error and an operator hands over the scoped kubeconfig from `~/.burrow/agents/` out of band.

An operator who wants **self-serve join** for a team can add a small Role granting exactly that read
and bind it to the joining group — for example:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: burrow-agent-token-reader
  namespace: burrow          # the control-plane namespace
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["burrow-agent-token"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: burrow-agent-token-reader
  namespace: burrow
subjects:
  - kind: Group
    name: your-team-group    # the identity your cluster maps joining users to
    apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: Role
  name: burrow-agent-token-reader
  apiGroup: rbac.authorization.k8s.io
```

This grants only `get` on the one shared agent-token Secret — nothing else — and is an opt-in an
operator applies deliberately; Burrow never applies it for you. (The agents share one `burrow-agent`
ServiceAccount today; per-user credentials keyed on an identity come with the later SSO work,
ADR-0038 §5.)

### Make `burrow-mcp` fail closed: `BURROW_MCP_REQUIRE_SCOPED=1`

`burrow-mcp` fails closed around the scoped credential, so a missing credential never silently
re-grants the agent full cluster access. Two behaviors matter:

- A handle that records a scoped credential whose file is gone is always an error. `burrow-mcp`
  refuses to fall back to the ambient (admin) kubeconfig and tells the operator to re-mint the
  credential with `burrow upgrade` (or `burrow install`). This holds even without the strict mode
  below.
- Set `BURROW_MCP_REQUIRE_SCOPED=1` in the agent's environment and `burrow-mcp` refuses the ambient
  fallback entirely. A context with no scoped credential at all (an unregistered context, or a
  cluster installed before the scoped credential existed) becomes an error too, instead of falling
  back to whatever ambient kubeconfig the agent can reach. This is the recommended setting for an
  agent that should reach nothing but the scoped, guardrailed control plane.

Strict mode still honors the explicit escape hatches an operator sets deliberately: a direct
control-plane URL (`BURROW_CONTROL_PLANE_URL`, which is not cluster admin) and an explicit
`BURROW_KUBECONFIG`. It refuses only the implicit ambient fallback. The value is truthy for `1`,
`true`, or `yes` (case-insensitive); empty or unset leaves strict mode off.

The human CLI keeps its graceful ambient fallback for a recorded-but-missing or absent scoped
credential; only `burrow-mcp`, the agent's surface, fails closed.

A coding agent (Claude Code, Cursor, …) still runs with a shell, so the shell-denies below are
defense in depth on top of that boundary. Unless you restrict it, it can:

- run the `burrow` CLI directly — including `burrow guard set`, changing the very guardrails
  meant to constrain it; and
- use `kubectl` with a broader kubeconfig — if one is still reachable in its environment — to
  operate the cluster directly, bypassing Burrow entirely.

Burrow can't fully prevent this from the inside — it has no control over your agent's other tools or
which kubeconfig its environment exposes. **You** close the gap, in your agent's permission settings
and by confining its kubeconfig to the scoped credential. The principle, whatever agent you use:

- **Deny the `burrow` CLI** → the agent can't `guard set` and can't shell around the guarded
  tools; it uses Burrow's MCP tools (`burrow_deploy`, `burrow_scale`, …), where the guardrails
  apply.
- **Deny direct cluster tooling** (`kubectl`, `helm`, anything that uses the kubeconfig) → a
  `deny` / `confirm` guardrail can't be sidestepped.
- **Allow `docker`** → the agent still builds and pushes images (the client-side build path,
  [ADR-0008](adr/0008-two-build-paths.md)), then deploys by reference through Burrow.

## Example: Claude Code

`burrow mcp claude install` does the first step for you: alongside adding the MCP server, it
merges the `Bash(burrow *)` deny rule into your user-wide `~/.claude/settings.json` (preserving
everything else, backing the file up first). So denying the `burrow` CLI is handled
automatically. Pass `--no-harden` to skip it if you manage permissions yourself.

The manual JSON below is still useful for the fuller lockdown: denying `kubectl` and `helm`
(still your call, since Burrow does not know which cluster tools you want blocked), pinning the
rules at the project level, or hardening another agent.

Claude Code enforces this with permission rules in a settings file, where `deny` rules take
precedence over `allow`. Put this in `.claude/settings.json` (project-level, checked into git)
or `~/.claude/settings.json` (user-wide):

```json
{
  "permissions": {
    "deny": [
      "Bash(burrow *)",
      "Bash(kubectl *)",
      "Bash(helm *)"
    ],
    "allow": [
      "Bash(docker *)"
    ]
  }
}
```

The space before `*` is a word boundary — `Bash(burrow *)` matches `burrow guard set …` but
not a tool named `burrowctl`. Because deny beats allow, the `docker` allow does not weaken the
denies.

**Caveat — defense-in-depth, not a sandbox.** A user can still override these rules with a
permission-bypass mode (e.g. `--dangerously-skip-permissions` / `bypassPermissions`). This
posture is a real boundary for a *cooperative* agent that honors its configuration — the
realistic threat is an over-eager assistant, not a hostile one — not an escape-proof jail. For
stronger isolation, run the agent in a container or VM whose only reachable credential is the
scoped agent kubeconfig Burrow mints (above), so a bypass still reaches nothing but the control
plane.

> The permission/deny-rule system shown here is specific to Claude Code. Other agent CLIs
> (Cursor and others) have their own permission models — apply the same principle (deny the
> Burrow CLI and direct cluster tools; allow image build/push) in your agent's mechanism.

This pairs with the guardrails: the control-plane guardrails decide what is allowed,
confirmed, or denied; this hardening ensures the agent can only act *through* them.
