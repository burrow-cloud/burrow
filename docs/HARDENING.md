# Hardening: make the control plane the agent's only path to the cluster

Burrow's guardrails ([ADR-0006](adr/0006-guardrails-in-the-control-plane.md),
[ADR-0020](adr/0020-guardrails-as-configurable-policy.md)) gate dangerous operations —
refusing or holding-for-confirmation things like scaling to zero or oversized scale-ups. But
they only govern operations that go **through the control plane**. They are a real boundary
only when the control plane is your agent's **only** path to the cluster
([ADR-0021](adr/0021-guardrails-require-control-plane-only-agent-access.md)).

## The scoped agent credential is the boundary

At `burrow cluster install` Burrow mints a **scoped, burrowd-only credential** for the agent
([ADR-0038](adr/0038-scoped-agent-credential.md)): a `burrow-agent` ServiceAccount with a narrow
Role granting only what the client needs to reach burrowd (proxy access to the burrowd Service and
`get` on the API-token Secret) and nothing else — no pods, no other secrets, no other namespaces,
no cluster-wide read. It writes a self-contained kubeconfig for that credential under
`~/.burrow/agents/` (never into `~/.kube/config`). The human keeps their own admin kubeconfig for
privileged setup and governance (`install`, `upgrade`, `guard set`, `env add`, registry/provider
credentials).

`burrow-agent` and the human CLI operate path (`deploy`, `status`, `logs`, `rollback`, `scale`, …) default
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
| Your agent, via `burrow-agent` | only the scoped kubeconfig (`~/.burrow/agents/<env>`), granting exactly: proxy to the `burrowd` Service, and `get` the `burrowd-api-token` Secret | The `burrow-agent` operate-verbs only (deploy, status, logs, scale, rollback, autoscale, config, secret list/unset, publish, addons, domains, reachability, metrics/logs query, guard read-only, audit read), every mutating verb guardrailed and audited. `addon sql` is on the list too and is **denied by default** — see below. | Anything else on the cluster: no arbitrary kubectl, no other Secrets, no other namespaces, no cluster-scoped reads, no exec, and it cannot change guardrails. It cannot leave burrowd. |

**Two independent layers.** The scoped credential is the wall that keeps the agent from going around
burrowd (touching the cluster directly). The guardrails are the policy for what is allowed through
burrowd. Different mechanisms; you need both.

**A guardrail can bind the agent and leave you alone.** Inside burrowd, the guardrails and the
`burrow-agent` verb surface are what decide which operation may be called (and the guard verb is
read-only). The scoped credential confines the agent to burrowd; it does not by itself enforce which
burrowd operation the agent may call — the guardrail policy does, and a disposition can now say
**which kind of credential it binds**
([ADR-0094](adr/0094-a-guardrail-can-bind-the-agent-and-leave-the-human-alone.md)):

```sh
burrow guard set --binds agent --env prod --name burrowd-cloud app.deploy deny
```

That refuses the deploy for anything holding an `agent` credential, and leaves your own credential
reading whatever the policy says underneath it. The refusal the agent receives says so, and says what
to relay: *"this disposition binds `agent` credentials; an operator can run it with their own."*

Four things are worth knowing before you reach for it:

- **`--binds` takes `user`, `agent` or `machine`** — the credential kinds recorded at issuance
  ([ADR-0084](adr/0084-everyone-who-uses-burrow-carries-their-own-token.md) §3). The kind is read from
  the stored credential row, never from the request, so a compromised agent cannot present itself as
  a person.
- **It composes with the other scopes rather than replacing them.** A binding can sit on the global
  policy, on one environment, or on one app or add-on instance. Within a tier the bound disposition
  answers for that kind and the unbound one answers for everybody else; the *target* still narrows
  first, so a cluster-wide `--binds agent` does not override something you set deliberately for one
  app.
- **It relaxes as well as it tightens.** A global `app.delete deny` with
  `guard set --binds user --env dev app.delete allow` under it lets you clean up a sandbox by hand
  while the agent stays refused everywhere.
- **It needs an install people have signed in to.** The shared install token carries no kind, so a
  bound disposition would bind nobody — `guard set --binds` refuses on an install with no per-caller
  credentials rather than storing a protection that protects nothing. Run `burrow auth login` first.
  An install that never signs anybody in behaves exactly as it always has: a disposition set without
  `--binds` binds every caller.

`guard list` resolves for the kind of the caller asking, so an agent reading the policy sees what
binds the agent, and a listing that contains any bound disposition grows a `BINDS` column so you can
see the binding rather than discover it later. Every audited row already records the caller's kind
next to the principal, so the trail distinguishes the two decisions with no new column.

**A lock is not one of these layers, and it is not a security control.** `burrow lock <app>` (and
`burrow lock addon <instance>`) is state on the thing itself: locked, deleting the app, removing the
instance, and `addon detach --delete-data` refuse, and everything else carries on. It refuses *every*
caller, including you at your own terminal with your own admin kubeconfig — which is exactly why it
is not a boundary: anyone with write access to the namespace deletes the same objects with `kubectl`
and never touches Burrow. It buys one thing, and it is worth having on its own terms: the destructive
path through Burrow takes a separate command whose only purpose is to permit destruction, so the
decision and the act happen at two different moments. Neither verb exists on `burrow-agent`, so the
agent can see a lock and report it and cannot remove one. Do not substitute it for a guardrail or for
the scoped credential; use it alongside them, on the things you would not want to lose.

**This is not RBAC**, and it is deliberately not: one axis, three values, fixed at issuance, and no
principal ever appears in a policy key. Whether a caller may issue a credential or grant the admin bit
is a different question with a different answering seam. A hardening-conscious operator should still
lean on the guardrails plus environment isolation, not assume the scope alone is a per-operation
boundary.

### Route and identity are separate, and both are required

Signing in with `burrow auth login --context <cluster>` asks that Burrow for a **credential of your
own** ([ADR-0084](adr/0084-everyone-who-uses-burrow-carries-their-own-token.md) §1): a random token
burrowd generates, hands back once, and stores only the hash of, under `~/.burrow/credentials/` with
the same 0600-under-0700 handling as the Burrow Cloud credential. No Kubernetes object is created, so
burrowd needs no authority to mint ServiceAccounts and nothing new from your cluster.

Afterwards two different things carry a request, and **neither is sufficient alone**:

| | What proves it | What it gets you |
|---|---|---|
| **Route** | the kubeconfig you already have | reaching burrowd through the API-server proxy — burrowd has no address of its own |
| **Identity** | the Burrow token, in `X-Burrow-Token` | being someone in particular |

Cluster access without a Burrow token does nothing. A Burrow token without cluster access cannot
reach burrowd. Reaching burrowd's pod address directly still gets nothing, exactly as before, so **no
NetworkPolicy is load-bearing** in this design.

What changes for hardening is that your kubeconfig stops being *how burrowd knows who you are*. A
caller carrying their own credential no longer needs `get` on `burrow-credentials`, and burrowd
records **who acted and what kind of credential they held** on every audited operation instead of one
literal on every row.

**The shared install token still works, deliberately.** An install nobody has signed in to behaves
exactly as it did, and the shared token is checked first, so a person who never runs `burrow auth
login` is not broken by any of this. It is also the **break-glass**
([ADR-0084](adr/0084-everyone-who-uses-burrow-carries-their-own-token.md) §8): if burrowd cannot
serve, it cannot check a minted token either, and the original path — kubeconfig, the
`burrowd-api-token` Secret, the API-server proxy — is the recovery route. That is a real credential
with real power, so treat it as one: reading `burrow-credentials` is an administrative act on the
cluster, gated by cluster RBAC, and it is how you get in when the front door is broken. It is not a
leftover to be tidied away.

Revoking a credential is a burrowd operation, so a lost laptop is answered by revoking that
credential rather than by rotating the install — but note the honest limit below: the revocation is
in the control plane and has no command on the CLI yet.

### A second person, and the agent, each carry their own

Signing in claims the install for the **first** person, who becomes its admin. Everybody after them
is invited: `burrow auth invite <name>` records them and prints an **invitation**, and they run
`burrow auth login --context <cluster> --invite <invitation>`
([ADR-0084](adr/0084-everyone-who-uses-burrow-carries-their-own-token.md) §2).

What matters for hardening is what does and does not travel. An invitation is not a credential: it
expires after a day, it is spent the first time it is exchanged, and burrowd refuses it at every
route but the exchange — so a copy left in a chat log buys somebody the ability to become a principal
who has been given nothing yet, not the ability to act. **The credential the invited person carries
is generated by the exchange, on their machine, and is never sent anywhere.**

The invited person needs a kubeconfig context that reaches the cluster, which is the route, and
nothing else. They need no cluster admin and **no `get` on `burrow-credentials`** — they never hold
the install's shared token at all, which is what makes giving somebody access to Burrow different
from giving them access to the cluster.

**Signing in also issues `burrow-agent` a credential of its own**
([ADR-0084](adr/0084-everyone-who-uses-burrow-carries-their-own-token.md) §3), under
`~/.burrow/agents/`, belonging to the same principal and recorded as `kind = agent`. Until now the
agent presented the install's shared token, so cutting it off meant rotating the install and logging
everybody out; now it is one revocation, and the person's terminal keeps working. The audit trail
also stops conflating them: a row says whether the person or their agent acted.

**Revocation has no command yet.** The control plane revokes one credential without touching another,
and that is what makes the separate rows worth having — but nothing on the `burrow` CLI reaches it,
and nothing lists what a principal holds. Until it does, the lever for a compromised agent is still
rotating the install, and the separate credential is buying attribution rather than a faster answer.
Plan for that when you decide how much to rely on it.

**What this does not yet give you.** The kind is recorded and read from the stored row, never from
the request — but `Policy.enforce` still takes no caller, so a `deny` binds the agent and the person
identically. A guardrail that binds the agent and leaves the human alone is the decision above, still
unmade. Until it is, the agent's separate credential buys revocation and attribution, not a narrower
set of operations.

Two further limits to know. An **admin's** agent credential is an admin's credential: it belongs to
the same principal, so it can invite and issue exactly as the person can. And an invitation is
authorization spent at issue rather than at exchange, so an admin who invites somebody and changes
their mind should revoke the principal rather than assume an unexchanged invitation is inert.

**One verb reaches application DATA, and it is closed until you open it.** `burrow-agent addon sql`
runs a statement against one app's database ([ADR-0087](adr/0087-running-sql-against-an-attached-database.md)).
Every other verb acts on the platform's own state; this one reads and can write what your users put
in. Three things bound it, and the third is the one to act on:

- burrowd connects as the **app's own role**, never the instance superuser, with the credential it
  already minted at attach — so a statement can touch exactly what that application can touch, and
  no more. The credential chooses the database, so there is no form of the verb that reaches the
  instance, `template1`, or another app's database.
- No connection to a database leaves the cluster. There is no port-forward and no proxy, deliberately:
  a tunnel would make your kubeconfig a credential for tenant data.
- The `addon.sql` guardrail is **`deny`** out of the box and nothing opens it but you. It is
  env-scopable, and the shape to reach for is a gradient rather than a switch:
  `burrow guard set --env dev addon.sql allow` and nothing in production. Burrow does **not** tell a
  read from a write and will not pretend to — a `SELECT` can delete — so "allow" means allow, not
  "allow reads".

Opening it also means the statement text lands in the audit log, which is what makes it accountable
and means a literal in a `WHERE` clause is recorded where anyone with audit access sees it.

**Attaching a database is held for confirmation.** `addon.attach` is `confirm` out of the box
([ADR-0095](adr/0095-attaching-a-database-is-held-for-a-human.md)): an attach puts a database and a
login role on a server every other app in the environment shares, restarts the app, and on an app
that is already attached rotates its password — which nothing can undo, so anything else holding the
old connection string stops working. A held attach provisions nothing and names what it would do.
Be clear about what a `confirm` is: the caller sets the flag, and nothing verifies a human saw the
prompt, so it is a raised floor rather than a boundary against an agent that decides to pass
`--confirm`. It is scoped to the add-on instance and env-scopable, so the shape to reach for is a
gradient — `burrow guard set --env dev addon.attach allow` for a sandbox, held or denied in
production. The guardrail also says nothing about how *many* databases exist: it answers whether one
call proceeds, not whether a tenant has had enough, which is a quota and a different mechanism.

### Joining an already-installed cluster (multi-user)

A second person on an already-installed cluster does not re-install: `burrow cluster install <context>`
detects the existing control plane and performs a **local join** — it reads the existing
`burrow-agent` credential and writes only their own `~/.burrow` scoped kubeconfig, making no cluster
changes. `burrow env list --discover` and `burrow cluster upgrade` do the same backfill for handles and for clusters
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

### Make `burrow-agent` fail closed: `BURROW_AGENT_REQUIRE_SCOPED=1`

`burrow-agent` fails closed around the scoped credential, so a missing credential never silently
re-grants the agent full cluster access. Two behaviors matter:

- A handle that records a scoped credential whose file is gone is always an error. `burrow-agent`
  refuses to fall back to the ambient (admin) kubeconfig and tells the operator to re-mint the
  credential with `burrow cluster upgrade` (or `burrow cluster install`). This holds even without the strict mode
  below.
- Set `BURROW_AGENT_REQUIRE_SCOPED=1` in the agent's environment and `burrow-agent` refuses the ambient
  fallback entirely. A context with no scoped credential at all (an unregistered context, or a
  cluster installed before the scoped credential existed) becomes an error too, instead of falling
  back to whatever ambient kubeconfig the agent can reach. This is the recommended setting for an
  agent that should reach nothing but the scoped, guardrailed control plane.

Strict mode still honors the explicit escape hatches an operator sets deliberately: a direct
control-plane URL (`BURROW_CONTROL_PLANE_URL`, which is not cluster admin) and an explicit
`BURROW_KUBECONFIG`. It refuses only the implicit ambient fallback. The value is truthy for `1`,
`true`, or `yes` (case-insensitive); empty or unset leaves strict mode off.

The human CLI keeps its graceful ambient fallback for a recorded-but-missing or absent scoped
credential; only `burrow-agent`, the agent's surface, fails closed.

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
  path; it uses `burrow-agent` (`burrow-agent deploy`, `burrow-agent scale`, …), where the
  guardrails apply.
- **Deny direct cluster tooling** (`kubectl`, `helm`, anything that uses the kubeconfig) → a
  `deny` / `confirm` guardrail can't be sidestepped.
- **Allow `docker`** → the agent still builds and pushes images (the client-side build path,
  [ADR-0008](adr/0008-two-build-paths.md)), then deploys by reference through Burrow.

## Example: Claude Code

`burrow agent claude install` does the first step for you: alongside allowing the scoped
`burrow-agent` binary, it merges the `Bash(burrow *)` deny rule into your user-wide
`~/.claude/settings.json` (preserving everything else, backing the file up first). So denying the
`burrow` CLI is handled automatically.

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
      "Bash(burrow-agent *)",
      "Bash(burrow-agent)",
      "Bash(docker *)"
    ]
  }
}
```

The space before `*` is a word boundary — `Bash(burrow *)` matches `burrow guard set …` but not
the scoped `burrow-agent` binary (no space follows `burrow` in `burrow-agent`) and not a tool named
`burrowctl`. Because deny beats allow, the `docker` and `burrow-agent` allows do not weaken the
denies — they let the agent run the scoped control channel while the human `burrow` CLI stays
blocked.

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
