// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

// Package api is the control plane's HTTP front end: it exposes the deploy engine's
// operations over JSON and authenticates its callers with a bearer token (ADR-0005).
// It is a thin transport adapter — it decodes requests, calls the engine, and maps the
// engine's typed outcomes to HTTP status codes; the orchestration and guardrails live
// in the engine (ADR-0006). The MCP server and the CLI are both clients of this API.
//
// It lives under controlplane/ (not controlplane/internal) so cmd/burrowd and the
// managed module can wire it; it is source-available under FSL-1.1-ALv2.
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
	v1.HandleFunc("POST /v1/apps/{app}/deploy", s.deploy)
	v1.HandleFunc("GET /v1/apps/{app}/status", s.status)
	v1.HandleFunc("GET /v1/apps/{app}/logs", s.logs)
	v1.HandleFunc("POST /v1/apps/{app}/rollback", s.rollback)
	v1.HandleFunc("POST /v1/apps/{app}/scale", s.scale)
	v1.HandleFunc("POST /v1/apps/{app}/expose", s.expose)
	v1.HandleFunc("POST /v1/apps/{app}/unexpose", s.unexpose)
	v1.HandleFunc("GET /v1/apps/{app}/reachability", s.reachability)
	v1.HandleFunc("GET /v1/guard", s.guardList)
	v1.HandleFunc("PUT /v1/guard/{code}", s.guardSet)
	v1.HandleFunc("POST /v1/providers", s.addProvider)
	v1.HandleFunc("GET /v1/providers", s.listProviders)
	v1.HandleFunc("POST /v1/domains", s.addDomain)
	v1.HandleFunc("DELETE /v1/domains/{host}", s.removeDomain)
	v1.HandleFunc("POST /v1/addons", s.installAddon)
	v1.HandleFunc("GET /v1/addons", s.listAddonsHandler)
	v1.HandleFunc("DELETE /v1/addons/{name}", s.removeAddon)

	root := http.NewServeMux()
	root.Handle("/v1/", requireToken(cfg.Token, v1))
	root.HandleFunc("GET /healthz", health)
	return root, nil
}

type server struct {
	engine *controlplane.Engine
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

func (s *server) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.engine.ListApps(r.Context())
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
	res, err := s.engine.Status(r.Context(), r.PathValue("app"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
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
	lines, err := s.engine.Logs(r.Context(), r.PathValue("app"), opts)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, logsResponse{Lines: lines})
}

func (s *server) rollback(w http.ResponseWriter, r *http.Request) {
	res, err := s.engine.Rollback(r.Context(), r.PathValue("app"))
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
	res, err := s.engine.Scale(r.Context(), r.PathValue("app"), req.Replicas, req.Confirm)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
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
	if err := s.engine.Unexpose(r.Context(), r.PathValue("app")); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"app": r.PathValue("app")})
}

func (s *server) reachability(w http.ResponseWriter, r *http.Request) {
	res, err := s.engine.Reachability(r.Context(), r.PathValue("app"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) guardList(w http.ResponseWriter, r *http.Request) {
	gs, err := s.engine.Guardrails(r.Context())
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
	code := controlplane.GuardrailCode(r.PathValue("code"))
	if err := s.engine.SetGuardrail(r.Context(), code, controlplane.Disposition(req.Disposition)); err != nil {
		writeEngineError(w, err)
		return
	}
	gs, err := s.engine.Guardrails(r.Context())
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, guardResponse{Guardrails: gs})
}

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

// addonInstallRequest is the body of an addon install (the type names the catalog entry).
type addonInstallRequest struct {
	Type    string `json:"type"`
	Confirm bool   `json:"confirm,omitempty"`
}

// addonsResponse wraps the add-on list so the shape can grow without breaking object decoders.
type addonsResponse struct {
	Addons []controlplane.AddonInfo `json:"addons"`
}

// guardResponse is the body of a guard list/set call: the full guardrail policy.
type guardResponse struct {
	Guardrails []controlplane.GuardrailInfo `json:"guardrails"`
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
	Replicas int32 `json:"replicas"`
	// Confirm acknowledges a confirm-disposition guardrail so the scale proceeds past it
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
