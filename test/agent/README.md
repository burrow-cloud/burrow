# Agent diagnosis test (local, manual, costs API tokens)

`diagnose.sh` is an end-to-end proof that a **headless Claude agent can diagnose a failing
application from Burrow's aggregated logs** — the agent-native loop Burrow exists for. It is
run **by hand**, never in CI, because it spends Anthropic API tokens.

## What it proves

End to end, with no human in the loop after launch:

1. Stand up a disposable k3d cluster and install Burrow into it (ko-built `burrowd` image,
   over the Kubernetes API-server proxy — the same setup the capstone
   `test/e2e/install-and-deploy.sh` uses).
2. `burrow addon install logs` — VictoriaLogs store + Fluent Bit collector.
3. Deploy a deliberately-failing `checkout` app that logs an unambiguous root cause
   (`FATAL: checkout cannot connect to database at db.default.svc.cluster.local:5432:
   connection refused`) and CrashLoopBackOffs, so the line is re-emitted continuously.
4. Confirm the failure line is actually **queryable back through Burrow**
   (`burrow addon logs FATAL`) before involving the agent — otherwise the agent test would
   be meaningless.
5. Run `claude -p` pointed at the **burrow MCP server** (`bin/burrow-mcp`, auto-connecting via
   `BURROW_KUBECONFIG`), allowed only the read-only burrow tools, and assert that the agent
   **both** (a) issued a `mcp__burrow__burrow_logs_query` tool call **and** (b) named the root
   cause in its final answer.

## Prerequisites

- **`claude`** — the Claude Code CLI — and **`ANTHROPIC_API_KEY`** exported in the
  environment (this is what costs tokens).
- **docker**, **k3d**, **ko** (`brew install ko`), **kubectl**, **jq**, **go**.

The script checks all of these up front and prints exactly what is missing.

## Run it

```sh
ANTHROPIC_API_KEY=sk-ant-... bash test/agent/diagnose.sh
```

On success it prints the agent's final answer and `AGENT DIAGNOSIS PASSED`. On failure it
prints the agent output tail and a cluster-state diagnostics dump, then exits non-zero.

### Debugging

```sh
KEEP=1 ANTHROPIC_API_KEY=sk-ant-... bash test/agent/diagnose.sh
```

`KEEP=1` leaves the k3d cluster and the temp work dir (kubeconfig, MCP config, and the raw
`agent-out.jsonl` stream) in place so you can inspect them. The cluster name defaults to
`burrow-agent-diag` and is overridable with `K3D_CLUSTER`.

## Note on the assertion

The agent is an LLM, so its **exact wording varies** between runs. The root-cause assertion
therefore checks for the underlying **concepts** — case-insensitive, the answer must mention
`database` **and** (`connection refused` **or** `5432`) — not a fixed string. The tool-use
assertion checks that a `burrow_logs_query` call appears in the stream, proving the agent
actually consulted Burrow's logs rather than guessing.

## Not wired into CI

This test is intentionally **not** in `.github/workflows` or the `Taskfile`. It costs money
to run and depends on a live model; run it deliberately when you want to validate the
agent-diagnosis loop against real infrastructure.
