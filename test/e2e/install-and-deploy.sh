#!/usr/bin/env bash
# Capstone end-to-end test: build the burrowd image, install Burrow into a k3d cluster,
# and deploy an app through the CLI — auto-connecting via the developer's kubeconfig and
# the Kubernetes API-server proxy (no port-forward). This exercises the entire stack the
# way a user would.
#
# Expects a running k3d cluster named $K3D_CLUSTER, plus kubectl, docker, go, and ko
# (https://ko.build — `brew install ko`), which builds the burrowd image.
set -euo pipefail

CLUSTER="${K3D_CLUSTER:-burrow-ci}"
KCFG=$(k3d kubeconfig write "$CLUSTER")
export KUBECONFIG="$KCFG"

# Failure handling separates a flake from a real bug. on_err (the ERR trap) prints the actual
# failing command FIRST, so the cause is not buried under the cluster dump, then classifies by
# phase: a failure during infra setup (cluster and control-plane bring-up) is most likely transient
# and exits 75 (safe to rerun); a failure once the test body is running is a real defect and exits 1
# (do not blindly rerun). dump_diagnostics prints pod status plus the control plane's describe and
# logs (including a crashed container's previous logs).
TEMPFAIL=75
PHASE=setup

dump_diagnostics() {
  echo "=== DIAGNOSTICS: dumping cluster state ==="
  kubectl get pods -A -o wide || true
  echo "--- burrow namespace events ---"
  kubectl -n burrow get events --sort-by=.lastTimestamp | tail -n 30 || true
  echo "--- postgres logs ---"
  kubectl -n burrow logs deploy/postgres --tail=40 || true
  echo "--- burrowd describe ---"
  kubectl -n burrow describe deploy/burrowd || true
  echo "--- burrowd logs (current) ---"
  kubectl -n burrow logs deploy/burrowd --tail=80 || true
  echo "--- burrowd logs (previous, if it restarted) ---"
  kubectl -n burrow logs deploy/burrowd --previous --tail=80 || true
  echo "--- burrow-addons namespace (logs add-on lives here) ---"
  kubectl -n burrow-addons get all || true
  echo "--- burrow-logs store logs ---"
  kubectl -n burrow-addons logs deploy/burrow-logs --tail=40 || true
  echo "--- fluent-bit collector logs ---"
  kubectl -n burrow-addons logs ds/burrow-logs-collector --tail=40 || true
  echo "--- burrow-metrics store logs ---"
  kubectl -n burrow-addons logs deploy/burrow-metrics --tail=40 || true
  echo "--- vmagent collector logs ---"
  kubectl -n burrow-addons logs deploy/burrow-metrics-collector --tail=40 || true
  echo "--- metricsapp pods (app deployed with --metrics-port) ---"
  kubectl -n burrow-apps get pods -l app.kubernetes.io/name=metricsapp -o wide || true
  echo "--- burrow-e2e-loki namespace (connected Loki fixture) ---"
  kubectl -n burrow-e2e-loki get all || true
  echo "--- loki fixture logs ---"
  kubectl -n burrow-e2e-loki logs deploy/loki --tail=60 || true
  echo "--- burrow-e2e-prom namespace (connected Prometheus fixture) ---"
  kubectl -n burrow-e2e-prom get all || true
  echo "--- prometheus fixture logs ---"
  kubectl -n burrow-e2e-prom logs deploy/prometheus --tail=60 || true
}
# on_err runs on any set -e command failure. It is passed the failing command's exit code, the line,
# and the command itself (captured before they change), so the log's first line is the real cause.
on_err() {
  local rc=$1 line=$2 cmd=$3
  echo "=== STEP FAILED (exit ${rc}): ${cmd}  [line ${line}, phase ${PHASE}] ==="
  dump_diagnostics
  if [ "$PHASE" = setup ]; then
    echo "=== classified: infra/setup failure, most likely transient: exiting ${TEMPFAIL} (safe to rerun) ==="
    exit "$TEMPFAIL"
  fi
  echo "=== classified: failure in the test body, treat as a real defect: exiting 1 (do not blindly rerun) ==="
  exit 1
}
trap 'on_err "$?" "$LINENO" "$BASH_COMMAND"' ERR

WORK=$(mktemp -d)
BURROW="$WORK/burrow"
echo "=== build the burrow CLI ==="
go build -o "$BURROW" ./cmd/burrow

echo "=== build + import the burrowd image (ko) ==="
# ko builds the Go binary on the host (reusing the build cache warm from the test runs above,
# instead of recompiling client-go inside a docker build) and loads it into the local docker
# daemon. KO_DOCKER_REPO=ko.local routes the load to the daemon; capture the exact image ref
# ko prints to stdout and use it for the import and the install.
BURROWD_IMAGE=$(KO_DOCKER_REPO=ko.local ko build ./cmd/burrowd)
k3d image import "$BURROWD_IMAGE" -c "$CLUSTER"

echo "=== burrow install (waits for the control plane to be ready) ==="
# install takes the target kube context as a required positional argument (ADR-0037): derive it
# from the kubeconfig so install targets this k3d cluster explicitly (a bare `burrow install` now
# only lists contexts).
CTX=$(kubectl --kubeconfig "$KCFG" config current-context)
"$BURROW" install "$CTX" --burrowd-image "$BURROWD_IMAGE" --kubeconfig "$KCFG"

# Past control-plane bring-up: failures from here are real defects in the stack under test, not the
# infra flakes that setup failures usually are. Classify accordingly (exit 1, not the rerunnable 75).
PHASE=test

echo "=== burrow app deploy (auto-connect: kubeconfig + API-server proxy, no port-forward) ==="
"$BURROW" app deploy web --image nginx:alpine --kubeconfig "$KCFG"

echo "=== wait for the app to become available ==="
ok=
for _ in $(seq 1 45); do
  status_out=$("$BURROW" app status web --kubeconfig "$KCFG")
  if grep -q "ready, available" <<<"$status_out"; then
    ok=1
    break
  fi
  sleep 4
done

echo "--- final status ---"
"$BURROW" app status web --kubeconfig "$KCFG"
if [ -z "$ok" ]; then
  echo "FAIL: app never became available"
  exit 1 # the ERR trap dumps diagnostics
fi

echo "=== rollback path: deploy a second image, then roll back ==="
"$BURROW" app deploy web --image nginx:1.27-alpine --kubeconfig "$KCFG"
rollback_out=$("$BURROW" app rollback web --kubeconfig "$KCFG")
grep -q "nginx:alpine" <<<"$rollback_out" || { echo "FAIL: rollback did not restore nginx:alpine"; exit 1; }

# =============================================================================
# AUDIT: the append-only record of guarded operations (ADR-0027)
# By now several guarded operations have run through the full stack (deploys and a
# rollback). Assert `burrow audit` lists them through CLI -> API -> in-cluster burrowd ->
# Postgres. Deterministic: the rows already exist (no async pipeline), so no polling.
# =============================================================================
echo "=== audit: the guarded operations above are recorded ==="
audit_out=$("$BURROW" audit --app web --kubeconfig "$KCFG")
printf '%s\n' "$audit_out"
# A deploy that executed and the rollback that executed must both appear for app web.
grep -q "deploy" <<<"$audit_out" \
  || { echo "FAIL: 'burrow audit' did not list a deploy operation for web"; exit 1; }
grep -q "rollback" <<<"$audit_out" \
  || { echo "FAIL: 'burrow audit' did not list the rollback operation for web"; exit 1; }
grep -q "executed" <<<"$audit_out" \
  || { echo "FAIL: 'burrow audit' showed no executed outcome for web"; exit 1; }
# The outcome filter narrows the read path server-side.
filtered_audit=$("$BURROW" audit --app web --operation rollback --outcome executed --kubeconfig "$KCFG")
grep -q "rollback" <<<"$filtered_audit" || { echo "FAIL: filtered 'burrow audit' did not return the rollback row"; exit 1; }
echo "--- audit trail lists the guarded operations through CLI -> API -> burrowd -> Postgres ---"

# =============================================================================
# ENV: non-secret config lifecycle (ADR-0028)
# Exercise the real env path end-to-end through the full stack:
#   burrow CLI -> control-plane API -> in-cluster burrowd -> Postgres store,
#   then rendered inline into the Deployment pod template, which rolls the workload.
# Two assertions, both deterministic and bounded:
#   1. `app config list` round-trips the value through CLI -> API -> burrowd -> Postgres.
#   2. a default (restarting) `app config set` mutates the live Deployment pod template, so
#      the var reaches the container — read straight off the Deployment, no log timing.
# =============================================================================
echo "=== env: set a variable on the running app (rolls the Deployment) ==="
"$BURROW" app config set web BURROW_E2E_ENV=hello-from-store --kubeconfig "$KCFG"

echo "=== config: assert the value round-trips through app config list ==="
config_list_out=$("$BURROW" app config list web --kubeconfig "$KCFG")
grep -qx "BURROW_E2E_ENV=hello-from-store" <<<"$config_list_out" \
  || { echo "FAIL: 'app config list' did not return the set variable"; exit 1; }

echo "=== env: assert the value reached the live Deployment pod template ==="
# A default `config set` re-applies the workload, so the value is rendered inline into the
# pod template's container env. Read it back off the Deployment deterministically (no log
# scraping, no timing): the rollout having been requested is enough for the spec to carry it.
env_in_template=$(kubectl --kubeconfig "$KCFG" -n burrow-apps get deploy/web \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="BURROW_E2E_ENV")].value}')
if [ "$env_in_template" != "hello-from-store" ]; then
  echo "FAIL: BURROW_E2E_ENV not rendered into the Deployment pod template (got: '$env_in_template')"
  exit 1
fi
echo "--- env rendered into the pod template: BURROW_E2E_ENV=$env_in_template ---"

echo "=== env: unset removes the variable from the store ==="
"$BURROW" app config unset web BURROW_E2E_ENV --no-restart --kubeconfig "$KCFG"
config_list_after=$("$BURROW" app config list web --kubeconfig "$KCFG")
if grep -q "BURROW_E2E_ENV" <<<"$config_list_after"; then
  echo "FAIL: BURROW_E2E_ENV still present after unset"
  exit 1
fi
echo "--- env unset removed the variable from the store ---"

# =============================================================================
# SECRET: secret config lifecycle (ADR-0029 + ADR-0004)
# The whole group goes through burrowd's authenticated control-plane API; what differs is whether
# a VALUE is carried:
#   - `secret set` carries the value -> it travels over the authenticated control-plane API and
#     burrowd writes it to the per-app Secret. The value is never carried over MCP, never logged,
#     and never stored in the database (ADR-0029).
#   - `secret list` (KEYS only) and `secret unset` (by KEY) carry no value -> also MCP-allowed.
# Assertions, all deterministic and bounded, read straight off the live cluster:
#   1. the value lands in the per-app Secret (kubectl reads burrow-app-web-secrets);
#   2. the Deployment pod template carries the envFrom secretRef so the value is injected;
#   3. `secret list` prints the KEY (APP_SECRET) but NEVER the value (s3cr3t);
#   4. `secret unset` removes the key from the Secret.
# =============================================================================
echo "=== secret: set a secret on the running app (via burrowd, rolls the Deployment) ==="
"$BURROW" app secret set web APP_SECRET=s3cr3t --kubeconfig "$KCFG"

echo "=== secret: assert the value landed in the per-app Kubernetes Secret ==="
# The value lives ONLY in burrow-app-web-secrets in the app namespace — burrowd received it over
# the control-plane API and wrote it here. Read it back and base64-decode deterministically.
secret_val=$(kubectl --kubeconfig "$KCFG" -n burrow-apps get secret burrow-app-web-secrets \
  -o jsonpath='{.data.APP_SECRET}' | base64 -d)
if [ "$secret_val" != "s3cr3t" ]; then
  echo "FAIL: APP_SECRET not stored in burrow-app-web-secrets (got: '$secret_val')"
  exit 1
fi
echo "--- secret value stored in the per-app Secret ---"

echo "=== secret: assert the Deployment injects the Secret via envFrom ==="
# Every workload sources the per-app Secret with an optional envFrom secretRef, so each key
# becomes an env var. Assert the reference is present on the pod template (read off the live
# Deployment — no log timing).
env_from=$(kubectl --kubeconfig "$KCFG" -n burrow-apps get deploy/web \
  -o jsonpath='{.spec.template.spec.containers[0].envFrom[?(@.secretRef.name=="burrow-app-web-secrets")].secretRef.name}')
if [ "$env_from" != "burrow-app-web-secrets" ]; then
  echo "FAIL: Deployment pod template missing envFrom secretRef burrow-app-web-secrets (got: '$env_from')"
  exit 1
fi
echo "--- Deployment injects the per-app Secret via envFrom ---"

echo "=== secret: list shows the KEY but never the value ==="
secret_list=$("$BURROW" app secret list web --kubeconfig "$KCFG")
if ! grep -qx "APP_SECRET" <<<"$secret_list"; then
  echo "FAIL: 'app secret list' did not show the key APP_SECRET"
  printf '%s\n' "$secret_list"
  exit 1
fi
if grep -q "s3cr3t" <<<"$secret_list"; then
  echo "FAIL: 'app secret list' leaked the secret VALUE"
  exit 1
fi
echo "--- secret list shows APP_SECRET (the key) and not the value ---"

echo "=== secret: unset removes the key from the Secret ==="
"$BURROW" app secret unset web APP_SECRET --no-restart --kubeconfig "$KCFG"
secret_list_after=$("$BURROW" app secret list web --kubeconfig "$KCFG")
if grep -q "APP_SECRET" <<<"$secret_list_after"; then
  echo "FAIL: APP_SECRET still present after unset"
  exit 1
fi
echo "--- secret unset removed the key ---"

# =============================================================================
# ADDON: logs pipeline
# Exercise the REAL production logs path end-to-end, reusing the already-installed
# control plane, $BURROW, and $KCFG above (no re-install):
#   burrow CLI -> control-plane API -> in-cluster burrowd -> Fluent Bit collector
#   -> VictoriaLogs store -> query back via `burrow addon logs`.
# The query MUST go through the in-cluster burrowd (the test host cannot resolve
# in-cluster Service DNS), so this is the only faithful way to verify the round trip.
# =============================================================================

echo "=== addon install logs (VictoriaLogs store + Fluent Bit collector) ==="
# --confirm bypasses the addon_install guardrail (confirm-by-default) so the run is
# non-interactive; the flag maps to the engine's confirm bool.
"$BURROW" addon install logs --confirm --kubeconfig "$KCFG"

echo "=== wait for the logs store to become ready ==="
# Add-ons live in the burrow-addons namespace; the store Deployment is named burrow-logs.
# rollout status blocks until the store is available (it is the readiness signal `addon
# list` reports as Ready); on timeout it exits non-zero and the ERR trap dumps state.
kubectl --kubeconfig "$KCFG" -n burrow-addons rollout status deploy/burrow-logs --timeout=120s

echo "--- installed add-ons ---"
"$BURROW" addon list --kubeconfig "$KCFG"

echo "=== deploy a logger fixture THROUGH Burrow (exercises the -- command override) ==="
# Deploy a busybox app whose command is overridden via the new `--` passthrough, so it is a
# real Burrow-managed workload rather than raw kubectl. It emits an error-shaped line carrying
# the BURROW_E2E_LOGLINE marker every 2s; Fluent Bit tails the node's container logs and ships
# them into VictoriaLogs. The single quotes keep the loop's $i from expanding in this shell so
# busybox evaluates it at runtime.
"$BURROW" app deploy burrow-e2e-logger --image busybox:1.36 --kubeconfig "$KCFG" -- \
  sh -c 'i=0; while true; do echo "BURROW_E2E_LOGLINE level=error iteration=$i app failed to connect"; i=$((i+1)); sleep 2; done'
kubectl --kubeconfig "$KCFG" -n burrow-apps rollout status deploy/burrow-e2e-logger --timeout=60s

echo "=== query the marker back through burrow addon logs (bounded poll) ==="
# Bounded poll (~90s) to cover Fluent Bit's tail+flush latency into VictoriaLogs. The
# query is the marker itself; assert the round-tripped output contains it.
found=
last_out=
for _ in $(seq 1 18); do
  last_out=$("$BURROW" addon logs 'BURROW_E2E_LOGLINE' --kubeconfig "$KCFG" 2>&1 || true)
  if grep -q "BURROW_E2E_LOGLINE" <<<"$last_out"; then
    found=1
    break
  fi
  sleep 5
done

if [ -z "$found" ]; then
  echo "FAIL: marker BURROW_E2E_LOGLINE never appeared via 'burrow addon logs'"
  echo "--- last query output ---"
  printf '%s\n' "$last_out"
  exit 1 # the ERR trap dumps diagnostics
fi
echo "--- marker round-tripped through the logs pipeline ---"
printf '%s\n' "$last_out" | grep "BURROW_E2E_LOGLINE" | head -n 3 || true

echo "=== tidy up the logs add-on and the logger fixture (best-effort) ==="
# Cleanup is non-fatal — the cluster is deleted after the run regardless.
"$BURROW" addon remove burrow-logs --confirm --kubeconfig "$KCFG" || true
kubectl --kubeconfig "$KCFG" -n burrow-apps delete deploy/burrow-e2e-logger --ignore-not-found || true

# =============================================================================
# ADDON: metrics pipeline
# Exercise the REAL production metrics path end-to-end, reusing the already-installed
# control plane, $BURROW, and $KCFG above (no re-install):
#   burrow CLI -> control-plane API -> in-cluster burrowd -> vmagent collector
#   -> VictoriaMetrics store -> query back via `burrow addon metrics`.
# vmagent self-scrapes (job="vmagent"), so up{job="vmagent"} 1 is guaranteed once it runs,
# with no app fixture required. The query MUST go through the in-cluster burrowd (the test
# host cannot resolve in-cluster Service DNS), so this is the only faithful round trip.
# =============================================================================

echo "=== addon install metrics (VictoriaMetrics store + vmagent collector) ==="
"$BURROW" addon install metrics --confirm --kubeconfig "$KCFG"

echo "=== wait for the metrics store and the vmagent collector to become ready ==="
# The store Deployment is burrow-metrics; the vmagent collector is burrow-metrics-collector.
# Both must roll out before the self-scrape series can appear.
kubectl --kubeconfig "$KCFG" -n burrow-addons rollout status deploy/burrow-metrics --timeout=120s
kubectl --kubeconfig "$KCFG" -n burrow-addons rollout status deploy/burrow-metrics-collector --timeout=120s

echo "--- installed add-ons ---"
"$BURROW" addon list --kubeconfig "$KCFG"

echo "=== query the vmagent self-scrape back through burrow addon metrics (bounded poll) ==="
# Bounded poll (~90s) to cover vmagent's first scrape + remote-write into the store. `up` is
# an instant PromQL query; vmagent self-scrapes localhost:8429, so up{job="vmagent"} 1 appears
# once the sample lands. Assert the round-tripped output names the vmagent job.
found=
last_out=
for _ in $(seq 1 18); do
  last_out=$("$BURROW" addon metrics 'up' --kubeconfig "$KCFG" 2>&1 || true)
  if grep -q 'job="vmagent"' <<<"$last_out"; then
    found=1
    break
  fi
  sleep 5
done

if [ -z "$found" ]; then
  echo "FAIL: up{job=\"vmagent\"} never appeared via 'burrow addon metrics'"
  echo "--- last query output ---"
  printf '%s\n' "$last_out"
  exit 1 # the ERR trap dumps diagnostics
fi
echo "--- vmagent self-scrape round-tripped through the metrics pipeline ---"
printf '%s\n' "$last_out" | grep 'job="vmagent"' | head -n 3 || true

echo "=== deploy a real metrics-exposing app THROUGH Burrow (--metrics-port) ==="
# The --metrics-port flag annotates the pod with prometheus.io/scrape=true, port, and
# path=/metrics, so vmagent's pod-discovery scrape config picks it up automatically. We use
# prom/prometheus only as a convenient app that serves its own /metrics on :9090 (its baked-in
# default config) — NOT as Prometheus. The image is preloaded in CI.
"$BURROW" app deploy metricsapp --image prom/prometheus:v3.1.0 --metrics-port 9090 --kubeconfig "$KCFG"
kubectl --kubeconfig "$KCFG" -n burrow-apps rollout status deploy/metricsapp --timeout=120s

echo "=== query the app's OWN metrics back through burrow addon metrics (bounded poll) ==="
# Proves the FULL LOOP: an app deployed with --metrics-port is auto-discovered and scraped by
# vmagent — its own metrics are queryable through `burrow addon metrics`. vmagent's relabel
# maps __meta_kubernetes_pod_name to a `pod` label, so the scraped target appears as
# up{pod="metricsapp-...",...}. A value of 1 means the scrape of /metrics on :9090 succeeded.
# The human output renders each sample as `{k="v",...}  <value>` (metricLabels in addon.go).
app_found=
app_out=
for _ in $(seq 1 18); do
  app_out=$("$BURROW" addon metrics 'up{pod=~"metricsapp.*"}' --kubeconfig "$KCFG" 2>&1 || true)
  if grep -q 'pod="metricsapp' <<<"$app_out"; then
    app_found=1
    break
  fi
  sleep 5
done

if [ -z "$app_found" ]; then
  echo "FAIL: up{pod=\"metricsapp...\"} never appeared via 'burrow addon metrics' — the app deployed with --metrics-port was not discovered/scraped"
  echo "--- last query output ---"
  printf '%s\n' "$app_out"
  exit 1 # the ERR trap dumps diagnostics
fi
echo "--- the app's own metrics round-tripped: an app deployed with --metrics-port is auto-discovered and scraped, and its metrics are queryable ---"
printf '%s\n' "$app_out" | grep 'pod="metricsapp' | head -n 3 || true

echo "=== tidy up the metrics-exposing app (best-effort) ==="
# `app delete` may not exist on this branch, so tear the Deployment down with kubectl.
kubectl --kubeconfig "$KCFG" -n burrow-apps delete deploy/metricsapp --ignore-not-found || true

echo "=== tidy up the metrics add-on (best-effort) ==="
"$BURROW" addon remove burrow-metrics --confirm --kubeconfig "$KCFG" || true

# =============================================================================
# ADDON: cache (ValKey)
# A backing service the app connects to (not one the agent queries): install it and prove it
# is reachable in-cluster with a valkey-cli PING. The generic add-on path handles it — no
# collector, no persistent volume (a cache is rebuildable).
# =============================================================================
echo "=== addon install cache (ValKey) ==="
"$BURROW" addon install cache --confirm --kubeconfig "$KCFG"
kubectl --kubeconfig "$KCFG" -n burrow-addons rollout status deploy/burrow-cache --timeout=120s
echo "--- installed add-ons (should show cache, mode installed) ---"
"$BURROW" addon list --kubeconfig "$KCFG"

echo "=== PING the cache from inside the cluster (proves it is reachable) ==="
# The test host cannot resolve in-cluster Service DNS, so PING from a one-shot in-cluster pod.
cache_out=$(kubectl --kubeconfig "$KCFG" -n burrow-addons run cache-ping \
  --image=valkey/valkey:8.0 --restart=Never --attach --rm -q -- \
  valkey-cli -h burrow-cache.burrow-addons.svc -p 6379 ping 2>&1 || true)
echo "$cache_out"
if ! grep -q "PONG" <<<"$cache_out"; then
  echo "FAIL: the cache did not answer PING with PONG"
  exit 1 # the ERR trap dumps diagnostics
fi
echo "--- cache answered PONG ---"

echo "=== tidy up the cache add-on (best-effort) ==="
"$BURROW" addon remove burrow-cache --confirm --kubeconfig "$KCFG" || true

# =============================================================================
# ADDON: connect Loki
# Exercise the CONNECT path (an existing backend the user already runs) end-to-end:
#   burrow CLI -> control-plane API -> in-cluster burrowd -> Loki query API.
# This runs AFTER the logs-pipeline cleanup above: the installed burrow-logs add-on is
# gone, so it no longer shadows the connected loki — `burrow addon logs` picks the first
# logs-capable add-on by name, and a leftover burrow-logs would be queried instead.
# The query MUST go through in-cluster burrowd (the test host cannot resolve in-cluster
# Service DNS), and the seed is pushed from inside the cluster for the same reason.
# =============================================================================

echo "=== deploy a minimal single-binary Loki fixture (burrow-e2e-loki namespace) ==="
# Monolithic Loki with filesystem storage and an in-memory ring (replication_factor 1) —
# enough to accept a push and answer a query_range. The tsdb/filesystem schema uses v13
# from an old date so any seeded line falls inside the active schema period.
kubectl --kubeconfig "$KCFG" apply -f - <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  name: burrow-e2e-loki
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: loki
  namespace: burrow-e2e-loki
data:
  loki.yaml: |
    auth_enabled: false
    server:
      http_listen_port: 3100
    common:
      path_prefix: /loki
      storage:
        filesystem:
          chunks_directory: /loki/chunks
          rules_directory: /loki/rules
      replication_factor: 1
      ring:
        kvstore:
          store: inmemory
    schema_config:
      configs:
        - from: 2020-01-01
          store: tsdb
          object_store: filesystem
          schema: v13
          index:
            prefix: index_
            period: 24h
    limits_config:
      allow_structured_metadata: true
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: loki
  namespace: burrow-e2e-loki
spec:
  replicas: 1
  selector:
    matchLabels:
      app: loki
  template:
    metadata:
      labels:
        app: loki
    spec:
      containers:
        - name: loki
          image: grafana/loki:3.2.1
          args:
            - -config.file=/etc/loki/loki.yaml
          ports:
            - containerPort: 3100
          volumeMounts:
            - name: config
              mountPath: /etc/loki
            - name: data
              mountPath: /loki
      volumes:
        - name: config
          configMap:
            name: loki
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: loki
  namespace: burrow-e2e-loki
spec:
  selector:
    app: loki
  ports:
    - port: 3100
      targetPort: 3100
YAML
kubectl --kubeconfig "$KCFG" -n burrow-e2e-loki rollout status deploy/loki --timeout=120s

echo "=== seed a known log line into Loki from inside the cluster ==="
# The test host cannot reach the Loki Service, so push from a one-shot in-cluster pod. The
# nanosecond timestamp is computed INSIDE the pod (cluster-now), so the line falls within the
# adapter's 1h lookback window. Loki may not accept pushes the instant it reports ready, so
# retry until it returns HTTP 204. --restart=Never --attach --rm surfaces the pod's exit code.
kubectl --kubeconfig "$KCFG" -n burrow-e2e-loki run loki-seed \
  --image=curlimages/curl:8.11.1 --restart=Never --attach --rm -q -- \
  sh -c '
    for i in $(seq 1 20); do
      # busybox `date` (the curl image) has no %N, so build nanoseconds as seconds * 1e9
      # (append nine zeros). A seconds value sent where Loki expects nanoseconds lands in
      # 1970 and is rejected as "timestamp too old"; recompute each attempt so it stays now.
      ts="$(date +%s)000000000"
      payload="{\"streams\":[{\"stream\":{\"app\":\"burrow-e2e\",\"job\":\"burrow-e2e\"},\"values\":[[\"$ts\",\"BURROW_E2E_LOKI_MARKER level=error checkout handler panicked\"]]}]}"
      code=$(curl -s -o /dev/null -w "%{http_code}" -XPOST \
        -H "Content-Type: application/json" \
        "http://loki.burrow-e2e-loki.svc:3100/loki/api/v1/push" \
        --data-raw "$payload")
      echo "push attempt $i -> HTTP $code"
      if [ "$code" = "204" ]; then echo "seed accepted"; exit 0; fi
      sleep 3
    done
    echo "seed never accepted"; exit 1
  '

echo "=== connect the existing Loki (unauthenticated; no --auth) ==="
"$BURROW" addon connect loki --endpoint loki.burrow-e2e-loki.svc:3100 --kubeconfig "$KCFG"

echo "--- registered add-ons (should show loki, mode connected) ---"
"$BURROW" addon list --kubeconfig "$KCFG"

echo "=== query the marker back through burrow addon logs (bounded poll) ==="
# `burrow addon logs <arg>` passes the arg straight through as the LogQL query to Loki's
# query_range. A bare word is NOT a valid LogQL query (Loki requires a stream selector), so
# use a selector + line filter that matches the seeded stream, and assert on the marker text.
loki_found=
loki_out=
for _ in $(seq 1 18); do
  loki_out=$("$BURROW" addon logs '{job="burrow-e2e"} |= "BURROW_E2E_LOKI_MARKER"' --kubeconfig "$KCFG" 2>&1 || true)
  if grep -q "BURROW_E2E_LOKI_MARKER" <<<"$loki_out"; then
    loki_found=1
    break
  fi
  sleep 5
done

if [ -z "$loki_found" ]; then
  echo "FAIL: marker BURROW_E2E_LOKI_MARKER never appeared via 'burrow addon logs' against the connected Loki"
  echo "--- last query output ---"
  printf '%s\n' "$loki_out"
  exit 1 # the ERR trap dumps diagnostics
fi
echo "--- marker round-tripped through the connected Loki ---"
printf '%s\n' "$loki_out" | grep "BURROW_E2E_LOKI_MARKER" | head -n 3 || true

echo "=== tidy up the connected Loki (best-effort) ==="
# Removing a connected add-on still goes through addon_remove, which is confirm-by-default,
# so pass --confirm. Cleanup is non-fatal — the cluster is deleted after the run regardless.
"$BURROW" addon remove loki --confirm --kubeconfig "$KCFG" || true
kubectl --kubeconfig "$KCFG" delete ns burrow-e2e-loki --ignore-not-found || true

# =============================================================================
# ADDON: connect Prometheus
# Exercise the metrics CONNECT path end-to-end against an existing store the user runs:
#   burrow CLI -> control-plane API -> in-cluster burrowd -> Prometheus /api/v1/query.
# Simpler than the Loki connect above: Prometheus self-scrapes, so there is nothing to
# seed — the `up` series for its own target appears a couple of scrape intervals after the
# pod is ready. The query MUST go through in-cluster burrowd (the test host cannot resolve
# in-cluster Service DNS), so this is the only faithful way to verify the round trip.
# =============================================================================

echo "=== deploy a minimal self-scraping Prometheus fixture (burrow-e2e-prom namespace) ==="
# Prometheus configured to scrape only itself (localhost:9090) every 5s. That single target
# guarantees an `up{job="prometheus"}` series with value 1 once the first scrape lands.
kubectl --kubeconfig "$KCFG" apply -f - <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  name: burrow-e2e-prom
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: prometheus-config
  namespace: burrow-e2e-prom
data:
  prometheus.yml: |
    global:
      scrape_interval: 5s
    scrape_configs:
      - job_name: prometheus
        static_configs:
          - targets: ['localhost:9090']
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: prometheus
  namespace: burrow-e2e-prom
spec:
  replicas: 1
  selector:
    matchLabels:
      app: prometheus
  template:
    metadata:
      labels:
        app: prometheus
    spec:
      containers:
        - name: prometheus
          image: prom/prometheus:v3.1.0
          args:
            - --config.file=/etc/prometheus/prometheus.yml
            - --storage.tsdb.path=/prometheus
          ports:
            - containerPort: 9090
          volumeMounts:
            - name: config
              mountPath: /etc/prometheus
            - name: data
              mountPath: /prometheus
      volumes:
        - name: config
          configMap:
            name: prometheus-config
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: prometheus
  namespace: burrow-e2e-prom
spec:
  selector:
    app: prometheus
  ports:
    - port: 9090
      targetPort: 9090
YAML
kubectl --kubeconfig "$KCFG" -n burrow-e2e-prom rollout status deploy/prometheus --timeout=120s

echo "=== connect the existing Prometheus (unauthenticated; no --auth) ==="
"$BURROW" addon connect prometheus --endpoint prometheus.burrow-e2e-prom.svc:9090 --kubeconfig "$KCFG"

echo "--- registered add-ons (should show prometheus, mode connected) ---"
"$BURROW" addon list --kubeconfig "$KCFG"

echo "=== query the self-scrape target back through burrow addon metrics (bounded poll) ==="
# `burrow addon metrics <query>` runs an instant PromQL query; `up` always exists for a
# self-scraping target. The human output renders each sample as `{k="v",...}  <value>`
# (metricLabels in cmd/burrow/addon.go), so the up series prints with job="prometheus" and a
# trailing value of 1 once the first scrape lands (~2 scrape intervals after ready). Bounded
# poll (~90s) to cover that initial scrape latency; assert on the job label.
prom_found=
prom_out=
for _ in $(seq 1 18); do
  prom_out=$("$BURROW" addon metrics 'up' --kubeconfig "$KCFG" 2>&1 || true)
  if grep -q 'job="prometheus"' <<<"$prom_out"; then
    prom_found=1
    break
  fi
  sleep 5
done

if [ -z "$prom_found" ]; then
  echo "FAIL: up{job=\"prometheus\"} never appeared via 'burrow addon metrics' against the connected Prometheus"
  echo "--- last query output ---"
  printf '%s\n' "$prom_out"
  exit 1 # the ERR trap dumps diagnostics
fi
echo "--- up series round-tripped through the connected Prometheus ---"
printf '%s\n' "$prom_out" | grep 'job="prometheus"' | head -n 3 || true

echo "=== tidy up the connected Prometheus (best-effort) ==="
# Removing a connected add-on still goes through addon_remove, which is confirm-by-default,
# so pass --confirm. Cleanup is non-fatal — the cluster is deleted after the run regardless.
"$BURROW" addon remove prometheus --confirm --kubeconfig "$KCFG" || true
kubectl --kubeconfig "$KCFG" delete ns burrow-e2e-prom --ignore-not-found || true

echo "=== CAPSTONE E2E PASSED: install -> deploy -> status -> rollback -> logs pipeline -> connect Loki -> connect Prometheus, all via the CLI over the proxy ==="
