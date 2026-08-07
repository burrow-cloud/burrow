# Getting started

Getting going with Burrow is two parts: set up Burrow on your cluster, then connect your agent.
Once both are done, you operate your apps by talking to your AI agent, and it drives Burrow for
you.

## Prerequisites

- An existing Kubernetes cluster you can reach, with a context in your kubeconfig (the same access
  `kubectl` uses). Any reachable cluster works.
- [Homebrew](https://brew.sh), to install the CLI.

## Part 1 - Set up Burrow on your cluster

### 1. Install the CLI

```sh
brew install burrow-cloud/tap/burrow
```

This installs two binaries: `burrow` (the human admin CLI) and `burrow-agent` (the scoped control
channel your agent drives).

### 2. Install Burrow into your cluster

Run `burrow cluster install` with no argument to list the contexts in your kubeconfig, then install
into the one you want:

```sh
burrow cluster install                 # lists your contexts (and which already run Burrow)
burrow cluster install <context>       # installs into the context you name
```

Naming the context is required, so Burrow never installs into the wrong cluster by accident. The
install creates the control plane in the `burrow` namespace and deploys your apps into the
`burrow-apps` namespace. On success it names and records the environment as your current one, and
tells you it is ready.

The control plane's own database runs on CloudNativePG, the same operator every Postgres add-on uses,
so it can fail over and be backed up. Install puts that operator on the cluster first, which creates
cluster-scoped CustomResourceDefinitions and needs cluster-admin, and states both before it applies
anything. If your cluster's administrators will not accept those definitions, install with
`--database plain`: the database runs as a single Deployment with no backups and no failover, and
Postgres add-ons still work once you run `burrow cluster postgres install`. The choice is made at
install and only there ([ADR-0086](adr/0086-burrow-installs-one-kind-of-postgres.md)); `burrow cluster`
reports which one you have.

`burrow cluster install` provisions only the control plane. Additive cluster components are separate,
opt-in commands you run when you want them, each with its own status, install, and uninstall:

```sh
burrow cluster ingress install    # ingress-nginx, cert-manager, and a Let's Encrypt issuer (public HTTPS)
burrow cluster registry install --host registry.example.com   # the optional in-cluster image registry (a zero-config build push target)
```

The in-cluster registry is reachable the same way on any cluster: the build pushes to an internal
service in-cluster, and nodes pull the image through the cluster ingress over TLS. It therefore needs
`burrow cluster ingress install` first and a `--host` for the public pull endpoint; a cluster with no
domain cannot use it today.

Run `burrow cluster ingress` or `burrow cluster registry` with no subcommand to see whether each is
installed. (`burrow cluster registry` manages the registry that runs in your cluster; to give the
cluster credentials to pull from an external registry such as GHCR, use `burrow config registry` —
see Private registries below.)

### 3. Point the CLI at the cluster you mean (optional)

If you work with more than one cluster, say which one Burrow talks to instead of relying on whatever
`kubectl config use-context` last selected:

```sh
burrow auth login                      # asks where you use Burrow, and lists your kube contexts
burrow auth status                     # what is configured, and which one is active
burrow auth switch <name>              # change the active one
```

Only the context **name** is stored, in `~/.burrow/config` — your credential stays in the kubeconfig,
so rotating it keeps working. This is also how a **second person** starts using a cluster that is
already set up: they select the context they already have and install nothing.

The first entry in the picker is `burrow-cloud.dev`, the managed product. Selecting it prints a short
code and opens your browser on an approval page; check the code there matches the one in your
terminal, approve, and you are signed in. Two credentials are issued, yours and `burrow-agent`'s,
written to files only you can read (`~/.burrow/credentials/` and `~/.burrow/agents/`) and never
displayed. From then on the application commands — `burrow app list`, `deploy`, `status`, `logs` and
the rest — act against your Burrow Cloud tenant, with no cluster and no kubeconfig involved.
Everything below needs none of that — the self-hosted path needs no account.

## Part 2 - Connect your agent

Your AI agent drives Burrow through `burrow-agent`, a single scoped binary already on your PATH
(installed alongside `burrow` in step 1). It is capability-reduced — it carries the safe
operate-verbs (deploy, status, logs, rollback, scale, and their read-only siblings) and holds no
cluster credentials — so pointing an agent at it is safe. Connecting your agent means writing its
permission rules so it may run `burrow-agent` but not the human `burrow` admin CLI, which is why
the two are separate binaries.

Preview what will be written first with `burrow agent <tool>`, then apply it with
`burrow agent <tool> install`. The change is idempotent, and any file Burrow edits is backed up first.

| Agent | Command | How it is wired |
|-------|---------|-----------------|
| Claude Code | `burrow agent claude install` | writes the allow/deny permission rules and a burrow-agent orientation into `~/.claude` |
| Any other agent | `burrow agent <tool>` | prints the exact rules to set by hand: allow `burrow-agent`, deny `burrow` |

After it is wired, restart your agent so it picks up the new permissions.

### Do not see your agent?

`burrow-agent` is a single binary on the agent's PATH, so any agent that can run a command can use
it. Wire another agent by hand: in its permission config, allow `Bash(burrow-agent *)` and deny
`Bash(burrow *)`, so it may run the scoped binary but not the human `burrow` admin CLI. If you would
like first-class `burrow agent` support for your agent, please open an issue to request it:
[github.com/burrow-cloud/burrow/issues/new](https://github.com/burrow-cloud/burrow/issues/new).

## First use

Open your agent and ask it to deploy something. For example:

> "Deploy ghcr.io/me/app:1.4 and serve it at example.com over HTTPS."

Your agent calls Burrow, Burrow runs the deploy on your cluster under the guardrails you control,
and it reports back what happened.

Tag each image with an incrementing version (for example `v0.1.0`, then `v0.1.1`) and never reuse a
tag, so every deploy is a distinct artifact and rollbacks stay clean.

### Private registries

If the image lives in a private registry, give the cluster credentials to pull it before you
deploy. Use a dedicated, long-lived Personal Access Token with the `read:packages` scope
([create one here](https://github.com/settings/tokens/new?scopes=read:packages)):

```sh
burrow config registry login ghcr.io -u <github-username>
```

Give it the username and it prompts for the token with the input hidden, so the token never lands
in your shell history or the process table. The prompt also links you to the right page to create
a token for your registry. For automation, pipe the token in with `--password-stdin` instead:

```sh
echo "$TOKEN" | burrow config registry login ghcr.io -u <github-username> --password-stdin
```

Make the token long-lived. Burrow stores it as-is in your cluster and does not refresh it, so an
ephemeral or CI token (such as an Actions `GITHUB_TOKEN`) will break future pulls once it expires.

This is a one-time credential step you run yourself at your terminal. The credential is stored in
your cluster and never travels over the agent control channel, so the agent cannot do it for you. Without it, a private
image lands in `ImagePullBackOff`, and `burrow status` (or the agent's status check) reports the
missing registry and this exact fix.

## Upgrade

Burrow installs two binaries: `burrow`, the CLI you run, and `burrow-agent`, the one your coding
agent runs. Homebrew updates both together:

```sh
brew update && brew upgrade burrow-cloud/tap/burrow
```

**Name the formula in full.** Burrow ships from its own tap, and Homebrew's core repository has an
unrelated formula called `burrow` — a Kafka consumer-lag checker. A bare `brew upgrade burrow`
resolves to that one, and it does not fail: Homebrew **uninstalls the Burrow CLI and installs the
other project in its place**, reporting it as an upgrade.

```
==> Upgraded 1 outdated package
burrow 0.14.0-rc.11 -> 1.9.6
```

If that has already happened, `brew uninstall burrow && brew install burrow-cloud/tap/burrow` puts
it back. A plain `brew upgrade` with no arguments is safe — Homebrew records which tap a formula
came from and stays on it. It is naming `burrow` on its own that is ambiguous.

Then **restart your agent session** (for example `claude --resume`). A running session keeps
executing the `burrow-agent` it started with, so it does not pick up the new one until it restarts.

To roll the in-cluster Burrow forward after a new release:

```sh
burrow cluster upgrade
```

`burrow version` shows all three at once, so you can see if any of them has fallen behind:

```
burrow (CLI):     v0.13.0
burrow-agent:     v0.13.0 (/opt/homebrew/bin/burrow-agent)
control plane:    v0.13.0 (context "prod", namespace "burrow")
```

If you installed `burrow-agent` from source rather than with Homebrew, the `brew upgrade` above will
not replace it; update it with `go install github.com/burrow-cloud/burrow/cmd/burrow-agent@<version>`.

`burrow cluster upgrade` updates the installed control plane in place and preserves your state.

**Upgrade one minor version at a time.** The control plane's database moves forward by exactly one
minor step per upgrade (`v0.12 → v0.13`); a wider jump, a downgrade, and a cross-major in-place move
are refused at startup with an error naming the version to install first
([ADR-0013](adr/0013-database-migrations-and-upgrade-policy.md)). If you are several minors behind,
install each intervening minor in turn. Doing that in one step is decided but not yet built
([ADR-0055](adr/0055-multi-version-upgrades.md), Proposed).

Keep the CLI within one minor of the control plane, too: burrowd serves any client within one minor
and tells you which side to upgrade otherwise
([ADR-0039](adr/0039-cli-control-plane-version-skew.md)).
