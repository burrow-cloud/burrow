# ADR-0024: Noun-grouped CLI command taxonomy

## Status

Accepted. Builds on [ADR-0019](0019-cli-framework-cobra.md) (Cobra) and the CLI/MCP split of
[ADR-0002](0002-four-layer-architecture.md).

## Context

The `burrow` CLI grew as a **flat list of verbs**: `install`, `upgrade`, `deploy`, `status`,
`logs`, `rollback`, `scale`, `expose`, `unexpose`, `reachability`, `domain`, `provider`,
`registry`, `ingress`, `guard`, `version`. v0.1 had a handful; v0.2 roughly doubled it. A flat
list does not convey **what goes with what** — a user scanning `burrow --help` sees fifteen
peers with no sense of which are one-time setup, which configure Burrow, and which operate a
running app.

Two things sharpen the need to organize by task rather than by verb:

- **The CLI is the human's surgeon and backstop, not the main path.** The agent does the bulk
  of the work over MCP ([ADR-0002](0002-four-layer-architecture.md)); a person reaches for the
  CLI to set things up or to inspect and fix something by hand. A surface used that way should
  be organized around *the task a person is trying to do*, so the groups themselves teach the
  model of the system.
- **Following kubectl's verbs blindly hurts.** `expose` is kubectl vocabulary that even
  heavy Kubernetes users rarely touch (they write YAML/Helm); as a Burrow verb it reads as
  vague and slightly dangerous rather than "make my app reachable."

## Decision

Group the CLI into **noun subcommands named for the task**, keeping a few genuinely top-level
verbs flat:

```
burrow
├── install / upgrade        # bootstrap + lifecycle of the control plane — top level
├── app <verb> [name]        # operate a deployed application
│   ├── list                 # (planned) every app Burrow manages
│   ├── deploy / status / logs / rollback / scale
│   ├── delete               # (planned) remove an app and its routing
│   ├── publish / unpublish  # make an app reachable at a host (was expose / unexpose)
│   └── domain  add/remove   # point a hostname's DNS at the app
├── config <area>            # the credentials Burrow uses on the user's behalf
│   ├── provider  add/list/types
│   └── registry  login/logout/list
├── system <area>            # cluster-wide infrastructure Burrow manages
│   └── ingress  install
├── guard  list/set          # guardrail policy — top level
└── version
```

Specific calls:

- **`install` / `upgrade` stay top level.** They are the bootstrap, the canonical first command
  (`burrow install`), and there is no "app install" to collide with — apps are run with
  `app deploy`. Burying the first step a user runs costs more than it clarifies.
- **`expose` → `publish`, `unexpose` → `unpublish`.** "Publish web at example.com" names the
  intent; `expose` carries kubectl baggage. The wire API and `controlplane.ExposeSpec` keep
  their names — only the user-facing verb changes.
- **`domain` lives under `app`.** Pointing a hostname at the cluster is the DNS half of the same
  reachability chain `publish` starts; it belongs with the app it makes reachable.
- **`reachability` becomes the body of `app status`** (the reachability surface was always meant
  to fold into status, [ADR-0018](0018-reaching-an-app-at-a-url.md)); the standalone command is
  retired once status carries the chain.
- **`provider` and `registry` move under `config`**, **`ingress` under `system`** — credentials
  the user configures vs. cluster infrastructure Burrow installs.
- **`guard` stays top level.** Guardrail policy spans every operation; it is a north star, not a
  configuration corner.
- **The MCP tool surface stays flat** (`burrow_deploy`, `burrow_domain_add`, …). Grouping is a
  human-discoverability aid; an agent selects tools by name and description and does not browse a
  menu. Tool names may later be aligned with the new verbs (`expose` → `publish`) but that is a
  separate, non-blocking change.

This is a breaking change to the CLI surface. It is made now, pre-1.0, while the surface is small
and the cost is lowest; the regrouping itself is mechanical under Cobra (parent commands plus two
renames). `app list`, `app delete`, and folding reachability into `status` need new control-plane
endpoints and land as follow-ups; this ADR fixes the taxonomy they slot into.

## Consequences

- Users relearn a small command tree once; in exchange `burrow --help` and each group's help
  teach the system's shape (setup vs. configure vs. operate).
- New verbs have an obvious home (a new per-app operation is an `app` subcommand; a new vendor
  credential is a `config` area), so the surface stays organized as it grows.
- Docs, the README quick-start, and examples move to the grouped form in lockstep
  ([ADR-0009](0009-honest-status.md): docs never lag the code).
- The MCP surface is unaffected, so the agent path and its tests are untouched by the regroup.

## Alternatives considered

- **Keep the flat verb list.** Simplest, but the lack of grouping is exactly the problem; it
  worsens with every added verb.
- **Full kubectl-style noun-verb for everything**, including `system install` / `system upgrade`.
  Rejected: it taxes the bootstrap path (`burrow install` is what a new user expects) for
  uniformity's sake.
- **Keep `expose`.** Rejected: the term tests poorly with the target user; `publish` states the
  goal.
