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

Run `burrow install` with no argument to list the contexts in your kubeconfig, then install into
the one you want:

```sh
burrow install                 # lists your contexts (and which already run Burrow)
burrow install <context>       # installs into the context you name
```

Naming the context is required, so Burrow never installs into the wrong cluster by accident. The
install creates the control plane in the `burrow` namespace and deploys your apps into the
`burrow-apps` namespace. On success it names and records the environment as your current one, and
tells you it is ready.

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

To update the CLI:

```sh
brew update && brew upgrade burrow
```

To roll the in-cluster Burrow forward after a new release:

```sh
burrow upgrade
```

`burrow upgrade` updates the installed control plane in place and preserves your state.
