// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package mcp

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/burrow-cloud/burrow/client"
)

// ClientForContext resolves a control-plane client for a kubeconfig context (the cluster whose
// burrowd a call targets; ADR-0035, ADR-0036). An empty context string means the current kubeconfig
// context. Each context's cluster runs its own burrowd with its own credentials and its own
// guardrail policy. The MCP server holds one factory instead of one client (ADR-0035, ADR-0005);
// burrow-mcp builds it to cache a client per context. The server resolves the agent's env argument
// (a local handle name) to a kube context through localconfig before calling this (ADR-0036 slice
// 5b).
type ClientForContext func(kubeContext string) (*client.Client, error)

// contextArg is embedded in every operating tool's input as a low-level, raw kube-context override.
// Its single field is promoted into the tool's generated input schema as an optional "context"
// property. An empty value means the current kubeconfig context. It is non-secret: a context name is
// a kubeconfig label, not a credential (ADR-0035, ADR-0005). On a per-app tool the env argument
// (a local environment handle) is the primary target and wins when both are set; context is the
// escape hatch for targeting a cluster that has no handle (ADR-0036 slice 5b).
type contextArg struct {
	Context string `json:"context,omitempty" jsonschema:"optional, low-level: the raw kubeconfig context (which cluster's burrowd) to target, e.g. prod-cluster; default is the current context. Prefer env, which names a local environment handle; env wins when both are set, and context is only for targeting a cluster that has no handle."`
}

// envArg is embedded in every per-app tool's input as the primary target: the LOCAL ENVIRONMENT
// HANDLE to operate (ADR-0036 slice 5b). Its single field is promoted into the tool's generated
// input schema as an optional "env" property. burrow-mcp resolves the handle through the local
// config to the cluster (kube context) it targets and the burrowd-registered environment NAME it
// sends; an empty value follows the current kube context with the default environment. An
// environment name is non-secret selector metadata, not a credential. Use burrow_environments to
// discover the handle names.
type envArg struct {
	Env string `json:"env,omitempty" jsonschema:"optional: the local environment handle to operate in (e.g. staging or prod); default follows the current kube context with the default environment. burrow-mcp resolves the handle to its cluster and the registered environment it sends. Use burrow_environments to see the handles you can name. Every mutating tool echoes the environment it acted in."`
}

// NewServer builds the Burrow MCP server: an agent-neutral surface (ADR-0003) exposing
// the control plane's operations as MCP tools. Each tool translates a call into a
// control-plane API call via the client and returns the structured result; a control
// plane error becomes a tool error the agent can read. The server holds no cluster
// credentials (ADR-0005). It targets one environment per call (ADR-0036): a per-app tool's env
// argument names a local environment handle, resolved through localconfig to the cluster (kube
// context) it targets and the burrowd-registered environment NAME it sends; the resolved client
// comes from clientFor, and every mutating tool echoes the environment it acted in.
// burrow_environments lists the local handles the agent can name (kubeconfig is the path used to
// mark the current one).
func NewServer(clientFor ClientForContext, kubeconfig, version string) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "burrow", Title: "Burrow", Version: version}, nil)
	sel := selector{kubeconfig: kubeconfig}

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_deploy",
		Description: "Deploy an application to the cluster by container image reference. To deploy an app from source: infer the app name from the working directory (the folder name) unless the user names one; build from committed code — if the working tree has uncommitted changes, commit them with a concise message and TELL the user you did so (never silently ship a dirty tree, and never block a non-technical user on it); build a container image from the working directory with your OWN container tooling (for example `docker build`) and push it to a registry the cluster can pull from, tagging it with an incrementing semantic version (start at `v0.1.0` for a first release, then bump the patch each subsequent deploy) — NEVER reuse a tag, so every deploy is a distinct, pullable artifact; determine the current version from the app's last deployed release (the rollback handle this tool returns) or from the registry's existing version tags. Then call this tool with that image reference. Match the depth of your explanation to how technical the user appears: concise for developers, more hand-holding for non-developers. Do NOT run the `burrow` CLI to build or deploy — you operate through these tools, not the CLI. If the image is already built and pushed, just pass its reference. Only the reference and small metadata are sent, never code (the image moves through the registry). Config is NOT passed here: an app's config is a separate, app-global store sourced at deploy time, so set any config vars the release needs with burrow_config_set BEFORE deploying — the new release then boots with it on first start. (burrow_config_set with no_restart=true followed by burrow_deploy is a single restart.) For SECRETS (DB URLs, API keys), do not put values in config and do not paste secret values into this conversation: ask the user to run `burrow app secret set <app> KEY=VALUE` themselves BEFORE deploying, then confirm the key with burrow_secret_list. If the image lives in a PRIVATE registry, the cluster needs pull credentials to fetch it — a one-time credential step that never goes over MCP (there is deliberately no tool for it), so ask the user to run `burrow config registry login <host> -u <username>` themselves at their OWN terminal BEFORE deploying (e.g. `burrow config registry login ghcr.io -u me`); the command then PROMPTS for the token with the input hidden and links them to the right page to create it (for GitHub, a dedicated long-lived `read:packages` PAT), so the token is never typed into this conversation or put on the command line where it would leak into shell history. Do NOT ask the user to paste the token here or pass it with `-p`. Warn them NOT to use an ephemeral or CI token (such as an Actions `GITHUB_TOKEN`) — the credential is stored as-is and is never refreshed, so a short-lived one silently breaks future pulls. Do not try to make the image public or wire a credential yourself. Deploying no longer resets the replica count: omit replicas to preserve the current scale (a new app defaults to 1), and replicas is ignored while autoscaling is enabled — scaling is a separate concern handled by burrow_scale or burrow_autoscale. Returns the new release and the release it superseded (the rollback handle), plus the environment it acted in. Pass env to target a local environment handle, such as staging or prod (default follows the current kube context with the default environment); this is how you deploy the same app to staging versus prod. Use burrow_environments to see the handles you can name; context is a low-level override for a cluster with no handle. Deploying is allowed by default, but an operator may configure the app.deploy guardrail to hold it for confirmation (e.g. confirm in prod) or deny it; when held, the error says so — ask the user, then retry with confirm set to true ONLY after the user approves, never on your own.",
	}, deployTool(clientFor, sel))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_config_set",
		Description: "Set (upsert) a non-secret config var for an app (configuration set as an environment variable). The config store is the single source of truth, sourced into the workload at deploy time. By default the running app is rolled so it picks the change up; set no_restart=true to only persist it and let it land on the next deploy (so setting config then deploying is a single restart). For secrets, do not use config: config vars are non-secret.",
	}, configSetTool(clientFor, sel))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_config_list",
		Description: "List an app's non-secret config vars (the config store). Read-only.",
	}, configListTool(clientFor, sel))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_config_unset",
		Description: "Remove a non-secret config var from an app. By default the running app is rolled so it drops the value; set no_restart=true to only persist the removal and let it land on the next deploy.",
	}, configUnsetTool(clientFor, sel))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_secret_list",
		Description: "List the KEYS of an app's secret environment variables — never the values (secret values never travel over MCP; ADR-0029). Read-only. Use this to confirm a secret the app needs is present before deploying. To SET a secret value, there is no tool: NEVER ask the user to paste a secret value into this conversation (anything in the prompt is retained in context and re-sent on later tool calls). Instead, ask the user to run `burrow app secret set <app> KEY=VALUE` themselves at their terminal, then confirm with this list tool.",
	}, secretListTool(clientFor, sel))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_secret_unset",
		Description: "Remove a secret environment variable from an app by KEY (no value crosses MCP). By default the running app is rolled so it drops the value; set no_restart=true to only persist the removal and let it land on the next deploy. To SET a secret, ask the user to run `burrow app secret set <app> KEY=VALUE` themselves — never have them paste a secret value into this conversation.",
	}, secretUnsetTool(clientFor, sel))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_apps",
		Description: "List the applications Burrow manages and each one's running state (image, ready/desired replicas, availability), so you can discover what is deployed before operating on it. Read-only. Pass env to survey a local environment handle such as staging or prod (default follows the current kube context with the default environment); this is how you compare prod versus staging. Use burrow_environments to see the handles you can name.",
	}, appsTool(clientFor, sel))

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
	}, appDeleteTool(clientFor, sel))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_status",
		Description: "Report an application's status: its most recent release and the live workload state (desired/ready replicas, availability). Pass env to read a local environment handle such as staging or prod (default follows the current kube context with the default environment); this is how you check prod versus staging. Use burrow_environments to see the handles you can name.",
	}, statusTool(clientFor, sel))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_logs",
		Description: "Return recent log lines for an application's workload.",
	}, logsTool(clientFor, sel))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_rollback",
		Description: "Roll an application back to its previously running release by redeploying that release's image reference. Returns the new release and which release it restored. Allowed by default (rollback is a recovery action), but an operator may configure a guardrail to hold it for confirmation; when held, the error says so — ask the user, then retry with confirm set to true.",
	}, rollbackTool(clientFor, sel))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_scale",
		Description: "Change an application's replica count. A guardrail may refuse it (e.g. above the replica ceiling) or hold it for confirmation (e.g. scaling to zero); when held, the error says so — ask the user, then retry with confirm set to true.",
	}, scaleTool(clientFor, sel))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_autoscale",
		Description: "Configure autoscaling for an application by applying a HorizontalPodAutoscaler on its Deployment, so it scales its replica count between a min and a max to hold a target CPU (and optionally memory) utilization. Use it for requests like \"make web autoscale\" (sane defaults) or \"make web autoscale at 90% CPU\". The max is bounded by the replica-ceiling guardrail: a max above the ceiling is denied exactly like scaling above it. Autoscaling needs metrics-server installed in the cluster; the HPA is created regardless, and the result warns (metrics_available false, a warning message) when metrics-server was not detected, meaning the autoscaler is set but will not scale until it is installed. Set off to true to remove autoscaling (idempotent). A guardrail may hold this for confirmation or deny it (e.g. an operator may deny autoscaling in prod); when held, the error says so, ask the user, then retry with confirm set to true. Echoes the environment it acted in.",
	}, autoscaleTool(clientFor, sel))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_expose",
		Description: "Make a deployed application reachable from outside the cluster at a hostname, by creating a Service and an Ingress. Public exposure is held for confirmation by a guardrail by default; when held, the error says so — ask the user, then retry with confirm set to true. Reachability also needs an ingress controller and DNS pointing the host at the cluster. If the cluster is not set up for public reachability (no ingress controller, or no cert-manager when tls is set), the error names each missing prerequisite and the exact burrow command that provisions it; run that command rather than inspecting the cluster directly.",
	}, exposeTool(clientFor, sel))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_unexpose",
		Description: "Remove an application's exposure (its Service and Ingress). Does not affect the running workload.",
	}, unexposeTool(clientFor, sel))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_reachability",
		Description: "Report whether an application is reachable at its hostname, link by link: deployed and ready, exposed, given an external address by an ingress controller, a TLS certificate when one was requested, and DNS pointing the host at that address. Returns a plain one-line summary plus the full chain, so you can tell the user exactly which link is missing and what to do. After deploying, exposing (burrow_expose), and pointing DNS at the cluster, call this with wait set to true to poll until the app is live and get its URL; when the app converges, reachable is true and url is the live address, and if it returns blocked_on that names the one link to fix. Read-only.",
	}, reachabilityTool(clientFor, sel))

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
		Description: "List your local environment handles: each names a cluster (kube context) and the registered environment to operate, so you know what to pass as the env argument on a per-app tool (ADR-0036). Each entry has the handle name, the kube context it targets, the app namespace, the registered environment it sends (empty for the cluster default), and whether it is the current selection. Read-only: it reads the local handle config and contacts no cluster.",
	}, environmentsTool(sel))

	return s
}

// Serve runs the Burrow MCP server over stdio until the client disconnects. It targets one
// environment per call through clientFor, resolving a per-app tool's env handle through the local
// config (ADR-0036). kubeconfig is the path used to mark the current handle in burrow_environments.
func Serve(ctx context.Context, clientFor ClientForContext, kubeconfig, version string) error {
	return NewServer(clientFor, kubeconfig, version).Run(ctx, &sdk.StdioTransport{})
}

type deployInput struct {
	contextArg
	envArg
	App         string   `json:"app" jsonschema:"the application name (a DNS-1123 label)"`
	Image       string   `json:"image" jsonschema:"the reference of an image you have built and pushed to a registry the cluster can pull from, e.g. registry.example.com/app:1.2.3"`
	Command     []string `json:"command,omitempty" jsonschema:"optional command override for the container"`
	MetricsPort int32    `json:"metrics_port,omitempty" jsonschema:"optional: annotate the pod so the metrics add-on scrapes /metrics on this port"`
	Replicas    int32    `json:"replicas,omitempty" jsonschema:"desired replicas; OPTIONAL — omit to keep the current count (new apps default to 1); ignored while autoscaling is enabled. To change scale deliberately use burrow_scale or burrow_autoscale."`
	Confirm     bool     `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed an operation a guardrail held for confirmation; do not self-confirm"`
}

type appInput struct {
	contextArg
	envArg
	App string `json:"app" jsonschema:"the application name"`
}

type logsInput struct {
	contextArg
	envArg
	App  string `json:"app" jsonschema:"the application name"`
	Tail int    `json:"tail,omitempty" jsonschema:"maximum number of recent log lines to return"`
}

type scaleInput struct {
	contextArg
	envArg
	App      string `json:"app" jsonschema:"the application name"`
	Replicas int32  `json:"replicas" jsonschema:"desired number of replicas"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed an operation a guardrail held for confirmation (e.g. scaling to zero); do not self-confirm"`
}

// deployOutput is the deploy result plus the environment the deploy acted in (ADR-0036): the
// client.DeployResult fields are promoted alongside an "environment" echo.
type deployOutput struct {
	client.DeployResult
	targeted
}

func deployTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[deployInput, deployOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in deployInput) (*sdk.CallToolResult, deployOutput, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, deployOutput{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, deployOutput{}, err
		}
		res, err := c.Deploy(ctx, in.App, client.DeployRequest{Env: tgt.env, Image: in.Image, Command: in.Command, MetricsPort: in.MetricsPort, Replicas: in.Replicas, Confirm: in.Confirm})
		if err != nil {
			return nil, deployOutput{}, err
		}
		return nil, deployOutput{DeployResult: res, targeted: tgt.echo()}, nil
	}
}

type configSetInput struct {
	contextArg
	envArg
	App       string `json:"app" jsonschema:"the application name"`
	Key       string `json:"key" jsonschema:"the config var name (e.g. LOG_LEVEL)"`
	Value     string `json:"value" jsonschema:"the value to set"`
	NoRestart bool   `json:"no_restart,omitempty" jsonschema:"true to persist without rolling the running app; the change lands on the next deploy"`
}

type configUnsetInput struct {
	contextArg
	envArg
	App       string `json:"app" jsonschema:"the application name"`
	Key       string `json:"key" jsonschema:"the config var name to remove"`
	NoRestart bool   `json:"no_restart,omitempty" jsonschema:"true to persist the removal without rolling the running app; the change lands on the next deploy"`
}

// keyAck is a small structured ack for a config or secret key mutation, with the environment it
// acted in echoed back (ADR-0036).
type keyAck struct {
	App string `json:"app"`
	Key string `json:"key"`
	targeted
}

type configOutput struct {
	Config map[string]string `json:"config"`
}

func configSetTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[configSetInput, keyAck] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in configSetInput) (*sdk.CallToolResult, keyAck, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, keyAck{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, keyAck{}, err
		}
		if err := c.SetConfig(ctx, in.App, tgt.env, in.Key, in.Value, in.NoRestart); err != nil {
			return nil, keyAck{}, err
		}
		return nil, keyAck{App: in.App, Key: in.Key, targeted: tgt.echo()}, nil
	}
}

func configUnsetTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[configUnsetInput, keyAck] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in configUnsetInput) (*sdk.CallToolResult, keyAck, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, keyAck{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, keyAck{}, err
		}
		if err := c.UnsetConfig(ctx, in.App, tgt.env, in.Key, in.NoRestart); err != nil {
			return nil, keyAck{}, err
		}
		return nil, keyAck{App: in.App, Key: in.Key, targeted: tgt.echo()}, nil
	}
}

func configListTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[appInput, configOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, configOutput, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, configOutput{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, configOutput{}, err
		}
		cfg, err := c.Config(ctx, in.App, tgt.env)
		if err != nil {
			return nil, configOutput{}, err
		}
		return nil, configOutput{Config: cfg}, nil
	}
}

type secretUnsetInput struct {
	contextArg
	envArg
	App       string `json:"app" jsonschema:"the application name"`
	Key       string `json:"key" jsonschema:"the secret environment variable name to remove (the KEY, not a value)"`
	NoRestart bool   `json:"no_restart,omitempty" jsonschema:"true to persist the removal without rolling the running app; the change lands on the next deploy"`
}

// secretsOutput is an app's secret KEYS only — never the values (ADR-0028/0004).
type secretsOutput struct {
	Keys []string `json:"keys"`
}

func secretListTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[appInput, secretsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, secretsOutput, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, secretsOutput{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, secretsOutput{}, err
		}
		keys, err := c.Secrets(ctx, in.App, tgt.env)
		if err != nil {
			return nil, secretsOutput{}, err
		}
		return nil, secretsOutput{Keys: keys}, nil
	}
}

func secretUnsetTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[secretUnsetInput, keyAck] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in secretUnsetInput) (*sdk.CallToolResult, keyAck, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, keyAck{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, keyAck{}, err
		}
		if err := c.UnsetSecret(ctx, in.App, tgt.env, in.Key, in.NoRestart); err != nil {
			return nil, keyAck{}, err
		}
		return nil, keyAck{App: in.App, Key: in.Key, targeted: tgt.echo()}, nil
	}
}

func statusTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[appInput, client.StatusResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, client.StatusResult, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, client.StatusResult{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, client.StatusResult{}, err
		}
		res, err := c.Status(ctx, in.App, tgt.env)
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

func logsTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[logsInput, logsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in logsInput) (*sdk.CallToolResult, logsOutput, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, logsOutput{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, logsOutput{}, err
		}
		lines, err := c.Logs(ctx, in.App, tgt.env, in.Tail)
		if err != nil {
			return nil, logsOutput{}, err
		}
		return nil, logsOutput{Lines: lines}, nil
	}
}

type rollbackInput struct {
	contextArg
	envArg
	App     string `json:"app" jsonschema:"the application name"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed a rollback that an operator's guardrail held for confirmation; do not self-confirm"`
}

// rollbackOutput is the rollback result plus the environment it acted in (ADR-0036).
type rollbackOutput struct {
	client.RollbackResult
	targeted
}

func rollbackTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[rollbackInput, rollbackOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in rollbackInput) (*sdk.CallToolResult, rollbackOutput, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, rollbackOutput{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, rollbackOutput{}, err
		}
		res, err := c.Rollback(ctx, in.App, tgt.env, in.Confirm)
		if err != nil {
			return nil, rollbackOutput{}, err
		}
		return nil, rollbackOutput{RollbackResult: res, targeted: tgt.echo()}, nil
	}
}

// scaleOutput is the scale result plus the environment it acted in (ADR-0036).
type scaleOutput struct {
	client.ScaleResult
	targeted
}

func scaleTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[scaleInput, scaleOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in scaleInput) (*sdk.CallToolResult, scaleOutput, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, scaleOutput{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, scaleOutput{}, err
		}
		res, err := c.Scale(ctx, in.App, tgt.env, in.Replicas, in.Confirm)
		if err != nil {
			return nil, scaleOutput{}, err
		}
		return nil, scaleOutput{ScaleResult: res, targeted: tgt.echo()}, nil
	}
}

type autoscaleInput struct {
	contextArg
	envArg
	App     string `json:"app" jsonschema:"the application name"`
	Off     bool   `json:"off,omitempty" jsonschema:"set true to REMOVE autoscaling from the app (idempotent); the min/max/cpu/memory fields are ignored"`
	Min     int32  `json:"min,omitempty" jsonschema:"minimum replicas the autoscaler will not go below (default 1); at least 1"`
	Max     int32  `json:"max,omitempty" jsonschema:"maximum replicas the autoscaler will not exceed (default 10); bounded by the replica-ceiling guardrail"`
	CPU     int32  `json:"cpu,omitempty" jsonschema:"target average CPU utilization percent, 1..100 (default 80)"`
	Memory  int32  `json:"memory,omitempty" jsonschema:"optional target average memory utilization percent, 1..100; 0 leaves it unset"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed an autoscale a guardrail held for confirmation; do not self-confirm"`
}

// autoscaleDefaults fills the sane defaults for an autoscale-on call so an agent can say "make web
// autoscale" with no numbers: min 1, max 10, cpu 80. They mirror the CLI's flag defaults. A supplied
// value (non-zero) wins; the off path ignores them.
func (in autoscaleInput) withDefaults() autoscaleInput {
	if in.Min == 0 {
		in.Min = 1
	}
	if in.Max == 0 {
		in.Max = 10
	}
	if in.CPU == 0 {
		in.CPU = 80
	}
	return in
}

// autoscaleOutput is the autoscale result plus the environment it acted in (ADR-0036). On the off
// path only App and the environment echo are set.
type autoscaleOutput struct {
	client.AutoscaleResult
	targeted
}

func autoscaleTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[autoscaleInput, autoscaleOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in autoscaleInput) (*sdk.CallToolResult, autoscaleOutput, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, autoscaleOutput{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, autoscaleOutput{}, err
		}
		if in.Off {
			if err := c.DisableAutoscale(ctx, in.App, tgt.env, in.Confirm); err != nil {
				return nil, autoscaleOutput{}, err
			}
			return nil, autoscaleOutput{AutoscaleResult: client.AutoscaleResult{App: in.App}, targeted: tgt.echo()}, nil
		}
		in = in.withDefaults()
		res, err := c.Autoscale(ctx, in.App, client.AutoscaleRequest{
			Env: tgt.env, Min: in.Min, Max: in.Max, CPU: in.CPU, Memory: in.Memory, Confirm: in.Confirm,
		})
		if err != nil {
			return nil, autoscaleOutput{}, err
		}
		return nil, autoscaleOutput{AutoscaleResult: res, targeted: tgt.echo()}, nil
	}
}

type exposeInput struct {
	contextArg
	envArg
	App     string `json:"app" jsonschema:"the application name"`
	Host    string `json:"host" jsonschema:"the external hostname to route to the app, e.g. app.example.com"`
	Port    int32  `json:"port" jsonschema:"the app's container port to forward to"`
	TLS     bool   `json:"tls,omitempty" jsonschema:"request an HTTPS certificate for the host via cert-manager"`
	Issuer  string `json:"issuer,omitempty" jsonschema:"the cert-manager ClusterIssuer to use when tls is set (e.g. letsencrypt)"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed exposing the app to the public internet; do not self-confirm"`
}

// exposeOutput is the expose result plus the environment it acted in (ADR-0036).
type exposeOutput struct {
	client.ExposeResult
	targeted
}

func exposeTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[exposeInput, exposeOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in exposeInput) (*sdk.CallToolResult, exposeOutput, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, exposeOutput{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, exposeOutput{}, err
		}
		res, err := c.Expose(ctx, in.App, tgt.env, in.Host, in.Port, in.TLS, in.Issuer, in.Confirm)
		if err != nil {
			return nil, exposeOutput{}, err
		}
		return nil, exposeOutput{ExposeResult: res, targeted: tgt.echo()}, nil
	}
}

// unexposeOutput is a small structured ack for the unexpose tool, with the environment it acted in
// echoed back (ADR-0036).
type unexposeOutput struct {
	App string `json:"app"`
	targeted
}

func unexposeTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[appInput, unexposeOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, unexposeOutput, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, unexposeOutput{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, unexposeOutput{}, err
		}
		if err := c.Unexpose(ctx, in.App, tgt.env); err != nil {
			return nil, unexposeOutput{}, err
		}
		return nil, unexposeOutput{App: in.App, targeted: tgt.echo()}, nil
	}
}

type reachabilityInput struct {
	contextArg
	envArg
	App  string `json:"app" jsonschema:"the application name"`
	Wait bool   `json:"wait,omitempty" jsonschema:"poll until the app is live (reachable) or a timeout, instead of a single point-in-time check; use after deploying, exposing, and pointing DNS to confirm the app is live and get its URL"`
}

// reachabilityWaitTimeout bounds how long the burrow_reachability tool polls in wait mode before
// returning the last verdict. The control-plane engine stays point-in-time; this wait lives in
// the thin client layer (ADR-0034 slice 3).
const reachabilityWaitTimeout = 3 * time.Minute

func reachabilityTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[reachabilityInput, client.ReachabilityResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in reachabilityInput) (*sdk.CallToolResult, client.ReachabilityResult, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, client.ReachabilityResult{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, client.ReachabilityResult{}, err
		}
		reach := func(ctx context.Context, app string) (client.ReachabilityResult, error) {
			return c.Reachability(ctx, app, tgt.env)
		}
		if in.Wait {
			reach = func(ctx context.Context, app string) (client.ReachabilityResult, error) {
				return c.WaitReachable(ctx, app, tgt.env, reachabilityWaitTimeout, nil)
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

// appsInput carries the optional context and environment: listing apps takes no other arguments.
type appsInput struct {
	contextArg
	envArg
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

func appsTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[appsInput, appsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appsInput) (*sdk.CallToolResult, appsOutput, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, appsOutput{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, appsOutput{}, err
		}
		apps, err := c.Apps(ctx, tgt.env)
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
	envArg
	App     string `json:"app" jsonschema:"the application name to delete (a DNS-1123 label)"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed this destructive delete; do not self-confirm"`
}

type appDeleteOutput struct {
	Deleted string `json:"deleted"`
	targeted
}

func appDeleteTool(clientFor ClientForContext, sel selector) sdk.ToolHandlerFor[appDeleteInput, appDeleteOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appDeleteInput) (*sdk.CallToolResult, appDeleteOutput, error) {
		tgt, err := sel.resolve(in.Env, in.Context)
		if err != nil {
			return nil, appDeleteOutput{}, err
		}
		c, err := clientFor(tgt.context)
		if err != nil {
			return nil, appDeleteOutput{}, err
		}
		if err := c.DeleteApp(ctx, in.App, tgt.env, in.Confirm); err != nil {
			return nil, appDeleteOutput{}, err
		}
		return nil, appDeleteOutput{Deleted: in.App, targeted: tgt.echo()}, nil
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
		gs, err := c.Guardrails(ctx, "")
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

// environmentsInput has no fields: burrow_environments lists the local environment handles and
// takes no arguments, not even a context (it lists what the agent can name as env; ADR-0036).
type environmentsInput struct{}

type environmentsOutput struct {
	Environments []environmentInfo `json:"environments"`
}

func environmentsTool(sel selector) sdk.ToolHandlerFor[environmentsInput, environmentsOutput] {
	return func(_ context.Context, _ *sdk.CallToolRequest, _ environmentsInput) (*sdk.CallToolResult, environmentsOutput, error) {
		envs, err := sel.list()
		if err != nil {
			return nil, environmentsOutput{}, err
		}
		return nil, environmentsOutput{Environments: envs}, nil
	}
}
