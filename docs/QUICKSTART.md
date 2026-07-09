# Quickstart: your agent deploys an app to a local cluster

This walkthrough takes you from nothing to *an AI agent deployed an app to a Kubernetes cluster
and got blocked trying to delete it* — on your laptop, no cloud, in about 5–10 minutes. It runs
against a disposable [k3d](https://k3d.io) cluster, so you can throw it away when you are done.

You will do it twice: first the **fast path** yourself through the `burrow` CLI to see the whole
loop, then **the real thing** — hand the same job to your coding agent and watch a guardrail stop
it from deleting the app without your say-so.

> Everything below was run verbatim against a real k3d cluster. If a command does not behave as
> shown, that is a bug — please open an issue.

## Prerequisites

- **Docker** — running. k3d runs the cluster in Docker, and you build the sample app image with it.
- **[k3d](https://k3d.io)** — `brew install k3d` (a lightweight k3s-in-Docker Kubernetes).
- **[kubectl](https://kubernetes.io/docs/tasks/tools/)** — to reach the app in your browser.
- **The `burrow` and `burrow-agent` binaries** on your `PATH`. Either:
  - install the released binaries (`brew install burrow-cloud/tap/burrow`), or
  - build them from a checkout: `go build -o burrow ./cmd/burrow` and
    `go build -o burrow-agent ./cmd/burrow-agent`, then move both onto your `PATH`.

`burrow` is your admin CLI; `burrow-agent` is the scoped binary your coding agent drives. You do
**not** need to build the control plane — `burrow install` pulls the published `burrowd` image.

This walkthrough deploys the sample app in this repo, so clone it and work from its root (if you
built the binaries from source you already have it):

```sh
git clone https://github.com/burrow-cloud/burrow
cd burrow
```

Check your tools:

```sh
docker info >/dev/null && k3d version && kubectl version --client && burrow version
```

## Fast path (CLI, ~3 minutes, no agent)

### 1. Create a local cluster

```sh
k3d cluster create burrow-quickstart
```

This creates a one-node Kubernetes cluster and adds a `k3d-burrow-quickstart` context to your
kubeconfig.

### 2. Install Burrow into it

```sh
burrow install k3d-burrow-quickstart
```

Burrow deploys its control plane (the published `burrowd` image plus an in-cluster Postgres),
waits for it to become ready, and records this cluster as your current environment. You will see:

```
Waiting for Burrow to become ready...
  database ... ✓
  control plane ... ✓

Burrow is installed and ready in namespace "burrow".
```

### 3. Build the sample app and load it into the cluster

The sample app lives at [`examples/hello`](../examples/hello) — a tiny Go HTTP server that
returns a "Hello from Burrow" page. Build it and import the image into k3d so the cluster can run
it without a registry:

```sh
docker build -t hello:1 examples/hello
k3d image import hello:1 -c burrow-quickstart
```

> **Code never travels through Burrow.** You build the image with your own tooling and put it
> where the cluster can pull it; Burrow only ever deploys *by image reference*. `k3d image
> import` is the local stand-in for pushing to a registry.

### 4. Deploy it

```sh
burrow app deploy hello --image hello:1
```

```
deployed hello as release 35241b6b... (image hello:1, 1 replica(s), deployed)
```

### 5. Check its status

```sh
burrow app status hello
```

```
app: hello
release: 35241b6b... (image hello:1, deployed)
workload: 1/1 replicas ready, available
```

### 6. Reach it in your browser

The app is running in the cluster. Forward its port to your laptop with `kubectl` — the fewest
moving parts for a local cluster (no ingress controller or DNS needed):

```sh
kubectl -n burrow-apps port-forward deploy/hello 8080:8080
```

Leave that running and open **http://localhost:8080** — you will see the *Hello from Burrow* page,
served by the pod. (Press Ctrl-C to stop forwarding when you are done.)

### 7. Try to delete it — and meet the guardrail

```sh
burrow app delete hello
```

```
burrow: control plane: guardrail holds app delete for confirmation: deleting the app "hello"
(its workload, routing, and release history) requires confirmation to proceed (app.delete,
http 422) — re-run with --confirm to proceed
```

**This is the point of Burrow.** Deleting an app is destructive, so the control plane *holds* the
operation instead of running it. `burrow app status hello` confirms the app is still there. Only
an explicit confirm goes through:

```sh
burrow app delete hello --confirm
```

## The real thing (the agent path)

The fast path was you driving. Now hand the job to your coding agent and watch the same guardrail
stop *it* — the moment Burrow was built for.

### 1. Wire your agent to Burrow

```sh
burrow agent claude install
```

This writes two permission rules for Claude Code: **allow** `burrow-agent` (the scoped control
channel) and **deny** the `burrow` admin CLI — so the agent operates your cluster only through
Burrow's guarded path, never around it. It also drops a short orientation block into
`~/.claude/CLAUDE.md` so the agent knows how to use `burrow-agent`. (Preview it first with
`burrow agent claude` — nothing is written until you add `install`.)

### 2. Launch your agent in the sample workspace

```sh
cd examples/hello/workspace
claude
```

The workspace holds a one-line ticket ([`TICKET.md`](../examples/hello/workspace/TICKET.md)) for
the agent. The `hello:1` image you imported earlier is already in the cluster.

### 3. Ask it to deploy

> **deploy this app and show me its status**

The agent runs `burrow-agent deploy hello --image hello:1` and `burrow-agent status hello` (each
command returns JSON it reasons over), then reports the release is up and running.

### 4. Ask it to delete it

> **now delete the hello app**

The agent runs `burrow-agent delete hello` and gets back, instead of a deletion:

```json
{
  "outcome": "held_for_confirmation",
  "operation": "delete",
  "code": "app.delete",
  "message": "guardrail holds app delete for confirmation: deleting the app \"hello\" ... requires confirmation to proceed",
  "confirm_required": true,
  "hint": "relay this to the human; re-run with --confirm ONLY after they approve. Never self-confirm."
}
```

**This is the hero moment.** The agent cannot delete your app on its own. The control plane holds
the destructive operation and tells the agent to come back to you; a well-behaved agent relays
the hold and asks for your approval rather than self-confirming. You stay in the loop for the
operations that matter, while the agent handles everything else.

## Clean up

```sh
k3d cluster delete burrow-quickstart
```

That deletes the whole cluster — Burrow, the app, and all of it — in one go.

## Where to go next

- [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) — how the four layers fit together and why code never
  crosses the control channel.
- [`examples/`](../examples) — richer "operate a real cluster with your agent" scenarios, like
  recovering an app that is crash-looping after a bad deploy.
- [`docs/adr/0049-burrow-agent-scoped-cli-control-channel.md`](adr/0049-burrow-agent-scoped-cli-control-channel.md)
  — the design of the scoped `burrow-agent` control channel the agent path uses.
