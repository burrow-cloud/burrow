# ADR-0037: CLI onboarding and command organization

## Status

Accepted. Extends the command taxonomy ([ADR-0024](0024-cli-command-taxonomy.md)), the agent-native
onboarding ([ADR-0034](0034-agent-native-onboarding.md)), and the environment model
([ADR-0036](0036-environment-selection.md)). Drops the `system` command group in favor of `cluster`,
and removes the `kubectl` binary dependency from `install`.

## Context

`burrow --help` lists twelve top-level commands as a flat wall — hard for a first-time user (even the
maintainer) to parse, and it buries the golden path (install → pick an environment → deploy). Three
specific first-run failure modes also lose users: `install` silently targeting whatever the current
kube context happens to be (dangerous — that could be prod); `install` requiring the `kubectl` binary
(an extra prerequisite, because it shells out to `kubectl apply`); and unhelpful errors when there is
no kubeconfig or no cluster at all. Two commands (`cluster`, `system`) both surfaced "ingress,"
signaling redundancy. KUBECONFIG is already honored (the `connect` package uses
`clientcmd.NewDefaultClientConfigLoadingRules()`), so that needs no change.

## Decision

**Organize the CLI into intent-based groups, make `install` explicit and self-contained, and treat a
missing local config as a first-time user.**

### Command groups

`burrow --help` uses Cobra command groups, ordered along the golden path. Membership:

- **Get started** — `install`, `upgrade`, `cluster`, `config`
- **Environments** — `env`
- **Operate** — `app`, `addon`
- **Govern** — `guard`, `audit`
- **Other** — `version`, `completion`, `help` (each on its own line with its description, like every
  other command — not a single packed line)

`config` here is the credentials store (providers, registries); a later rename to disambiguate it from
`app config` (env vars) is noted but out of scope.

### `install` is explicit and non-interactive

- **`burrow install <context>`** — the context is a **required positional argument**. Install targets
  exactly that kube context; nothing is ever installed into the current context implicitly.
- **`burrow install` with no argument** — lists the kubeconfig contexts (marking the current one) and
  instructs the user to re-run with one: it does **not** install and does **not** prompt. This keeps
  it fully non-interactive (no TTY dependency, no picker library, safe in CI and for the agent) while
  still never guessing the target. (An interactive picker was considered and rejected — see below.)
- **No kubeconfig found** (no `$KUBECONFIG`, no `~/.kube/config`) — a clear stop explaining Burrow
  operates a cluster you point it at, rather than a raw library error.
- On success, `install` **names the environment** (ADR-0036): an explicit `--environment <name>`, or a
  generated friendly name (adjective-animal, e.g. `sequestered-pirate`) when omitted. It **writes the
  environment into `~/.burrow/config`** (so first-run detection flips and `burrow env list` shows it
  without connecting) and prints how to rename it (`burrow env rename <old> <new>`).

### `install` uses client-go server-side apply (no `kubectl` dependency)

`install` applies its manifests with client-go **server-side apply** instead of shelling out to
`kubectl apply`. Burrow becomes a self-contained binary that needs only a reachable kubeconfig, not a
separately-installed `kubectl`. (The CLI already uses client-go for everything else; the `kubectl`
shell-out was the lone outlier.)

### First-run config awareness

When `~/.burrow/config` does not exist, the user is treated as brand-new: bare `burrow` / `burrow
--help` leads with a short banner pointing at `burrow install`, and de-emphasizes the rest. Once the
config exists (at least one environment installed), the full grouped help shows.

### `system` dropped; folded into `cluster`

`cluster` becomes the single home for cluster capabilities **and** infrastructure: `burrow cluster`
inspects what the cluster can do (capabilities), and `burrow cluster ingress install [--expose …]`
provisions ingress/TLS (moved from `system ingress install`). `cluster` is the concrete, unambiguous
noun for the Kubernetes cluster; `system` read as if it might mean the control plane. The `system`
group is removed.

### Shell completion

Cobra's built-in `completion` command (bash, zsh, fish, PowerShell) is enabled. This is not a
load-bearing decision, so it gets no ADR of its own — it is documented in the README with the
per-shell one-liner (covering Windows/PowerShell, not just the maintainer's zsh).

## Consequences

- The golden path is legible at a glance, and the first-run experience routes a new user straight to
  `install` with no silent mis-targeting and no `kubectl` prerequisite.
- A genuine prerequisite remains: Burrow needs a cluster. That narrows the top of the funnel; the
  parked cluster-provisioning ADR is the eventual answer for the "no cluster yet" user — until then
  `install` explains the requirement plainly.
- Dropping `system` and removing the `kubectl` shell-out revise surfaces shipped in ADR-0024/0034.
  Acceptable pre-1.0, consistent with the `app env`→`app config` and `burrow context` revisions.

## Rejected alternatives

- **`burrow install` defaulting to the current context.** Rejected: too easy to install into prod by
  accident; the target must be explicit.
- **An interactive context picker** (a TUI drop-down via an external library). Rejected: it needs a
  TTY (breaks CI and the agent) and a dependency (against the small-graph principle); list-then-re-run
  with a positional argument is explicit, dependency-free, and works everywhere.
- **Keeping `install` on `kubectl apply`.** Rejected: it forces an extra prerequisite; server-side
  apply via client-go makes the CLI self-contained.
- **Keeping `system` alongside `cluster`.** Rejected: the inspect/provision split across two nouns on
  the same domain is the redundancy; one `cluster` noun covers both.
- **Moving `install` under `env` (`burrow env install`).** Rejected ([discussion], kept top-level):
  install is the activation entry point and must be top-level-discoverable; `env scan`/help cross-link
  to it instead.
