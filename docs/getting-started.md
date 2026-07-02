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

This installs two binaries: `burrow` (the CLI) and `burrow-mcp` (the MCP server your agent talks
to).

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

Burrow is driven by your AI agent over MCP: the agent talks to `burrow-mcp`, a stdio server that
uses your kubeconfig and active environment, so there is nothing extra to configure. Add Burrow to
your agent with one command.

Preview what will be added first with `burrow mcp <tool>`, then apply it with
`burrow mcp <tool> install`. The change is idempotent, and any file Burrow edits is backed up first.

| Agent | Command | How it is configured |
|-------|---------|----------------------|
| Claude Code | `burrow mcp claude install` | its own `claude mcp add` |
| Codex | `burrow mcp codex install` | its own `codex mcp add` |
| Copilot | `burrow mcp copilot install` | its own `copilot mcp add` |
| Cursor | `burrow mcp cursor install` | edits `~/.cursor/mcp.json` |
| OpenCode | `burrow mcp opencode install` | edits `~/.config/opencode/opencode.json` |
| Aider | not supported | Aider has no MCP support; use an MCP bridge pointed at `burrow-mcp` |
| Any other agent | `burrow mcp` | add `burrow-mcp` (stdio) to that agent's MCP config |

After it is added, restart your agent (or reload its MCP servers) so it picks up Burrow.

### Do not see your agent?

`burrow-mcp` is a plain stdio server that any MCP-capable tool can use, so you can add it to any
agent's MCP config by hand. If you would like first-class `burrow mcp` support for your agent,
please open an issue to request it:
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
burrow config registry login ghcr.io -u <github-username> -p <read:packages-PAT>
```

Make the token long-lived. Burrow stores it as-is in your cluster and does not refresh it, so an
ephemeral or CI token (such as an Actions `GITHUB_TOKEN`) will break future pulls once it expires.

This is a one-time credential step you run yourself at your terminal. The credential is stored in
your cluster and never travels over MCP, so the agent cannot do it for you. Without it, a private
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
