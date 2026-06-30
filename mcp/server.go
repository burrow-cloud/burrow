// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package mcp

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/connect"
)

// ClientForContext resolves a control-plane client for a kubeconfig context (an environment;
// ADR-0035 phase 1). An empty context string means the current kubeconfig context (today's
// behavior). Each context's cluster runs its own burrowd with its own credentials and its own
// guardrail policy, so the agent targets an environment per call by naming its context. The MCP
// server holds one factory instead of one client (ADR-0035, ADR-0005); burrow-mcp builds it to
// cache a client per context.
type ClientForContext func(kubeContext string) (*client.Client, error)

// EnvironmentLister lists the environments (kubeconfig contexts) the agent can target, marking
// the current one, so the burrow_environments tool can tell the agent what it may name in any
// tool's context argument (ADR-0035 phase 1).
type EnvironmentLister func() ([]connect.Context, error)

// contextArg is embedded in every tool's input so each call can target a specific environment.
// Its single field is promoted into the tool's generated input schema as an optional "context"
// property. An empty value means the current kubeconfig context. It is non-secret: a context
// name is a kubeconfig label, not a credential (ADR-0035, ADR-0005).
type contextArg struct {
	Context string `json:"context,omitempty" jsonschema:"optional: the kubeconfig context (environment) to target, e.g. prod-cluster or staging; default is the current context. Each environment is a separate cluster with its own guardrail policy, so this is how you operate prod versus staging."`
}

// NewServer builds the Burrow MCP server: an agent-neutral surface (ADR-0003) exposing
// the control plane's operations as MCP tools. Each tool translates a call into a
// control-plane API call via the client and returns the structured result; a control
// plane error becomes a tool error the agent can read. The server holds no cluster
// credentials (ADR-0005). It targets one environment per call (ADR-0035): every tool takes
// an optional context, resolved to that cluster's client through clientFor, and
// burrow_environments lists the contexts the agent can name.
func NewServer(clientFor ClientForContext, environments EnvironmentLister, version string) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "burrow", Title: "Burrow", Version: version}, nil)

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_deploy",
		Description: "Deploy an application to the cluster by container image reference. The image must already be pushed to a registry the cluster can pull from; only the reference and small metadata are sent, never code. Environment configuration is NOT passed here: an app's env is a separate, app-global store sourced at deploy time, so set any env the release needs with burrow_env_set BEFORE deploying — the new release then boots with it on first start. (burrow_env_set with no_restart=true followed by burrow_deploy is a single restart.) For SECRETS (DB URLs, API keys), do not put values in env and do not paste secret values into this conversation: ask the user to run `burrow app secret set <app> KEY=VALUE` themselves BEFORE deploying, then confirm the key with burrow_secret_list. Returns the new release and the release it superseded (the rollback handle). Pass context to target a specific cluster/environment (default the current one); this is how you deploy to staging versus prod, and each environment enforces its own guardrails.",
	}, deployTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_env_set",
		Description: "Set (upsert) a non-secret environment variable for an app. The env store is the single source of truth, sourced into the workload at deploy time. By default the running app is rolled so it picks the change up; set no_restart=true to only persist it and let it land on the next deploy (so setting env then deploying is a single restart). For secrets, do not use env — env values are non-secret config.",
	}, envSetTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_env_list",
		Description: "List an app's non-secret environment variables (the env store). Read-only.",
	}, envListTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_env_unset",
		Description: "Remove a non-secret environment variable from an app. By default the running app is rolled so it drops the value; set no_restart=true to only persist the removal and let it land on the next deploy.",
	}, envUnsetTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_secret_list",
		Description: "List the KEYS of an app's secret environment variables — never the values (secret values never travel over MCP; ADR-0029). Read-only. Use this to confirm a secret the app needs is present before deploying. To SET a secret value, there is no tool: NEVER ask the user to paste a secret value into this conversation (anything in the prompt is retained in context and re-sent on later tool calls). Instead, ask the user to run `burrow app secret set <app> KEY=VALUE` themselves at their terminal, then confirm with this list tool.",
	}, secretListTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_secret_unset",
		Description: "Remove a secret environment variable from an app by KEY (no value crosses MCP). By default the running app is rolled so it drops the value; set no_restart=true to only persist the removal and let it land on the next deploy. To SET a secret, ask the user to run `burrow app secret set <app> KEY=VALUE` themselves — never have them paste a secret value into this conversation.",
	}, secretUnsetTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_apps",
		Description: "List the applications Burrow manages and each one's running state (image, ready/desired replicas, availability), so you can discover what is deployed before operating on it. Read-only. Pass context to survey a specific cluster/environment (default the current one); this is how you compare prod versus staging.",
	}, appsTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_addons",
		Description: "List the backing-service add-ons installed on the cluster (logs, …), their mode (installed/connected), in-cluster endpoint, and the capabilities you can query. Read-only.",
	}, addonsTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_addon_install",
		Description: "Install a vetted, self-hostable backing service for a capability (e.g. logs → VictoriaLogs) and register it as queryable, in one step. Held for confirmation by a guardrail; set confirm=true ONLY after the user approves, never on your own.",
	}, addonInstallTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_addon_remove",
		Description: "Remove an installed add-on by name. Held for confirmation by a guardrail (removing a backing service can break dependent apps); set confirm=true ONLY after the user approves.",
	}, addonRemoveTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_addon_attach",
		Description: "Give an application its own database on the installed Postgres add-on and wire it in. You supply only the add-on type (\"postgres\") and the app name — NO secret. Burrow generates the database, role, and connection string server-side and writes it into the app's Secret as DATABASE_URL; the value is never returned to you or shown in this conversation. After attaching, write integration code that reads the DATABASE_URL environment variable. Re-attaching rotates the password. Returns only the app, the add-on, and the key name (DATABASE_URL) — never a connection string. Install the postgres add-on first with burrow_addon_install if it is not yet installed.",
	}, addonAttachTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_addon_backup",
		Description: "Back up an application's database on the installed Postgres add-on. You supply only the add-on type (\"postgres\") and the app name — NO secret. Burrow runs an in-cluster Job that dumps the database to a backup volume and records the backup; the database superuser password never crosses this tool or appears in the result. Returns the recorded backup (its id, the app, the on-volume path, the size, and the status) — never a connection string. Backup destroys nothing, so it is allowed. To RESTORE a backup (which overwrites live data) there is no tool: ask the user to run `burrow addon restore postgres <app> --backup <id>` themselves.",
	}, addonBackupTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_addon_backups",
		Description: "List the recorded database backups for the Postgres add-on (id, app, time, size, status), so you can see what restore points exist. Pass the add-on type (\"postgres\") and optionally an app to restrict the listing; omit the app to list every app's backups. Read-only — restoring is CLI-only.",
	}, addonBackupsTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_logs_query",
		Description: "Query the cluster's aggregated logs (the installed logs add-on) with a VictoriaLogs LogsQL query to investigate why an app is failing or slow — e.g. `error`, `level:error`, `panic AND web`. Returns recent matching records (most recent first). Needs a logs add-on installed first (burrow_addon_install with capability \"logs\").",
	}, logsQueryTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_metrics_query",
		Description: "Run an instant PromQL query against the cluster's connected metrics store (Prometheus or VictoriaMetrics) to answer how an app is performing — CPU, memory, request rate, error rate, latency. Examples: `up`, `rate(http_requests_total[5m])`, `sum(rate(http_requests_total{status=~\"5..\"}[5m]))`, `container_memory_usage_bytes`. Returns the matching samples (each with its labels and value). Needs a metrics add-on connected first (`burrow addon connect prometheus`).",
	}, metricsQueryTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_app_delete",
		Description: "Delete an application entirely: its workload, its routing (Service and Ingress), and its recorded release history, so it disappears from the apps listing and from status. This is destructive and irreversible. Held for confirmation by a guardrail by default; set confirm=true ONLY after the user explicitly approves, never on your own.",
	}, appDeleteTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_status",
		Description: "Report an application's status: its most recent release and the live workload state (desired/ready replicas, availability). Pass context to read a specific cluster/environment (default the current one); this is how you check prod versus staging.",
	}, statusTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_logs",
		Description: "Return recent log lines for an application's workload.",
	}, logsTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_rollback",
		Description: "Roll an application back to its previously running release by redeploying that release's image reference. Returns the new release and which release it restored. Allowed by default (rollback is a recovery action), but an operator may configure a guardrail to hold it for confirmation; when held, the error says so — ask the user, then retry with confirm set to true.",
	}, rollbackTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_scale",
		Description: "Change an application's replica count. A guardrail may refuse it (e.g. above the replica ceiling) or hold it for confirmation (e.g. scaling to zero); when held, the error says so — ask the user, then retry with confirm set to true.",
	}, scaleTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_expose",
		Description: "Make a deployed application reachable from outside the cluster at a hostname, by creating a Service and an Ingress. Public exposure is held for confirmation by a guardrail by default; when held, the error says so — ask the user, then retry with confirm set to true. Reachability also needs an ingress controller and DNS pointing the host at the cluster.",
	}, exposeTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_unexpose",
		Description: "Remove an application's exposure (its Service and Ingress). Does not affect the running workload.",
	}, unexposeTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_reachability",
		Description: "Report whether an application is reachable at its hostname, link by link: deployed and ready, exposed, given an external address by an ingress controller, a TLS certificate when one was requested, and DNS pointing the host at that address. Returns a plain one-line summary plus the full chain, so you can tell the user exactly which link is missing and what to do. After deploying, exposing (burrow_expose), and pointing DNS at the cluster, call this with wait set to true to poll until the app is live and get its URL; when the app converges, reachable is true and url is the live address, and if it returns blocked_on that names the one link to fix. Read-only.",
	}, reachabilityTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_domain_add",
		Description: "Point a hostname at the cluster by creating or updating a DNS record at a configured provider (e.g. DigitalOcean or Cloudflare). Give the cluster's external address (the IP or hostname from burrow_reachability once the app is exposed); an IPv4 address becomes an A record, a hostname a CNAME. A guardrail holds public DNS writes for confirmation by default; when held, the error says so — ask the user, then retry with confirm set to true. The provider must already be configured by the operator.",
	}, domainAddTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_domain_remove",
		Description: "Remove the DNS record a configured provider holds for a hostname. Deleting a public DNS record is held for confirmation by a guardrail by default; when held, the error says so — ask the user, then retry with confirm set to true.",
	}, domainRemoveTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_providers",
		Description: "List the configured cloud providers and the capabilities each serves (e.g. dns), so you know which provider name to pass for an operation like burrow_domain_add. Read-only: provider credentials are configured by the operator via the CLI, never by an agent.",
	}, providersTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_guard",
		Description: "List the control-plane guardrails and their current dispositions (allow, confirm, or deny), so you can tell in advance whether an operation will be allowed, held for the user's confirmation, or denied. Read-only: guardrail policy is changed only by the operator via the CLI, never by an agent. Pass context to read a specific cluster/environment's guardrails (default the current one); each environment has its own policy, so prod can be locked down while staging stays permissive.",
	}, guardTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_cluster",
		Description: "See what this cluster can do before you change anything (ADR-0034): a neutral, read-only report of its capabilities, read live. Tells you whether an ingress controller is installed and which IngressClass to use, whether there is a default StorageClass (and its name) for persistent volumes, whether Service type=LoadBalancer is likely supported (inferred from the detected cloud provider) or the cluster is NodePort-only, whether cert-manager is installed (for TLS), the cloud provider, and whether a DNS provider is configured. Use it to survey a cluster and explain its state — and to know whether an operation like exposing an app or requesting a certificate will work — before doing anything. When an ingress controller or cert-manager is missing, the remediation is not an agent action: recommend the human run `burrow system ingress install` (it installs whichever pieces are missing, with a cost-aware LoadBalancer-vs-NodePort choice). Read-only: it changes nothing and returns no secret. Pass context to survey a specific cluster/environment (default the current one); this is how you compare prod versus staging.",
	}, clusterTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_audit",
		Description: "Review the control plane's append-only audit log (ADR-0027): the durable record of the guarded, mutating operations that ran and the guardrail outcome of each — allowed, held (confirmation required, not executed), denied, executed (allowed, or confirmed and carried out), or failed. Use it to answer \"what did the agent do,\" \"what was held or denied,\" and to show that a dangerous action asked first. Newest first; optionally filter by app/host/add-on target, operation (e.g. deploy, rollback, app_delete), outcome, and limit. Read-only — the log has no write or alter path. Args are redacted at the source to KEY NAMES and safe metadata (image reference, replica count, env/secret key names) — never an env value, token, or secret.",
	}, auditTool(clientFor))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_environments",
		Description: "List the environments you can target: the kubeconfig contexts, each a separate cluster running its own burrowd with its own guardrail policy (ADR-0035). Marks the current context, which any tool uses when you omit its context argument. Pass one of these names as the context argument on another tool to operate that environment, so you can deploy to staging and gate prod by naming the context. Read-only: it reads the local kubeconfig and contacts no cluster.",
	}, environmentsTool(environments))

	return s
}

// Serve runs the Burrow MCP server over stdio until the client disconnects. It targets one
// environment per call through clientFor (ADR-0035), and lists the available environments via
// environments.
func Serve(ctx context.Context, clientFor ClientForContext, environments EnvironmentLister, version string) error {
	return NewServer(clientFor, environments, version).Run(ctx, &sdk.StdioTransport{})
}

type deployInput struct {
	contextArg
	App         string   `json:"app" jsonschema:"the application name (a DNS-1123 label)"`
	Image       string   `json:"image" jsonschema:"the pullable container image reference to deploy, e.g. registry.example.com/app:1.2.3"`
	Command     []string `json:"command,omitempty" jsonschema:"optional command override for the container"`
	MetricsPort int32    `json:"metrics_port,omitempty" jsonschema:"optional: annotate the pod so the metrics add-on scrapes /metrics on this port"`
	Replicas    int32    `json:"replicas" jsonschema:"desired number of replicas"`
	Confirm     bool     `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed an operation a guardrail held for confirmation; do not self-confirm"`
}

type appInput struct {
	contextArg
	App string `json:"app" jsonschema:"the application name"`
}

type logsInput struct {
	contextArg
	App  string `json:"app" jsonschema:"the application name"`
	Tail int    `json:"tail,omitempty" jsonschema:"maximum number of recent log lines to return"`
}

type scaleInput struct {
	contextArg
	App      string `json:"app" jsonschema:"the application name"`
	Replicas int32  `json:"replicas" jsonschema:"desired number of replicas"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed an operation a guardrail held for confirmation (e.g. scaling to zero); do not self-confirm"`
}

func deployTool(clientFor ClientForContext) sdk.ToolHandlerFor[deployInput, client.DeployResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in deployInput) (*sdk.CallToolResult, client.DeployResult, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, client.DeployResult{}, err
		}
		res, err := c.Deploy(ctx, in.App, client.DeployRequest{Image: in.Image, Command: in.Command, MetricsPort: in.MetricsPort, Replicas: in.Replicas, Confirm: in.Confirm})
		if err != nil {
			return nil, client.DeployResult{}, err
		}
		return nil, res, nil
	}
}

type envSetInput struct {
	contextArg
	App       string `json:"app" jsonschema:"the application name"`
	Key       string `json:"key" jsonschema:"the environment variable name (e.g. LOG_LEVEL)"`
	Value     string `json:"value" jsonschema:"the value to set"`
	NoRestart bool   `json:"no_restart,omitempty" jsonschema:"true to persist without rolling the running app; the change lands on the next deploy"`
}

type envUnsetInput struct {
	contextArg
	App       string `json:"app" jsonschema:"the application name"`
	Key       string `json:"key" jsonschema:"the environment variable name to remove"`
	NoRestart bool   `json:"no_restart,omitempty" jsonschema:"true to persist the removal without rolling the running app; the change lands on the next deploy"`
}

// envAck is a small structured ack for an env mutation.
type envAck struct {
	App string `json:"app"`
	Key string `json:"key"`
}

type envOutput struct {
	Env map[string]string `json:"env"`
}

func envSetTool(clientFor ClientForContext) sdk.ToolHandlerFor[envSetInput, envAck] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in envSetInput) (*sdk.CallToolResult, envAck, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, envAck{}, err
		}
		if err := c.SetEnv(ctx, in.App, in.Key, in.Value, in.NoRestart); err != nil {
			return nil, envAck{}, err
		}
		return nil, envAck{App: in.App, Key: in.Key}, nil
	}
}

func envUnsetTool(clientFor ClientForContext) sdk.ToolHandlerFor[envUnsetInput, envAck] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in envUnsetInput) (*sdk.CallToolResult, envAck, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, envAck{}, err
		}
		if err := c.UnsetEnv(ctx, in.App, in.Key, in.NoRestart); err != nil {
			return nil, envAck{}, err
		}
		return nil, envAck{App: in.App, Key: in.Key}, nil
	}
}

func envListTool(clientFor ClientForContext) sdk.ToolHandlerFor[appInput, envOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, envOutput, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, envOutput{}, err
		}
		env, err := c.Env(ctx, in.App)
		if err != nil {
			return nil, envOutput{}, err
		}
		return nil, envOutput{Env: env}, nil
	}
}

type secretUnsetInput struct {
	contextArg
	App       string `json:"app" jsonschema:"the application name"`
	Key       string `json:"key" jsonschema:"the secret environment variable name to remove (the KEY, not a value)"`
	NoRestart bool   `json:"no_restart,omitempty" jsonschema:"true to persist the removal without rolling the running app; the change lands on the next deploy"`
}

// secretsOutput is an app's secret KEYS only — never the values (ADR-0028/0004).
type secretsOutput struct {
	Keys []string `json:"keys"`
}

func secretListTool(clientFor ClientForContext) sdk.ToolHandlerFor[appInput, secretsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, secretsOutput, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, secretsOutput{}, err
		}
		keys, err := c.Secrets(ctx, in.App)
		if err != nil {
			return nil, secretsOutput{}, err
		}
		return nil, secretsOutput{Keys: keys}, nil
	}
}

func secretUnsetTool(clientFor ClientForContext) sdk.ToolHandlerFor[secretUnsetInput, envAck] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in secretUnsetInput) (*sdk.CallToolResult, envAck, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, envAck{}, err
		}
		if err := c.UnsetSecret(ctx, in.App, in.Key, in.NoRestart); err != nil {
			return nil, envAck{}, err
		}
		return nil, envAck{App: in.App, Key: in.Key}, nil
	}
}

func statusTool(clientFor ClientForContext) sdk.ToolHandlerFor[appInput, client.StatusResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, client.StatusResult, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, client.StatusResult{}, err
		}
		res, err := c.Status(ctx, in.App)
		if err != nil {
			return nil, client.StatusResult{}, err
		}
		return nil, res, nil
	}
}

// logsOutput wraps the lines so the tool has a structured object output.
type logsOutput struct {
	Lines []client.LogLine `json:"lines"`
}

func logsTool(clientFor ClientForContext) sdk.ToolHandlerFor[logsInput, logsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in logsInput) (*sdk.CallToolResult, logsOutput, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, logsOutput{}, err
		}
		lines, err := c.Logs(ctx, in.App, in.Tail)
		if err != nil {
			return nil, logsOutput{}, err
		}
		return nil, logsOutput{Lines: lines}, nil
	}
}

type rollbackInput struct {
	contextArg
	App     string `json:"app" jsonschema:"the application name"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed a rollback that an operator's guardrail held for confirmation; do not self-confirm"`
}

func rollbackTool(clientFor ClientForContext) sdk.ToolHandlerFor[rollbackInput, client.RollbackResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in rollbackInput) (*sdk.CallToolResult, client.RollbackResult, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, client.RollbackResult{}, err
		}
		res, err := c.Rollback(ctx, in.App, in.Confirm)
		if err != nil {
			return nil, client.RollbackResult{}, err
		}
		return nil, res, nil
	}
}

func scaleTool(clientFor ClientForContext) sdk.ToolHandlerFor[scaleInput, client.ScaleResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in scaleInput) (*sdk.CallToolResult, client.ScaleResult, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, client.ScaleResult{}, err
		}
		res, err := c.Scale(ctx, in.App, in.Replicas, in.Confirm)
		if err != nil {
			return nil, client.ScaleResult{}, err
		}
		return nil, res, nil
	}
}

type exposeInput struct {
	contextArg
	App     string `json:"app" jsonschema:"the application name"`
	Host    string `json:"host" jsonschema:"the external hostname to route to the app, e.g. app.example.com"`
	Port    int32  `json:"port" jsonschema:"the app's container port to forward to"`
	TLS     bool   `json:"tls,omitempty" jsonschema:"request an HTTPS certificate for the host via cert-manager"`
	Issuer  string `json:"issuer,omitempty" jsonschema:"the cert-manager ClusterIssuer to use when tls is set (e.g. letsencrypt)"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed exposing the app to the public internet; do not self-confirm"`
}

func exposeTool(clientFor ClientForContext) sdk.ToolHandlerFor[exposeInput, client.ExposeResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in exposeInput) (*sdk.CallToolResult, client.ExposeResult, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, client.ExposeResult{}, err
		}
		res, err := c.Expose(ctx, in.App, in.Host, in.Port, in.TLS, in.Issuer, in.Confirm)
		if err != nil {
			return nil, client.ExposeResult{}, err
		}
		return nil, res, nil
	}
}

// unexposeOutput is a small structured ack for the unexpose tool.
type unexposeOutput struct {
	App string `json:"app"`
}

func unexposeTool(clientFor ClientForContext) sdk.ToolHandlerFor[appInput, unexposeOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, unexposeOutput, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, unexposeOutput{}, err
		}
		if err := c.Unexpose(ctx, in.App); err != nil {
			return nil, unexposeOutput{}, err
		}
		return nil, unexposeOutput{App: in.App}, nil
	}
}

type reachabilityInput struct {
	contextArg
	App  string `json:"app" jsonschema:"the application name"`
	Wait bool   `json:"wait,omitempty" jsonschema:"poll until the app is live (reachable) or a timeout, instead of a single point-in-time check; use after deploying, exposing, and pointing DNS to confirm the app is live and get its URL"`
}

// reachabilityWaitTimeout bounds how long the burrow_reachability tool polls in wait mode before
// returning the last verdict. The control-plane engine stays point-in-time; this wait lives in
// the thin client layer (ADR-0034 slice 3).
const reachabilityWaitTimeout = 3 * time.Minute

func reachabilityTool(clientFor ClientForContext) sdk.ToolHandlerFor[reachabilityInput, client.ReachabilityResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in reachabilityInput) (*sdk.CallToolResult, client.ReachabilityResult, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, client.ReachabilityResult{}, err
		}
		reach := c.Reachability
		if in.Wait {
			reach = func(ctx context.Context, app string) (client.ReachabilityResult, error) {
				return c.WaitReachable(ctx, app, reachabilityWaitTimeout, nil)
			}
		}
		res, err := reach(ctx, in.App)
		if err != nil {
			return nil, client.ReachabilityResult{}, err
		}
		return nil, res, nil
	}
}

type domainAddInput struct {
	contextArg
	Host     string `json:"host" jsonschema:"the hostname to point at the cluster, e.g. app.example.com"`
	Provider string `json:"provider,omitempty" jsonschema:"the configured DNS provider to write the record at (its name from burrow_providers); omit to use the only one configured"`
	Address  string `json:"address,omitempty" jsonschema:"the cluster's external IPv4 address or hostname to point at; omit if you set app instead"`
	App      string `json:"app,omitempty" jsonschema:"an exposed app whose external address to point at, instead of address — the control plane reads it from the app's ingress"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed writing the public DNS record; do not self-confirm"`
}

func domainAddTool(clientFor ClientForContext) sdk.ToolHandlerFor[domainAddInput, client.DomainResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in domainAddInput) (*sdk.CallToolResult, client.DomainResult, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, client.DomainResult{}, err
		}
		res, err := c.AddDomain(ctx, in.Host, in.Provider, in.Address, in.App, in.Confirm)
		if err != nil {
			return nil, client.DomainResult{}, err
		}
		return nil, res, nil
	}
}

type domainRemoveInput struct {
	contextArg
	Host     string `json:"host" jsonschema:"the hostname whose DNS record to remove"`
	Provider string `json:"provider,omitempty" jsonschema:"the configured DNS provider holding the record; omit to use the only one configured"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed deleting the public DNS record; do not self-confirm"`
}

func domainRemoveTool(clientFor ClientForContext) sdk.ToolHandlerFor[domainRemoveInput, client.DomainResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in domainRemoveInput) (*sdk.CallToolResult, client.DomainResult, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, client.DomainResult{}, err
		}
		res, err := c.RemoveDomain(ctx, in.Host, in.Provider, in.Confirm)
		if err != nil {
			return nil, client.DomainResult{}, err
		}
		return nil, res, nil
	}
}

// providersInput carries only the optional context: listing providers takes no other arguments.
type providersInput struct {
	contextArg
}

// providerInfo is the non-secret view of a configured provider the agent sees.
type providerInfo struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Capabilities []string `json:"capabilities"`
}

type providersOutput struct {
	Providers []providerInfo `json:"providers"`
}

// appsInput carries only the optional context: listing apps takes no other arguments.
type appsInput struct {
	contextArg
}

// appInfo is one app's running state in the apps listing.
type appInfo struct {
	App             string `json:"app"`
	Image           string `json:"image"`
	DesiredReplicas int32  `json:"desired_replicas"`
	ReadyReplicas   int32  `json:"ready_replicas"`
	Available       bool   `json:"available"`
}

type appsOutput struct {
	Apps []appInfo `json:"apps"`
}

func appsTool(clientFor ClientForContext) sdk.ToolHandlerFor[appsInput, appsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appsInput) (*sdk.CallToolResult, appsOutput, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, appsOutput{}, err
		}
		apps, err := c.Apps(ctx)
		if err != nil {
			return nil, appsOutput{}, err
		}
		out := appsOutput{Apps: make([]appInfo, 0, len(apps))}
		for _, a := range apps {
			out.Apps = append(out.Apps, appInfo{
				App: a.App, Image: a.Image,
				DesiredReplicas: a.DesiredReplicas, ReadyReplicas: a.ReadyReplicas, Available: a.Available,
			})
		}
		return nil, out, nil
	}
}

// addonItem is the agent's view of one installed add-on.
type addonItem struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Mode         string   `json:"mode"`
	Endpoint     string   `json:"endpoint"`
	Capabilities []string `json:"capabilities"`
	Ready        bool     `json:"ready"`
}

func toAddonItem(a client.Addon) addonItem {
	return addonItem{Name: a.Name, Type: a.Type, Mode: a.Mode, Endpoint: a.Endpoint, Capabilities: a.Capabilities, Ready: a.Ready}
}

// addonsInput carries only the optional context: listing add-ons takes no other arguments.
type addonsInput struct {
	contextArg
}

type addonsOutput struct {
	Addons []addonItem `json:"addons"`
}

func addonsTool(clientFor ClientForContext) sdk.ToolHandlerFor[addonsInput, addonsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addonsInput) (*sdk.CallToolResult, addonsOutput, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, addonsOutput{}, err
		}
		as, err := c.Addons(ctx)
		if err != nil {
			return nil, addonsOutput{}, err
		}
		out := addonsOutput{Addons: make([]addonItem, 0, len(as))}
		for _, a := range as {
			out.Addons = append(out.Addons, toAddonItem(a))
		}
		return nil, out, nil
	}
}

type addonInstallInput struct {
	contextArg
	Capability string `json:"capability" jsonschema:"the capability to install a vetted backing service for, e.g. logs"`
	Confirm    bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed an operation a guardrail held for confirmation; do not self-confirm"`
}

func addonInstallTool(clientFor ClientForContext) sdk.ToolHandlerFor[addonInstallInput, addonItem] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addonInstallInput) (*sdk.CallToolResult, addonItem, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, addonItem{}, err
		}
		a, err := c.InstallAddon(ctx, in.Capability, in.Confirm)
		if err != nil {
			return nil, addonItem{}, err
		}
		return nil, toAddonItem(a), nil
	}
}

type appDeleteInput struct {
	contextArg
	App     string `json:"app" jsonschema:"the application name to delete (a DNS-1123 label)"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed this destructive delete; do not self-confirm"`
}

type appDeleteOutput struct {
	Deleted string `json:"deleted"`
}

func appDeleteTool(clientFor ClientForContext) sdk.ToolHandlerFor[appDeleteInput, appDeleteOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appDeleteInput) (*sdk.CallToolResult, appDeleteOutput, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, appDeleteOutput{}, err
		}
		if err := c.DeleteApp(ctx, in.App, in.Confirm); err != nil {
			return nil, appDeleteOutput{}, err
		}
		return nil, appDeleteOutput{Deleted: in.App}, nil
	}
}

type addonRemoveInput struct {
	contextArg
	Name    string `json:"name" jsonschema:"the add-on instance name to remove"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed; do not self-confirm"`
}

type addonRemoveOutput struct {
	Removed string `json:"removed"`
}

// addonAttachInput carries only the add-on type and app name — never a secret (ADR-0031).
type addonAttachInput struct {
	contextArg
	Addon string `json:"addon" jsonschema:"the add-on type to attach, e.g. postgres"`
	App   string `json:"app" jsonschema:"the application name to give a database (a DNS-1123 label)"`
}

// addonAttachOutput is the non-secret ack: the app, the add-on, and the KEY the connection string
// was written under — never the value (ADR-0031).
type addonAttachOutput struct {
	App       string `json:"app"`
	Addon     string `json:"addon"`
	SecretKey string `json:"secret_key"`
}

func addonAttachTool(clientFor ClientForContext) sdk.ToolHandlerFor[addonAttachInput, addonAttachOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addonAttachInput) (*sdk.CallToolResult, addonAttachOutput, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, addonAttachOutput{}, err
		}
		res, err := c.AttachAddon(ctx, in.Addon, in.App)
		if err != nil {
			return nil, addonAttachOutput{}, err
		}
		return nil, addonAttachOutput{App: res.App, Addon: res.Addon, SecretKey: res.SecretKey}, nil
	}
}

// addonBackupInput carries only the add-on type and app name — never a secret (ADR-0032).
type addonBackupInput struct {
	contextArg
	Addon string `json:"addon" jsonschema:"the add-on type to back up, e.g. postgres"`
	App   string `json:"app" jsonschema:"the application whose database to back up (a DNS-1123 label)"`
}

// backupItem is the agent's view of one recorded backup — id, app, time, size, status, and the
// on-volume path. Never a credential (ADR-0032).
type backupItem struct {
	ID        string `json:"id"`
	App       string `json:"app"`
	CreatedAt string `json:"created_at"`
	Path      string `json:"path,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Status    string `json:"status"`
}

func toBackupItem(b client.Backup) backupItem {
	return backupItem{ID: b.ID, App: b.App, CreatedAt: b.CreatedAt, Path: b.Path, SizeBytes: b.SizeBytes, Status: b.Status}
}

func addonBackupTool(clientFor ClientForContext) sdk.ToolHandlerFor[addonBackupInput, backupItem] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addonBackupInput) (*sdk.CallToolResult, backupItem, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, backupItem{}, err
		}
		res, err := c.BackupAddon(ctx, in.Addon, in.App)
		if err != nil {
			return nil, backupItem{}, err
		}
		return nil, toBackupItem(res.Backup), nil
	}
}

// addonBackupsInput carries the add-on type and an optional app filter — never a secret.
type addonBackupsInput struct {
	contextArg
	Addon string `json:"addon" jsonschema:"the add-on type to list backups for, e.g. postgres"`
	App   string `json:"app,omitempty" jsonschema:"optional: restrict to one app; omit to list every app's backups"`
}

type addonBackupsOutput struct {
	Backups []backupItem `json:"backups"`
}

func addonBackupsTool(clientFor ClientForContext) sdk.ToolHandlerFor[addonBackupsInput, addonBackupsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addonBackupsInput) (*sdk.CallToolResult, addonBackupsOutput, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, addonBackupsOutput{}, err
		}
		bs, err := c.Backups(ctx, in.Addon, in.App)
		if err != nil {
			return nil, addonBackupsOutput{}, err
		}
		out := addonBackupsOutput{Backups: make([]backupItem, 0, len(bs))}
		for _, b := range bs {
			out.Backups = append(out.Backups, toBackupItem(b))
		}
		return nil, out, nil
	}
}

func addonRemoveTool(clientFor ClientForContext) sdk.ToolHandlerFor[addonRemoveInput, addonRemoveOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addonRemoveInput) (*sdk.CallToolResult, addonRemoveOutput, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, addonRemoveOutput{}, err
		}
		if err := c.RemoveAddon(ctx, in.Name, in.Confirm); err != nil {
			return nil, addonRemoveOutput{}, err
		}
		return nil, addonRemoveOutput{Removed: in.Name}, nil
	}
}

type logsQueryInput struct {
	contextArg
	Query   string `json:"query,omitempty" jsonschema:"a VictoriaLogs LogsQL query; empty matches everything, newest first"`
	Limit   int    `json:"limit,omitempty" jsonschema:"maximum records to return (default 200)"`
	Backend string `json:"backend,omitempty" jsonschema:"optional: which backend to query when more than one serves this capability, e.g. loki or victorialogs"`
}

type logEntry struct {
	Time    string `json:"time,omitempty"`
	Message string `json:"message"`
	Pod     string `json:"pod,omitempty"`
}

type logsQueryOutput struct {
	Entries []logEntry `json:"entries"`
}

func logsQueryTool(clientFor ClientForContext) sdk.ToolHandlerFor[logsQueryInput, logsQueryOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in logsQueryInput) (*sdk.CallToolResult, logsQueryOutput, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, logsQueryOutput{}, err
		}
		es, err := c.QueryLogs(ctx, in.Query, in.Limit, in.Backend)
		if err != nil {
			return nil, logsQueryOutput{}, err
		}
		out := logsQueryOutput{Entries: make([]logEntry, 0, len(es))}
		for _, e := range es {
			out.Entries = append(out.Entries, logEntry{Time: e.Time, Message: e.Message, Pod: e.Pod})
		}
		return nil, out, nil
	}
}

type metricsQueryInput struct {
	contextArg
	Query   string `json:"query" jsonschema:"an instant PromQL query, e.g. up or rate(http_requests_total[5m])"`
	Backend string `json:"backend,omitempty" jsonschema:"optional: which backend to query when more than one serves this capability, e.g. prometheus or victoriametrics"`
}

type metricSample struct {
	Labels map[string]string `json:"labels,omitempty"`
	Value  string            `json:"value"`
	Time   string            `json:"time,omitempty"`
}

type metricsQueryOutput struct {
	Samples []metricSample `json:"samples"`
}

func metricsQueryTool(clientFor ClientForContext) sdk.ToolHandlerFor[metricsQueryInput, metricsQueryOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in metricsQueryInput) (*sdk.CallToolResult, metricsQueryOutput, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, metricsQueryOutput{}, err
		}
		ss, err := c.QueryMetrics(ctx, in.Query, in.Backend)
		if err != nil {
			return nil, metricsQueryOutput{}, err
		}
		out := metricsQueryOutput{Samples: make([]metricSample, 0, len(ss))}
		for _, sm := range ss {
			out.Samples = append(out.Samples, metricSample{Labels: sm.Labels, Value: sm.Value, Time: sm.Time})
		}
		return nil, out, nil
	}
}

func providersTool(clientFor ClientForContext) sdk.ToolHandlerFor[providersInput, providersOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in providersInput) (*sdk.CallToolResult, providersOutput, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, providersOutput{}, err
		}
		ps, err := c.Providers(ctx)
		if err != nil {
			return nil, providersOutput{}, err
		}
		out := providersOutput{Providers: make([]providerInfo, 0, len(ps))}
		for _, p := range ps {
			out.Providers = append(out.Providers, providerInfo{Name: p.Name, Type: p.Type, Capabilities: p.Capabilities})
		}
		return nil, out, nil
	}
}

// guardInput carries only the optional context: listing guardrails takes no other arguments.
type guardInput struct {
	contextArg
}

type guardOutput struct {
	Guardrails []client.Guardrail `json:"guardrails"`
}

func guardTool(clientFor ClientForContext) sdk.ToolHandlerFor[guardInput, guardOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in guardInput) (*sdk.CallToolResult, guardOutput, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, guardOutput{}, err
		}
		gs, err := c.Guardrails(ctx)
		if err != nil {
			return nil, guardOutput{}, err
		}
		return nil, guardOutput{Guardrails: gs}, nil
	}
}

// clusterInput carries only the optional context: reading cluster capabilities takes no other
// arguments.
type clusterInput struct {
	contextArg
}

func clusterTool(clientFor ClientForContext) sdk.ToolHandlerFor[clusterInput, client.ClusterCapabilities] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in clusterInput) (*sdk.CallToolResult, client.ClusterCapabilities, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, client.ClusterCapabilities{}, err
		}
		caps, err := c.Cluster(ctx)
		if err != nil {
			return nil, client.ClusterCapabilities{}, err
		}
		return nil, caps, nil
	}
}

// auditInput narrows an audit query. A zero value lists the latest rows across every app. The
// filters mirror the `burrow audit` CLI (ADR-0027).
type auditInput struct {
	contextArg
	App       string `json:"app,omitempty" jsonschema:"optional: filter to one app/host/add-on target"`
	Operation string `json:"operation,omitempty" jsonschema:"optional: filter to one operation, e.g. deploy, rollback, app_delete"`
	Outcome   string `json:"outcome,omitempty" jsonschema:"optional: filter to one outcome — allowed, held, denied, executed, or failed"`
	Limit     int    `json:"limit,omitempty" jsonschema:"optional: maximum rows to return (default 200), newest first"`
}

type auditOutput struct {
	Entries []client.AuditEntry `json:"entries"`
}

func auditTool(clientFor ClientForContext) sdk.ToolHandlerFor[auditInput, auditOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in auditInput) (*sdk.CallToolResult, auditOutput, error) {
		c, err := clientFor(in.Context)
		if err != nil {
			return nil, auditOutput{}, err
		}
		entries, err := c.Audit(ctx, client.AuditFilter{App: in.App, Operation: in.Operation, Outcome: in.Outcome, Limit: in.Limit})
		if err != nil {
			return nil, auditOutput{}, err
		}
		return nil, auditOutput{Entries: entries}, nil
	}
}

// environmentsInput has no fields: listing environments reads the local kubeconfig and takes no
// arguments, not even a context (it lists the contexts the agent can target).
type environmentsInput struct{}

// environmentInfo is the agent's view of one environment (a kubeconfig context).
type environmentInfo struct {
	Name    string `json:"name"`
	Cluster string `json:"cluster"`
	Current bool   `json:"current"`
}

type environmentsOutput struct {
	Environments []environmentInfo `json:"environments"`
}

func environmentsTool(environments EnvironmentLister) sdk.ToolHandlerFor[environmentsInput, environmentsOutput] {
	return func(_ context.Context, _ *sdk.CallToolRequest, _ environmentsInput) (*sdk.CallToolResult, environmentsOutput, error) {
		ctxs, err := environments()
		if err != nil {
			return nil, environmentsOutput{}, err
		}
		out := environmentsOutput{Environments: make([]environmentInfo, 0, len(ctxs))}
		for _, c := range ctxs {
			out.Environments = append(out.Environments, environmentInfo{Name: c.Name, Cluster: c.Cluster, Current: c.Current})
		}
		return nil, out, nil
	}
}
