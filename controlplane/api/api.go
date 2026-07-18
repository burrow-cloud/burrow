// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package api is the control plane's HTTP front end: it exposes the deploy engine's
// operations over JSON and authenticates its callers with a bearer token (ADR-0005).
// It is a thin transport adapter — it decodes requests, calls the engine, and maps the
// engine's typed outcomes to HTTP status codes; the orchestration and guardrails live
// in the engine (ADR-0006). The MCP server and the CLI are both clients of this API.
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
	// next-tag suggests the app's next semver release tags from its current running tag (ADR-0052 §8).
	// It is read-only guidance the agent applies to its own build; there is no mutating counterpart.
	v1.HandleFunc("GET /v1/apps/{app}/next-tag", s.nextTag)
	v1.HandleFunc("POST /v1/apps/{app}/scale", s.scale)
	// run executes a one-off command in the app's own current image and environment (ADR-0048).
	v1.HandleFunc("POST /v1/apps/{app}/run", s.run)
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
	// value), never audited, never stored in Postgres, and never exposed over MCP — there is no
	// burrow_secret_set tool (ADR-0029/0004). list and unset carry no value.
	v1.HandleFunc("POST /v1/apps/{app}/secrets", s.setSecret)
	v1.HandleFunc("GET /v1/apps/{app}/secrets", s.listSecrets)
	v1.HandleFunc("DELETE /v1/apps/{app}/secrets/{key}", s.unsetSecret)
	v1.HandleFunc("GET /v1/guard", s.guardList)
	v1.HandleFunc("PUT /v1/guard/{code}", s.guardSet)
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
	v1.HandleFunc("POST /v1/addons/restore", s.restoreAddon)
	v1.HandleFunc("GET /v1/addons", s.listAddonsHandler)
	v1.HandleFunc("DELETE /v1/addons/{name}", s.removeAddon)
	v1.HandleFunc("POST /v1/logs/query", s.queryLogs)
	v1.HandleFunc("POST /v1/metrics/query", s.queryMetrics)
	v1.HandleFunc("GET /v1/audit", s.audit)
	// Environments register namespace-per-environment targets (ADR-0035 phase 2). add records a
	// name->namespace mapping (the namespace and burrowd's Role there are created kubeconfig-side by
	// `burrow env add`); list returns them with the implicit `default` first. They move no secret.
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
// response carries the app and KEY only — never the value. This endpoint is deliberately not
// exposed over MCP (there is no burrow_secret_set tool; ADR-0029/0004): the agent references a
// secret key and asks the human to set the value, who does so through the CLI or the UI.
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
// default environment, any other name passes through. It mirrors the engine's own canonicalization so
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
// human/CLI operation; there is no MCP tool that adds a provider or carries a token.
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
	info, err := s.engine.InstallAddon(r.Context(), controlplane.AddonType(req.Type), req.Confirm)
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
// never the value. Connecting an authenticated backend is a human/CLI operation; no MCP tool carries
// a token.
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

func (s *server) listAddonsHandler(w http.ResponseWriter, r *http.Request) {
	addons, err := s.engine.ListAddons(r.Context())
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, addonsResponse{Addons: addons})
}

func (s *server) removeAddon(w http.ResponseWriter, r *http.Request) {
	confirm := r.URL.Query().Get("confirm") == "true"
	if err := s.engine.RemoveAddon(r.Context(), r.PathValue("name"), confirm); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": r.PathValue("name")})
}

// attachAddon gives an app its own database on the installed Postgres add-on and wires it in
// (ADR-0031). The request carries only the add-on type and app name — NO secret value. burrowd
// generates the DATABASE_URL server-side and writes it into the app's Secret; the response is the
// key name only (AttachResult), never the value. The value is never logged, never audited, never
// stored in Postgres, and never returned — so attach is safe to expose over MCP.
func (s *server) attachAddon(w http.ResponseWriter, r *http.Request) {
	var req addonAttachRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.engine.AttachAddon(r.Context(), controlplane.AddonType(req.Addon), req.App)
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
	if err := s.engine.DetachAddon(r.Context(), controlplane.AddonType(req.Addon), req.App, req.Confirm); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"addon": req.Addon, "app": req.App})
}

// backupAddon backs up an app's database on the installed Postgres add-on (ADR-0032). burrowd runs
// an in-cluster Job that pg_dumps to the backup PVC and records the backup in the control-plane
// database; the response is the recorded backup (id, app, path, size, status) — no secret value. The
// backup Job reads the superuser password only via secretKeyRef, never logged or returned.
func (s *server) backupAddon(w http.ResponseWriter, r *http.Request) {
	var req addonBackupRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.engine.BackupAddon(r.Context(), controlplane.AddonType(req.Addon), req.App)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// listBackupsHandler lists recorded backups from the control-plane database (ADR-0032). An app query
// param restricts to one app; absent, it lists every app's backups. Read-only; no secret value.
func (s *server) listBackupsHandler(w http.ResponseWriter, r *http.Request) {
	addon := r.URL.Query().Get("addon")
	if addon == "" {
		addon = string(controlplane.AddonPostgres)
	}
	backups, err := s.engine.ListBackups(r.Context(), controlplane.AddonType(addon), r.URL.Query().Get("app"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backupsResponse{Backups: backups})
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
	if err := s.engine.RestoreAddon(r.Context(), controlplane.AddonType(req.Addon), req.App, req.Backup, req.Confirm); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"addon": req.Addon, "app": req.App, "backup": req.Backup})
}

// addonBackupRequest is the body of an addon backup: the add-on type and the app. No secret.
type addonBackupRequest struct {
	Addon string `json:"addon"`
	App   string `json:"app"`
}

// addonRestoreRequest is the body of an addon restore: the add-on type, the app, the backup id, and
// confirm (restore is held by a confirm guardrail).
type addonRestoreRequest struct {
	Addon   string `json:"addon"`
	App     string `json:"app"`
	Backup  string `json:"backup"`
	Confirm bool   `json:"confirm,omitempty"`
}

// backupsResponse wraps the backup list so the shape can grow without breaking object decoders.
type backupsResponse struct {
	Backups []controlplane.Backup `json:"backups"`
}

// addonAttachRequest is the body of an addon attach: the add-on type and the app name. It carries
// no secret — burrowd generates the connection string server-side (ADR-0031).
type addonAttachRequest struct {
	Addon string `json:"addon"`
	App   string `json:"app"`
}

// addonDetachRequest is the body of an addon detach: the add-on type, the app, and confirm.
type addonDetachRequest struct {
	Addon   string `json:"addon"`
	App     string `json:"app"`
	Confirm bool   `json:"confirm,omitempty"`
}

// addonInstallRequest is the body of an addon install (the type names the catalog entry).
type addonInstallRequest struct {
	Type    string `json:"type"`
	Confirm bool   `json:"confirm,omitempty"`
}

// addonConnectRequest is the body of an addon connect (the backend names the catalog entry; the
// endpoint is the in-cluster host:port of the existing backend). SecretKey, when set, names the key
// in the burrow-credentials Secret under which the backend's bearer token lives. Token is the bearer
// token VALUE for an authenticated backend: it travels over this authenticated, TLS-protected API
// and is written to burrow-credentials (ADR-0030) — never logged, never stored in Postgres, never
// echoed back, and never carried over MCP.
type addonConnectRequest struct {
	Backend   string `json:"backend"`
	Endpoint  string `json:"endpoint"`
	SecretKey string `json:"secret_key"`
	Token     string `json:"token,omitempty"`
}

// addonsResponse wraps the add-on list so the shape can grow without breaking object decoders.
type addonsResponse struct {
	Addons []controlplane.AddonInfo `json:"addons"`
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
}

func writeError(w http.ResponseWriter, status int, msg, code string) {
	writeJSON(w, status, errorResponse{Error: msg, Code: code})
}

// writeEngineError maps a deploy-engine error to its HTTP status and structured body.
func writeEngineError(w http.ResponseWriter, err error) {
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
	// Missing cluster prerequisites is a structured, actionable outcome (ADR-0006): the request was
	// valid but the cluster is not set up for it. The full checklist rides in the error text so the
	// agent gets each missing piece and its burrow fix over MCP without inspecting the cluster.
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
