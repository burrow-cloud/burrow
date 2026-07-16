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
// API's JSON contract; the MCP layer deliberately does not import the
// control-plane packages, so it stays a decoupled client across the module
// boundary (LICENSING.md).
//
// The Client is auth-agnostic (ADR-0045): it holds no credential and sets no auth header.
// Authentication is the job of the supplied *http.Client's RoundTripper — for self-host that
// is NewTokenRoundTripper, which adds X-Burrow-Token (ADR-0015). This lets a Transport swap in
// a different credential scheme while reusing the request methods unchanged.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a control-plane API client for baseURL authenticating with token over
// X-Burrow-Token, using a default HTTP client whose transport is NewTokenRoundTripper. It sends no
// client-version header; use NewClientVersion to include the ADR-0039 handshake.
func NewClient(baseURL, token string) *Client {
	return NewClientVersion(baseURL, token, "")
}

// NewClientVersion is NewClient plus the ADR-0039 client-version handshake: it sends clientVersion
// in X-Burrow-Client-Version on every request so burrowd can turn version skew into an actionable
// error rather than an opaque one. An empty clientVersion behaves exactly like NewClient. It is the
// constructor the direct-URL transport and the MCP server's direct path use, passing the binary's
// own release version.
func NewClientVersion(baseURL, token, clientVersion string) *Client {
	hc := &http.Client{
		Timeout:   60 * time.Second,
		Transport: NewTokenRoundTripper(token, clientVersion, nil),
	}
	return NewClientWithHTTP(baseURL, hc)
}

// NewClientWithHTTP builds a client on the supplied *http.Client, which owns authentication
// through its RoundTripper. The connect package uses this to route requests through the
// Kubernetes API-server proxy with a kubeconfig-authenticated transport wrapped in
// NewTokenRoundTripper (ADR-0014). A nil hc gets a default, unauthenticated client.
func NewClientWithHTTP(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
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

// DeployRequest carries a deploy's code-free metadata. The non-secret config is deliberately absent:
// an app's config is an independently-managed store, set with SetConfig and sourced at apply time
// rather than passed per deploy (ADR-0028). Env names the target environment (ADR-0035 phase 2b):
// empty or "default" targets the default environment's namespace, a registered name targets that
// environment's namespace.
type DeployRequest struct {
	Env         string   `json:"env,omitempty"`
	Image       string   `json:"image"`
	Command     []string `json:"command,omitempty"`
	MetricsPort int32    `json:"metrics_port,omitempty"`
	Replicas    int32    `json:"replicas"`
	Confirm     bool     `json:"confirm,omitempty"`
}

// SourceRef names the git source an in-cluster build clones and checks out inside the cluster
// (ADR-0053 §3): a repository URL plus the commit or tag to build. It is the only thing a build
// carries over the control channel — never source bytes (ADR-0004). The field names are capitalized
// to match the control-plane's SourceRef JSON shape, which carries no struct tags.
type SourceRef struct {
	Repo string `json:"Repo"`
	Ref  string `json:"Ref"`
}

// BuildRequest describes an in-cluster build-then-deploy (ADR-0053): the git source to clone and
// build inside the cluster and the target image reference the built image is pushed to. On success
// the built image rejoins the guarded deploy path, so a build is a front-end that ends where deploy
// begins. Env names the target environment (ADR-0035); empty or "default" targets the default
// environment. TargetImage is required in this phase (the in-cluster registry default is a later
// phase); Confirm acknowledges the app.deploy guardrail so a held deploy proceeds.
type BuildRequest struct {
	Env         string    `json:"env,omitempty"`
	Source      SourceRef `json:"source"`
	TargetImage string    `json:"target_image"`
	Confirm     bool      `json:"confirm,omitempty"`
}

// BuildResult reports the outcome of a successful build-then-deploy (ADR-0053 §4): the digest of the
// image the builder produced and the deploy that shipped it. Because the build ends where deploy
// begins, Deploy carries the same release, rollback handle, and hints an explicit deploy returns.
type BuildResult struct {
	Digest string       `json:"digest"`
	Deploy DeployResult `json:"deploy"`
}

// RunRequest is a one-off command to run in an app's own current image and environment (ADR-0048).
// Command is the argv (non-empty); TTLSeconds overrides the finished-Job TTL (nil applies the
// default of one hour, 0 deletes it as soon as the output is captured); Confirm acknowledges the
// app.run guardrail so a held run proceeds.
type RunRequest struct {
	Env        string   `json:"env,omitempty"`
	Command    []string `json:"command"`
	TTLSeconds *int32   `json:"ttl_seconds,omitempty"`
	Confirm    bool     `json:"confirm,omitempty"`
}

// RunResult reports the outcome of a one-off command (ADR-0048). A non-zero ExitCode is a normal
// structured outcome, not a transport error. Stdout carries the command's captured output (Kubernetes
// interleaves stdout and stderr into one stream); Stderr is reserved for a future separation.
type RunResult struct {
	App      string `json:"app"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

type Release struct {
	ID          string            `json:"id"`
	App         string            `json:"app"`
	Environment string            `json:"environment,omitempty"`
	Image       string            `json:"image"`
	Digest      string            `json:"digest,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Replicas    int32             `json:"replicas"`
	Status      string            `json:"status"`
	Supersedes  string            `json:"supersedes,omitempty"`
	// Trigger is how the deploy was triggered (ADR-0052 §5): "manual" for an explicit CLI or agent
	// deploy, "auto" for the pull-based passive watcher. AutoLevel and AutoTag are set only for auto.
	Trigger   string    `json:"trigger,omitempty"`
	AutoLevel string    `json:"auto_level,omitempty"`
	AutoTag   string    `json:"auto_tag,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type WorkloadStatus struct {
	App             string `json:"app"`
	Kind            string `json:"kind"`
	Image           string `json:"image"`
	DesiredReplicas int32  `json:"desired_replicas"`
	ReadyReplicas   int32  `json:"ready_replicas"`
	UpdatedReplicas int32  `json:"updated_replicas"`
	Available       bool   `json:"available"`
	// Issue is a human- and agent-actionable explanation of why an unavailable workload is
	// blocked (e.g. an image the cluster cannot pull, naming the registry and the
	// `burrow config registry login` fix); empty when the workload is healthy. IssueReason is the
	// raw Kubernetes reason behind it (e.g. "ImagePullBackOff"). See ADR-0006.
	Issue       string `json:"issue,omitempty"`
	IssueReason string `json:"issue_reason,omitempty"`
}

type DeployResult struct {
	Release             Release `json:"release"`
	SupersededReleaseID string  `json:"superseded_release_id,omitempty"`
	// Hints are non-blocking notes about the deploy (ADR-0052 §8): today, a nudge toward semver when
	// the deployed tag cannot be classified for auto-update. They never gate the deploy.
	Hints []string `json:"hints,omitempty"`
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

// AutoscaleRequest carries a desired autoscaling shape for an app (ADR-0006): the replica band and
// the CPU (and optional memory) utilization targets, plus the target environment. Env names the
// environment whose namespace the app lives in (ADR-0035 phase 2b).
type AutoscaleRequest struct {
	Env     string `json:"env,omitempty"`
	Min     int32  `json:"min"`
	Max     int32  `json:"max"`
	CPU     int32  `json:"cpu"`
	Memory  int32  `json:"memory,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

// AutoscaleResult reports the applied autoscaling shape, the app and environment it acted in, and
// whether metrics-server is present. When it is absent, MetricsAvailable is false and Warning
// explains the autoscaler is set but will not scale until metrics-server is installed.
type AutoscaleResult struct {
	App              string `json:"app"`
	Env              string `json:"env,omitempty"`
	MinReplicas      int32  `json:"min_replicas"`
	MaxReplicas      int32  `json:"max_replicas"`
	CPUPercent       int32  `json:"cpu_percent"`
	MemoryPercent    int32  `json:"memory_percent,omitempty"`
	MetricsAvailable bool   `json:"metrics_available"`
	Warning          string `json:"warning,omitempty"`
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
	CertReady          bool     `json:"cert_ready"`
	DNSPointsAtCluster bool     `json:"dns_points_at_cluster"`
	DNSAddresses       []string `json:"dns_addresses,omitempty"`
	Reachable          bool     `json:"reachable"`
	URL                string   `json:"url,omitempty"`
	BlockedOn          string   `json:"blocked_on,omitempty"`
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
	// Source reports where a guardrail's effective disposition came from when listed for a named
	// environment (ADR-0035 phase 2c): "env" (an environment-specific override), "global" (the global
	// policy), or "default" (the built-in default). It is empty in the global listing.
	Source string `json:"source,omitempty"`
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

// AddProviderRequest registers a vendor credential. The token VALUE travels in this request over
// burrowd's authenticated, TLS-protected control-plane API; burrowd validates it and writes it into
// the burrow-credentials Secret (ADR-0030). The value is never logged, never stored in Postgres,
// never echoed back, and still never carried over MCP — provider add is a human/CLI operation.
type AddProviderRequest struct {
	Name      string `json:"name,omitempty"`
	Type      string `json:"type"`
	SecretKey string `json:"secret_key,omitempty"`
	Token     string `json:"token,omitempty"`
}

// DomainResult mirrors the control plane's DNS-record outcome (ADR-0018).
type DomainResult struct {
	Host     string `json:"host"`
	Provider string `json:"provider"`
	Type     string `json:"type,omitempty"`
	Address  string `json:"address,omitempty"`
}

// AuditEntry mirrors a control-plane audit row (ADR-0027): a guarded mutating operation and the
// guardrail decision and outcome that applied. Args is redacted at the source — it carries only
// safe metadata (names, image reference, replica count, env/secret key NAMES), never a value.
type AuditEntry struct {
	ID            int64             `json:"id,omitempty"`
	Timestamp     time.Time         `json:"timestamp"`
	Operation     string            `json:"operation"`
	Target        string            `json:"target,omitempty"`
	Args          map[string]string `json:"args,omitempty"`
	GuardrailCode string            `json:"guardrail_code,omitempty"`
	Disposition   string            `json:"disposition,omitempty"`
	Outcome       string            `json:"outcome"`
	Result        string            `json:"result,omitempty"`
	Caller        string            `json:"caller,omitempty"`
	// Principal is the acting identity (the actor), distinct from Caller (the control-plane
	// boundary). The json tag must match the engine's AuditEntry.Principal tag exactly — the two
	// structs serialize/deserialize across the API, and a mismatched tag would silently drop the
	// field (ADR-0038).
	Principal string `json:"principal,omitempty"`
	// ClientVersion is the release version of the client that drove the operation, from the
	// X-Burrow-Client-Version handshake (ADR-0039). Empty for a pre-handshake client. The json tag
	// must match the engine's AuditEntry.ClientVersion tag exactly, or the field would silently drop.
	ClientVersion string `json:"client_version,omitempty"`
}

// AuditFilter narrows an audit query. A zero value lists the latest rows across all apps.
type AuditFilter struct {
	App       string
	Operation string
	Outcome   string
	Limit     int
}

// Audit lists audit rows newest-first, optionally filtered by app, operation, and outcome
// (ADR-0027). It is read-only — the audit log has no write or delete path through the API.
func (c *Client) Audit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	q := url.Values{}
	if f.App != "" {
		q.Set("app", f.App)
	}
	if f.Operation != "" {
		q.Set("operation", f.Operation)
	}
	if f.Outcome != "" {
		q.Set("outcome", f.Outcome)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	path := "/v1/audit"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out struct {
		Entries []AuditEntry `json:"entries"`
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Entries, err
}

// ClusterCapabilities mirrors the control plane's neutral, read-only report of what the cluster
// can do (ADR-0034): an ingress controller and its IngressClass, a default StorageClass,
// LoadBalancer support, cert-manager, the cloud provider, and whether a DNS provider is configured.
// It carries no secret value.
type ClusterCapabilities struct {
	Ingress      IngressCapability      `json:"ingress"`
	Storage      StorageCapability      `json:"storage"`
	LoadBalancer LoadBalancerCapability `json:"load_balancer"`
	CertManager  CertManagerCapability  `json:"cert_manager"`
	Provider     ProviderCapability     `json:"provider"`
	DNS          DNSCapability          `json:"dns"`
}

// IngressCapability reports the ingress-controller situation. Present is true only when an ingress
// controller is actually running (not merely when an IngressClass exists — a cluster-scoped class
// can outlive its controller); Classes are the IngressClass names, reported independently of Present.
type IngressCapability struct {
	Present bool     `json:"present"`
	Classes []string `json:"classes,omitempty"`
}

// StorageCapability reports the default-StorageClass situation.
type StorageCapability struct {
	DefaultPresent bool     `json:"default_present"`
	DefaultClass   string   `json:"default_class,omitempty"`
	Classes        []string `json:"classes,omitempty"`
}

// LoadBalancerCapability reports whether Service type=LoadBalancer is likely supported and by what:
// a cloud provider (billable), k3s's servicelb, or MetalLB. Provider names the mechanism (a cloud
// id, "servicelb", or "metallb"), empty when none is detected.
type LoadBalancerCapability struct {
	Supported bool   `json:"supported"`
	Inferred  bool   `json:"inferred"`
	Provider  string `json:"provider,omitempty"`
}

// CertManagerCapability reports whether cert-manager is installed (detected via its API group).
type CertManagerCapability struct {
	Present bool `json:"present"`
}

// ProviderCapability reports the detected cloud provider.
type ProviderCapability struct {
	Cloud string `json:"cloud,omitempty"`
	Name  string `json:"name,omitempty"`
}

// DNSCapability reports whether a DNS provider is configured in the registry (ADR-0023).
type DNSCapability struct {
	Configured bool     `json:"configured"`
	Providers  []string `json:"providers,omitempty"`
}

// Cluster reports the cluster's capabilities, read live (ADR-0034). It is read-only — it changes
// nothing and carries no secret value.
func (c *Client) Cluster(ctx context.Context) (ClusterCapabilities, error) {
	var out ClusterCapabilities
	err := c.do(ctx, http.MethodGet, "/v1/cluster", nil, &out)
	return out, err
}

// CapacityReport mirrors the control plane's cluster capacity/headroom surface (issue #275): per
// node and cluster-total allocatable / committed (sum of pod requests) / free headroom, the top CPU
// and memory consumers, and a plain-language verdict on whether a typical in-cluster build fits and
// whether another node is needed. It is scheduling headroom from the Kubernetes API alone — no
// metrics-server. CPU figures are milli-CPU (1000 = one core); memory figures are bytes. It carries
// no secret value.
type CapacityReport struct {
	Nodes           []NodeCapacity `json:"nodes"`
	Cluster         NodeCapacity   `json:"cluster"`
	TopCPU          []Consumer     `json:"top_cpu"`
	TopMemory       []Consumer     `json:"top_memory"`
	BuildCPUMillis  int64          `json:"build_cpu_millis"`
	BuildMemBytes   int64          `json:"build_mem_bytes"`
	BuildFits       bool           `json:"build_fits"`
	BuildFitsNode   string         `json:"build_fits_node,omitempty"`
	Verdict         string         `json:"verdict"`
	UtilizationNote string         `json:"utilization_note"`
}

// NodeCapacity is the allocatable / committed / free-headroom breakdown for one node, or the
// cluster-wide total when Name is empty. CPU figures are milli-CPU; memory figures are bytes.
type NodeCapacity struct {
	Name           string `json:"name,omitempty"`
	Pods           int    `json:"pods"`
	AllocCPUMillis int64  `json:"alloc_cpu_millis"`
	UsedCPUMillis  int64  `json:"committed_cpu_millis"`
	FreeCPUMillis  int64  `json:"free_cpu_millis"`
	AllocMemBytes  int64  `json:"alloc_mem_bytes"`
	UsedMemBytes   int64  `json:"committed_mem_bytes"`
	FreeMemBytes   int64  `json:"free_mem_bytes"`
}

// Consumer is one pod's contribution (its resource request, not live usage) to the committed total,
// for the top-consumers lists. CPUMillis is milli-CPU; MemBytes is bytes.
type Consumer struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Node      string `json:"node,omitempty"`
	CPUMillis int64  `json:"cpu_millis"`
	MemBytes  int64  `json:"mem_bytes"`
}

// Capacity reports the cluster's scheduling capacity and headroom, read live (issue #275). It is
// read-only — it changes nothing and carries no secret value.
func (c *Client) Capacity(ctx context.Context) (CapacityReport, error) {
	var out CapacityReport
	err := c.do(ctx, http.MethodGet, "/v1/cluster/capacity", nil, &out)
	return out, err
}

func (c *Client) Deploy(ctx context.Context, app string, req DeployRequest) (DeployResult, error) {
	var out DeployResult
	err := c.do(ctx, http.MethodPost, c.appPath(app, "deploy"), req, &out)
	return out, err
}

// Build builds an app's image from a git source reference inside the cluster and, on success, hands
// the resulting digest-pinned reference into the guarded deploy path (ADR-0053): the returned
// BuildResult carries the built digest and the deploy that shipped it. It is gated by the app.deploy
// guardrail — a held deploy returns a guardrail error the caller surfaces for confirmation, re-invoking
// with Confirm set only on explicit human approval.
func (c *Client) Build(ctx context.Context, app string, req BuildRequest) (BuildResult, error) {
	var out BuildResult
	err := c.do(ctx, http.MethodPost, c.appPath(app, "build"), req, &out)
	return out, err
}

func (c *Client) Status(ctx context.Context, app, env string) (StatusResult, error) {
	var out StatusResult
	err := c.do(ctx, http.MethodGet, withEnv(c.appPath(app, "status"), env), nil, &out)
	return out, err
}

// History returns an app's deploy timeline: the releases recorded for it, newest first — what
// versions the app has been rolled to, when, and whether each landed (the release Status conveys
// success or failure). It is read-only; the release records have no write or delete path through
// this client. env names the target environment (ADR-0035 phase 2b); empty is the default.
func (c *Client) History(ctx context.Context, app, env string) ([]Release, error) {
	var out struct {
		Releases []Release `json:"releases"`
	}
	err := c.do(ctx, http.MethodGet, withEnv(c.appPath(app, "history"), env), nil, &out)
	return out.Releases, err
}

// Run executes a one-off command in an app's own current image and environment (ADR-0048). It returns
// a structured result carrying the command's captured output and exit code; a non-zero exit is a
// normal outcome, not an error. It is gated by the app.run guardrail (confirm by default): a held run
// returns a guardrail error the caller surfaces for confirmation, re-invoking with Confirm set only
// on explicit human approval.
func (c *Client) Run(ctx context.Context, app string, req RunRequest) (RunResult, error) {
	var out RunResult
	err := c.do(ctx, http.MethodPost, c.appPath(app, "run"), req, &out)
	return out, err
}

// Apps lists the workload status of every Burrow-managed app in the target environment (ADR-0035
// phase 2b). An empty env lists the default environment's namespace.
func (c *Client) Apps(ctx context.Context, env string) ([]WorkloadStatus, error) {
	var out struct {
		Apps []WorkloadStatus `json:"apps"`
	}
	err := c.do(ctx, http.MethodGet, withEnv("/v1/apps", env), nil, &out)
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
// token lives; token is the bearer token VALUE for an authenticated backend, which travels over
// burrowd's authenticated, TLS-protected API and is written to the Secret (ADR-0030) — never logged,
// never stored in Postgres, never echoed back, never over MCP. Pass an empty token (and empty
// secretKey) for an unauthenticated backend.
func (c *Client) ConnectAddon(ctx context.Context, backend, endpoint, secretKey, token string) (Addon, error) {
	var out Addon
	body := map[string]any{"backend": backend, "endpoint": endpoint, "secret_key": secretKey, "token": token}
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

// AttachResult is the non-secret outcome of attaching an app to an add-on (ADR-0031): the KEY NAME
// the generated connection string was written under in the app's Secret — never the value.
type AttachResult struct {
	App       string `json:"app"`
	Addon     string `json:"addon"`
	SecretKey string `json:"secret_key"`
}

// AttachAddon gives an app its own database on the installed Postgres add-on and wires it in
// (ADR-0031). The agent supplies only the add-on type and app name; burrowd generates the
// DATABASE_URL server-side and writes it into the app's Secret — no secret value crosses this API
// or MCP. The result carries the KEY name only, never the value.
func (c *Client) AttachAddon(ctx context.Context, addonType, app string) (AttachResult, error) {
	var out AttachResult
	body := map[string]any{"addon": addonType, "app": app}
	err := c.do(ctx, http.MethodPost, "/v1/addons/attach", body, &out)
	return out, err
}

// DetachAddon detaches an app from an add-on, dropping its data (e.g. its Postgres database). It is
// held for confirmation by a guardrail by default; pass confirm=true to proceed past the hold.
func (c *Client) DetachAddon(ctx context.Context, addonType, app string, confirm bool) error {
	body := map[string]any{"addon": addonType, "app": app, "confirm": confirm}
	return c.do(ctx, http.MethodPost, "/v1/addons/detach", body, nil)
}

// Backup is one recorded per-app database backup (ADR-0032): the control-plane index row for a dump
// on the backup PVC. It names the app, the on-PVC path, the size, and the status — never a credential.
type Backup struct {
	ID        string `json:"id"`
	App       string `json:"app"`
	CreatedAt string `json:"created_at"`
	Path      string `json:"path,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Status    string `json:"status"`
}

// BackupResult is the outcome of an on-demand backup (ADR-0032): the recorded backup row.
type BackupResult struct {
	Backup Backup `json:"backup"`
}

// BackupAddon backs up an app's database on the installed Postgres add-on (ADR-0032). burrowd runs
// an in-cluster Job that pg_dumps to the backup PVC and records the backup; no secret value crosses
// this API. The result is the recorded backup (id, app, path, size, status), never a credential.
func (c *Client) BackupAddon(ctx context.Context, addonType, app string) (BackupResult, error) {
	var out BackupResult
	body := map[string]any{"addon": addonType, "app": app}
	err := c.do(ctx, http.MethodPost, "/v1/addons/backup", body, &out)
	return out, err
}

// Backups lists recorded backups from the control-plane database (ADR-0032). An empty app lists
// every app's backups; a non-empty app restricts to that app. Read-only; no secret value.
func (c *Client) Backups(ctx context.Context, addonType, app string) ([]Backup, error) {
	var out struct {
		Backups []Backup `json:"backups"`
	}
	path := "/v1/addons/backups?addon=" + url.QueryEscape(addonType)
	if app != "" {
		path += "&app=" + url.QueryEscape(app)
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Backups, err
}

// RestoreAddon restores an app's database from a recorded backup, overwriting its live contents
// (ADR-0032). It is held for confirmation by a guardrail by default; pass confirm=true to proceed.
func (c *Client) RestoreAddon(ctx context.Context, addonType, app, backupID string, confirm bool) error {
	body := map[string]any{"addon": addonType, "app": app, "backup": backupID, "confirm": confirm}
	return c.do(ctx, http.MethodPost, "/v1/addons/restore", body, nil)
}

// LogEntry is one record from a logs query.
type LogEntry struct {
	Time    string `json:"time,omitempty"`
	Message string `json:"message"`
	Pod     string `json:"pod,omitempty"`
}

// QueryLogs queries the installed logs add-on with a LogsQL query (empty matches everything). A
// non-empty backend targets a specific logs add-on (by its concrete backend or registry name) when
// more than one serves the logs capability; empty picks the first.
func (c *Client) QueryLogs(ctx context.Context, query string, limit int, backend string) ([]LogEntry, error) {
	var out struct {
		Entries []LogEntry `json:"entries"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/logs/query", map[string]any{"query": query, "limit": limit, "backend": backend}, &out)
	return out.Entries, err
}

// MetricSample is one sample from a metrics query. Value is the metric's value as a string so
// PromQL's exact numeric formatting is preserved.
type MetricSample struct {
	Labels map[string]string `json:"labels,omitempty"`
	Value  string            `json:"value"`
	Time   string            `json:"time,omitempty"`
}

// QueryMetrics runs an instant PromQL query against the connected metrics add-on (e.g. Prometheus). A
// non-empty backend targets a specific metrics add-on (by its concrete backend or registry name) when
// more than one serves the metrics capability; empty picks the first.
func (c *Client) QueryMetrics(ctx context.Context, query string, backend string) ([]MetricSample, error) {
	var out struct {
		Samples []MetricSample `json:"samples"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/metrics/query", map[string]any{"query": query, "backend": backend}, &out)
	return out.Samples, err
}

func (c *Client) Logs(ctx context.Context, app, env string, tail int) ([]LogLine, error) {
	path := c.appPath(app, "logs")
	if tail > 0 {
		path += "?tail=" + strconv.Itoa(tail)
	}
	var out struct {
		Lines []LogLine `json:"lines"`
	}
	err := c.do(ctx, http.MethodGet, withEnv(path, env), nil, &out)
	return out.Lines, err
}

// DeleteApp removes an app entirely — its workload, routing, and release history — in the target
// environment (ADR-0035 phase 2b). The delete is guarded and held for confirmation by default; pass
// confirm=true to proceed past the hold.
func (c *Client) DeleteApp(ctx context.Context, app, env string, confirm bool) error {
	path := "/v1/apps/" + url.PathEscape(app)
	if confirm {
		path += "?confirm=true"
	}
	return c.do(ctx, http.MethodDelete, withEnv(path, env), nil, nil)
}

func (c *Client) Rollback(ctx context.Context, app, env string, confirm bool) (RollbackResult, error) {
	var out RollbackResult
	path := c.appPath(app, "rollback")
	if confirm {
		path += "?confirm=true"
	}
	err := c.do(ctx, http.MethodPost, withEnv(path, env), nil, &out)
	return out, err
}

func (c *Client) Scale(ctx context.Context, app, env string, replicas int32, confirm bool) (ScaleResult, error) {
	var out ScaleResult
	body := map[string]any{"env": env, "replicas": replicas, "confirm": confirm}
	err := c.do(ctx, http.MethodPost, c.appPath(app, "scale"), body, &out)
	return out, err
}

// Autoscale configures autoscaling for an app: it applies a HorizontalPodAutoscaler on the app's
// Deployment with the requested replica band and utilization targets (ADR-0006). The result reports
// the applied shape and, when metrics-server is absent, a warning that the autoscaler will not scale
// until it is installed.
func (c *Client) Autoscale(ctx context.Context, app string, req AutoscaleRequest) (AutoscaleResult, error) {
	var out AutoscaleResult
	body := map[string]any{"env": req.Env, "min": req.Min, "max": req.Max, "cpu": req.CPU, "memory": req.Memory, "confirm": req.Confirm}
	err := c.do(ctx, http.MethodPost, c.appPath(app, "autoscale"), body, &out)
	return out, err
}

// DisableAutoscale turns autoscaling off for an app by removing its HorizontalPodAutoscaler
// (ADR-0006). It is idempotent: removing autoscaling from an app that has none succeeds.
func (c *Client) DisableAutoscale(ctx context.Context, app, env string, confirm bool) error {
	path := c.appPath(app, "autoscale")
	if confirm {
		path += "?confirm=true"
	}
	return c.do(ctx, http.MethodDelete, withEnv(path, env), nil, nil)
}

func (c *Client) Expose(ctx context.Context, app, env, host string, port int32, tls bool, issuer string, confirm bool) (ExposeResult, error) {
	var out ExposeResult
	body := map[string]any{"env": env, "host": host, "port": port, "tls": tls, "issuer": issuer, "confirm": confirm}
	err := c.do(ctx, http.MethodPost, c.appPath(app, "expose"), body, &out)
	return out, err
}

func (c *Client) Unexpose(ctx context.Context, app, env string) error {
	return c.do(ctx, http.MethodPost, withEnv(c.appPath(app, "unexpose"), env), nil, nil)
}

// Reachability reports whether an app is reachable at its hostname, link by link, in the target
// environment (ADR-0035 phase 2b).
func (c *Client) Reachability(ctx context.Context, app, env string) (ReachabilityResult, error) {
	var out ReachabilityResult
	err := c.do(ctx, http.MethodGet, withEnv(c.appPath(app, "reachability"), env), nil, &out)
	return out, err
}

// ReachabilityPollInterval is how often WaitReachable re-checks reachability while polling.
const ReachabilityPollInterval = 3 * time.Second

// WaitReachable polls Reachability until the app converges to live (Reachable) or timeout
// elapses, then returns the last verdict. It is the thin-client wait-until-live behind
// `burrow app reachability --wait` and the burrow_reachability MCP tool's wait mode; the
// control-plane engine stays point-in-time, so the polling and the clock live here, never in
// the engine (ADR-0034 slice 3).
//
// A returned result with Reachable true means the app is live at result.URL; a returned result
// with Reachable false means the timeout elapsed and result.BlockedOn names the link to fix.
// after supplies the poll clock as a one-shot timer channel so tests can drive the loop without
// real time; pass nil for the real clock (time.After).
func (c *Client) WaitReachable(ctx context.Context, app, env string, timeout time.Duration, after func(time.Duration) <-chan time.Time) (ReachabilityResult, error) {
	if after == nil {
		after = time.After
	}
	interval := ReachabilityPollInterval
	if interval > timeout {
		interval = timeout
	}
	res, err := c.Reachability(ctx, app, env)
	if err != nil || res.Reachable {
		return res, err
	}
	// remaining is a logical countdown decremented by each poll interval, so convergence and
	// timeout are deterministic without reading a wall clock here.
	for remaining := timeout; remaining > 0; remaining -= interval {
		wait := interval
		if wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-after(wait):
		}
		res, err = c.Reachability(ctx, app, env)
		if err != nil || res.Reachable {
			return res, err
		}
	}
	return res, nil
}

// SetConfig upserts one non-secret config var for an app (ADR-0028). By default the running workload
// rolls so the app picks the value up; noRestart only persists, landing the change on the next
// deploy.
func (c *Client) SetConfig(ctx context.Context, app, env, key, value string, noRestart bool) error {
	body := map[string]any{"env": env, "key": key, "value": value, "no_restart": noRestart}
	return c.do(ctx, http.MethodPost, c.appPath(app, "config"), body, nil)
}

// UnsetConfig removes one config var for an app (ADR-0028). By default the running workload rolls; with
// noRestart the removal only persists and lands on the next deploy. env names the target environment
// (ADR-0035 phase 2b).
func (c *Client) UnsetConfig(ctx context.Context, app, env, key string, noRestart bool) error {
	path := "/v1/apps/" + url.PathEscape(app) + "/config/" + url.PathEscape(key)
	if noRestart {
		path += "?no_restart=true"
	}
	return c.do(ctx, http.MethodDelete, withEnv(path, env), nil, nil)
}

// Config returns the app's non-secret config store (ADR-0028). env names the target environment
// (ADR-0035 phase 2b).
func (c *Client) Config(ctx context.Context, app, env string) (map[string]string, error) {
	var out struct {
		Config map[string]string `json:"config"`
	}
	err := c.do(ctx, http.MethodGet, withEnv(c.appPath(app, "config"), env), nil, &out)
	return out.Config, err
}

// SetSecret upserts one secret key=value for an app (ADR-0029). The value travels over burrowd's
// authenticated, TLS-protected control-plane API, which writes it to the per-app Kubernetes
// Secret; it is never logged, never stored in Postgres, and is still never carried over MCP (there
// is no secret-set MCP tool). By default the running workload rolls so it picks the value up; with
// noRestart the change only persists and lands on the next deploy.
func (c *Client) SetSecret(ctx context.Context, app, env, key, value string, noRestart bool) error {
	body := map[string]any{"env": env, "key": key, "value": value, "no_restart": noRestart}
	return c.do(ctx, http.MethodPost, c.appPath(app, "secrets"), body, nil)
}

// Secrets returns the KEYS in an app's per-app Secret, never the values (ADR-0028/0004). Secret
// values live only in the Kubernetes Secret; a list reads keys only and never returns a value. env
// names the target environment (ADR-0035 phase 2b), whose namespace holds the per-app Secret.
func (c *Client) Secrets(ctx context.Context, app, env string) ([]string, error) {
	var out struct {
		Keys []string `json:"keys"`
	}
	err := c.do(ctx, http.MethodGet, withEnv(c.appPath(app, "secrets"), env), nil, &out)
	return out.Keys, err
}

// UnsetSecret removes one key from an app's per-app Secret (ADR-0028). Removing a key carries no
// value, so it is allowed over the API/MCP. By default the running workload rolls so it drops the
// value; with noRestart the change only persists and lands on the next deploy. env names the target
// environment (ADR-0035 phase 2b).
func (c *Client) UnsetSecret(ctx context.Context, app, env, key string, noRestart bool) error {
	path := "/v1/apps/" + url.PathEscape(app) + "/secrets/" + url.PathEscape(key)
	if noRestart {
		path += "?no_restart=true"
	}
	return c.do(ctx, http.MethodDelete, withEnv(path, env), nil, nil)
}

func (c *Client) appPath(app, verb string) string {
	return "/v1/apps/" + url.PathEscape(app) + "/" + verb
}

// withEnv appends an env query parameter to path when env is non-empty, so an operation targets a
// named environment (ADR-0035 phase 2b). An empty env leaves the path unchanged and the server
// defaults to the default environment.
func withEnv(path, env string) string {
	if env == "" {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "env=" + url.QueryEscape(env)
}

// Guardrails lists the control-plane guardrails and their current dispositions. An empty env lists
// the global policy; a named environment lists its effective policy under the env to global to
// default fallback, each entry marking whether its disposition is env-specific or inherited
// (ADR-0035 phase 2c).
func (c *Client) Guardrails(ctx context.Context, env string) ([]Guardrail, error) {
	var out struct {
		Guardrails []Guardrail `json:"guardrails"`
	}
	err := c.do(ctx, http.MethodGet, withEnv("/v1/guard", env), nil, &out)
	return out.Guardrails, err
}

// SetGuardrail sets a guardrail's disposition and returns the updated policy. An empty env sets the
// global disposition; a named environment scopes it to that environment, storing the env-prefixed
// code (ADR-0035 phase 2c).
func (c *Client) SetGuardrail(ctx context.Context, env, code, disposition string) ([]Guardrail, error) {
	var out struct {
		Guardrails []Guardrail `json:"guardrails"`
	}
	body := map[string]string{"disposition": disposition}
	err := c.do(ctx, http.MethodPut, withEnv("/v1/guard/"+url.PathEscape(code), env), body, &out)
	return out.Guardrails, err
}

// AutoDeployResult is the auto-deploy configuration for an app in one environment (ADR-0052 §2):
// the app, the canonical environment name, and the effective auto-deploy level, plus the enriched
// read-only upgrade view a show returns (ADR-0052 §3) — the current running version, the tag
// auto-deploy would move to within the level, the highest available upgrade above the level's cap,
// whether the registry upgrade check ran, and a short note when it could not. The upgrade fields are
// omitempty, so a set (which reports the level only) carries just app/env/level.
type AutoDeployResult struct {
	App        string `json:"app"`
	Env        string `json:"env"`
	Level      string `json:"level"`
	Repository string `json:"repository,omitempty"`
	Current    string `json:"current,omitempty"`
	Target     string `json:"target,omitempty"`
	Upgrade    string `json:"upgrade,omitempty"`
	Checked    bool   `json:"checked,omitempty"`
	Note       string `json:"note,omitempty"`
	// DisabledReason is why auto-deploy is off when the safety stop turned it off (ADR-0052 §5):
	// "disabled by rollback" or "disabled by downgrade". Empty when the level was human-set or is not off.
	DisabledReason string `json:"disabled_reason,omitempty"`
}

// AutoDeploy returns the auto-deploy level configured for app in env (ADR-0052 §2). An empty env
// reads the default environment. A missing configuration reads as the default level (minor).
func (c *Client) AutoDeploy(ctx context.Context, app, env string) (AutoDeployResult, error) {
	var out AutoDeployResult
	err := c.do(ctx, http.MethodGet, withEnv(c.appPath(app, "auto-deploy"), env), nil, &out)
	return out, err
}

// SetAutoDeploy sets the auto-deploy level for app in env and returns the updated configuration
// (ADR-0052 §6). Setting the level is a human operator action, so it lives on this admin client
// only — there is no agent verb for it.
func (c *Client) SetAutoDeploy(ctx context.Context, app, env, level string) (AutoDeployResult, error) {
	var out AutoDeployResult
	body := map[string]string{"level": level}
	err := c.do(ctx, http.MethodPut, withEnv(c.appPath(app, "auto-deploy"), env), body, &out)
	return out, err
}

// NextTags are the suggested next release tags after a current semver tag (ADR-0052 §8).
type NextTags struct {
	Patch string `json:"patch"`
	Minor string `json:"minor"`
	Major string `json:"major"`
}

// NextTagResult is the read-only next-semver-tag suggestion for an app in one environment
// (ADR-0052 §8): the current running tag plus the suggested next patch/minor/major tags. When there
// is no running release or the current tag is not semver, Next is nil and Note carries a short human
// reason — the suggestion degrades gracefully rather than erroring (ADR-0040).
type NextTagResult struct {
	App     string    `json:"app"`
	Env     string    `json:"env"`
	Current string    `json:"current,omitempty"`
	Next    *NextTags `json:"next,omitempty"`
	Note    string    `json:"note,omitempty"`
}

// NextTag returns the suggested next semver release tags for app in env, from its current running tag
// (ADR-0052 §8). It is read-only: it reads the running tag the control plane already knows and
// computes the next patch/minor/major, so the agent can apply the number to its build. An empty env
// reads the default environment.
func (c *Client) NextTag(ctx context.Context, app, env string) (NextTagResult, error) {
	var out NextTagResult
	err := c.do(ctx, http.MethodGet, withEnv(c.appPath(app, "next-tag"), env), nil, &out)
	return out, err
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

// Environment mirrors a control-plane environment (ADR-0035 phase 2): a namespace-per-environment
// target. Name is the handle (a DNS-1123 label), Namespace the Kubernetes namespace its apps deploy
// into, and Default marks the implicit `default` environment (the app namespace burrowd runs
// against).
type Environment struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Default   bool   `json:"default"`
}

// AddEnvironment registers a named environment mapping name to namespace (ADR-0035 phase 2). The
// namespace and burrowd's Role there are created kubeconfig-side by `burrow env add` before this
// call; this records the registry entry. A duplicate name is rejected.
func (c *Client) AddEnvironment(ctx context.Context, name, namespace string) error {
	body := map[string]any{"name": name, "namespace": namespace}
	return c.do(ctx, http.MethodPost, "/v1/environments", body, nil)
}

// ListEnvironments lists the environments the cluster's burrowd knows about (ADR-0035 phase 2): the
// implicit `default` environment first, then the registered ones in name order.
func (c *Client) ListEnvironments(ctx context.Context) ([]Environment, error) {
	var out struct {
		Environments []Environment `json:"environments"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/environments", nil, &out)
	return out.Environments, err
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
	// Authentication is the http.Client's RoundTripper's job (ADR-0045): the self-host
	// transport wraps it in NewTokenRoundTripper, which adds X-Burrow-Token (ADR-0015). do
	// stays auth-agnostic and sets no credential header.
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
