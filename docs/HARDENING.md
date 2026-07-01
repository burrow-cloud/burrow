# Hardening: make the control plane the agent's only path to the cluster

Burrow's guardrails ([ADR-0006](adr/0006-guardrails-in-the-control-plane.md),
[ADR-0020](adr/0020-guardrails-as-configurable-policy.md)) gate dangerous operations —
refusing or holding-for-confirmation things like scaling to zero or oversized scale-ups. But
they only govern operations that go **through the control plane**. They are a real boundary
only when the control plane is your agent's **only** path to the cluster
([ADR-0021](adr/0021-guardrails-require-control-plane-only-agent-access.md)).

A coding agent (Claude Code, Cursor, …) runs with a shell. Unless you restrict it, it can:

- run the `burrow` CLI directly — including `burrow guard set`, changing the very guardrails
  meant to constrain it; and
- use `kubectl` with your kubeconfig to operate the cluster directly, bypassing Burrow
  entirely.

Burrow can't prevent this from the inside — it has no control over your agent's other tools.
**You** close the gap, in your agent's permission settings. The principle, whatever agent you
use:

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
stronger isolation, run the agent in a container or VM whose only reachable credential is
Burrow's.

> The permission/deny-rule system shown here is specific to Claude Code. Other agent CLIs
> (Cursor and others) have their own permission models — apply the same principle (deny the
> Burrow CLI and direct cluster tools; allow image build/push) in your agent's mechanism.

This pairs with the guardrails: the control-plane guardrails decide what is allowed,
confirmed, or denied; this hardening ensures the agent can only act *through* them.
