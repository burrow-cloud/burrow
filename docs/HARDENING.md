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
