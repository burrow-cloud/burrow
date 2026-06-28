# crashloop-after-deploy — ANSWER KEY (don't show your agent)

> This file is the grader's view. It names the planted bug and the expected fix. The agent
> works one level down in [`workspace/`](workspace/) and is given only
> [`workspace/TICKET.md`](workspace/TICKET.md) — the symptom, like a real support ticket. Keep
> this README out of the agent's context so the test is honest.

## The scenario

`checkout` was running a healthy release (`nginx:alpine`). Then a bad release shipped over it:
`busybox:1.36` with a startup command that fails immediately —

```
FATAL: checkout: config migration v2 failed: unknown column "region" — aborting startup
```

— so the new pod exits non-zero on boot and lands in **CrashLoopBackOff**. The app is down,
and it went down "right after the latest deploy." Burrow still holds the previous, healthy
release.

## The planted bug

The **latest release is broken**; the previous one was fine. There is nothing to fix in any
file — the failure only exists in the *running* release, so the agent has to use Burrow's
tools to see it, not read the workspace.

## Expected diagnosis path

A good run looks like:

1. `burrow_status` (or `burrow_apps`) on `checkout` → workload not available, pod
   CrashLoopBackOff.
2. `burrow_logs` for `checkout` → the `FATAL: ... config migration v2 failed ...` line,
   re-emitted every restart.
3. Conclude: **the most recent deploy is bad** (it was healthy before today's release).

## Expected fix

**Roll back to the previous release** — one Burrow operation (`burrow_rollback` /
`burrow rollback checkout`). That restores `nginx:alpine`, which becomes available again.

A redeploy of a known-good image is also a legitimate fix and passes the grader; rollback is
the cleanest and the one the ticket's "since the last deploy" framing points to.

## Grading

`verify.sh` is outcome-based: it passes when `checkout` is **available** again. It then notes
whether the serving image is `nginx:alpine` (the rollback path) or something else (a redeploy)
— informational, not part of the pass gate.

```sh
bash setup.sh            # plant the broken state
# ... run your agent in workspace/ ...
bash verify.sh           # grade
```
