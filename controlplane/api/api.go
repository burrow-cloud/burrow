// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package api is the control plane's HTTP front end: it exposes the deploy engine's
// operations over JSON and authenticates its callers with a bearer token (ADR-0005).
// It is a thin transport adapter — it decodes requests, calls the engine, and maps the
// engine's typed outcomes to HTTP status codes; the orchestration and guardrails live
// in the engine (ADR-0006). The `burrow` CLI and `burrow-agent` are both clients of this API.
//
// It lives under controlplane/ (not controlplane/internal) so cmd/burrowd and the
// managed module can wire it; it is licensed Apache-2.0.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// Config configures the API handler.
type Config struct {
	// Engine is the deploy engine the API fronts. Required.
	Engine *controlplane.Engine
	// Token is the bearer token clients must present on every /v1 request
	// (ADR-0005). Required — the control plane authenticates its callers.
	Token string
	// Version is burrowd's own release version, the compatibility anchor for the client-version
	// handshake (ADR-0039): a client more than one minor behind is refused with an actionable error,
	// and an unknown route reports this version so a newer client learns to upgrade the control
	// plane. Optional — empty (a local or e2e build) makes the handshake permissive.
	Version string
}

// New builds the control-plane HTTP handler. The /v1 routes require the bearer token;
// /healthz is unauthenticated for liveness probes.
func New(cfg Config) (http.Handler, error) {
	if cfg.Engine == nil {
		return nil, fmt.Errorf("api: New: Engine is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("api: New: Token is required (the control plane authenticates its clients)")
	}
	s := &server{engine: cfg.Engine}

	v1 := http.NewServeMux()
	v1.HandleFunc("GET /v1/apps", s.listApps)
	v1.HandleFunc("DELETE /v1/apps/{app}", s.deleteApp)
	v1.HandleFunc("POST /v1/apps/{app}/deploy", s.deploy)
	// build clones a git source and builds the image inside the cluster, then hands the result into
	// the same guarded deploy path (ADR-0053): only the git ref crosses, never source bytes.
	v1.HandleFunc("POST /v1/apps/{app}/build", s.build)
	v1.HandleFunc("GET /v1/apps/{app}/status", s.status)
	// history is the read-only deploy timeline: the releases recorded for an app, newest first
	// (ADR-0007). The optional env query validates a named environment; empty is the default.
	v1.HandleFunc("GET /v1/apps/{app}/history", s.history)
	v1.HandleFunc("GET /v1/apps/{app}/logs", s.logs)
	v1.HandleFunc("POST /v1/apps/{app}/rollback", s.rollback)
	// auto-deploy: GET reads an app's per-environment auto-deploy level, PUT sets it (ADR-0052 §6).
	// Setting is a human operator action, so it is exposed on this admin API but never as an agent
	// verb — burrow-agent may read the level but not change it.
	v1.HandleFunc("GET /v1/apps/{app}/auto-deploy", s.getAutoDeploy)
	v1.HandleFunc("PUT /v1/apps/{app}/auto-deploy", s.setAutoDeploy)
	// health: GET reads the declared health endpoint and the readiness probe Burrow authors from it,
	// PUT declares one, DELETE returns the app to the conservative default (ADR-0076 §3, §5).
	v1.HandleFunc("GET /v1/apps/{app}/health", s.getHealth)
	v1.HandleFunc("PUT /v1/apps/{app}/health", s.setHealth)
	v1.HandleFunc("DELETE /v1/apps/{app}/health", s.unsetHealth)
	// checks: GET reports the deploy-time dependency check — whether it runs, and what Burrow
	// derived from what it provisioned that it would check (ADR-0076 §4). PUT turns it on or off.
	// The read is on the agent surface: it names key names and in-cluster addresses, never a value.
	// The write is an operator action, like the auto-deploy level and a lifecycle hook: it decides
	// whether Burrow keeps verifying what it handed the app, so it lives on this admin API only.
	v1.HandleFunc("GET /v1/apps/{app}/checks", s.getChecks)
	v1.HandleFunc("PUT /v1/apps/{app}/checks", s.setChecks)
	// next-tag suggests the app's next semver release tags from its current running tag (ADR-0052 §8).
	// It is read-only guidance the agent applies to its own build; there is no mutating counterpart.
	v1.HandleFunc("GET /v1/apps/{app}/next-tag", s.nextTag)
	v1.HandleFunc("POST /v1/apps/{app}/scale", s.scale)
	// run executes a one-off command in the app's own current image and environment (ADR-0048).
	v1.HandleFunc("POST /v1/apps/{app}/run", s.run)
	// Lifecycle hooks: the command an app runs at a named phase (ADR-0072 §1). One mechanism with the
	// phase in the path, so a further phase is a new value rather than a new route. Setting one is an
	// operator action — a pre-deploy hook runs a command on every deploy of the app, which is the
	// blast radius of configuration set once and forgotten — so, like the auto-deploy level, the write
	// lives on this admin API and carries no `burrow-agent` verb.
	v1.HandleFunc("GET /v1/apps/{app}/hooks", s.listHooks)
	v1.HandleFunc("PUT /v1/apps/{app}/hooks/{phase}", s.setHook)
	v1.HandleFunc("DELETE /v1/apps/{app}/hooks/{phase}", s.unsetHook)
	// autoscale applies (POST) or removes (DELETE) an app's HorizontalPodAutoscaler (ADR-0006).
	v1.HandleFunc("POST /v1/apps/{app}/autoscale", s.autoscale)
	v1.HandleFunc("DELETE /v1/apps/{app}/autoscale", s.disableAutoscale)
	v1.HandleFunc("POST /v1/apps/{app}/expose", s.expose)
	v1.HandleFunc("POST /v1/apps/{app}/unexpose", s.unexpose)
	v1.HandleFunc("GET /v1/apps/{app}/reachability", s.reachability)
	v1.HandleFunc("GET /v1/apps/{app}/config", s.listConfig)
	v1.HandleFunc("POST /v1/apps/{app}/config", s.setConfig)
	v1.HandleFunc("DELETE /v1/apps/{app}/config/{key}", s.unsetConfig)
	// Secrets: set carries a VALUE in its POST body, list returns KEYS only, unset removes a key.
	// set is the ONE secret endpoint that carries a value — it travels over this authenticated,
	// TLS-protected API and burrowd writes it to the per-app Kubernetes Secret (ADR-0029). The
	// value is never logged (the access log records method+path+status only; the path holds no
	// value), never audited, never stored in Postgres, and never reachable from the agent control
	// channel — `burrow-agent` has no secret-set command (ADR-0029/0004). list and unset carry no value.
	v1.HandleFunc("POST /v1/apps/{app}/secrets", s.setSecret)
	v1.HandleFunc("GET /v1/apps/{app}/secrets", s.listSecrets)
	v1.HandleFunc("DELETE /v1/apps/{app}/secrets/{key}", s.unsetSecret)
	v1.HandleFunc("GET /v1/guard", s.guardList)
	v1.HandleFunc("PUT /v1/guard/{code}", s.guardSet)
	// The operational limits (ADR-0068). The write is reachable only from the operator CLI: the
	// agent binary carries no `cluster config` verb at all, which is what the surface guard asserts
	// — a bound the agent can raise is not a bound (ADR-0068 §4).
	v1.HandleFunc("GET /v1/config", s.limitsList)
	v1.HandleFunc("PUT /v1/config/{code}", s.limitSet)
	v1.HandleFunc("POST /v1/providers", s.addProvider)
	v1.HandleFunc("GET /v1/providers", s.listProviders)
	v1.HandleFunc("POST /v1/domains", s.addDomain)
	v1.HandleFunc("DELETE /v1/domains/{host}", s.removeDomain)
	v1.HandleFunc("POST /v1/addons", s.installAddon)
	v1.HandleFunc("POST /v1/addons/connect", s.connectAddon)
	// attach/detach give an app its own database on the installed Postgres add-on (ADR-0031).
	// attach carries NO secret value — burrowd generates the DATABASE_URL server-side and writes it
	// to the app's Secret; the response carries the key name only. detach is held by a confirm
	// guardrail (it drops data).
	v1.HandleFunc("POST /v1/addons/attach", s.attachAddon)
	v1.HandleFunc("POST /v1/addons/detach", s.detachAddon)
	// backup/backups/restore manage per-app Postgres backups (ADR-0032). backup and the backups
	// listing move no secret value (an in-cluster Job does the dump). restore is held by a confirm
	// guardrail (it overwrites the live database).
	v1.HandleFunc("POST /v1/addons/backup", s.backupAddon)
	v1.HandleFunc("GET /v1/addons/backups", s.listBackupsHandler)
	// backup-health is ADR-0063 §7's status surface: the age of the last successful backup, the age
	// of the last one that left the cluster, the last failure, and whether each registered
	// object-storage destination answers right now. Read-only, and it moves no secret — the
	// destination probe signs a request with the stored credential and reports names, never values.
	v1.HandleFunc("GET /v1/addons/backup-health", s.backupHealthHandler)
	v1.HandleFunc("POST /v1/addons/restore", s.restoreAddon)
	v1.HandleFunc("GET /v1/addons", s.listAddonsHandler)
	v1.HandleFunc("DELETE /v1/addons/{name}", s.removeAddon)
	v1.HandleFunc("POST /v1/logs/query", s.queryLogs)
	v1.HandleFunc("POST /v1/metrics/query", s.queryMetrics)
	v1.HandleFunc("GET /v1/audit", s.audit)
	// The cluster-wide failure listing (ADR-0074 §8): what, across everything Burrow manages, is
	// broken — read from the ledger the observer writes, never from the cluster. Read-only, moves no
	// secret value (a ledger row carries an object name, a reason from a closed set, and one bounded
	// Burrow-authored line). It answers ROWS AND NOT GROUPS: grouping by shared reason is a
	// presentation heuristic the `burrow failures` listing applies, and ADR-0074 §5 keeps it out of
	// the API so an agent correlates on its own terms. Every answer carries its own observation
	// coverage, so an empty list can be told apart from an hour nobody was watching.
	v1.HandleFunc("GET /v1/failures", s.failures)
	// Environments register namespace-per-environment targets (ADR-0035 phase 2). add records a
	// name->namespace mapping (the namespace and burrowd's Role there are created kubeconfig-side by
	// `burrow env add`); list returns them with the default environment `prod` first. They move no secret.
	v1.HandleFunc("POST /v1/environments", s.addEnvironment)
	v1.HandleFunc("GET /v1/environments", s.listEnvironments)
	v1.HandleFunc("DELETE /v1/environments/{name}", s.removeEnvironment)
	// The cluster capabilities are read live (ADR-0034): a neutral, read-only report of what the
	// cluster can do — ingress, storage, LoadBalancer support, cert-manager, provider, DNS. It moves
	// no secret value.
	v1.HandleFunc("GET /v1/cluster", s.cluster)
	// The cluster capacity/headroom surface is read live (issue #275): per node and cluster-total
	// allocatable / committed / free, the top CPU and memory consumers, and a build-fit verdict —
	// all from the Kubernetes API alone, no metrics-server. Read-only; moves no secret value.
	v1.HandleFunc("GET /v1/cluster/capacity", s.capacity)

	root := http.NewServeMux()
	// Authenticate first, then apply the client-version handshake (ADR-0039): the too-old gate wraps
	// the mux, clientVersionContext records the acting client's version onto the request context for
	// the audit log, and v1NotFound turns a route this server lacks into a structured "upgrade the
	// control plane" error. Only authenticated callers reach the version machinery, so it never leaks
	// the server version to an anonymous request.
	root.Handle("/v1/", requireToken(cfg.Token, versionGate(cfg.Version, clientVersionContext(v1NotFound(cfg.Version, v1)))))
	root.HandleFunc("GET /healthz", health)
	return root, nil
}

type server struct {
	engine *controlplane.Engine
}

func (s *server) deleteApp(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	confirm := r.URL.Query().Get("confirm") == "true"
	if err := s.engine.DeleteApp(r.Context(), app, r.URL.Query().Get("env"), confirm); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"app": app})
}

func (s *server) deploy(w http.ResponseWriter, r *http.Request) {
	var req controlplane.DeployRequest
	if !decode(w, r, &req) {
		return
	}
	req.App = r.PathValue("app") // the path is authoritative for the app name
	res, err := s.engine.Deploy(r.Context(), req)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// build clones a git source reference and builds the app's image inside the cluster, then hands the
// resulting digest-pinned reference into the guarded deploy path (ADR-0053). Only the git ref crosses;
// no source bytes travel over the API (ADR-0004). A builder error is surfaced structurally and nothing
// is deployed; the deploy the build hands off to is gated by the app.deploy guardrail exactly as an
// explicit deploy is, so a held deploy maps to 422 with needs_confirmation.
func (s *server) build(w http.ResponseWriter, r *http.Request) {
	var req controlplane.BuildRequest
	if !decode(w, r, &req) {
		return
	}
	req.App = r.PathValue("app") // the path is authoritative for the app name
	res, err := s.engine.Build(r.Context(), req)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// run executes a one-off command in the app's own current image and environment (ADR-0048). The
// command's captured output and exit code come back as a structured result; a non-zero exit is a
// normal outcome, not an error. It is gated by the app.run guardrail (confirm by default) — a held
// run maps to 422 with needs_confirmation, like the other guarded operations.
func (s *server) run(w http.ResponseWriter, r *http.Request) {
	var req controlplane.RunRequest
	if !decode(w, r, &req) {
		return
	}
	req.App = r.PathValue("app") // the path is authoritative for the app name
	res, err := s.engine.Run(r.Context(), req)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// listHooks returns the lifecycle hooks configured for an app in the target environment (ADR-0072
// §1). A phase with no hook is absent rather than present and empty: unset means no hook. It moves
// no secret value — a hook is a command, and the app's config and Secret reach it at run time.
func (s *server) listHooks(w http.ResponseWriter, r *http.Request) {
	hooks, err := s.engine.Hooks(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hooksResponse{Hooks: hooks})
}

// hooksResponse wraps the hook listing so the shape can grow without breaking object decoders.
type hooksResponse struct {
	Hooks []controlplane.Hook `json:"hooks"`
}

// hookRequest is the body of a hook write: the command to run, as an argv. The phase is in the path
// and the app is in the path; neither is read from the body.
type hookRequest struct {
	Command []string `json:"command"`
}

// setHook configures the command an app runs at a phase, replacing any command already set there
// (ADR-0072 §1). An unknown phase — including `post-deploy`, which this control plane does not fire
// yet — is a 400 rather than a silently-stored setting that never runs (ADR-0009).
func (s *server) setHook(w http.ResponseWriter, r *http.Request) {
	var req hookRequest
	if !decode(w, r, &req) {
		return
	}
	hook, err := s.engine.SetHook(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"),
		controlplane.HookPhase(r.PathValue("phase")), req.Command)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hook)
}

// unsetHook removes an app's hook at a phase. Unsetting a phase with no hook succeeds: afterwards
// that phase runs nothing, which is what the caller asked for.
func (s *server) unsetHook(w http.ResponseWriter, r *http.Request) {
	app, phase := r.PathValue("app"), controlplane.HookPhase(r.PathValue("phase"))
	if err := s.engine.UnsetHook(r.Context(), app, r.URL.Query().Get("env"), phase); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"app": app, "phase": string(phase)})
}

func (s *server) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.engine.ListApps(r.Context(), r.URL.Query().Get("env"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, appsResponse{Apps: apps})
}

// appsResponse wraps the apps list so the shape can grow without breaking clients that decode
// an object.
type appsResponse struct {
	Apps []controlplane.WorkloadStatus `json:"apps"`
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	res, err := s.engine.Status(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// history returns an app's deploy timeline: the releases recorded for it, newest first (ADR-0007).
// It is read-only and moves no secret value. The optional env query validates a named environment.
func (s *server) history(w http.ResponseWriter, r *http.Request) {
	releases, err := s.engine.History(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, historyResponse{Releases: releases})
}

// historyResponse wraps the release timeline so the shape can grow without breaking object decoders.
type historyResponse struct {
	Releases []controlplane.Release `json:"releases"`
}

func (s *server) logs(w http.ResponseWriter, r *http.Request) {
	opts := controlplane.LogOptions{}
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid tail parameter %q", v), "invalid")
			return
		}
		opts.TailLines = n
	}
	lines, err := s.engine.Logs(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"), opts)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, logsResponse{Lines: lines})
}

func (s *server) rollback(w http.ResponseWriter, r *http.Request) {
	confirm := r.URL.Query().Get("confirm") == "true"
	res, err := s.engine.Rollback(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"), confirm)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) scale(w http.ResponseWriter, r *http.Request) {
	var req scaleRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.engine.Scale(r.Context(), r.PathValue("app"), req.Env, req.Replicas, req.Confirm)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) autoscale(w http.ResponseWriter, r *http.Request) {
	var req autoscaleRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.engine.Autoscale(r.Context(), r.PathValue("app"), req.Env, controlplane.AutoscaleSpec{
		MinReplicas:   req.Min,
		MaxReplicas:   req.Max,
		CPUPercent:    req.CPU,
		MemoryPercent: req.Memory,
	}, req.Confirm)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) disableAutoscale(w http.ResponseWriter, r *http.Request) {
	confirm := r.URL.Query().Get("confirm") == "true"
	if err := s.engine.DisableAutoscale(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"), confirm); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"app": r.PathValue("app")})
}

func (s *server) expose(w http.ResponseWriter, r *http.Request) {
	var req controlplane.ExposeRequest
	if !decode(w, r, &req) {
		return
	}
	req.App = r.PathValue("app") // the path is authoritative for the app name
	res, err := s.engine.Expose(r.Context(), req)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) unexpose(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Unexpose(r.Context(), r.PathValue("app"), r.URL.Query().Get("env")); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"app": r.PathValue("app")})
}

func (s *server) reachability(w http.ResponseWriter, r *http.Request) {
	res, err := s.engine.Reachability(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) listConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.ListConfig(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, configResponse{Config: cfg})
}

func (s *server) setConfig(w http.ResponseWriter, r *http.Request) {
	var req configSetRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.engine.SetConfig(r.Context(), r.PathValue("app"), req.Env, req.Key, req.Value, req.NoRestart); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"app": r.PathValue("app"), "key": req.Key})
}

func (s *server) unsetConfig(w http.ResponseWriter, r *http.Request) {
	noRestart := r.URL.Query().Get("no_restart") == "true"
	key := r.PathValue("key")
	if err := s.engine.UnsetConfig(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"), key, noRestart); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"app": r.PathValue("app"), "key": key})
}

// configResponse wraps the config map so the shape can grow without breaking object decoders.
type configResponse struct {
	Config map[string]string `json:"config"`
}

// setSecret is the ONE secret endpoint that carries a value: it decodes {key, value, no_restart}
// from the POST body and hands the value to the engine, which writes it to the per-app Kubernetes
// Secret (ADR-0029). The value is never logged, never audited, never stored in Postgres, and the
// response carries the app and KEY only — never the value. This endpoint is deliberately absent
// from the agent surface (`burrow-agent` has no secret-set command; ADR-0029/0004): the agent
// references a secret key and asks the human to set the value, who does so through the CLI or the UI.
func (s *server) setSecret(w http.ResponseWriter, r *http.Request) {
	var req secretSetRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.engine.SetSecret(r.Context(), r.PathValue("app"), req.Env, req.Key, req.Value, req.NoRestart); err != nil {
		writeEngineError(w, err)
		return
	}
	// Respond with the app and KEY only — never echo the value back.
	writeJSON(w, http.StatusOK, map[string]string{"app": r.PathValue("app"), "key": req.Key})
}

func (s *server) listSecrets(w http.ResponseWriter, r *http.Request) {
	keys, err := s.engine.ListSecrets(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, secretsResponse{Keys: keys})
}

func (s *server) unsetSecret(w http.ResponseWriter, r *http.Request) {
	noRestart := r.URL.Query().Get("no_restart") == "true"
	key := r.PathValue("key")
	if err := s.engine.UnsetSecret(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"), key, noRestart); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"app": r.PathValue("app"), "key": key})
}

// secretsResponse carries an app's secret KEYS only — never the values, which live only in the
// per-app Kubernetes Secret (ADR-0028/0004).
type secretsResponse struct {
	Keys []string `json:"keys"`
}

// secretSetRequest is the body of a secret set (the app comes from the path). Value is the secret
// value: it travels over this authenticated, TLS-protected API and is written to the per-app
// Kubernetes Secret (ADR-0029) — it is never logged, never audited, and never stored in Postgres.
// NoRestart persists it without rolling the running workload; the change lands on the next deploy.
type secretSetRequest struct {
	// Env is the environment whose namespace the secret lands in (ADR-0035 phase 2b); empty targets
	// the default environment.
	Env       string `json:"env,omitempty"`
	Key       string `json:"key"`
	Value     string `json:"value"`
	NoRestart bool   `json:"no_restart,omitempty"`
}

// configSetRequest is the body of a config set (the app comes from the path). NoRestart persists the
// change without rolling the running workload; the change lands on the next deploy (ADR-0028).
type configSetRequest struct {
	// Env is the environment whose workload is rolled when the config changes (ADR-0035 phase 2b);
	// empty targets the default environment.
	Env       string `json:"env,omitempty"`
	Key       string `json:"key"`
	Value     string `json:"value"`
	NoRestart bool   `json:"no_restart,omitempty"`
}

func (s *server) guardList(w http.ResponseWriter, r *http.Request) {
	// The optional env query selects a named environment's effective policy; empty is the global
	// policy, reproducing the pre-environments behavior (ADR-0035 phase 2c).
	gs, err := s.engine.Guardrails(r.Context(), r.URL.Query().Get("env"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, guardResponse{Guardrails: gs})
}

func (s *server) guardSet(w http.ResponseWriter, r *http.Request) {
	var req guardSetRequest
	if !decode(w, r, &req) {
		return
	}
	// The optional env query scopes the set to a named environment (storing the env-prefixed code);
	// empty sets the global disposition (ADR-0035 phase 2c).
	env := r.URL.Query().Get("env")
	code := controlplane.GuardrailCode(r.PathValue("code"))
	if err := s.engine.SetGuardrail(r.Context(), env, code, controlplane.Disposition(req.Disposition)); err != nil {
		writeEngineError(w, err)
		return
	}
	gs, err := s.engine.Guardrails(r.Context(), env)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, guardResponse{Guardrails: gs})
}

// limitsList returns every operational limit with its effective value (ADR-0068 §3). The optional
// env query selects a named environment's effective configuration; empty is the cluster tier.
func (s *server) limitsList(w http.ResponseWriter, r *http.Request) {
	ls, err := s.engine.Limits(r.Context(), r.URL.Query().Get("env"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, limitsResponse{Limits: ls})
}

// limitSet sets one operational limit's value and returns the updated configuration. The optional
// env query scopes the set to a named environment (storing the env-prefixed code); empty sets the
// cluster value (ADR-0068 §3).
func (s *server) limitSet(w http.ResponseWriter, r *http.Request) {
	var req limitSetRequest
	if !decode(w, r, &req) {
		return
	}
	env := r.URL.Query().Get("env")
	code := controlplane.LimitCode(r.PathValue("code"))
	if err := s.engine.SetLimit(r.Context(), env, code, req.Value); err != nil {
		writeEngineError(w, err)
		return
	}
	ls, err := s.engine.Limits(r.Context(), env)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, limitsResponse{Limits: ls})
}

// getAutoDeploy returns the enriched, read-only auto-deploy view for an app in the selected
// environment (ADR-0052 §2/§3): the level plus, when the registry could be listed, the current
// running version, the tag auto-deploy would move to within the level, and any higher available
// upgrade above the cap. The optional env query selects a named environment; empty is the default
// environment. A registry failure degrades to the level alone (checked=false with a note) and never
// errors the call, keeping this path independent of registry reachability (ADR-0040).
func (s *server) getAutoDeploy(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	env := r.URL.Query().Get("env")
	st, err := s.engine.AutoDeployStatus(r.Context(), app, env)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, autoDeployResponse{
		App:            st.App,
		Env:            st.Env,
		Level:          string(st.Level),
		Repository:     st.Repository,
		Current:        st.Current,
		Target:         st.Target,
		Upgrade:        st.Upgrade,
		Checked:        st.Checked,
		Note:           st.Note,
		DisabledReason: st.DisabledReason,
	})
}

// setAutoDeploy sets an app's auto-deploy level for the selected environment (ADR-0052 §6). The level
// is validated at the boundary with ParseAutoDeployLevel so an unknown value is a clean 400 before
// the engine is touched. Setting the level is a human operator action; there is deliberately no agent
// verb for it, so the agent cannot change what deploys unattended (ADR-0038).
func (s *server) setAutoDeploy(w http.ResponseWriter, r *http.Request) {
	var req autoDeploySetRequest
	if !decode(w, r, &req) {
		return
	}
	level, err := controlplane.ParseAutoDeployLevel(req.Level)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid")
		return
	}
	app := r.PathValue("app")
	env := r.URL.Query().Get("env")
	if err := s.engine.SetAutoDeploy(r.Context(), app, env, level); err != nil {
		writeEngineError(w, err)
		return
	}
	effective, err := s.engine.AutoDeploy(r.Context(), app, env)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, autoDeployResponse{App: app, Env: envName(env), Level: string(effective)})
}

// getHealth returns the health endpoint declared for an app and the readiness probe Burrow authors
// as a result (ADR-0076 §3, §5). It is a read: nothing is changed and no workload is rolled. The
// answer carries the §5 guidance whenever no endpoint has been declared, so an agent that asks what
// the probe is also learns why declaring one is worth doing and what a good one must not check.
func (s *server) getHealth(w http.ResponseWriter, r *http.Request) {
	rep, err := s.engine.AppHealth(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// setHealth declares the health endpoint an app serves and re-applies the running workload so the
// probe reaches its pods. A path that is not a path — a URL, a host — is rejected by the engine as
// invalid, which is ADR-0076 §2 enforced at the boundary rather than trusted.
func (s *server) setHealth(w http.ResponseWriter, r *http.Request) {
	var req healthSetRequest
	if !decode(w, r, &req) {
		return
	}
	rep, err := s.engine.SetAppHealth(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"), req.Path, req.Port)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// unsetHealth removes an app's declared endpoint, returning it to ADR-0076 §3's default, and
// re-applies the running workload. Unsetting one that was never declared succeeds: it is the state
// the app is already in.
func (s *server) unsetHealth(w http.ResponseWriter, r *http.Request) {
	rep, err := s.engine.UnsetAppHealth(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// healthSetRequest is the body of a health declare call: the path the app serves its readiness
// answer on, and optionally the port. The app and the environment come from the path and the query.
// Port is omitted or zero to mean "the port the app is published on", resolved on every apply.
type healthSetRequest struct {
	Path string `json:"path"`
	Port int32  `json:"port,omitempty"`
}

// getChecks reports the deploy-time dependency check for an app: whether it runs, and what Burrow
// derived from what it provisioned that it would check (ADR-0076 §4). It is a read — no check is
// run — and it moves no secret value: a Dependency carries an environment variable's KEY NAME and an
// in-cluster address Burrow composed, never a credential.
func (s *server) getChecks(w http.ResponseWriter, r *http.Request) {
	rep, err := s.engine.AppChecks(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// setChecks turns the deploy-time dependency check on or off for an app. It is the "disableable
// rather than silent" half of putting a Burrow-supplied default on a path ADR-0072 described as
// user-configured.
func (s *server) setChecks(w http.ResponseWriter, r *http.Request) {
	var req checksSetRequest
	if !decode(w, r, &req) {
		return
	}
	rep, err := s.engine.SetAppChecks(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"), req.Enabled)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// checksSetRequest is the body of a checks call: whether the deploy-time dependency check runs.
type checksSetRequest struct {
	Enabled bool `json:"enabled"`
}

// nextTag returns the app's suggested next semver release tags from its current running tag
// (ADR-0052 §8): the current tag plus the next patch/minor/major. It is read-only guidance the agent
// applies to its build. A missing release or a non-semver current tag degrades to a note with no
// suggestion and never errors the call, keeping this guidance best-effort (ADR-0040).
func (s *server) nextTag(w http.ResponseWriter, r *http.Request) {
	res, err := s.engine.NextTag(r.Context(), r.PathValue("app"), r.URL.Query().Get("env"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// envName canonicalizes an environment name for a response body: an empty name reads as the reserved
// default environment `prod` (ADR-0067 §2), any other name passes through. It mirrors the engine's own canonicalization so
// the recorded environment is legible on both sides.
func envName(env string) string {
	if env == "" {
		return controlplane.DefaultEnvironment
	}
	return env
}

// addProvider decodes a provider registration — including the token VALUE — from the POST body and
// hands it to the engine, which validates the token, writes it into burrow-credentials, and records
// the registry entry (ADR-0030). The token travels only in the body (never the path or query), is
// never logged (the access log carries method+path+status, no body), is never stored in Postgres,
// and the response — the recorded Provider — carries the Secret key only, never the value. This is a
// human/CLI operation; `burrow-agent` has no command that adds a provider or carries a token.
func (s *server) addProvider(w http.ResponseWriter, r *http.Request) {
	var req controlplane.AddProviderRequest
	if !decode(w, r, &req) {
		return
	}
	p, err := s.engine.AddProvider(r.Context(), req)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) listProviders(w http.ResponseWriter, r *http.Request) {
	ps, err := s.engine.Providers(r.Context())
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, providersResponse{Providers: ps})
}

// providersResponse wraps the registry list so the shape can grow without breaking clients
// that decode an object.
type providersResponse struct {
	Providers []controlplane.Provider `json:"providers"`
}

func (s *server) addDomain(w http.ResponseWriter, r *http.Request) {
	var req controlplane.AddDomainRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.engine.AddDomain(r.Context(), req)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) removeDomain(w http.ResponseWriter, r *http.Request) {
	req := controlplane.RemoveDomainRequest{
		Host:     r.PathValue("host"), // the path is authoritative for the host
		Provider: r.URL.Query().Get("provider"),
		Confirm:  r.URL.Query().Get("confirm") == "true",
	}
	res, err := s.engine.RemoveDomain(r.Context(), req)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) installAddon(w http.ResponseWriter, r *http.Request) {
	var req addonInstallRequest
	if !decode(w, r, &req) {
		return
	}
	info, err := s.engine.InstallAddon(r.Context(), controlplane.AddonType(req.Type), req.Env, req.Confirm)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// connectAddon decodes a connect request — including the bearer token VALUE for an authenticated
// backend — from the POST body and hands it to the engine, which writes it into burrow-credentials
// (ADR-0030). The token travels only in the body (never the path or query), is never logged, is
// never stored in Postgres, and the response — the recorded AddonInfo — carries the Secret key only,
// never the value. Connecting an authenticated backend is a human/CLI operation; no command on the
// agent surface carries a token.
func (s *server) connectAddon(w http.ResponseWriter, r *http.Request) {
	var req addonConnectRequest
	if !decode(w, r, &req) {
		return
	}
	info, err := s.engine.ConnectAddon(r.Context(), req.Backend, req.Endpoint, req.SecretKey, req.Token)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// listAddonsHandler returns the registered add-ons and, alongside them, the volumes an earlier
// removal left behind (ADR-0064 §6). The two are separate fields, not one merged list: a retained
// claim is storage with no workload, and reading it as a running add-on would be worse than not
// reporting it.
//
// The retained listing is best-effort. It is a live cluster read layered over a registry-backed
// answer, so a cluster that cannot be read leaves it empty rather than failing the listing — the
// same posture ListAddons already takes for its readiness probe.
func (s *server) listAddonsHandler(w http.ResponseWriter, r *http.Request) {
	addons, err := s.engine.ListAddons(r.Context())
	if err != nil {
		writeEngineError(w, err)
		return
	}
	retained, err := s.engine.RetainedAddonVolumes(r.Context())
	if err != nil {
		retained = nil
	}
	writeJSON(w, http.StatusOK, addonsResponse{Addons: addons, RetainedVolumes: retained})
}

// removeAddon tears an add-on down. delete_data is the explicit, separate opt-in that also destroys
// the add-on's data volume; its absence is the safe default, so a caller that forgets it stops the
// add-on rather than destroying every attached app's database (ADR-0025/0031). The response reports
// what was kept so the caller can say so.
//
// skip_final_backup is the override for ADR-0064 §5's final backup, and it is absent-means-safe for
// the same reason delete_data is: a caller that forgets it gets the backup, not the shortcut past
// it. It is honoured only alongside delete_data, since it names nothing to skip otherwise.
func (s *server) removeAddon(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := controlplane.RemoveAddonOptions{
		DeleteData:        q.Get("delete_data") == "true",
		SkipFinalBackup:   q.Get("skip_final_backup") == "true",
		BackupDestination: q.Get("backup_destination"),
		Confirm:           q.Get("confirm") == "true",
	}
	res, err := s.engine.RemoveAddon(r.Context(), r.PathValue("name"), opts)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// attachAddon gives an app its own database on the installed Postgres add-on and wires it in
// (ADR-0031). The request carries only the add-on type and app name — NO secret value. burrowd
// generates the DATABASE_URL server-side and writes it into the app's Secret; the response is the
// key name only (AttachResult), never the value. The value is never logged, never audited, never
// stored in Postgres, and never returned — so attach is safe to expose on the agent surface.
func (s *server) attachAddon(w http.ResponseWriter, r *http.Request) {
	var req addonAttachRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.engine.AttachAddon(r.Context(), controlplane.AddonType(req.Addon), req.App, req.Env)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// detachAddon detaches an app from an add-on, dropping its data (e.g. its Postgres database). It is
// held by a confirm guardrail by default (ADR-0031).
func (s *server) detachAddon(w http.ResponseWriter, r *http.Request) {
	var req addonDetachRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.engine.DetachAddon(r.Context(), controlplane.AddonType(req.Addon), req.App, req.Env, req.Confirm); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"addon": req.Addon, "app": req.App})
}

// backupAddon backs up an app's database on the installed Postgres add-on (ADR-0032, ADR-0063 §7).
// burrowd runs an in-cluster Job that pg_dumps to the backup PVC and, when an object-storage
// destination is registered, writes it on to the store and reads it back before the backup is
// recorded as completed; the response is the recorded backup (id, app, path, destination, size,
// status) — no secret value. The backup Job reads the superuser password only via secretKeyRef and
// the destination credential only from a Job-owned Secret; neither is logged or returned.
func (s *server) backupAddon(w http.ResponseWriter, r *http.Request) {
	var req addonBackupRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.engine.BackupAddon(r.Context(), controlplane.AddonType(req.Addon), req.App, req.Env, req.Destination)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// listBackupsHandler lists recorded backups from the control-plane database (ADR-0032). An app query
// param restricts to one app and an env query param to one environment; absent, they list every
// app's and every environment's (ADR-0067 §1). Read-only; no secret value.
func (s *server) listBackupsHandler(w http.ResponseWriter, r *http.Request) {
	addon := r.URL.Query().Get("addon")
	if addon == "" {
		addon = string(controlplane.AddonPostgres)
	}
	backups, err := s.engine.ListBackups(r.Context(), controlplane.AddonType(addon), r.URL.Query().Get("app"), r.URL.Query().Get("env"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backupsResponse{Backups: backups})
}

// backupHealthHandler reports what Burrow observed about an add-on's backups (ADR-0063 §7,
// ADR-0066 §5). An app query param narrows to one app and an env param to one environment; absent,
// the report spans every app and environment, exactly as the backups listing does. Read-only; no
// secret value.
func (s *server) backupHealthHandler(w http.ResponseWriter, r *http.Request) {
	addon := r.URL.Query().Get("addon")
	if addon == "" {
		addon = string(controlplane.AddonPostgres)
	}
	health, err := s.engine.BackupHealth(r.Context(), controlplane.AddonType(addon), r.URL.Query().Get("app"), r.URL.Query().Get("env"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, health)
}

// restoreAddon restores an app's database from a recorded backup, overwriting its live contents
// (ADR-0032). It is held by the addon.restore confirm guardrail by default. burrowd runs an
// in-cluster Job that pg_restores the named dump; the Job reads the superuser password only via
// secretKeyRef.
func (s *server) restoreAddon(w http.ResponseWriter, r *http.Request) {
	var req addonRestoreRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.engine.RestoreAddon(r.Context(), controlplane.AddonType(req.Addon), req.App, req.Backup, req.Env, req.Confirm); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"addon": req.Addon, "app": req.App, "backup": req.Backup})
}

// addonBackupRequest is the body of an addon backup: the add-on type, the app, and the environment
// whose instance is dumped. No secret.
type addonBackupRequest struct {
	Addon string `json:"addon"`
	App   string `json:"app"`
	// Env is the environment whose Postgres instance the dump is taken from (ADR-0067 §1); empty
	// targets the default environment, and is refused when more than one environment is registered.
	Env string `json:"env,omitempty"`
	// Destination NAMES the object-storage provider this backup is written to (ADR-0063 §6). Empty
	// resolves it, which works when exactly one is registered and is refused when several are — the
	// destination of a backup is not a thing to guess at. It is a registry name, never a credential.
	Destination string `json:"destination,omitempty"`
}

// addonRestoreRequest is the body of an addon restore: the add-on type, the app, the backup id, and
// confirm (restore is held by a confirm guardrail).
type addonRestoreRequest struct {
	Addon  string `json:"addon"`
	App    string `json:"app"`
	Backup string `json:"backup"`
	// Env is the environment whose instance is restored INTO (ADR-0067 §1). The backup must have been
	// taken from the same environment: a dump from another environment's instance is not a valid
	// source for this one's live database.
	Env     string `json:"env,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

// backupsResponse wraps the backup list so the shape can grow without breaking object decoders.
type backupsResponse struct {
	Backups []controlplane.Backup `json:"backups"`
}

// addonAttachRequest is the body of an addon attach: the add-on type, the app name, and the
// environment whose instance the database is provisioned on. It carries no secret — burrowd
// generates the connection string server-side (ADR-0031).
type addonAttachRequest struct {
	Addon string `json:"addon"`
	App   string `json:"app"`
	// Env is the environment whose Postgres instance the app's database is created on, and whose
	// namespace the DATABASE_URL Secret lands in (ADR-0067 §1). Empty targets the default
	// environment, and is refused when more than one environment is registered (ADR-0047 §1).
	Env string `json:"env,omitempty"`
}

// addonDetachRequest is the body of an addon detach: the add-on type, the app, the environment, and
// confirm.
type addonDetachRequest struct {
	Addon string `json:"addon"`
	App   string `json:"app"`
	// Env is the environment whose instance the app's database is dropped from (ADR-0067 §1).
	Env     string `json:"env,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

// addonInstallRequest is the body of an addon install (the type names the catalog entry, the
// environment names which environment's instance to stand up).
type addonInstallRequest struct {
	Type string `json:"type"`
	// Env is the environment the instance serves (ADR-0067 §1). Each environment gets its own
	// instance, so this decides both the registry key and the cluster resource names; empty targets
	// the default environment, whose instance keeps the names an existing install already has.
	Env     string `json:"env,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

// addonConnectRequest is the body of an addon connect (the backend names the catalog entry; the
// endpoint is the in-cluster host:port of the existing backend). SecretKey, when set, names the key
// in the burrow-credentials Secret under which the backend's bearer token lives. Token is the bearer
// token VALUE for an authenticated backend: it travels over this authenticated, TLS-protected API
// and is written to burrow-credentials (ADR-0030) — never logged, never stored in Postgres, never
// echoed back, and never carried over the agent control channel.
type addonConnectRequest struct {
	Backend   string `json:"backend"`
	Endpoint  string `json:"endpoint"`
	SecretKey string `json:"secret_key"`
	Token     string `json:"token,omitempty"`
}

// addonsResponse wraps the add-on list so the shape can grow without breaking object decoders.
type addonsResponse struct {
	Addons []controlplane.AddonInfo `json:"addons"`
	// RetainedVolumes are the add-on volumes an earlier removal kept: allocated storage with no
	// add-on left to use it (ADR-0064 §6). Omitted when there are none.
	RetainedVolumes []controlplane.AddonVolume `json:"retained_volumes,omitempty"`
}

func (s *server) queryLogs(w http.ResponseWriter, r *http.Request) {
	var req logsQueryRequest
	if !decode(w, r, &req) {
		return
	}
	entries, err := s.engine.QueryLogs(r.Context(), req.Query, req.Limit, req.Backend)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, logsQueryResponse{Entries: entries})
}

type logsQueryRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
	// Backend targets a specific logs add-on (by its concrete backend or registry name) when more
	// than one serves the logs capability; empty picks the first.
	Backend string `json:"backend,omitempty"`
}

type logsQueryResponse struct {
	Entries []controlplane.LogEntry `json:"entries"`
}

func (s *server) queryMetrics(w http.ResponseWriter, r *http.Request) {
	var req metricsQueryRequest
	if !decode(w, r, &req) {
		return
	}
	samples, err := s.engine.QueryMetrics(r.Context(), req.Query, req.Backend)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, metricsQueryResponse{Samples: samples})
}

type metricsQueryRequest struct {
	Query string `json:"query"`
	// Backend targets a specific metrics add-on (by its concrete backend or registry name) when more
	// than one serves the metrics capability; empty picks the first.
	Backend string `json:"backend,omitempty"`
}

type metricsQueryResponse struct {
	Samples []controlplane.MetricSample `json:"samples"`
}

// Note (ADR-0038, principal seam): there is no auth change here today — the control plane
// authenticates with a single API token and every agent shares one ServiceAccount, so the
// engine's principal seam (controlplane.principalFromContext) simply returns the shared-agent
// constant. When per-user SSO lands, middleware wrapping these handlers would resolve the SSO
// identity (e.g. via TokenReview) and put it on the request context here, and the engine seam
// would read it off ctx — no call-site changes, and past audit rows keep their meaning.

func (s *server) audit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := controlplane.AuditFilter{
		App:       q.Get("app"),
		Operation: q.Get("operation"),
		Outcome:   controlplane.AuditOutcome(q.Get("outcome")),
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid limit parameter %q", v), "invalid")
			return
		}
		filter.Limit = n
	}
	entries, err := s.engine.Audit(r.Context(), filter)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, auditResponse{Entries: entries})
}

// auditResponse wraps the audit rows so the shape can grow without breaking object decoders.
type auditResponse struct {
	Entries []controlplane.AuditEntry `json:"entries"`
}

// failures serves the cluster-wide failure listing (ADR-0074 §8). The response is
// controlplane.FailureReport verbatim: the ledger rows and the observation coverage behind them.
//
// `since` is a DURATION ("1h", "24h"), not a timestamp, and it is resolved against the control
// plane's clock. The ledger's timestamps were written by that clock, so a client resolving "the last
// hour" against its own would query a window skewed by however wrong that clock is — and it also
// removes the one way a caller could ask this endpoint for a window it could not otherwise name.
func (s *server) failures(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := controlplane.FailureQuery{
		Kind:            controlplane.FailureKind(q.Get("kind")),
		Name:            q.Get("name"),
		Environment:     q.Get("env"),
		Reason:          q.Get("reason"),
		IncludeResolved: q.Get("all") == "true",
	}
	if v := q.Get("since"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("invalid since parameter %q: expected a positive duration such as 1h or 24h", v), "invalid")
			return
		}
		query.Since = d
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid limit parameter %q", v), "invalid")
			return
		}
		query.Limit = n
	}
	report, err := s.engine.Failures(r.Context(), query)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// cluster reports the cluster's capabilities live (ADR-0034): a read-only probe of ingress,
// storage, LoadBalancer support, cert-manager, provider, and configured DNS. It changes nothing
// and moves no secret value.
func (s *server) cluster(w http.ResponseWriter, r *http.Request) {
	caps, err := s.engine.ClusterCapabilities(r.Context())
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, caps)
}

// capacity reports the cluster's scheduling capacity and headroom live (issue #275): per node and
// cluster-total allocatable / committed (sum of pod requests) / free, the top CPU and memory
// consumers, and a verdict on whether a typical in-cluster build fits and whether another node is
// needed — all from the Kubernetes API alone. It changes nothing and moves no secret value.
func (s *server) capacity(w http.ResponseWriter, r *http.Request) {
	report, err := s.engine.ClusterCapacity(r.Context())
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// addEnvironment registers a namespace-per-environment target (ADR-0035 phase 2): it decodes
// {name, namespace} and records the mapping. The privileged namespace + RBAC setup is done
// kubeconfig-side by `burrow env add` before this call — burrowd holds only namespaced Roles and
// cannot create namespaces or RBAC itself. It moves no secret value.
func (s *server) addEnvironment(w http.ResponseWriter, r *http.Request) {
	var req environmentAddRequest
	if !decode(w, r, &req) {
		return
	}
	env, err := s.engine.AddEnvironment(r.Context(), req.Name, req.Namespace)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, env)
}

func (s *server) listEnvironments(w http.ResponseWriter, r *http.Request) {
	envs, err := s.engine.ListEnvironments(r.Context())
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, environmentsResponse{Environments: envs})
}

// removeEnvironment unregisters a namespace-per-environment target (ADR-0035 phase 2), the inverse
// of addEnvironment. It removes only the registry mapping; the namespace and its apps are managed
// out of band (kubeconfig-side in the single-tenant install, by the managed control plane in the
// cloud). It moves no secret value.
func (s *server) removeEnvironment(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.engine.RemoveEnvironment(r.Context(), name); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

// environmentAddRequest is the body of an environment add: the environment name and the namespace
// its apps deploy into.
type environmentAddRequest struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// environmentsResponse wraps the environment list so the shape can grow without breaking object
// decoders.
type environmentsResponse struct {
	Environments []controlplane.Environment `json:"environments"`
}

// guardResponse is the body of a guard list/set call: the full guardrail policy.
type guardResponse struct {
	Guardrails []controlplane.GuardrailInfo `json:"guardrails"`
}

// autoDeployResponse is the body of an auto-deploy get/set call: the app, the canonical environment
// name, and the effective auto-deploy level (ADR-0052 §2), plus the enriched read-only upgrade view
// a get returns (ADR-0052 §3) — the current running version, the tag auto-deploy would move to
// within the level, the highest version above the level's cap surfaced as an available upgrade,
// whether the registry upgrade check ran, and a short note when it could not. The upgrade fields are
// omitempty so a set response (which reports the level only) stays the same shape as before.
type autoDeployResponse struct {
	App        string `json:"app"`
	Env        string `json:"env"`
	Level      string `json:"level"`
	Repository string `json:"repository,omitempty"`
	Current    string `json:"current,omitempty"`
	Target     string `json:"target,omitempty"`
	Upgrade    string `json:"upgrade,omitempty"`
	Checked    bool   `json:"checked,omitempty"`
	Note       string `json:"note,omitempty"`
	// DisabledReason is why auto-deploy is off when the safety stop turned it off (ADR-0052 §5).
	DisabledReason string `json:"disabled_reason,omitempty"`
}

// autoDeploySetRequest is the body of an auto-deploy set call (the app comes from the path, the
// environment from the query).
type autoDeploySetRequest struct {
	Level string `json:"level"`
}

// guardSetRequest is the body of a guard set call (the guardrail code comes from the path).
type guardSetRequest struct {
	Disposition string `json:"disposition"`
}

// limitsResponse is the body of a config list/set call: every operational limit with its effective
// value and the tier that value came from (ADR-0068).
type limitsResponse struct {
	Limits []controlplane.LimitInfo `json:"limits"`
}

// limitSetRequest is the body of a config set call (the limit code comes from the path). Value is
// the limit's canonical text form — "50" for a count, "72h" for a duration — validated server-side
// against the limit's kind and permitted range.
type limitSetRequest struct {
	Value string `json:"value"`
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// scaleRequest is the body of a scale call (the app comes from the path).
type scaleRequest struct {
	// Env is the environment whose namespace the workload lives in (ADR-0035 phase 2b); empty
	// targets the default environment.
	Env      string `json:"env,omitempty"`
	Replicas int32  `json:"replicas"`
	// Confirm acknowledges a confirm-disposition guardrail so the scale proceeds past it
	// (ADR-0020).
	Confirm bool `json:"confirm,omitempty"`
}

// autoscaleRequest is the body of an autoscale call (the app comes from the path). It carries the
// replica band, the CPU (and optional memory) utilization targets, and the env whose namespace the
// app lives in (ADR-0006, ADR-0035 phase 2b).
type autoscaleRequest struct {
	Env    string `json:"env,omitempty"`
	Min    int32  `json:"min"`
	Max    int32  `json:"max"`
	CPU    int32  `json:"cpu"`
	Memory int32  `json:"memory,omitempty"`
	// Confirm acknowledges a confirm-disposition guardrail so the autoscale proceeds past it
	// (ADR-0020).
	Confirm bool `json:"confirm,omitempty"`
}

// logsResponse wraps the log lines so the shape can grow (cursors, truncation) without
// breaking clients that decode an object.
type logsResponse struct {
	Lines []controlplane.LogLine `json:"lines"`
}

// requireToken rejects any request whose bearer token does not match, in constant time.
func requireToken(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := presentedToken(r)
		if got == "" || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or invalid token", "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// presentedToken reads the API token from X-Burrow-Token (the header that survives the
// Kubernetes API-server proxy, since the kubeconfig transport owns Authorization there —
// ADR-0014) or, failing that, an Authorization: Bearer header (direct / ingress path).
func presentedToken(r *http.Request) string {
	if t := r.Header.Get("X-Burrow-Token"); t != "" {
		return t
	}
	return bearerToken(r)
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return h[len(prefix):]
	}
	return ""
}

// decode reads a JSON request body into v, writing a 400 and returning false on failure.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// errorResponse is the JSON body of every error. Code is a machine-readable tag the
// agent can branch on; Requested/Limit are populated for guardrail refusals.
type errorResponse struct {
	Error     string `json:"error"`
	Code      string `json:"code,omitempty"`
	Requested *int32 `json:"requested,omitempty"`
	Limit     *int32 `json:"limit,omitempty"`
	// NeedsConfirmation is set on a guardrail that holds the operation for confirmation
	// rather than refusing it: the caller may retry with confirm set (ADR-0020).
	NeedsConfirmation bool `json:"needs_confirmation,omitempty"`
	// ServerVersion is this control plane's release version, set on the client_too_old refusal so
	// the client can name the version it must reach in its own, install-aware remedy (ADR-0039).
	// The control plane cannot know how the caller was installed; the caller can.
	ServerVersion string `json:"server_version,omitempty"`
}

func writeError(w http.ResponseWriter, status int, msg, code string) {
	writeJSON(w, status, errorResponse{Error: msg, Code: code})
}

// writeEngineError maps a deploy-engine error to its HTTP status and structured body.
func writeEngineError(w http.ResponseWriter, err error) {
	// An operational limit exceeded is a structured refusal, not a system failure and not a policy
	// decision (ADR-0068 §2): the request was understood, it crosses a bound a human set, and no
	// confirmation opens it. It carries the same requested/limit pair a guardrail refusal does, and
	// NeedsConfirmation is deliberately absent — there is nothing to confirm.
	if l, ok := controlplane.AsLimit(err); ok {
		req, lim := l.Requested, l.Limit
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{
			Error: err.Error(), Code: string(l.Code), Requested: &req, Limit: &lim,
		})
		return
	}
	if g, ok := controlplane.AsGuardrail(err); ok {
		req, lim := g.Requested, g.Limit
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{
			Error: g.Error(), Code: string(g.Code), Requested: &req, Limit: &lim,
			NeedsConfirmation: g.NeedsConfirmation,
		})
		return
	}
	// An ambiguous environment target is a structured, actionable refusal (ADR-0047 §1): the mutating
	// request named no environment while more than one is registered, so burrowd refuses to pick one.
	// The listing of environments rides in the error text so the agent re-issues the call naming a
	// target, without a separate probe. It is an unprocessable request, not a system failure.
	if a, ok := controlplane.AsAmbiguousEnvironment(err); ok {
		writeError(w, http.StatusUnprocessableEntity, a.Error(), "ambiguous_environment")
		return
	}
	// A failed lifecycle hook is a structured, actionable refusal (ADR-0072 §3): the request was
	// understood, the guardrail allowed it, and the user's own command exited non-zero, so the deploy
	// (or rollback) did not happen and the running version is untouched. The phase, the command, the
	// exit code and the command's own output ride in the error text, which is what makes the failure
	// diagnosable from the response instead of from a hunt through the cluster. NeedsConfirmation is
	// deliberately absent — there is nothing to confirm; the command has to be fixed.
	if _, ok := controlplane.AsHook(err); ok {
		writeError(w, http.StatusUnprocessableEntity, err.Error(), "hook_failed")
		return
	}
	// Missing cluster prerequisites is a structured, actionable outcome (ADR-0006): the request was
	// valid but the cluster is not set up for it. The full checklist rides in the error text so the
	// agent gets each missing piece and its burrow fix in one response, without inspecting the cluster.
	if _, ok := controlplane.AsMissingPrerequisites(err); ok {
		writeError(w, http.StatusUnprocessableEntity, err.Error(), "missing_prerequisites")
		return
	}
	switch {
	case errors.Is(err, controlplane.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error(), "not_found")
	case errors.Is(err, controlplane.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error(), "invalid")
	case errors.Is(err, controlplane.ErrNotImplemented):
		writeError(w, http.StatusNotImplemented, err.Error(), "not_implemented")
	default:
		writeError(w, http.StatusInternalServerError, err.Error(), "internal")
	}
}
