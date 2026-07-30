# ADR-0076: Health checks — readiness only, and dependencies checked at deploy time

## Status

✅ Accepted

## TL;DR

Burrow sets no probe on a user application, so Kubernetes marks a pod ready the moment its container
starts. **An app that boots, binds its port, and returns 500 to every request is a successful deploy.**
[ADR-0072](0072-deploy-and-run-lifecycle-hooks.md) §7 named this gap; this record closes it.

- **Readiness only. Burrow never sets a liveness probe by default.** Readiness removes a pod from
  service; liveness *restarts the container*, so a wrong liveness probe manufactures the crashloop it
  was meant to detect.
- **A readiness probe never checks an external dependency.** If every app's readiness tested the shared
  database, one database blip would take every app out of service at once — converting a degraded
  dependency into a total outage.
- **Dependency checks — "can I reach my database, can I write to my volume" — run once at deploy
  time**, not continuously. Burrow can *derive* them from what it provisioned, with no user input.
- **The default probe is conservative**: an app whose port Burrow knows gets a TCP check; an app whose
  port it does not gets none. A probe Burrow guessed wrong is worse than no probe.
- **An explicit endpoint is better, and the agent is told why.** Burrow states the benefit on its
  surface so an agent can implement `/healthz` in the user's code — the answer for a user who does not
  want to learn what a readiness probe is.

**The limit:** only the app can know it is *healthy*. Burrow can prove the port is open and the
dependencies it provisioned are reachable. Everything past that needs the app's cooperation, and §5 is
how a user gets there without needing to understand any of this.

Closes [ADR-0072](0072-deploy-and-run-lifecycle-hooks.md) §7's gap. Uses
[ADR-0072](0072-deploy-and-run-lifecycle-hooks.md) §4's `post-deploy` phase as the deploy-time
mechanism. Follows [ADR-0065](0065-what-belongs-on-the-agent-surface.md) for what the agent is told.
Supersedes nothing.

## Context

### What exists today

- **No probe on a user application.** `ReadinessProbe` appears once in the tree, on the add-on path.
  Nothing sets one on an app's Deployment.
- **So "available" means "running".** `deploymentRolledOut` requires `AvailableReplicas >= desired`,
  and without a probe Kubernetes counts a pod available as soon as its container starts and stays up.
- **Burrow knows the port only sometimes.** `ExposeSpec.Port` is the container port an exposure routes
  to, so it exists for published apps; a deployed-but-unpublished app has no port recorded.
  `MetricsPort` is separate and optional.
- **Burrow does know its own dependencies.** It provisioned the Postgres add-on and wrote the
  connection string into the app's Secret; it mounted the volume. What it gave the app is in the
  registry.
- **ADR-0072 §7 states the consequence** and declines to fix it there: a `post-deploy` hook is told the
  deploy *happened*, and has to decide for itself whether it *worked*.

### What breaks

**A broken deploy reports success.** The most common shape of a bad release is not a pod that fails to
start — it is a pod that starts and cannot serve: a missing environment variable, a database it cannot
reach, a config file it cannot parse. Every one of those produces a running container, an available
replica, and a green deploy.

**Rollback has nothing to trigger on.** `burrow rollback` exists, and the signal that would justify
using it — "the new version is not serving" — is precisely the signal Burrow does not have.

**And the traffic switch is unguarded.** An exposure routes to a Service whose endpoints are ready
pods. With no probe, a new pod receives traffic the instant it starts, before it has connected to
anything. During a rollout that means requests are served by a replica that is not able to serve them.

### What this record resolves

Which probes Burrow sets, what they are allowed to test, where dependency checks belong instead, and
how a user who does not want to think about any of this ends up with a good one.

## Decision

### 1. Readiness only; liveness is never set by default

A **readiness** probe controls whether a pod receives traffic. A failing one removes the pod from the
Service and nothing else — a recoverable, reversible state.

A **liveness** probe restarts the container. A liveness probe that is wrong — too aggressive a timeout,
a dependency hiccup, a slow start under load — kills a working process, repeatedly, and presents as
`CrashLoopBackOff`. It manufactures the exact failure it was installed to detect, and it does so under
load, which is when it is least welcome.

So Burrow sets readiness and does not set liveness. A user who wants one may configure it explicitly
and should be told what it does; Burrow will not choose it for them.

**Startup probes are the exception worth allowing later**, because they exist to stop a slow-booting
app being killed by a liveness probe — a problem this record avoids by not setting liveness at all.

### 2. A readiness probe never checks an external dependency

This is the rule most likely to be broken by someone trying to be helpful, and it is the one with the
largest blast radius.

If an app's readiness probe checks the database, then when the database is briefly unavailable **every
replica of every app that checks it fails readiness at the same moment**. Kubernetes removes them all
from their Services. A dependency that was degraded — slow, or down for ten seconds — becomes a total
outage across every app, and recovery is slower than the original blip because everything must pass
readiness again at once.

The failure mode is worse on this product than on most, because the database is *shared*: a single
Postgres instance backs many apps, so the correlation is total rather than partial.

A readiness probe answers **"can this pod serve?"** — a property of the pod. It must not answer "is the
system healthy", which is a property of everything.

### 3. The default is conservative, and absent where it would be a guess

- **Port known** (the app is published, so `ExposeSpec.Port` exists) → a **TCP** readiness check
  against that port. It proves the process bound the socket, which is strictly more than today's
  "the container did not exit".
- **Port unknown** → **no probe**. Behaviour is exactly as it is now.

Burrow does not scan for a listening port, does not assume 8080, and does not guess a path like
`/healthz`. **A probe Burrow invented is worse than no probe**: it fails a working deploy, and the user
cannot tell whether their app is broken or Burrow's guess is. Today's failure is silent success; the
failure introduced by guessing is loud and wrong, which is not obviously the better trade and is
certainly not one to make on the user's behalf.

An HTTP path is used **only when the user or their agent supplies one** (§5).

### 4. Dependency checks run at deploy time, not continuously

The checks the maintainer actually wants — *can I connect to the database, can I create and read a file
in my volume* — are real and valuable. They are **verification**, not readiness, and the distinction is
§2's: run continuously they amplify failures; run once at deploy they catch exactly the misconfiguration
that makes a deploy silently bad.

So they run as a **deploy-time check**, on ADR-0072 §4's `post-deploy` phase, and Burrow supplies a
**default** derived from what it provisioned:

- an attached Postgres add-on → connect using the app's own `DATABASE_URL`, run `SELECT 1`
- a mounted volume → create, read back, and delete a file under it
- a published exposure → request the app's port and report the status code

**Derived, not configured.** Burrow provisioned these things and recorded them; it does not need to ask
the user what their app depends on to check the things it gave them. This is the part no generic
platform can do — a PaaS that did not attach your database cannot test your database.

**It runs from inside the app's container**, using the app's environment and credentials, because a
check run from anywhere else proves the *cluster* can reach the database and not that the *app* can —
and the difference is exactly where misconfiguration lives.

A failed dependency check does **not** roll back and does not fail the deploy retroactively. It is
reported, with the specific failure, through the same surface as any other deploy outcome. ADR-0072 §6
already decided that Burrow does not roll back by itself.

### 5. The agent is told why an explicit endpoint is better, and can implement it

The best readiness signal is an endpoint the application serves, because only the application knows
whether it can do its job. Burrow cannot write one. An agent with the user's source **can**.

So Burrow's surface states the case where the agent will meet it: that an app with no health endpoint
is one whose broken deploys look successful, that adding one is usually a few lines, and what it should
and should not check — **its own readiness to serve, not the health of its dependencies** (§2, which an
agent will otherwise get wrong, because checking the database from `/healthz` is the internet's most
common example).

This is the answer for a non-technical user. They do not need to learn what a readiness probe is; they
need their agent to know it matters, why, and what a good one looks like. That is ADR-0065's tier 2 —
a capability the agent exercises on the user's behalf, with the reasoning attached rather than assumed.

Once an endpoint exists, `burrow app health set <app> --path /healthz [--port N]` records it and the
probe becomes an HTTP check. Unset returns to §3's default.

### 6. A wrong probe must fail toward deployed

Every default here errs toward *not* failing a deploy: no liveness, no guessed path, no probe where the
port is unknown, dependency failures reported rather than blocking.

That asymmetry is deliberate. A probe that wrongly reports healthy costs a user a bad release they can
roll back. A probe that wrongly reports unhealthy costs them the ability to deploy at all, during an
incident, with no obvious cause — and they will disable health checking entirely rather than debug it,
which loses the feature permanently.

## Consequences

- **A published app gets a real readiness signal for free**, and traffic stops being routed to a pod
  that has not yet bound its port.
- **Deploy-time dependency checks catch the common broken deploy** — wrong credentials, unreachable
  database, unwritable volume — and name it, rather than leaving a green deploy and a confused user.
- **An unpublished app gains nothing by default.** That is honest but uneven, and the fix is knowing
  the port, which means either asking or extending the deploy spec — a separate decision.
- **Burrow needs a way to run a check inside the app's container** for §4, and the app's image may
  contain no shell, no `psql`, no `curl`. Injecting a small static probe binary via an init container
  is the likely mechanism and it is real work; the alternative — requiring the image to carry tools —
  fails on exactly the minimal images users are told to build.
- **`post-deploy` gains a Burrow-supplied default**, so a hook the user never configured can still run.
  That is new behaviour on a path ADR-0072 described as user-configured, and it should be visible and
  disableable rather than silent.
- **The agent may add a health endpoint that checks the database**, because that is the most common
  example on the internet and §2 says it is wrong. The guidance has to be explicit about it, and it
  will still happen sometimes.
- **Health checking becomes a thing users can get wrong**, where today there is nothing to configure.
  §6 is the mitigation and it is a posture, not a guarantee.

## Rejected alternatives

- **Set a liveness probe too.** Standard practice in most Kubernetes guides, and it restarts a hung
  process without human involvement. Rejected in §1: the failure mode is a working app restarted in a
  loop, indistinguishable from the crashloop it was meant to catch, and most often triggered under the
  load that made the process briefly slow.
- **Let the readiness probe check the database.** The intuitive reading of "is my app healthy", and
  what the maintainer's framing initially suggests. Rejected in §2 as a correlated-failure amplifier:
  one shared database blip would remove every app from service simultaneously. The check is valuable —
  it belongs at deploy time, where §4 puts it.
- **Default to an HTTP GET on `/healthz` and port 8080.** Covers a large fraction of real apps with no
  configuration. Rejected in §3: for every app it fits, it breaks one it does not, and the breakage is
  a deploy that fails for a reason the user cannot see. A default that is wrong 20% of the time is not
  a default, it is a trap.
- **Probe by scanning for a listening port.** Removes the "port unknown" gap. Rejected because the port
  a process happens to bind is not necessarily the one that serves — a metrics port, a debug listener,
  or a sidecar would all satisfy it — so it manufactures confidence rather than measuring anything.
- **Require every app to declare a health endpoint.** The strongest signal, uniformly. Rejected because
  it makes health checking a precondition of deploying, which is the opposite of the on-ramp this
  product is for, and because an off-the-shelf image the user did not write cannot comply.
- **Run dependency checks continuously as a separate non-blocking monitor**, rather than at deploy.
  Appealing — it would catch a database that broke *after* deploy. Rejected here as
  [ADR-0074](0074-burrow-observes-what-it-manages.md)'s job rather than this record's: continuous
  observation of what Burrow manages is exactly the ledger, and duplicating it in the health path would
  put the same fact in two places with two retention policies.
- **Have Burrow write the health endpoint into the user's code.** It has the repository during a
  source build. Rejected as a boundary this project does not cross ([ADR-0004](0004-code-never-over-mcp.md)
  is the neighbouring principle): Burrow deploys code, it does not author it. The agent authors code;
  Burrow tells the agent why it should.
