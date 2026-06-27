// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is a thin HTTP client for the control-plane API (ADR-0005). The MCP server
// holds the API bearer token to authenticate to the control plane, but never any
// cluster credentials — those live only in the control plane. These DTOs mirror the
// API's JSON contract; the MCP layer (Apache-2.0) deliberately does not import the
// control-plane packages (FSL), so it stays a decoupled client across the license
// boundary (LICENSING.md).
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient returns a control-plane API client for baseURL authenticating with token,
// using a default HTTP client.
func NewClient(baseURL, token string) *Client {
	return NewClientWithHTTP(baseURL, token, nil)
}

// NewClientWithHTTP is like NewClient but uses the supplied *http.Client. The connect
// package uses this to route requests through the Kubernetes API-server proxy with a
// kubeconfig-authenticated transport (ADR-0014). A nil hc gets a default client.
func NewClientWithHTTP(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    hc,
	}
}

// APIError is a non-2xx response from the control plane, carrying its structured error
// (a machine-readable code and a human message) so a tool can surface both.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	// NeedsConfirmation is true when a guardrail held the operation for confirmation
	// rather than refusing it: retrying with confirm set lets it proceed (ADR-0020).
	NeedsConfirmation bool
}

func (e *APIError) Error() string {
	hint := ""
	if e.NeedsConfirmation {
		hint = " — re-run with --confirm to proceed"
	}
	if e.Code != "" {
		return fmt.Sprintf("control plane: %s (%s, http %d)%s", e.Message, e.Code, e.StatusCode, hint)
	}
	return fmt.Sprintf("control plane: %s (http %d)%s", e.Message, e.StatusCode, hint)
}

// The DTOs below mirror the control-plane API's JSON shapes (snake_case).

type DeployRequest struct {
	Image    string            `json:"image"`
	Env      map[string]string `json:"env,omitempty"`
	Command  []string          `json:"command,omitempty"`
	Replicas int32             `json:"replicas"`
	Confirm  bool              `json:"confirm,omitempty"`
}

type Release struct {
	ID         string            `json:"id"`
	App        string            `json:"app"`
	Image      string            `json:"image"`
	Digest     string            `json:"digest,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Command    []string          `json:"command,omitempty"`
	Replicas   int32             `json:"replicas"`
	Status     string            `json:"status"`
	Supersedes string            `json:"supersedes,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
}

type WorkloadStatus struct {
	App             string `json:"app"`
	Kind            string `json:"kind"`
	Image           string `json:"image"`
	DesiredReplicas int32  `json:"desired_replicas"`
	ReadyReplicas   int32  `json:"ready_replicas"`
	UpdatedReplicas int32  `json:"updated_replicas"`
	Available       bool   `json:"available"`
}

type DeployResult struct {
	Release             Release `json:"release"`
	SupersededReleaseID string  `json:"superseded_release_id,omitempty"`
}

type StatusResult struct {
	App        string         `json:"app"`
	HasRelease bool           `json:"has_release"`
	Release    Release        `json:"release,omitempty"`
	Running    bool           `json:"running"`
	Workload   WorkloadStatus `json:"workload,omitempty"`
}

type ScaleResult struct {
	App              string `json:"app"`
	PreviousReplicas int32  `json:"previous_replicas"`
	Replicas         int32  `json:"replicas"`
}

type RollbackResult struct {
	Release               Release `json:"release"`
	RolledBackToReleaseID string  `json:"rolled_back_to_release_id"`
	SupersededReleaseID   string  `json:"superseded_release_id"`
}

type ExposeResult struct {
	App  string `json:"app"`
	Host string `json:"host"`
	Port int32  `json:"port"`
	URL  string `json:"url"`
}

type ReachabilityResult struct {
	App                string   `json:"app"`
	Deployed           bool     `json:"deployed"`
	Ready              bool     `json:"ready"`
	Exposed            bool     `json:"exposed"`
	Host               string   `json:"host,omitempty"`
	Address            string   `json:"address,omitempty"`
	TLS                bool     `json:"tls"`
	DNSPointsAtCluster bool     `json:"dns_points_at_cluster"`
	DNSAddresses       []string `json:"dns_addresses,omitempty"`
	Reachable          bool     `json:"reachable"`
	Summary            string   `json:"summary"`
}

type LogLine struct {
	Pod       string    `json:"pod"`
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
}

type Guardrail struct {
	Code        string `json:"code"`
	Disposition string `json:"disposition"`
	Description string `json:"description"`
}

// Provider mirrors a control-plane provider registry entry (ADR-0023). It carries no
// token — only the non-secret registry: the vendor type, the capabilities it serves, and
// the key under which its token lives in the burrow-credentials Secret.
type Provider struct {
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	Capabilities []string  `json:"capabilities"`
	SecretKey    string    `json:"secret_key"`
	CreatedAt    time.Time `json:"created_at"`
}

// AddProviderRequest registers a vendor credential. The token is not part of this request:
// the CLI writes it into the burrow-credentials Secret with the developer's kubeconfig, and
// only the registry entry — naming the Secret key — flows through the control plane.
type AddProviderRequest struct {
	Name      string `json:"name,omitempty"`
	Type      string `json:"type"`
	SecretKey string `json:"secret_key,omitempty"`
}

// DomainResult mirrors the control plane's DNS-record outcome (ADR-0018).
type DomainResult struct {
	Host     string `json:"host"`
	Provider string `json:"provider"`
	Type     string `json:"type,omitempty"`
	Address  string `json:"address,omitempty"`
}

func (c *Client) Deploy(ctx context.Context, app string, req DeployRequest) (DeployResult, error) {
	var out DeployResult
	err := c.do(ctx, http.MethodPost, c.appPath(app, "deploy"), req, &out)
	return out, err
}

func (c *Client) Status(ctx context.Context, app string) (StatusResult, error) {
	var out StatusResult
	err := c.do(ctx, http.MethodGet, c.appPath(app, "status"), nil, &out)
	return out, err
}

// Apps lists the workload status of every Burrow-managed app.
func (c *Client) Apps(ctx context.Context) ([]WorkloadStatus, error) {
	var out struct {
		Apps []WorkloadStatus `json:"apps"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/apps", nil, &out)
	return out.Apps, err
}

// Addon is one installed (and, later, connected) add-on instance.
type Addon struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Mode         string   `json:"mode"`
	Image        string   `json:"image,omitempty"`
	Endpoint     string   `json:"endpoint"`
	Capabilities []string `json:"capabilities"`
	Ready        bool     `json:"ready"`
}

// InstallAddon installs the vetted backing service for an add-on type (e.g. "logs").
func (c *Client) InstallAddon(ctx context.Context, addonType string, confirm bool) (Addon, error) {
	var out Addon
	err := c.do(ctx, http.MethodPost, "/v1/addons", map[string]any{"type": addonType, "confirm": confirm}, &out)
	return out, err
}

// ConnectAddon registers an existing backend the user already runs (e.g. an in-cluster Loki) as a
// queryable add-on, recording its endpoint (ADR-0026). Unlike install it deploys nothing. secretKey,
// when non-empty, names the key in the burrow-credentials Secret under which the backend's bearer
// token lives; the token itself never travels over this API, only the key (ADR-0004/0023).
func (c *Client) ConnectAddon(ctx context.Context, backend, endpoint, secretKey string) (Addon, error) {
	var out Addon
	body := map[string]any{"backend": backend, "endpoint": endpoint, "secret_key": secretKey}
	err := c.do(ctx, http.MethodPost, "/v1/addons/connect", body, &out)
	return out, err
}

// Addons lists the installed add-on instances.
func (c *Client) Addons(ctx context.Context) ([]Addon, error) {
	var out struct {
		Addons []Addon `json:"addons"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/addons", nil, &out)
	return out.Addons, err
}

// RemoveAddon removes the named add-on instance.
func (c *Client) RemoveAddon(ctx context.Context, name string, confirm bool) error {
	path := "/v1/addons/" + name
	if confirm {
		path += "?confirm=true"
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// LogEntry is one record from a logs query.
type LogEntry struct {
	Time    string `json:"time,omitempty"`
	Message string `json:"message"`
	Pod     string `json:"pod,omitempty"`
}

// QueryLogs queries the installed logs add-on with a LogsQL query (empty matches everything).
func (c *Client) QueryLogs(ctx context.Context, query string, limit int) ([]LogEntry, error) {
	var out struct {
		Entries []LogEntry `json:"entries"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/logs/query", map[string]any{"query": query, "limit": limit}, &out)
	return out.Entries, err
}

// MetricSample is one sample from a metrics query. Value is the metric's value as a string so
// PromQL's exact numeric formatting is preserved.
type MetricSample struct {
	Labels map[string]string `json:"labels,omitempty"`
	Value  string            `json:"value"`
	Time   string            `json:"time,omitempty"`
}

// QueryMetrics runs an instant PromQL query against the connected metrics add-on (e.g. Prometheus).
func (c *Client) QueryMetrics(ctx context.Context, query string) ([]MetricSample, error) {
	var out struct {
		Samples []MetricSample `json:"samples"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/metrics/query", map[string]any{"query": query}, &out)
	return out.Samples, err
}

func (c *Client) Logs(ctx context.Context, app string, tail int) ([]LogLine, error) {
	path := c.appPath(app, "logs")
	if tail > 0 {
		path += "?tail=" + strconv.Itoa(tail)
	}
	var out struct {
		Lines []LogLine `json:"lines"`
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Lines, err
}

func (c *Client) Rollback(ctx context.Context, app string) (RollbackResult, error) {
	var out RollbackResult
	err := c.do(ctx, http.MethodPost, c.appPath(app, "rollback"), nil, &out)
	return out, err
}

func (c *Client) Scale(ctx context.Context, app string, replicas int32, confirm bool) (ScaleResult, error) {
	var out ScaleResult
	body := map[string]any{"replicas": replicas, "confirm": confirm}
	err := c.do(ctx, http.MethodPost, c.appPath(app, "scale"), body, &out)
	return out, err
}

func (c *Client) Expose(ctx context.Context, app, host string, port int32, tls bool, issuer string, confirm bool) (ExposeResult, error) {
	var out ExposeResult
	body := map[string]any{"host": host, "port": port, "tls": tls, "issuer": issuer, "confirm": confirm}
	err := c.do(ctx, http.MethodPost, c.appPath(app, "expose"), body, &out)
	return out, err
}

func (c *Client) Unexpose(ctx context.Context, app string) error {
	return c.do(ctx, http.MethodPost, c.appPath(app, "unexpose"), nil, nil)
}

// Reachability reports whether an app is reachable at its hostname, link by link.
func (c *Client) Reachability(ctx context.Context, app string) (ReachabilityResult, error) {
	var out ReachabilityResult
	err := c.do(ctx, http.MethodGet, c.appPath(app, "reachability"), nil, &out)
	return out, err
}

func (c *Client) appPath(app, verb string) string {
	return "/v1/apps/" + url.PathEscape(app) + "/" + verb
}

// Guardrails lists the control-plane guardrails and their current dispositions.
func (c *Client) Guardrails(ctx context.Context) ([]Guardrail, error) {
	var out struct {
		Guardrails []Guardrail `json:"guardrails"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/guard", nil, &out)
	return out.Guardrails, err
}

// SetGuardrail sets a guardrail's disposition and returns the updated policy.
func (c *Client) SetGuardrail(ctx context.Context, code, disposition string) ([]Guardrail, error) {
	var out struct {
		Guardrails []Guardrail `json:"guardrails"`
	}
	body := map[string]string{"disposition": disposition}
	err := c.do(ctx, http.MethodPut, "/v1/guard/"+url.PathEscape(code), body, &out)
	return out.Guardrails, err
}

// AddProvider registers a vendor credential in the control-plane registry and returns the
// recorded provider (ADR-0023).
func (c *Client) AddProvider(ctx context.Context, req AddProviderRequest) (Provider, error) {
	var out Provider
	err := c.do(ctx, http.MethodPost, "/v1/providers", req, &out)
	return out, err
}

// Providers lists the configured providers, name order.
func (c *Client) Providers(ctx context.Context) ([]Provider, error) {
	var out struct {
		Providers []Provider `json:"providers"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/providers", nil, &out)
	return out.Providers, err
}

// AddDomain points host at an address through the named DNS provider (ADR-0018). Give either an
// explicit address or the name of an exposed app whose ingress address the control plane reads.
func (c *Client) AddDomain(ctx context.Context, host, provider, address, app string, confirm bool) (DomainResult, error) {
	var out DomainResult
	body := map[string]any{"host": host, "provider": provider, "address": address, "app": app, "confirm": confirm}
	err := c.do(ctx, http.MethodPost, "/v1/domains", body, &out)
	return out, err
}

// RemoveDomain removes the DNS record the provider holds for host.
func (c *Client) RemoveDomain(ctx context.Context, host, provider string, confirm bool) (DomainResult, error) {
	var out DomainResult
	path := "/v1/domains/" + url.PathEscape(host) + "?provider=" + url.QueryEscape(provider)
	if confirm {
		path += "&confirm=true"
	}
	err := c.do(ctx, http.MethodDelete, path, nil, &out)
	return out, err
}

// do issues a request, decoding a 2xx body into out and a non-2xx body into an APIError.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var br io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		br = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, br)
	if err != nil {
		return err
	}
	// Authenticate with X-Burrow-Token only — never Authorization. On the API-server
	// proxy path the kubeconfig transport authenticates to the API server via the
	// Authorization header, and client-go does not overwrite an Authorization header that
	// is already set, so setting it here would block the kubeconfig credential and the API
	// server would reject the request. burrowd reads X-Burrow-Token, which the proxy
	// forwards untouched; the direct/ingress path works the same way (ADR-0015).
	req.Header.Set("X-Burrow-Token", c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("control plane request: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	if resp.StatusCode/100 != 2 {
		var e struct {
			Error             string `json:"error"`
			Code              string `json:"code"`
			NeedsConfirmation bool   `json:"needs_confirmation"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Error
		if msg == "" {
			if msg = strings.TrimSpace(string(data)); msg == "" {
				msg = resp.Status
			}
		}
		return &APIError{StatusCode: resp.StatusCode, Code: e.Code, Message: msg, NeedsConfirmation: e.NeedsConfirmation}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}
