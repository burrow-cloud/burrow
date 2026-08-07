// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is a thin HTTP client for the control-plane API (ADR-0005). The caller holds the API
// bearer token to authenticate to the control plane, but never any cluster credentials — those live
// only in the control plane. These DTOs mirror the API's JSON CONTRACT rather than the control
// plane's Go types, so the client stays decoupled across the module boundary (LICENSING.md): the one
// thing it takes from the control-plane package is the declared bound on how long a call may take
// (see timeouts below), because two independently chosen bounds is exactly the defect that made a
// successful deploy report as failed.
//
// The Client is auth-agnostic (ADR-0045): it holds no credential and sets no auth header.
// Authentication is the job of the supplied *http.Client's RoundTripper — for self-host that
// is NewTokenRoundTripper, which adds X-Burrow-Token (ADR-0015). This lets a Transport swap in
// a different credential scheme while reusing the request methods unchanged.
type Client struct {
	baseURL string
	http    *http.Client
	// budget is the per-request timeout table. It is a field rather than a package-level constant
	// read at the call site so a test can drive the loop with millisecond budgets instead of minutes.
	budget budgets
}

// NewClient returns a control-plane API client for baseURL authenticating with token over
// X-Burrow-Token, using a default HTTP client whose transport is NewTokenRoundTripper. It sends no
// client-version header; use NewClientVersion to include the ADR-0039 handshake.
func NewClient(baseURL, token string) *Client {
	return NewClientVersion(baseURL, token, "")
}

// NewClientVersion is NewClient plus the ADR-0039 client-version handshake: it sends clientVersion
// in X-Burrow-Client-Version on every request so burrowd can turn version skew into an actionable
// error rather than an opaque one. An empty clientVersion behaves exactly like NewClient. It sends
// no client NAME; a binary that knows which of Burrow's two clients it is should use NewNamedClient
// so a too-old refusal can name the binary the user must actually update.
func NewClientVersion(baseURL, token, clientVersion string) *Client {
	return NewNamedClient(baseURL, token, "", clientVersion)
}

// NewNamedClient is NewClientVersion plus the client-NAME half of the ADR-0039 handshake: it sends
// clientName (ClientNameCLI or ClientNameAgent) in X-Burrow-Client alongside the version. It is the
// constructor the direct-URL transport uses, passing the binary's own name and release version.
func NewNamedClient(baseURL, token, clientName, clientVersion string) *Client {
	// No http.Client.Timeout: a single blanket bound cannot tell a status read from a deploy that
	// waits for a rollout, and the one that was here — sixty seconds — was shorter than the deploy it
	// waited on. The bound is per request now; see budgets.
	hc := &http.Client{
		Transport: NewNamedTokenRoundTripper(token, clientName, clientVersion, nil),
	}
	return NewClientWithHTTP(baseURL, hc)
}

// NewClientWithHTTP builds a client on the supplied *http.Client, which owns authentication
// through its RoundTripper. The connect package uses this to route requests through the
// Kubernetes API-server proxy with a kubeconfig-authenticated transport wrapped in
// NewTokenRoundTripper (ADR-0014). A nil hc gets a default, unauthenticated client.
//
// The per-request budgets apply whatever transport is supplied, so both paths are bounded the same
// way: the API-server proxy path previously had no client-side bound at all, which made a hung
// request hang forever rather than misreport, and one place now decides how long a call may take.
// An hc that carries its OWN http.Client.Timeout keeps it, and it wins where it is shorter — that is
// the caller's deliberate choice, and it is the one way a bound too short for a deploy can still
// exist.
func NewClientWithHTTP(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    hc,
		budget:  derivedBudgets(),
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
	// ServerVersion is the control plane's release version, set on a CodeClientTooOld refusal so a
	// client can name the version it must reach in an install-aware remedy of its own (ADR-0039).
	ServerVersion string
	// ServerInstallID is the id of the install that answered, set on a CodeInstallMismatch refusal
	// (ADR-0084 §5). It is the counterpart of ServerVersion: the refusal message already names both
	// ids, and carrying the answering one structurally lets a caller re-point a target at the install
	// that is actually there without parsing prose.
	ServerInstallID string
}

// CodeClientTooOld is the machine-readable code burrowd returns when it refuses a client outside
// the compatibility window (ADR-0039). A client matches on it to replace the server's necessarily
// generic remedy with one it can establish locally — which binary it is, and where it is installed.
const CodeClientTooOld = "client_too_old"

// CodeUnknownOperation is the machine-readable code burrowd returns for a route it does not have
// (ADR-0039): a newer client calling a feature the server lacks, told as a structured refusal
// naming both versions and the upgrade rather than as a bare 404. A client matches on it to say
// which of ITS features the gap corresponds to, since the server can only name the route.
const CodeUnknownOperation = "unknown_operation"

// CodeInstallMismatch is the machine-readable code burrowd returns when the caller named an install
// id and this control plane is a different install (ADR-0084 §5) — the kube context resolved, the
// credential was accepted, and the Burrow on the other end is not the one the target was pointed at.
const CodeInstallMismatch = "install_mismatch"

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
// empty or "prod" targets the default environment's namespace, a name added later targets that
// environment's namespace.
type DeployRequest struct {
	Env         string   `json:"env,omitempty"`
	Image       string   `json:"image"`
	Command     []string `json:"command,omitempty"`
	MetricsPort int32    `json:"metrics_port,omitempty"`
	Replicas    int32    `json:"replicas"`
	Confirm     bool     `json:"confirm,omitempty"`
	// Progress receives the deploy's stages as the control plane reports them (issue #480). Setting
	// it is what asks for them: Deploy then negotiates the streaming response, and a control plane
	// that does not offer one is handled transparently. Nil — the default, and the shape that goes on
	// the wire, since json ignores this field — is the deploy this package has always issued.
	//
	// It is called from Deploy's own goroutine as each line arrives, so a slow reporter slows the
	// read. It is never called after Deploy returns.
	Progress func(DeployProgress) `json:"-"`
}

// DeployProgress is one stage transition of a running deploy or build: which stage the control plane
// is in, and what happened to it. Both are members of the control plane's closed vocabularies
// (controlplane.DeployStages or controlplane.BuildStages, and controlplane.DeployStatuses); a value
// outside them is a newer control plane's, and a caller renders it rather than failing on it.
type DeployProgress struct {
	Stage  string `json:"stage"`
	Status string `json:"status"`
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
// environment. TargetImage is optional: when empty, the build pushes to the in-cluster registry if one
// is installed (ADR-0054), else the server rejects it. Confirm acknowledges the app.deploy guardrail so
// a held deploy proceeds.
type BuildRequest struct {
	Env         string    `json:"env,omitempty"`
	Source      SourceRef `json:"source"`
	TargetImage string    `json:"target_image"`
	Confirm     bool      `json:"confirm,omitempty"`
	// Progress receives the build's stages as the control plane reports them (issue #503): the build's
	// own stages and then the deploy stages it hands off to, as one continuous sequence. Setting it is
	// what asks for them: Build then negotiates the streaming response, and a control plane that does
	// not offer one is handled transparently. Nil — the default, and the shape that goes on the wire,
	// since json ignores this field — is the build this package has always issued.
	//
	// It is called from Build's own goroutine as each line arrives, so a slow reporter slows the read.
	// It is never called after Build returns.
	Progress func(DeployProgress) `json:"-"`
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
	// blocked — an image the cluster cannot pull (naming the registry and the
	// `burrow config registry login` fix), a pod no node can run (naming the taint or the
	// resource), a container crash-looping (naming the exit code); empty when the workload is
	// healthy. IssueReason is the machine-usable reason behind it, a member of the closed set
	// controlplane.IssueReasons() enumerates (e.g. "ImagePullBackOff", "Unschedulable",
	// "CrashLoopBackOff"), for branching without parsing the prose. See ADR-0006, ADR-0074 §2.
	Issue       string `json:"issue,omitempty"`
	IssueReason string `json:"issue_reason,omitempty"`
}

type DeployResult struct {
	Release             Release `json:"release"`
	SupersededReleaseID string  `json:"superseded_release_id,omitempty"`
	// Hints are non-blocking notes about the deploy (ADR-0052 §8): today, a nudge toward semver when
	// the deployed tag cannot be classified for auto-update. They never gate the deploy.
	Hints []string `json:"hints,omitempty"`
	// Dependencies is what the deploy-time dependency check found (ADR-0076 §4): for each thing
	// Burrow provisioned for this app, whether the app could reach it from inside its own container.
	// A failed entry sits on a SUCCESSFUL deploy — the check is reported, never fatal.
	Dependencies []DependencyResult `json:"dependencies,omitempty"`
}

type StatusResult struct {
	App        string         `json:"app"`
	HasRelease bool           `json:"has_release"`
	Release    Release        `json:"release,omitempty"`
	Running    bool           `json:"running"`
	Workload   WorkloadStatus `json:"workload,omitempty"`
	// Failures is the app's recent failure history from the ledger (ADR-0074 §8), oldest first,
	// resolved episodes included. Workload above is the live present tense; this is the part
	// nothing can reconstruct afterwards — whether it crash-looped at 02:00 and recovered.
	Failures []Failure `json:"failures,omitempty"`
	// Coverage is what the observer was doing over that window. An empty Failures list means
	// "nothing broke" only if Coverage says something was watching.
	Coverage Coverage `json:"coverage"`
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
	// Hints are the control plane's non-blocking notes about the rollback: that a `pre-rollback` hook
	// was skipped and which command did not run (ADR-0080 §4), and what a `post-deploy` hook made of
	// the rollout (ADR-0072 §4). The field must exist here or the notes are decoded away before any
	// caller sees them — a skip that is reported to nobody is the silence the flag exists to avoid.
	Hints []string `json:"hints,omitempty"`
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

// LogLine is a single line of application log output. Timestamp is the instant the cluster
// recorded the line, in UTC, and is zero only when no time could be read for it; Message is the
// application's own output with that timestamp stripped off.
type LogLine struct {
	Pod       string    `json:"pod"`
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
}

type Guardrail struct {
	Code        string `json:"code"`
	Disposition string `json:"disposition"`
	Description string `json:"description"`
	// Source reports where a guardrail's effective disposition came from when listed for something
	// narrower than the whole cluster (ADR-0035 phase 2c, ADR-0085 §2): "name" (set for the one app
	// or add-on instance asked about), "env" (an environment-specific override), "global" (the
	// global policy), or "default" (the built-in default). It is empty in the global listing.
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
	// ObjectStore is the non-secret destination an object-storage provider was registered with —
	// endpoint, region, the RECORDED bucket, and the NAMES of the two burrow-credentials keys
	// holding the credential pair (ADR-0063 §1). Nil for every other provider type.
	ObjectStore *ObjectStoreConfig `json:"object_store,omitempty"`
	// Verification is what configuration-time verification observed: the probe object written and
	// deleted, the bucket, and the lifecycle reconciliation (ADR-0063 §2-§4). It is present on the
	// registration that performed the checks and absent from a listing, because it describes one
	// moment rather than a stored fact.
	Verification *ProviderVerification `json:"verification,omitempty"`
}

// ObjectStoreConfig mirrors the non-secret object-storage configuration on a provider row
// (ADR-0063 §1). It carries key NAMES, never key values.
type ObjectStoreConfig struct {
	Endpoint           string `json:"endpoint"`
	Region             string `json:"region,omitempty"`
	Bucket             string `json:"bucket"`
	Created            bool   `json:"created,omitempty"`
	AccessKeyIDKey     string `json:"access_key_id_key"`
	SecretAccessKeyKey string `json:"secret_access_key_key"`
	RetentionDays      int    `json:"retention_days,omitempty"`
}

// ProviderVerification mirrors what the control plane observed while registering an object-storage
// provider: it wrote and deleted a probe object, it created or was pointed at a bucket, and it
// reconciled the bucket's lifecycle rules against backup retention (ADR-0063 §2-§4).
type ProviderVerification struct {
	Bucket        string         `json:"bucket"`
	BucketCreated bool           `json:"bucket_created"`
	ProbeObject   bool           `json:"probe_object"`
	Lifecycle     LifecycleCheck `json:"lifecycle"`
}

// LifecycleCheck mirrors the outcome of reconciling a bucket's lifecycle rules against backup
// retention (ADR-0063 §3). Status is "ok", "conflict", or "unknown" — and "unknown" means the
// configuration could not be READ, so the invariant is not verified and must not be reported as
// though it were.
type LifecycleCheck struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
	Rule   string `json:"rule,omitempty"`
	Backup string `json:"backup,omitempty"`
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

	// The fields below configure an OBJECT-STORAGE provider (ADR-0063), whose credential is a PAIR
	// and whose configuration names a destination. AccessKeyID and SecretAccessKey are credential
	// VALUES and are held to exactly the same rules as Token: body only, never logged, never
	// echoed back.
	Endpoint        string `json:"endpoint,omitempty"`
	Region          string `json:"region,omitempty"`
	Bucket          string `json:"bucket,omitempty"`
	CreateBucket    bool   `json:"create_bucket,omitempty"`
	RetentionDays   int    `json:"retention_days,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	Confirm         bool   `json:"confirm,omitempty"`
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
	Ingress       IngressCapability       `json:"ingress"`
	Storage       StorageCapability       `json:"storage"`
	LoadBalancer  LoadBalancerCapability  `json:"load_balancer"`
	CertManager   CertManagerCapability   `json:"cert_manager"`
	MetricsServer MetricsServerCapability `json:"metrics_server"`
	CloudNativePG CloudNativePGCapability `json:"cloudnative_pg"`
	PgBackRest    PgBackRestCapability    `json:"pgbackrest"`
	// ControlPlaneDatabase is which shape the control plane's own database runs in (ADR-0086 §2).
	ControlPlaneDatabase ControlPlaneDatabaseCapability `json:"control_plane_database"`
	Provider             ProviderCapability             `json:"provider"`
	DNS                  DNSCapability                  `json:"dns"`
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

// MetricsServerCapability reports whether metrics-server is serving the Kubernetes Metrics API
// (detected via the metrics.k8s.io API group). It powers `kubectl top`, HPA autoscaling, and the
// utilization layer of capacity reporting.
type MetricsServerCapability struct {
	Present bool `json:"present"`
}

// CloudNativePGCapability reports the CloudNativePG operator (ADR-0066 §1). Present is whether its
// API group is served (the CRDs are installed); Ready is whether a controller is actually running,
// which is separate because a CRD outlives the operator that installed it; Version is the running
// operator's release and Pinned the release Burrow targets.
type CloudNativePGCapability struct {
	Present bool   `json:"present"`
	Ready   bool   `json:"ready"`
	Version string `json:"version,omitempty"`
	Pinned  string `json:"pinned,omitempty"`
}

// PgBackRestCapability reports the CloudNativePG pgBackRest plugin (ADR-0066 §3), the component a
// Postgres instance archives its write-ahead log and takes its base backups through. Present is
// whether its API group is served; Ready is whether its controller is actually running; Pinned is the
// release Burrow targets. No running version is reported: the plugin's release artifact does not
// carry one Burrow can read back.
type PgBackRestCapability struct {
	Present bool   `json:"present"`
	Ready   bool   `json:"ready"`
	Pinned  string `json:"pinned,omitempty"`
}

// ControlPlaneDatabaseCapability reports which of the two shapes the control plane's own database
// runs in (ADR-0086 §2). Kind is "cloudnativepg" or "plain", empty when the control plane could not
// read it; Ready is whether an instance is serving; BackedUp is whether it archives off-cluster,
// which a "plain" database never does and a "cloudnativepg" one does once an object-storage
// provider is registered.
type ControlPlaneDatabaseCapability struct {
	Kind     string `json:"kind,omitempty"`
	Ready    bool   `json:"ready"`
	BackedUp bool   `json:"backed_up"`
}

// The values ControlPlaneDatabaseCapability.Kind takes.
const (
	ControlPlaneDatabaseCloudNativePG = "cloudnativepg"
	ControlPlaneDatabasePlain         = "plain"
)

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

// Deploy applies an image to an app. It takes the DEPLOY budget, not the default one: a deploy runs
// the app's lifecycle hooks and waits for the rollout to settle whenever there is something to wait
// for — a `post-deploy` hook to tell (ADR-0072 §4), or a dependency Burrow derived from an attached
// database or a published port to check (ADR-0076 §4) — and that wait is bounded by an operational
// limit, not by anything this client picked (issue #404).
//
// Setting req.Progress asks the control plane to report the deploy's stages over the same call
// (issue #480). It changes nothing else: the same endpoint, the same body, the same budget, and the
// same errors — including a guardrail hold, which still arrives status-coded because the control
// plane writes the stream's header only once it has committed to doing work.
// The ENVIRONMENT RIDES THE ROUTE (see narrowing). It was a body field, and a control plane that
// predates named environments drops a body field it does not know exactly as it drops an unknown
// query parameter, so a deploy aimed at staging replaces what is RUNNING IN PRODUCTION and reports
// the release as staging's (issue #485).
func (c *Client) Deploy(ctx context.Context, app string, req DeployRequest) (DeployResult, error) {
	var out DeployResult
	env := req.Env
	path := narrowing(c.appPath(app, "deploy"), "env", env)
	req.Env = "" // the route carries it; one place holds the scope, so there is no second copy
	var err error
	if req.Progress == nil {
		err = c.doWithin(ctx, c.budget.deploy, http.MethodPost, path, req, &out)
	} else {
		err = c.within(ctx, c.budget.deploy, func(ctx context.Context) error {
			return c.streaming(ctx, path, req, req.Progress, &out)
		})
	}
	if err != nil && env != "" {
		what := fmt.Sprintf("this control plane cannot deploy into a NAMED environment, so nothing was deployed: the same call against it would have deployed %q into the DEFAULT environment rather than into %q, replacing whatever is running there", app, env)
		return out, scopeRefusal(what, "named environments", err)
	}
	return out, err
}

// streamLine is one line of the control plane's ndjson progress stream: an event while the operation
// runs, then one terminal line that is either the result or the error. The result stays raw because
// the framing is shared — a deploy and a build differ only in what that key carries — and the caller
// is the one that knows which type it asked for.
type streamLine struct {
	Event  *DeployProgress `json:"event"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Status            int    `json:"status"`
		Error             string `json:"error"`
		Code              string `json:"code"`
		NeedsConfirmation bool   `json:"needs_confirmation"`
		ServerVersion     string `json:"server_version"`
		ServerInstallID   string `json:"server_install_id"`
	} `json:"error"`
}

// streaming issues an operation asking for the progress stream, reports each stage to progress as it
// arrives, and decodes the terminal result into out. It is ONE implementation for every streaming
// operation — a deploy (issue #480) and a build (issue #503) — because the negotiation, the framing,
// and the fallback are the same problem in both.
//
// IT MUST SURVIVE A CONTROL PLANE THAT DOES NOT OFFER ONE. The Accept header is a request, not a
// requirement: a server predating this ignores it and answers with the single JSON object it always
// has, so the response's Content-Type decides which path is taken. That is what makes a new client
// safe against an old control plane, which is the ordinary state of an install mid-upgrade.
func (c *Client) streaming(ctx context.Context, path string, req any, progress func(DeployProgress), out any) error {
	b, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", ndjsonMediaType)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("control plane request: %w", err)
	}
	defer resp.Body.Close()
	// A refusal is always an ordinary status-coded body — a hold, a denial, an unknown app — and a
	// control plane with no progress stream answers 200 with a plain object. Both are the same
	// non-streaming decode, so neither can be mistaken for a truncated stream.
	if resp.StatusCode/100 != 2 || !isNDJSON(resp.Header.Get("Content-Type")) {
		return decodeResponse(resp, out)
	}

	dec := json.NewDecoder(resp.Body)
	for {
		var line streamLine
		if err := dec.Decode(&line); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("reading the progress stream: %w", err)
		}
		switch {
		case line.Error != nil:
			// An error raised after the first stage — a failed pre-deploy hook, a failed apply. It
			// carries the status and the fields an ordinary error body has, so the *APIError rebuilt
			// here is indistinguishable from the one the non-streaming path would have produced.
			return &APIError{
				StatusCode:        line.Error.Status,
				Code:              line.Error.Code,
				Message:           line.Error.Error,
				NeedsConfirmation: line.Error.NeedsConfirmation,
				ServerVersion:     line.Error.ServerVersion,
				ServerInstallID:   line.Error.ServerInstallID,
			}
		case len(line.Result) > 0 && !bytes.Equal(line.Result, jsonNull):
			// A literal null is not a result. Unmarshalling it would leave out untouched and return
			// success carrying a zero value, which is the one outcome worse than an error here.
			if err := json.Unmarshal(line.Result, out); err != nil {
				return fmt.Errorf("decoding the result: %w", err)
			}
			return nil
		case line.Event != nil:
			progress(*line.Event)
		}
	}
	// The stream ended with neither a result nor an error: burrowd went away mid-operation. What it
	// was doing may well have landed, so this says so rather than implying it did not happen.
	return fmt.Errorf("the control plane closed the progress stream without reporting an outcome; the operation may still be in progress — check the app's status before retrying")
}

// ndjsonMediaType is the content type of the progress stream: one JSON object per line.
const ndjsonMediaType = "application/x-ndjson"

// jsonNull is the encoding of a null result, which is not a result. See streaming.
var jsonNull = []byte("null")

// isNDJSON reports whether a response's Content-Type is the progress stream, ignoring any parameters
// (a charset, say) the server chose to add.
func isNDJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == ndjsonMediaType
}

// Build builds an app's image from a git source reference inside the cluster and, on success, hands
// the resulting digest-pinned reference into the guarded deploy path (ADR-0053): the returned
// BuildResult carries the built digest and the deploy that shipped it. It is gated by the app.deploy
// guardrail — a held deploy returns a guardrail error the caller surfaces for confirmation, re-invoking
// with Confirm set only on explicit human approval.
//
// Setting req.Progress asks the control plane to report the build's stages over the same call (issue
// #503), which is what makes a build that runs for minutes survivable across a proxy with a read
// timeout. It changes nothing else: the same endpoint, the same body, the same budget, and the same
// errors — a guardrail hold included, which arrives as an *APIError with NeedsConfirmation either
// way, because a build's hold is decided after its stages are already on the wire and therefore
// travels in the stream carrying the status it would have been written with.
func (c *Client) Build(ctx context.Context, app string, req BuildRequest) (BuildResult, error) {
	var out BuildResult
	path := c.appPath(app, "build")
	if req.Progress == nil {
		return out, c.doWithin(ctx, c.budget.build, http.MethodPost, path, req, &out)
	}
	err := c.within(ctx, c.budget.build, func(ctx context.Context) error {
		return c.streaming(ctx, path, req, req.Progress, &out)
	})
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
	err := c.doWithin(ctx, c.budget.run, http.MethodPost, c.appPath(app, "run"), req, &out)
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
	Name string `json:"name"`
	Type string `json:"type"`
	// Environment is the environment this instance serves (ADR-0067 §1): each environment gets its
	// own instance, so this is what distinguishes two rows of the same type.
	Environment string `json:"environment,omitempty"`
	Mode        string `json:"mode"`
	// Backend is the concrete implementation serving this instance: "victorialogs" for logs,
	// "cloudnative-pg" for the Postgres add-on, whose instance is a CloudNativePG Cluster the
	// operator reconciles (ADR-0066 §1).
	Backend      string   `json:"backend,omitempty"`
	Image        string   `json:"image,omitempty"`
	Endpoint     string   `json:"endpoint"`
	Capabilities []string `json:"capabilities"`
	Ready        bool     `json:"ready"`
	// Warning is a non-blocking note about the install, not persisted anywhere: today, that the
	// instance was created WITHOUT write-ahead-log archiving because the cluster has no pgBackRest
	// plugin, even though an object-storage destination is registered (ADR-0066 §3). Empty when
	// there is nothing to say.
	Warning string `json:"warning,omitempty"`
	// Backups is what this instance does about backups, read back from the instance itself at install
	// time. Nil on a row read from the registry: it is a fact about one moment, not a property of the
	// add-on, so it is never persisted.
	Backups *AddonBackups `json:"backups,omitempty"`
}

// The values AddonBackups.State takes. A caller deciding whether backups are on switches on one of
// these rather than reading a sentence.
const (
	// AddonBackupsArchiving: the instance's own wiring says it archives to object storage.
	AddonBackupsArchiving = "archiving"
	// AddonBackupsNone: it archives nowhere, or the add-on type has no backup path at all.
	AddonBackupsNone = "none"
	// AddonBackupsUnknown: the wiring could not be read back, so Burrow does not claim either way.
	AddonBackupsUnknown = "unknown"
)

// The values AddonBackups.BaseBackup takes, for an archiving instance. Archived write-ahead log with
// no base backup under it cannot be restored, so this is the half of the answer "archiving" does not
// give.
const (
	// AddonBaseBackupPresent: the repository holds at least one full backup.
	AddonBaseBackupPresent = "present"
	// AddonBaseBackupRequested: this install asked for the first one; it has not landed yet.
	AddonBaseBackupRequested = "requested"
	// AddonBaseBackupNone: the repository holds none and none was requested.
	AddonBaseBackupNone = "none"
	// AddonBaseBackupUnknown: the repository's own count could not be read.
	AddonBaseBackupUnknown = "unknown"
)

// AddonBackups is what one installed add-on instance does about backups: whether it archives, where,
// on what schedule, and whether anything exists for the archived write-ahead log to be replayed onto.
// Names and numbers only — never a credential.
type AddonBackups struct {
	State string `json:"state"`
	// Provider is the registry name of the object store, reported only when the instance's own
	// repository is demonstrably the one that provider names.
	Provider string `json:"provider,omitempty"`
	// Bucket and RepoPath come from the instance's pgBackRest stanza, not from what the install
	// resolved.
	Bucket   string `json:"bucket,omitempty"`
	RepoPath string `json:"repo_path,omitempty"`
	// RetentionDays is how long a full backup is kept; 0 when the repository declares no window.
	RetentionDays int `json:"retention_days,omitempty"`
	// Schedule is the base backup's cron expression in CloudNativePG's six-field form (leading field
	// is seconds).
	Schedule string `json:"schedule,omitempty"`
	// BaseBackup is one of the AddonBaseBackup* values, empty when the instance does not archive.
	BaseBackup string `json:"base_backup,omitempty"`
	// Detail is one line elaborating the state, safe to print.
	Detail string `json:"detail,omitempty"`
}

// InstallAddonOptions is everything `addon install` carries beyond the add-on's type and its
// environment.
type InstallAddonOptions struct {
	// Confirm satisfies the addon.install guardrail's confirmation hold.
	Confirm bool
	// ArchiveDestination names the object-storage provider a Postgres instance archives to
	// (ADR-0066 §3). Only needed when more than one is registered.
	ArchiveDestination string
}

// InstallAddon installs the vetted backing service for an add-on type (e.g. "logs") in one
// environment. Each environment gets its own instance (ADR-0067 §1); an empty env targets the
// default environment `prod`, which keeps the instance an existing install already has.
//
// BOTH NARROWINGS RIDE THE ROUTE (see narrowing), because both decide what gets created rather than
// how the call is answered. Against a control plane that drops the environment, the instance stands
// up as the DEFAULT environment's — and if one is already there, an install meant to give staging its
// own Postgres lands on production's. Against one that drops the archive destination, the instance is
// created with NO write-ahead-log archiving and reported as installed: a database with no way back,
// indistinguishable from one with a repository behind it until the day it is needed (issue #485).
func (c *Client) InstallAddon(ctx context.Context, addonType, env string, opts InstallAddonOptions) (Addon, error) {
	var out Addon
	body := map[string]any{"type": addonType, "env": "", "confirm": opts.Confirm, "archive_destination": ""}
	path := narrowing(narrowing("/v1/addons", "env", env), "archive-destination", opts.ArchiveDestination)
	err := c.do(ctx, http.MethodPost, path, body, &out)
	if err != nil && (env != "" || opts.ArchiveDestination != "") {
		// The archive destination is named first when both were asked for: an instance in the wrong
		// environment is visible and removable, an instance with no way back is neither.
		what := fmt.Sprintf("this control plane cannot install an add-on into a NAMED environment, so nothing was installed: the same call against it would have stood the %s instance up as the DEFAULT environment's rather than %q's", addonType, env)
		predates := "per-environment add-on instances"
		if opts.ArchiveDestination != "" {
			what = fmt.Sprintf("this control plane cannot archive an add-on's write-ahead log to an object store, so nothing was installed: the same call against it would have created the %s instance with NO archiving rather than archiving to %q, and reported it installed", addonType, opts.ArchiveDestination)
			predates = "archiving an add-on to object storage"
			if env != "" {
				what += fmt.Sprintf(" — and as the DEFAULT environment's instance rather than %q's", env)
			}
		}
		return out, scopeRefusal(what, predates, err)
	}
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

// RetainedVolume is an add-on volume an earlier `addon remove` deliberately left in place: storage
// that is still allocated, and on a managed provider still billed, with no add-on left to use it
// (ADR-0064 §6). Reporting it is what makes keeping data by default defensible — an invisible
// leftover claim is a silent bill.
type RetainedVolume struct {
	// Name is the claim name — what `kubectl delete pvc` takes to reclaim the storage.
	Name string `json:"name"`
	// Namespace is the add-on namespace the claim lives in.
	Namespace string `json:"namespace,omitempty"`
	// Addon is the add-on type the volume belonged to.
	Addon string `json:"addon"`
	// Environment is the environment the claim served (ADR-0067 §1). Empty for a claim created
	// before add-ons were per-environment, which is the default environment's.
	Environment string `json:"environment,omitempty"`
	// Role is what the claim holds: "data" (the add-on's own volume) or "backup" (its dumps).
	Role string `json:"role"`
	// Size is the claim's capacity, e.g. "10Gi". Size, not cost: cost needs the provider's pricing.
	Size string `json:"size,omitempty"`
	// ReinstallAdopts reports whether reinstalling the add-on picks this volume back up with its
	// data intact.
	ReinstallAdopts bool `json:"reinstall_adopts"`
}

// AddonListing is the whole answer to `addon list`: the registered add-ons, plus the volumes an
// earlier removal left behind. They are separate fields because they are different kinds of thing —
// one is a running backing service, the other is storage with nothing attached to it.
type AddonListing struct {
	Addons          []Addon          `json:"addons"`
	RetainedVolumes []RetainedVolume `json:"retained_volumes,omitempty"`
}

// AddonList returns the add-on listing: the registered instances and the retained volumes.
func (c *Client) AddonList(ctx context.Context) (AddonListing, error) {
	var out AddonListing
	err := c.do(ctx, http.MethodGet, "/v1/addons", nil, &out)
	return out, err
}

// Addons lists the installed add-on instances. It is the add-ons alone, for callers that only ask
// what is running; AddonList also carries the retained volumes.
func (c *Client) Addons(ctx context.Context) ([]Addon, error) {
	listing, err := c.AddonList(ctx)
	return listing.Addons, err
}

// RemoveAddonResult is the outcome of removing an add-on: what was torn down, and what was
// deliberately LEFT IN PLACE. Removal keeps the add-on's data volume unless deleteData is passed, so
// this reports the retained volume names — the data volume a reinstall would reuse and the Postgres
// backup volume, which outlives the database either way (ADR-0025/0031/0032).
type RemoveAddonResult struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Namespace is where the add-on's resources, and any retained volume, live.
	Namespace string `json:"namespace,omitempty"`
	// DataDeleted reports whether the add-on's data volume was destroyed.
	DataDeleted bool `json:"data_deleted"`
	// RetainedDataVolume is the PVC left in place; empty when the add-on had no volume or it was
	// deleted.
	RetainedDataVolume string `json:"retained_data_volume,omitempty"`
	// RetainedBackupVolume is the backup PVC left in place; empty when no backup volume exists.
	RetainedBackupVolume string `json:"retained_backup_volume,omitempty"`
	// AttachedApps are the apps that held a database on this instance at removal time.
	AttachedApps []string `json:"attached_apps,omitempty"`
	// FinalBackups are the backups taken before the data volume was destroyed, one per attached
	// database (ADR-0064 §5). Each reached an object store — the removal is abandoned otherwise — so
	// this is the list of copies that outlived the instance.
	FinalBackups []Backup `json:"final_backups,omitempty"`
	// FinalBackupSkipped reports that the data was destroyed with no off-cluster copy, and
	// FinalBackupNote says why. Reported rather than inferred from an empty FinalBackups: "nothing
	// was backed up" and "nothing needed backing up" must not look the same.
	FinalBackupSkipped bool   `json:"final_backup_skipped,omitempty"`
	FinalBackupNote    string `json:"final_backup_note,omitempty"`
}

// RemoveAddonOptions is everything `addon remove` carries beyond the add-on's name. It is a struct
// rather than a run of positional booleans because this is the most destructive call in the API.
type RemoveAddonOptions struct {
	// DeleteData is the explicit opt-in that also DESTROYS the add-on's data volume — for Postgres,
	// every attached app's database. Without it the removal tears down the workload and leaves the
	// volume, so a reinstall picks the data back up.
	DeleteData bool
	// SkipFinalBackup destroys the data without the final backup ADR-0064 §5 otherwise takes first.
	// It exists because an add-on is often removed BECAUSE it is wedged, and a wedged instance
	// cannot be dumped — without it a broken add-on would be undeletable.
	SkipFinalBackup bool
	// BackupDestination names the object-storage provider the final backup goes to, needed only when
	// several are registered.
	BackupDestination string
	// Confirm satisfies the addon.remove guardrail hold. It is not the data-loss acknowledgement.
	Confirm bool
}

// RemoveAddon removes the named add-on instance. With opts.DeleteData the add-on's data volume is
// destroyed too, and — where an object-storage provider is registered — burrowd takes a final backup
// of every attached database first and refuses the whole removal if it does not reach the store
// (ADR-0064 §5).
//
// WHAT HAPPENS TO THE DATA RIDES THE ROUTE, and unlike every other case that follows the rule (see
// guardPath) there is no unnarrowed form to fall back to: BOTH dispositions are routes, because on
// this call it is the ABSENCE of the parameter that destroys. `delete_data` inverted the default
// (issue #323) — before it, a removal destroyed the volume unconditionally — so a current client
// that says nothing, MEANING KEEP MY DATA, is the request an older control plane answers by
// destroying the volume, with no final backup to fall back on because it takes none. A query
// parameter cannot express that: its absence is indistinguishable from a caller that never had it.
// A route can, and a control plane that has neither route refuses both removals with nothing
// touched (issue #485).
func (c *Client) RemoveAddon(ctx context.Context, name string, opts RemoveAddonOptions) (RemoveAddonResult, error) {
	var out RemoveAddonResult
	disposition := "keep"
	if opts.DeleteData {
		disposition = "delete"
	}
	path := "/v1/addons/" + url.PathEscape(name) + "/data/" + disposition
	q := url.Values{}
	if opts.Confirm {
		q.Set("confirm", "true")
	}
	if opts.SkipFinalBackup {
		q.Set("skip_final_backup", "true")
	}
	if opts.BackupDestination != "" {
		q.Set("backup_destination", opts.BackupDestination)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	// skip_final_backup and backup_destination stay parameters, and are safe as parameters, because
	// they can only ever reach a control plane that has the route above — which is a control plane
	// that shipped after they did. A parameter that cannot outrun its own route needs no protection.
	err := c.doWithin(ctx, c.budget.backup, http.MethodDelete, path, nil, &out)
	if err != nil {
		return out, removeAddonScopeRefusal(name, opts.DeleteData, err)
	}
	return out, nil
}

// removeAddonScopeRefusal names what a control plane without the data-disposition routes would have
// done instead, which is different in each direction and destructive in both.
func removeAddonScopeRefusal(name string, deleteData bool, err error) error {
	what := fmt.Sprintf("this control plane cannot be asked to KEEP an add-on's data, so nothing was removed: it destroys the data volume on every removal, so the same call against it would have destroyed %q's data — and every attached app's database with it — with no final backup, which is the opposite of what was asked", name)
	if deleteData {
		what = fmt.Sprintf("this control plane cannot take the final backup that a data-destroying removal makes first, so nothing was removed: the same call against it would have destroyed %q's data volume with no copy left off the cluster", name)
	}
	return scopeRefusal(what, "the choice of what a removal does to an add-on's data", err)
}

// AttachResult is the non-secret outcome of attaching an app to an add-on (ADR-0031): the KEY NAME
// the generated connection string was written under in the app's Secret — never the value.
type AttachResult struct {
	App   string `json:"app"`
	Addon string `json:"addon"`
	// Environment is the environment whose instance the database was provisioned on (ADR-0067 §1) —
	// which is what says WHICH database the app was given, since databases keep their simple names.
	Environment string `json:"environment,omitempty"`
	SecretKey   string `json:"secret_key"`
}

// AttachAddon gives an app its own database on ENVIRONMENT env's Postgres instance and wires it in
// (ADR-0031/0067 §1). The caller supplies only the add-on type, app name, and environment; burrowd
// generates the DATABASE_URL server-side and writes it into the app's Secret in that environment's
// namespace — no secret value crosses this API or the agent control channel. The result carries the
// environment and the KEY name, never the value.
func (c *Client) AttachAddon(ctx context.Context, addonType, app, env string) (AttachResult, error) {
	var out AttachResult
	body := map[string]any{"addon": addonType, "app": app, "env": env}
	err := c.do(ctx, http.MethodPost, "/v1/addons/attach", body, &out)
	return out, err
}

// DetachAddon detaches an app from an add-on, dropping its data (e.g. its Postgres database). It is
// held for confirmation by a guardrail by default; pass confirm=true to proceed past the hold.
//
// The ENVIRONMENT RIDES THE ROUTE (see guardPath). A control plane that predates per-environment
// add-ons drops the body field it does not know exactly as it drops an unknown query parameter, so a
// detach aimed at staging would land on the default environment's instance — production's database,
// dropped, reported as staging's (issue #485).
func (c *Client) DetachAddon(ctx context.Context, addonType, app, env string, confirm bool) error {
	body := map[string]any{"addon": addonType, "app": app, "confirm": confirm}
	if env == "" {
		// Nothing was narrowed — the default environment is one every control plane has — so the
		// unnarrowed route and its unchanged wire shape are exactly right.
		body["env"] = ""
		return c.do(ctx, http.MethodPost, "/v1/addons/detach", body, nil)
	}
	path := "/v1/addons/detach/env/" + url.PathEscape(env)
	err := c.do(ctx, http.MethodPost, path, body, nil)
	if err != nil {
		what := fmt.Sprintf("this control plane cannot detach an app from an add-on in a NAMED environment, so nothing was detached: the same call against it would have dropped %q's database on the default environment's instance rather than on %q's", app, env)
		return scopeRefusal(what, "per-environment add-on instances", err)
	}
	return nil
}

// Backup is one recorded per-app database backup (ADR-0032): the control-plane index row for a dump
// on the backup PVC. It names the app, the on-PVC path, the size, and the status — never a credential.
type Backup struct {
	ID string `json:"id"`
	// Kind is which mechanism took it: "logical" (one app's `pg_dump`) or "physical" (a
	// CloudNativePG base backup of the whole instance). A physical backup has no App.
	Kind string `json:"kind,omitempty"`
	App  string `json:"app"`
	// Environment is the environment whose instance the dump was taken from (ADR-0067 §1). A dump is
	// only a valid source for the environment it came from.
	Environment string `json:"environment,omitempty"`
	CreatedAt   string `json:"created_at"`
	Path        string `json:"path,omitempty"`
	// Volume is the claim the dump was written to — this environment's backup claim. Each
	// environment holds its dumps on its own volume, so the path alone does not address a dump.
	Volume    string `json:"volume,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Status    string `json:"status"`
	// Destination is where this backup's bytes ended up: "object-store" for one that left the
	// cluster, "cluster" for one that did not (ADR-0063). It is recorded per backup rather than
	// derived from the current configuration, so registering a destination today does not make
	// yesterday's in-cluster dumps read as durable.
	Destination string `json:"destination,omitempty"`
	// Provider and ObjectKey address the dump at the vendor. Names and a key, never a credential.
	Provider  string `json:"provider,omitempty"`
	ObjectKey string `json:"object_key,omitempty"`
	// FailureReason is why a failed backup failed, from a closed set a caller can branch on;
	// FailureDetail is one Burrow-authored line. Neither carries a vendor response body.
	FailureReason string `json:"failure_reason,omitempty"`
	FailureDetail string `json:"failure_detail,omitempty"`
}

// BackupResult is the outcome of an on-demand backup (ADR-0032): the recorded backup row.
type BackupResult struct {
	Backup Backup `json:"backup"`
}

// BackupAddon backs up an app's database on the installed Postgres add-on (ADR-0032, ADR-0063 §7).
// burrowd runs an in-cluster Job that pg_dumps to the backup PVC and, when an object-storage
// destination is registered, writes it on to the store and reads it back before recording the backup
// as completed — so a returned completed backup is one whose bytes actually arrived. destination
// names the provider to write to; empty resolves it, which works when exactly one is registered. No
// secret value crosses this API; the result is the recorded backup, never a credential.
//
// BOTH NARROWINGS RIDE THE ROUTE (see narrowing). The environment says which instance is dumped; the
// destination says whether the bytes leave the cluster at all. This route is older than object
// storage, so a control plane in between drops the destination, writes the dump to the in-cluster
// volume, and records a COMPLETED backup — a backup that reads as durable and is on the same disk as
// the database it is insurance against. Nobody finds out until they need it (issue #485).
func (c *Client) BackupAddon(ctx context.Context, addonType, app, env, destination string) (BackupResult, error) {
	var out BackupResult
	body := map[string]any{"addon": addonType, "app": app, "env": "", "destination": ""}
	path := narrowing(narrowing("/v1/addons/backup", "env", env), "destination", destination)
	err := c.doWithin(ctx, c.budget.backup, http.MethodPost, path, body, &out)
	if err != nil && (env != "" || destination != "") {
		what := fmt.Sprintf("this control plane cannot back up a NAMED environment's instance, so nothing was backed up: the same call against it would have dumped %q's database from the DEFAULT environment's instance rather than from %q's", app, env)
		predates := "per-environment add-on instances"
		if destination != "" {
			what = fmt.Sprintf("this control plane cannot write a backup to an object store, so nothing was backed up: the same call against it would have left %q's dump on a volume inside the cluster rather than sending it to %q, and recorded it as a completed backup", app, destination)
			predates = "backups that leave the cluster"
			if env != "" {
				what += fmt.Sprintf(" — and taken it from the DEFAULT environment's instance rather than %q's", env)
			}
		}
		return out, scopeRefusal(what, predates, err)
	}
	return out, err
}

// BackupInstance takes a PHYSICAL base backup of one environment's whole Postgres instance
// (ADR-0066 §2): every database on it, restorable to any point in the write-ahead-log window, taken
// by CloudNativePG through the pgBackRest plugin and written to the object store. It is not
// interchangeable with BackupAddon, which dumps ONE app's database and can be restored into that app
// alone; a physical backup restores the whole instance and cannot be applied per app.
//
// destination names the object-storage provider holding the repository; empty resolves it, which
// works when exactly one is registered. No secret value crosses this API.
func (c *Client) BackupInstance(ctx context.Context, addonType, env, destination string) (BackupResult, error) {
	var out BackupResult
	body := map[string]any{"addon": addonType, "env": env, "destination": destination}
	err := c.doWithin(ctx, c.budget.backup, http.MethodPost, "/v1/addons/backup-instance", body, &out)
	return out, err
}

// Backups lists recorded backups from the control-plane database (ADR-0032). An empty app lists
// every app's backups and an empty env every environment's; a non-empty value restricts to that app
// or environment (ADR-0067 §1). Read-only; no secret value.
//
// THE ENVIRONMENT RIDES THE ROUTE, WHICH IS THE WRITE RULE ON A READ, and this is the only read that
// gets it. What comes back here is not something a person reads and moves on from: it is the picker
// for RestoreAddon and RestoreInstance, and the id chosen from it is handed to a call that overwrites
// a live database. An id chosen from the wrong environment's list cannot be caught at the restore —
// an id is an opaque string, and the restore has no way to know which listing produced it. So the
// list itself has to be refused rather than answered one environment out (issue #485).
//
// `addon` and `app` stay query parameters: neither aims a write, and an absent one widens a listing
// rather than pointing it somewhere else.
func (c *Client) Backups(ctx context.Context, addonType, app, env string) ([]Backup, error) {
	var out struct {
		Backups []Backup `json:"backups"`
	}
	path := narrowing("/v1/addons/backups", "env", env) + "?addon=" + url.QueryEscape(addonType)
	if app != "" {
		path += "&app=" + url.QueryEscape(app)
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	if err != nil && env != "" {
		what := fmt.Sprintf("this control plane cannot list one environment's backups, so there is nothing to show: what it would answer with is every environment's, and a backup chosen from that list is restored over a live database on the strength of a list that was never %q's", env)
		return nil, scopeRefusal(what, "per-environment add-on instances", err)
	}
	return out.Backups, err
}

// BackupObservation is one recorded backup as the health surface reports it, with its age already
// computed against the moment the report was assembled.
type BackupObservation struct {
	ID          string `json:"id"`
	App         string `json:"app"`
	Environment string `json:"environment,omitempty"`
	At          string `json:"at"`
	// AgeSeconds is how long ago the backup was recorded. Seconds, not a rendered duration, so a
	// caller compares a number rather than parsing prose.
	AgeSeconds  int64  `json:"age_seconds"`
	Status      string `json:"status"`
	Destination string `json:"destination,omitempty"`
	Provider    string `json:"provider,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	// Reason and Detail are set on a failure only. Neither carries a vendor response body.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// BackupDestinationHealth is one registered object-storage destination and whether it answered when
// the report was assembled. It is probed on demand and never cached — a stored verdict goes stale
// while continuing to read as current.
type BackupDestinationHealth struct {
	Provider  string `json:"provider"`
	Endpoint  string `json:"endpoint,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Reachable bool   `json:"reachable"`
	// Detail is one Burrow-authored line describing what was observed. Never a vendor response body
	// and never a credential.
	Detail string `json:"detail,omitempty"`
}

// BackupHealth is what Burrow observed about an add-on's backups (ADR-0063 §7, ADR-0066 §5): the
// last successful backup, the last one that left the cluster, the last failure, and whether each
// registered destination answers.
type BackupHealth struct {
	Addon       string `json:"addon"`
	App         string `json:"app,omitempty"`
	Environment string `json:"environment,omitempty"`
	ObservedAt  string `json:"observed_at"`
	// State is "never", "cluster-only" or "durable" — what kind of coverage exists, not a verdict
	// against a threshold.
	State string `json:"state"`
	// LastSuccess is the newest completed backup wherever it went; LastDurableSuccess is the newest
	// one that reached an object store, which is the age ADR-0063 §7 is about.
	LastSuccess        *BackupObservation `json:"last_success,omitempty"`
	LastDurableSuccess *BackupObservation `json:"last_durable_success,omitempty"`
	LastFailure        *BackupObservation `json:"last_failure,omitempty"`
	// Pending counts rows still recorded as pending. A pending row never reads as a success.
	Pending      int                       `json:"pending,omitempty"`
	Destinations []BackupDestinationHealth `json:"destinations,omitempty"`
	Summary      string                    `json:"summary"`
}

// BackupHealth reports what Burrow observed about an add-on's backups (ADR-0063 §7, ADR-0066 §5).
// An empty app spans every app and an empty env every environment. Read-only; it moves no secret.
func (c *Client) BackupHealth(ctx context.Context, addonType, app, env string) (BackupHealth, error) {
	var out BackupHealth
	path := "/v1/addons/backup-health?addon=" + url.QueryEscape(addonType)
	if app != "" {
		path += "&app=" + url.QueryEscape(app)
	}
	if env != "" {
		path += "&env=" + url.QueryEscape(env)
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// RestoreAddon restores an app's database from a recorded backup, overwriting its live contents
// (ADR-0032). It is held for confirmation by a guardrail by default; pass confirm=true to proceed.
//
// The ENVIRONMENT RIDES THE ROUTE, for the reason DetachAddon's does: a restore aimed at staging,
// against a control plane that drops the field, overwrites the live database of whatever app of that
// name is on the default environment's instance (issue #485).
func (c *Client) RestoreAddon(ctx context.Context, addonType, app, backupID, env string, confirm bool) error {
	body := map[string]any{"addon": addonType, "app": app, "backup": backupID, "confirm": confirm}
	if env == "" {
		// Unnarrowed, so the route and the body stay exactly as they were (see DetachAddon).
		body["env"] = ""
		return c.doWithin(ctx, c.budget.backup, http.MethodPost, "/v1/addons/restore", body, nil)
	}
	path := "/v1/addons/restore/env/" + url.PathEscape(env)
	err := c.doWithin(ctx, c.budget.backup, http.MethodPost, path, body, nil)
	if err != nil {
		what := fmt.Sprintf("this control plane cannot restore into a NAMED environment, so nothing was restored: the same call against it would have overwritten the live database of %q on the default environment's instance rather than on %q's", app, env)
		return scopeRefusal(what, "per-environment add-on instances", err)
	}
	return nil
}

// StrandedApp is one app a physical restore's DATABASE_URL cutover did not finish for, and why.
type StrandedApp struct {
	App    string `json:"app"`
	Reason string `json:"reason"`
}

// RestoreInstanceResult is what a physical restore did (ADR-0066 §4). It names the instance, the
// point it was rewound to, the backup taken of what was there, and every app that was on it — never
// a credential and never a connection string.
type RestoreInstanceResult struct {
	Addon       string `json:"addon"`
	Environment string `json:"environment"`
	Instance    string `json:"instance"`
	// RecoveryTarget is the point that was recovered to. It is deliberately not `target`, which is
	// the control plane the command acted on (ADR-0078 §4).
	RecoveryTarget string `json:"recovery_target"`
	// SafetyBackup is the physical backup taken of the pre-restore state, which is the way back from
	// a restore to the wrong point. SafetyBackupNote says why there is none when there is none.
	SafetyBackup     string `json:"safety_backup,omitempty"`
	SafetyBackupNote string `json:"safety_backup_note,omitempty"`
	// Apps is every app that was on the instance, by name — the blast radius, recorded rather than
	// counted. Reconnected is those now pointing at the recovered instance; Stranded is those the
	// cutover could not finish for.
	Apps        []string      `json:"apps"`
	Reconnected []string      `json:"reconnected"`
	Stranded    []StrandedApp `json:"stranded,omitempty"`
}

// RestoreInstanceOptions is everything `addon restore-instance` needs beyond the add-on type and the
// environment. Exactly one recovery target is required — the server refuses zero and refuses several.
type RestoreInstanceOptions struct {
	Backup           string
	ToTime           string
	Latest           bool
	SkipSafetyBackup bool
	// Destination names the object-storage provider holding this instance's repository; empty
	// resolves it when exactly one is registered.
	Destination string
	Confirm     bool
}

// RestoreInstance rewinds one environment's WHOLE Postgres instance to a point in its object-storage
// repository (ADR-0066 §4): every database on it goes back together, the instance is replaced by one
// recovered from the repository, and every attached app is re-pointed at it and restarted.
//
// It is NOT the same operation as RestoreAddon, and the difference is the blast radius rather than
// the mechanism. RestoreAddon replaces one app's database from that app's own dump and touches no
// other app; this replaces the instance every app in the environment shares. It is held for
// confirmation by the addon.restore_instance guardrail by default.
//
// It takes its own budget: a recovery waits for a base backup to be restored and write-ahead log
// replayed, which is bounded by how much data there is rather than by a Job's timeout.
func (c *Client) RestoreInstance(ctx context.Context, addonType, env string, opts RestoreInstanceOptions) (RestoreInstanceResult, error) {
	var out RestoreInstanceResult
	body := map[string]any{
		"addon":              addonType,
		"env":                env,
		"backup":             opts.Backup,
		"to_time":            opts.ToTime,
		"latest":             opts.Latest,
		"skip_safety_backup": opts.SkipSafetyBackup,
		"destination":        opts.Destination,
		"confirm":            opts.Confirm,
	}
	err := c.doWithin(ctx, c.budget.restoreInstance, http.MethodPost, "/v1/addons/restore-instance", body, &out)
	return out, err
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
//
// The ENVIRONMENT RIDES THE ROUTE (see guardPath). It is the one parameter on this call that decides
// WHICH app is destroyed, and against a control plane that predates named environments a query
// parameter naming staging is dropped and the PRODUCTION app is deleted instead — workload, routing
// and release history — under a line that says staging (issue #485).
func (c *Client) DeleteApp(ctx context.Context, app, env string, confirm bool) error {
	path := "/v1/apps/" + url.PathEscape(app)
	if env != "" {
		path += "/env/" + url.PathEscape(env)
	}
	if confirm {
		path += "?confirm=true"
	}
	err := c.do(ctx, http.MethodDelete, path, nil, nil)
	if err != nil && env != "" {
		what := fmt.Sprintf("this control plane cannot delete an app in a NAMED environment, so nothing was deleted: the same call against it would have deleted %q — its workload, routing and release history — in the DEFAULT environment rather than in %q", app, env)
		return scopeRefusal(what, "named environments", err)
	}
	return err
}

// RollbackOptions carries the caller's choices for a rollback: the guardrail confirmation, and the
// operator-only override that rolls back around a broken `pre-rollback` hook (ADR-0080).
type RollbackOptions struct {
	// Confirm satisfies a guardrail whose disposition holds the rollback for confirmation.
	Confirm bool
	// SkipHooks rolls back without running the app's `pre-rollback` hook. The hook stays configured
	// and the control plane records the skip. It is set by the operator CLI's `--skip-hooks` and by
	// nothing on the agent surface (ADR-0080 §3).
	SkipHooks bool
}

// Rollback returns an app to its previous release. It takes the deploy budget: a rollback runs the
// `pre-rollback` hook and then waits for the rollout to settle and tells the `post-deploy` hook the
// same way a deploy does (ADR-0072 §8), so it waits on the same server-side bounds.
//
// The ENVIRONMENT RIDES THE ROUTE (see narrowing): a rollback aimed at staging, against a control
// plane that drops the parameter, replaces what is running in PRODUCTION with production's previous
// release (issue #485). `confirm` and `skip_hooks` stay parameters — neither decides what the
// rollback touches, and an ignored `confirm` makes an older server HOLD, never proceed.
func (c *Client) Rollback(ctx context.Context, app, env string, opts RollbackOptions) (RollbackResult, error) {
	var out RollbackResult
	path := narrowing(c.appPath(app, "rollback"), "env", env)
	var params []string
	if opts.Confirm {
		params = append(params, "confirm=true")
	}
	if opts.SkipHooks {
		params = append(params, "skip_hooks=true")
	}
	if len(params) > 0 {
		path += "?" + strings.Join(params, "&")
	}
	err := c.doWithin(ctx, c.budget.deploy, http.MethodPost, path, nil, &out)
	if err != nil && env != "" {
		what := fmt.Sprintf("this control plane cannot roll an app back in a NAMED environment, so nothing was rolled back: the same call against it would have rolled %q back in the DEFAULT environment rather than in %q, replacing what is running there with its previous release", app, env)
		return out, scopeRefusal(what, "named environments", err)
	}
	return out, err
}

// Scale sets an app's replica count in one environment.
//
// The ENVIRONMENT RIDES THE ROUTE (see narrowing). It was a body field, and against a control plane
// that drops it a scale aimed at staging resizes PRODUCTION — and a scale to zero stops production
// serving (issue #485).
func (c *Client) Scale(ctx context.Context, app, env string, replicas int32, confirm bool) (ScaleResult, error) {
	var out ScaleResult
	body := map[string]any{"env": "", "replicas": replicas, "confirm": confirm}
	err := c.do(ctx, http.MethodPost, narrowing(c.appPath(app, "scale"), "env", env), body, &out)
	if err != nil && env != "" {
		what := fmt.Sprintf("this control plane cannot scale an app in a NAMED environment, so nothing was scaled: the same call against it would have scaled %q in the DEFAULT environment to %d replicas rather than %q's", app, replicas, env)
		return out, scopeRefusal(what, "named environments", err)
	}
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

// Expose publishes an app at a hostname in one environment.
//
// The ENVIRONMENT RIDES THE ROUTE (see narrowing): against a control plane that drops it, a hostname
// meant for staging is pointed at PRODUCTION's workload, and a certificate is issued for it
// (issue #485).
func (c *Client) Expose(ctx context.Context, app, env, host string, port int32, tls bool, issuer string, confirm bool) (ExposeResult, error) {
	var out ExposeResult
	body := map[string]any{"env": "", "host": host, "port": port, "tls": tls, "issuer": issuer, "confirm": confirm}
	err := c.do(ctx, http.MethodPost, narrowing(c.appPath(app, "expose"), "env", env), body, &out)
	if err != nil && env != "" {
		what := fmt.Sprintf("this control plane cannot publish an app in a NAMED environment, so nothing was published: the same call against it would have pointed %q at %q in the DEFAULT environment rather than at %q's", host, app, env)
		return out, scopeRefusal(what, "named environments", err)
	}
	return out, err
}

// Unexpose withdraws an app's published hostname in one environment.
//
// The ENVIRONMENT RIDES THE ROUTE (see narrowing): against a control plane that drops it, unexposing
// staging takes PRODUCTION's ingress and routing down (issue #485).
func (c *Client) Unexpose(ctx context.Context, app, env string) error {
	err := c.do(ctx, http.MethodPost, narrowing(c.appPath(app, "unexpose"), "env", env), nil, nil)
	if err != nil && env != "" {
		what := fmt.Sprintf("this control plane cannot unpublish an app in a NAMED environment, so nothing was unpublished: the same call against it would have removed the ingress and routing of %q in the DEFAULT environment rather than in %q", app, env)
		return scopeRefusal(what, "named environments", err)
	}
	return err
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
//
// The ENVIRONMENT RIDES THE ROUTE (see narrowing). The secrets route is older than named
// environments, so against a control plane in between, a value meant for staging is written into
// PRODUCTION's Secret and the production workload is rolled to pick it up — and a secret cannot be
// unwritten from the place it should never have reached (issue #485). The refusal names the KEY and
// never the value, like everything else on this path.
func (c *Client) SetSecret(ctx context.Context, app, env, key, value string, noRestart bool) error {
	body := map[string]any{"env": "", "key": key, "value": value, "no_restart": noRestart}
	err := c.do(ctx, http.MethodPost, narrowing(c.appPath(app, "secrets"), "env", env), body, nil)
	if err != nil && env != "" {
		what := fmt.Sprintf("this control plane cannot set a secret in a NAMED environment, so nothing was written: the same call against it would have written %q into %q's Secret in the DEFAULT environment rather than in %q, and rolled that workload to pick it up", key, app, env)
		return scopeRefusal(what, "named environments", err)
	}
	return err
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
//
// The ENVIRONMENT RIDES THE ROUTE, for the reason SetSecret's does: against a control plane that
// drops it, removing staging's key takes the value PRODUCTION is running on out from under it and
// rolls the workload (issue #485).
func (c *Client) UnsetSecret(ctx context.Context, app, env, key string, noRestart bool) error {
	path := narrowing("/v1/apps/"+url.PathEscape(app)+"/secrets/"+url.PathEscape(key), "env", env)
	if noRestart {
		path += "?no_restart=true"
	}
	err := c.do(ctx, http.MethodDelete, path, nil, nil)
	if err != nil && env != "" {
		what := fmt.Sprintf("this control plane cannot remove a secret in a NAMED environment, so nothing was removed: the same call against it would have removed %q from %q's Secret in the DEFAULT environment rather than in %q, and rolled that workload without it", key, app, env)
		return scopeRefusal(what, "named environments", err)
	}
	return err
}

// Hook is one configured lifecycle command: the phase it fires at and the command it runs, for one
// app in one environment (ADR-0072 §1). Phase is `pre-deploy` (before a deploy's image reaches the
// cluster, from that image) or `pre-rollback` (before a rollback's older image does, from the image
// being left). Command is an argv, so argument boundaries survive the round trip.
type Hook struct {
	App         string   `json:"app"`
	Environment string   `json:"environment"`
	Phase       string   `json:"phase"`
	Command     []string `json:"command"`
}

// Hooks returns the lifecycle hooks configured for an app in the target environment (ADR-0072 §1). A
// phase with no hook is absent from the result: unset means no hook and today's behaviour exactly.
func (c *Client) Hooks(ctx context.Context, app, env string) ([]Hook, error) {
	var out struct {
		Hooks []Hook `json:"hooks"`
	}
	err := c.do(ctx, http.MethodGet, withEnv(c.appPath(app, "hooks"), env), nil, &out)
	return out.Hooks, err
}

// SetHook configures the command an app runs at a phase, replacing any command already set there
// (ADR-0072 §1). A pre-deploy hook runs on EVERY deploy of the app in that environment, automated
// ones included, so a command that starts failing blocks deploys until someone changes it.
func (c *Client) SetHook(ctx context.Context, app, env, phase string, command []string) (Hook, error) {
	var out Hook
	err := c.do(ctx, http.MethodPut, withEnv(c.hookPath(app, phase), env), map[string]any{"command": command}, &out)
	return out, err
}

// UnsetHook removes an app's hook at a phase. Unsetting a phase with no hook succeeds; afterwards
// that phase runs nothing.
func (c *Client) UnsetHook(ctx context.Context, app, env, phase string) error {
	return c.do(ctx, http.MethodDelete, withEnv(c.hookPath(app, phase), env), nil, nil)
}

func (c *Client) hookPath(app, phase string) string {
	return c.appPath(app, "hooks") + "/" + url.PathEscape(phase)
}

func (c *Client) appPath(app, verb string) string {
	return "/v1/apps/" + url.PathEscape(app) + "/" + verb
}

// narrowing appends a narrowing to path as a ROUTE SEGMENT — `/<name>/<value>` — which is where a
// parameter that decides what a write touches belongs (see guardPath, and issue #485).
//
// THE SEGMENT IS NAMED AFTER THE PARAMETER IT REPLACES, so a route reads as the request it carries
// and a reader can check one against the other. A call with two of them appends them in the order the
// control plane registers, which is the order they are written here.
//
// An EMPTY value narrows nothing: the default environment is one every control plane has, and an
// unset destination is one the server resolves. So it returns the path unchanged, keeping the wire
// shape clients have always sent, and the caller raises no refusal for it — there is no wider scope
// the call could have landed at.
func narrowing(path, name, value string) string {
	if value == "" {
		return path
	}
	return path + "/" + name + "/" + url.PathEscape(value)
}

// withEnv appends an env query parameter to path when env is non-empty, so an operation targets a
// named environment (ADR-0035 phase 2b). An empty env leaves the path unchanged and the server
// defaults to the default environment.
//
// It is the READ side's form. A write's environment rides the route instead (see narrowing): the
// difference is that a read answered one scope out misinforms a reader, where a write performed one
// scope out changes something nobody asked to change. The one read that follows the write rule is
// Backups, because its answer is an argument to a restore rather than something a person reads.
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

// GuardScope selects whose guardrail policy is read or written: an environment, or one app or
// add-on instance within it (ADR-0085 §1). The zero value is the global policy. Which combinations
// are legal is the control plane's decision, not this client's — a name without an environment is
// refused there, so every client hears the same reason.
type GuardScope struct {
	Env  string
	Name string
}

// Guardrails lists the control-plane guardrails and their current dispositions. An empty scope lists
// the global policy; a named environment lists its effective policy under the env to global to
// default fallback, and a name narrows that to one app or add-on instance, each entry marking which
// tier its disposition came from (ADR-0035 phase 2c, ADR-0085 §4).
//
// A control plane too old to know the name tier answers the name-scoped route with a 404, which
// becomes a refusal rather than the environment's policy relabelled as one app's (see guardPath).
func (c *Client) Guardrails(ctx context.Context, scope GuardScope) ([]Guardrail, error) {
	var out struct {
		Guardrails []Guardrail `json:"guardrails"`
	}
	if err := c.do(ctx, http.MethodGet, guardPath(scope, ""), nil, &out); err != nil {
		return nil, guardScopeRefusal(scope, "", err)
	}
	return out.Guardrails, nil
}

// SetGuardrail sets a guardrail's disposition and returns the updated policy. An empty scope sets
// the global disposition; a named environment scopes it to that environment, storing the
// env-prefixed code (ADR-0035 phase 2c); a name scopes it to one app or add-on instance in that
// environment, storing env.name.code (ADR-0085 §1).
//
// A control plane too old to know the name tier answers the name-scoped route with a 404, so the
// write is refused instead of landing one tier wider than it was meant to (see guardPath).
func (c *Client) SetGuardrail(ctx context.Context, scope GuardScope, code, disposition string) ([]Guardrail, error) {
	var out struct {
		Guardrails []Guardrail `json:"guardrails"`
	}
	body := map[string]string{"disposition": disposition}
	if err := c.do(ctx, http.MethodPut, guardPath(scope, code), body, &out); err != nil {
		return nil, guardScopeRefusal(scope, code, err)
	}
	return out.Guardrails, nil
}

// guardPath builds the guard route for a scope: the environment rides a query parameter, and the
// NAME RIDES THE PATH.
//
// That asymmetry is deliberate and it is a safety property, not a style choice. A scope that
// narrows a write has to be something an older control plane can FAIL on. A query parameter cannot
// be: a control plane that predates the name tier (ADR-0085) ignores an unknown query parameter,
// performs the write one tier wider — for every app in the environment rather than for the one app
// named — and answers 200, so the client reports a success whose scope is the opposite of the one
// asked for. That is issue #472, and on the install it was found on it would have frozen every
// application in an environment while the operator believed one was protected.
//
// In the path it is a route, and a route the server does not have is already handled: the
// ADR-0039 handshake turns an unknown route into a structured "this control plane (vX) does not
// recognize ...; ask an operator to run `burrow cluster upgrade`" refusal, naming both versions. So the
// scope the server cannot honour is refused by the mechanism that already exists, with nothing
// written, rather than by a second mechanism that would have to be told about every new parameter.
//
// The rule this encodes, for anything added later: A REQUEST PARAMETER THAT NARROWS THE SCOPE OF A
// WRITE BELONGS IN THE ROUTE. A parameter that selects an existing route's behaviour in a way an
// older server would honour or reject on its own may stay a parameter. It applies to a BODY field
// exactly as it applies to a query parameter: every burrowd released before v0.15 read the body with
// unknown fields allowed, so it drops a field it does not know just as silently. (A current burrowd
// refuses one instead — see the control plane's `decode` — but that only helps once the control plane
// on the other end is a current one, which is exactly what cannot be assumed here.)
//
// Issue #485 audited the rest of the API against this rule. Every write that violated it now follows
// it: RemoveAddon, DeleteApp, DetachAddon and RestoreAddon, then Deploy, Rollback, Scale, Expose,
// Unexpose, SetSecret, UnsetSecret, InstallAddon and BackupAddon.
//
// THE READS DO NOT, WITH ONE EXCEPTION, and the exception is the test of the rule rather than a hole
// in it. A read answered one scope out misinforms a reader and changes nothing, and every write it
// might lead to is refused on its own — so Status, Logs, Apps, Reachability, Config and Secrets keep
// the query parameter (see withEnv). Backups does not, because its answer is an ARGUMENT: the id it
// returns is fed to a restore that overwrites a live database, and no later refusal can tell that the
// id came from the wrong list.
//
// A parameter that CANNOT OUTRUN ITS OWN ROUTE needs none of this, whatever it narrows: it can only
// reach a control plane that shipped no earlier than it did. That is why `skip_final_backup` stays a
// parameter on the removal, and why the physical backup's and physical restore's `env` and
// `destination` do — those routes and their parameters shipped in the same release.
func guardPath(scope GuardScope, code string) string {
	path := "/v1/guard"
	if scope.Name != "" {
		path += "/name/" + url.PathEscape(scope.Name)
	}
	if code != "" {
		path += "/" + url.PathEscape(code)
	}
	return withEnv(path, scope.Env)
}

// CodeScopeUnsupported is the machine-readable code on a refusal this CLIENT raises: the caller
// asked for a scope the control plane on the other end cannot express, so the call was not
// performed at a wider one. It is not a code any control plane returns — it is the client naming a
// gap it detected from the server's own unknown-route refusal (ADR-0039) — and it is an APIError
// like every other refusal so a --json caller and an agent branch on it the same way.
const CodeScopeUnsupported = "scope_unsupported"

// guardScopeRefusal words scopeRefusal for the guardrail name tier. It fires only for a name-scoped
// call, because that is the only scope on this route a supported control plane can lack.
//
// An empty code is the read and a code is the write, and the two are worded differently because
// only one of them could have changed anything: "nothing was written" is the sentence an operator
// needs, and printing it after a listing would be a lie about what was at stake.
func guardScopeRefusal(scope GuardScope, code string, err error) error {
	if scope.Name == "" {
		return err
	}
	where := "the default environment"
	if scope.Env != "" {
		where = strconv.Quote(scope.Env)
	}
	what := fmt.Sprintf("this control plane cannot report the policy for one app or add-on instance, so there is nothing to show: what it would answer with is the policy for every app in %s", where)
	if code != "" {
		what = fmt.Sprintf("this control plane cannot scope a guardrail to one app or add-on instance, so nothing was written: the same call without the name would have set %q for every app in %s", code, where)
	}
	return scopeRefusal(what, "per-app guardrails", err)
}

// scopeRefusal is the shared body of every "the scope you asked for is not one this control plane
// can express" refusal: guardScopeRefusal's rule (issue #472), generalized to the destructive calls
// that carried the same shape (issue #485).
//
// what is the sentence naming the operation and — the part an operator needs — what the same call
// would have done instead, in the past tense of something that did NOT happen. predates names the
// feature the control plane is missing, for the one case where it cannot say so itself.
//
// It fires only for the two answers that mean "no such route": the structured unknown_operation the
// ADR-0039 handshake produces, and a bare 404 from a control plane older than the handshake itself.
// An engine 404 — an unknown environment, say — carries "not_found" and is a real answer to a real
// route, so it passes through unchanged, and so does every other error. Callers invoke it only when
// they narrowed the request, because an unnarrowed call has no wider scope to have landed at.
//
// The server's own message rides along verbatim when there is one, because it is the half that names
// both versions and the upgrade; wording it again here would either duplicate it or contradict it as
// the server's message improves.
func scopeRefusal(what, predates string, err error) error {
	var api *APIError
	if !errors.As(err, &api) || api.StatusCode != http.StatusNotFound {
		return err
	}
	if api.Code != "" && api.Code != CodeUnknownOperation {
		return err
	}
	msg := what
	if api.Code == CodeUnknownOperation {
		msg += ". The control plane reports: " + api.Message
	} else {
		msg += ". It predates " + predates + "; ask an operator to run `burrow cluster upgrade` to update the control plane, then run this again"
	}
	return &APIError{
		StatusCode:    api.StatusCode,
		Code:          CodeScopeUnsupported,
		Message:       msg,
		ServerVersion: api.ServerVersion,
	}
}

// Limit is one operational limit and its effective value (ADR-0068): a bound a human sets, which is
// not a guardrail — there is no disposition on it, and exceeding it is refused rather than held.
// Scope reports which tier the effective value came from ("environment", "cluster", or "default"),
// EnvScoped whether it may be set for one environment at all, and Default the built-in value it
// reverts to.
type Limit struct {
	Code        string `json:"code"`
	Value       string `json:"value"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	Scope       string `json:"scope"`
	EnvScoped   bool   `json:"env_scoped"`
	Default     string `json:"default"`
}

// Limits lists the operational limits and their effective values. An empty env reads the cluster
// tier; a named environment reads its effective configuration under the environment to cluster to
// default fallback, each entry marking which tier its value came from (ADR-0068 §3).
func (c *Client) Limits(ctx context.Context, env string) ([]Limit, error) {
	var out struct {
		Limits []Limit `json:"limits"`
	}
	err := c.do(ctx, http.MethodGet, withEnv("/v1/config", env), nil, &out)
	return out.Limits, err
}

// SetLimit sets an operational limit's value and returns the updated configuration. An empty env
// sets the cluster value; a named environment scopes it to that environment, storing the
// env-prefixed code (ADR-0068 §3). It is on this client because it is on the operator CLI:
// `burrow-agent` carries no command that reaches it (ADR-0068 §4).
func (c *Client) SetLimit(ctx context.Context, env, code, value string) ([]Limit, error) {
	var out struct {
		Limits []Limit `json:"limits"`
	}
	body := map[string]string{"value": value}
	err := c.do(ctx, http.MethodPut, withEnv("/v1/config/"+url.PathEscape(code), env), body, &out)
	return out.Limits, err
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

// HealthResult is an app's health configuration (ADR-0076): the endpoint the user or their agent
// declared, and — the part that actually matters — the readiness probe Burrow authors on the
// container as a result, which is not the same fact. An endpoint declared before the app was
// published resolves to no probe at all, and a surface that reported only the declaration would let
// that gap sit unnoticed.
type HealthResult struct {
	App         string `json:"app"`
	Environment string `json:"environment,omitempty"`
	// Path and Port are the DECLARED endpoint; empty and zero when none was declared.
	Path string `json:"path,omitempty"`
	Port int32  `json:"port,omitempty"`
	// Probe is what Burrow authors: "http", "tcp", or "none".
	Probe     string `json:"probe"`
	ProbePort int32  `json:"probe_port,omitempty"`
	ProbePath string `json:"probe_path,omitempty"`
	// Source is where the probe came from: "endpoint", "exposure" (the conservative TCP default on
	// the published port), or "none".
	Source string `json:"source"`
	// Liveness is always false: Burrow never sets a liveness probe by default (ADR-0076 §1). It is
	// reported anyway because "does this restart my container?" is the first question a reader has.
	Liveness bool `json:"liveness"`
	// Hint is the ADR-0076 §5 guidance, present when no endpoint has been declared.
	Hint string `json:"hint,omitempty"`
	// AppliesOn says when the reported probe reaches the running pods, when it is not there yet.
	AppliesOn string `json:"applies_on,omitempty"`
}

// Health returns an app's declared health endpoint and the readiness probe Burrow authors from it
// (ADR-0076). An empty env reads the default environment.
func (c *Client) Health(ctx context.Context, app, env string) (HealthResult, error) {
	var out HealthResult
	err := c.do(ctx, http.MethodGet, withEnv(c.appPath(app, "health"), env), nil, &out)
	return out, err
}

// SetHealth declares the health endpoint app serves in env and re-applies the running workload so
// the probe reaches its pods (ADR-0076 §5). A zero port means "the port the app is published on".
func (c *Client) SetHealth(ctx context.Context, app, env, path string, port int32) (HealthResult, error) {
	var out HealthResult
	body := map[string]any{"path": path, "port": port}
	err := c.do(ctx, http.MethodPut, withEnv(c.appPath(app, "health"), env), body, &out)
	return out, err
}

// UnsetHealth removes app's declared endpoint, returning it to the conservative default — a TCP
// check on the published port, or no probe when the app is not published (ADR-0076 §3).
func (c *Client) UnsetHealth(ctx context.Context, app, env string) (HealthResult, error) {
	var out HealthResult
	err := c.do(ctx, http.MethodDelete, withEnv(c.appPath(app, "health"), env), nil, &out)
	return out, err
}

// Dependency is one thing Burrow provisioned for an app and can therefore check at deploy time
// (ADR-0076 §4). It carries no credential: EnvKey is a key NAME the app already reads, and Endpoint
// is an in-cluster address Burrow composed.
type Dependency struct {
	Kind        string `json:"kind"`
	Provisioned string `json:"provisioned"`
	EnvKey      string `json:"env_key,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
}

// DependencyResult is what one deploy-time dependency check found: passed, failed, or skipped, with
// a reason from a closed set and one bounded Burrow-authored line. A failed result never means the
// deploy failed — the check is reported, never fatal (ADR-0076 §4).
type DependencyResult struct {
	Kind    string `json:"kind"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Status  int    `json:"status,omitempty"`
}

// ChecksResult is what `burrow app checks` reports: whether the deploy-time dependency check runs
// for an app, and what Burrow derived from what it provisioned that it would check (ADR-0076 §4).
type ChecksResult struct {
	App          string       `json:"app"`
	Environment  string       `json:"environment,omitempty"`
	Enabled      bool         `json:"enabled"`
	Dependencies []Dependency `json:"dependencies"`
	Note         string       `json:"note,omitempty"`
}

// Checks reports the deploy-time dependency check for an app (ADR-0076 §4). An empty env reads the
// default environment. It runs no check — it reports what one would do.
func (c *Client) Checks(ctx context.Context, app, env string) (ChecksResult, error) {
	var out ChecksResult
	err := c.do(ctx, http.MethodGet, withEnv(c.appPath(app, "checks"), env), nil, &out)
	return out, err
}

// SetChecks turns the deploy-time dependency check on or off for app in env (ADR-0076 §4).
func (c *Client) SetChecks(ctx context.Context, app, env string, enabled bool) (ChecksResult, error) {
	var out ChecksResult
	err := c.do(ctx, http.MethodPut, withEnv(c.appPath(app, "checks"), env), map[string]any{"enabled": enabled}, &out)
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
	err := c.doWithin(ctx, c.budget.provider, http.MethodPost, "/v1/providers", req, &out)
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
// into, and Default marks the default environment `prod` (the app namespace burrowd runs
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
// default environment `prod` first, then the ones added later in name order.
func (c *Client) ListEnvironments(ctx context.Context) ([]Environment, error) {
	var out struct {
		Environments []Environment `json:"environments"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/environments", nil, &out)
	return out.Environments, err
}

// do issues a request under the DEFAULT budget, decoding a 2xx body into out and a non-2xx body
// into an APIError. It is the call for everything that does not wait on the cluster; a call that
// does uses doWithin with the budget derived from the bound it is waiting on.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doWithin(ctx, c.budget.def, method, path, body, out)
}

// doWithin is do under an explicit budget. The budget is applied as a context deadline rather than
// an http.Client.Timeout because a deadline can differ per request and a client-wide timeout cannot
// — which is the whole of issue #404.
//
// A caller's own deadline still wins where it is shorter: context.WithTimeout keeps the earlier of
// the two, so an agent that gives a deploy thirty seconds gets thirty seconds. What can no longer
// happen is this package silently imposing a bound shorter than the work it is waiting for.
func (c *Client) doWithin(ctx context.Context, budget time.Duration, method, path string, body, out any) error {
	return c.within(ctx, budget, func(ctx context.Context) error {
		return c.request(ctx, method, path, body, out)
	})
}

// within applies a per-request budget to call and adds this package's own deadline message to the
// error when the budget — rather than the caller's own deadline — is what cut the call short. It is
// factored out of doWithin so a call that does not go through request, such as the streaming deploy,
// is bounded and reports a timeout identically rather than growing a second story about deadlines.
func (c *Client) within(ctx context.Context, budget time.Duration, call func(context.Context) error) error {
	reqCtx := ctx
	if budget > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	err := call(reqCtx)
	// A request THIS package's budget cut short gets a message saying what a bare "deadline
	// exceeded" does not: the control plane is not a transaction that rolls back when the caller
	// stops listening, so the operation may well be finishing right now. Retrying is what turns one
	// deploy into two. A deadline the CALLER set is left to speak for itself — the caller knows what
	// it asked for.
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w. Burrow gave up waiting after %s; the control plane may still be completing the operation, so check the app's status before retrying — a retry runs it again", err, budget)
	}
	return err
}

// request issues the prepared call, decoding a 2xx body into out and a non-2xx body into an APIError.
func (c *Client) request(ctx context.Context, method, path string, body, out any) error {
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
	return decodeResponse(resp, out)
}

// decodeResponse turns a completed response into either a decoded result or an *APIError. It is
// shared with the streaming deploy, which takes this path whenever the control plane answered with
// an ordinary JSON body — a refusal, or a server that does not offer a progress stream at all.
func decodeResponse(resp *http.Response, out any) error {
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return apiErrorFrom(resp.StatusCode, resp.Status, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

// apiErrorFrom builds the *APIError for a non-2xx body. It is the ONE place the control plane's
// error shape becomes this package's error type, so a refusal that arrives inside a progress stream
// carries the same StatusCode, Code, Message and NeedsConfirmation an ordinary one does — which is
// what lets `burrow-agent` classify a hold identically however the deploy was issued.
func apiErrorFrom(status int, statusText string, body []byte) *APIError {
	var e struct {
		Error             string `json:"error"`
		Code              string `json:"code"`
		NeedsConfirmation bool   `json:"needs_confirmation"`
		ServerVersion     string `json:"server_version"`
		ServerInstallID   string `json:"server_install_id"`
	}
	_ = json.Unmarshal(body, &e)
	msg := e.Error
	if msg == "" {
		if msg = strings.TrimSpace(string(body)); msg == "" {
			msg = statusText
		}
	}
	return &APIError{
		StatusCode:        status,
		Code:              e.Code,
		Message:           msg,
		NeedsConfirmation: e.NeedsConfirmation,
		ServerVersion:     e.ServerVersion,
		ServerInstallID:   e.ServerInstallID,
	}
}
