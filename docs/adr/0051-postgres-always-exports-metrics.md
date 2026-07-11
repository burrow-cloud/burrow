# ADR-0051: Postgres always exports metrics; the scraper discovers the add-on namespace

## Status

✅ Accepted

## TL;DR

The Postgres add-on always runs a small `postgres_exporter` sidecar that exposes Prometheus
metrics — including `pg_stat_statements` slow-query stats — on `:9187`, and the metrics add-on's
vmagent now discovers pods in both the app namespace and the add-on namespace. So once a user
installs the metrics add-on, their database is scraped automatically and the agent can rule the
database in or out when an app is slow — no matter whether the metrics add-on was installed before
or after Postgres.

Builds on the Postgres add-on ([ADR-0031](0031-postgres-addon.md)) and the metrics/scrape model
([ADR-0025](0025-building-block-addons.md), [ADR-0026](0026-observability-query-adapters.md));
reuses the superuser-secret discipline of [ADR-0031](0031-postgres-addon.md) for the exporter's
credential. Supersedes nothing.

## Context

A database is the most common reason a "why is my app slow?" investigation ends somewhere other
than the app code: a missing index, a lock, a slow query. To answer that, the agent needs database
metrics — connection counts, transaction rates, and per-statement timing from
`pg_stat_statements`. Today the metrics add-on (VictoriaMetrics + a vmagent scraper) discovers only
app pods in the app namespace; nothing exports or scrapes the shared Postgres instance, which runs
in the separate add-on namespace ([ADR-0025](0025-building-block-addons.md)).

Two forces shape the fix:

1. **Install order must not matter.** A user may install Postgres first and metrics later, or the
   reverse. Metrics discovery is dynamic (the Kubernetes API, re-read continuously), so if Postgres
   is annotated for scraping and vmagent watches its namespace, the samples flow whichever add-on
   landed first. A one-time wire-up at install time would be order-sensitive and would miss the
   common "add metrics after the fact" case.
2. **The exporter is cheap; the store is not.** Running an exporter sidecar costs a few MiB of RAM
   and produces metrics nobody stores until a metrics backend exists. Storing and retaining metrics
   (a VictoriaMetrics instance and its volume) is the real cost, and that stays opt-in. So the
   exporter is always-on and the store remains a deliberate `addon install metrics`.

## Decision

### 1. The Postgres add-on always exports metrics

Every Postgres add-on pod runs a second container, a pinned `postgres_exporter`
(`quay.io/prometheuscommunity/postgres-exporter`, Apache-2.0), listening on `:9187`. It connects to
the co-located server over the pod loopback as the Burrow superuser; its password is sourced by
`secretKeyRef` from the `burrow-postgres` Secret and is never inlined into the pod spec
([ADR-0031](0031-postgres-addon.md)). Its footprint is tiny (32Mi request / 64Mi limit), so the
always-on cost is negligible.

The exporter enables the `stat_statements` collector. For that data to exist, Postgres itself runs
with `shared_preload_libraries=pg_stat_statements` (a server-start setting), and the extension is
created on first boot by an init script mounted into the official image's
`/docker-entrypoint-initdb.d` (`CREATE EXTENSION IF NOT EXISTS pg_stat_statements`).

The pod carries the standard scrape annotations (`prometheus.io/scrape: "true"`,
`prometheus.io/port: "9187"`, `prometheus.io/path: "/metrics"`) — the same contract app pods use —
so any Prometheus-style scraper that discovers the pod picks it up.

### 2. The scraper discovers the add-on namespace, dynamically

vmagent's pod discovery now lists both the app namespace and the add-on namespace, deduped to a
single entry when the two are the same. Discovery is namespace-based and continuously re-read, so
install order does not matter: the exporter is scraped as soon as both the annotated Postgres pod
and a running vmagent exist.

### 3. RBAC extends to the add-on namespace

vmagent's least-privilege pod-discovery grant (staged kubeconfig-side at
`burrow addon install metrics`, because burrowd cannot create RBAC) gains a Role and RoleBinding in
the add-on namespace in addition to the existing app-namespace pair. When the app and add-on
namespaces are the same, only the app-namespace pair is emitted — it already covers the add-on
namespace, and two identically-named Roles in one namespace would collide on apply.

## Consequences

- Installing the metrics add-on now yields database metrics automatically, with no extra step and
  no attach-time wiring; the agent can query CPU/connection/transaction health and the slowest
  statements to rule the database in or out.
- The always-on exporter adds a small, bounded resource cost to every Postgres add-on, whether or
  not a metrics store is ever installed. The metrics *store* stays opt-in.
- The metrics RBAC grant widens by one namespace (still read-only pod discovery, nothing more).
- **Known limitation:** the add-on install path is create-only (an already-existing resource is a
  no-op), so an **existing** Postgres add-on does not gain the exporter until it is reinstalled;
  new installs get it. An in-place upgrade of running add-ons is out of scope here.

## Rejected alternatives

- **Wire scraping at attach/install time instead of by discovery.** Order-sensitive and brittle: it
  would miss the common "install metrics after the database" case and require re-running on every
  add-on change. Namespace discovery is dynamic and self-healing.
- **Make the exporter opt-in.** It is cheap enough that always-on removes a step and a failure mode
  (metrics installed but the database silently unscraped). The expensive part — storing metrics —
  is what stays opt-in.
- **A separate exporter Deployment rather than a sidecar.** A sidecar shares the pod's loopback (no
  cross-pod network or extra Service), ties the exporter's lifecycle to the database it observes,
  and reuses the same superuser Secret already mounted in that namespace.
- **Grant vmagent cluster-wide pod discovery.** Wider than needed. Two namespaced Roles keep the
  grant least-privilege and legible.
