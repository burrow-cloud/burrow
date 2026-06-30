// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package mcp

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/localconfig"
)

// Resolved is the concrete environment a tool call targets (ADR-0036 slice 5): which kube
// context (which cluster's burrowd), which control-plane namespace burrowd runs in, and which
// app namespace the operation acts in. Name is the handle the agent named, empty when it
// targeted a raw kube context with no handle. The mcp package itself does no file I/O; it
// receives this from an injected EnvResolver so the handlers stay pure (ADR-0010).
type Resolved struct {
	Name                  string
	Context               string
	ControlPlaneNamespace string
	AppNamespace          string
}

// EnvResolver maps a tool's env handle name and raw context override to the concrete target a
// call routes to (ADR-0036). env is the normal path: a handle in the local Burrow config that
// resolves to a kube context and an app namespace. context is a low-level escape hatch that
// names a kube context directly, without a handle. When both are set env wins: a named handle
// fully determines the target and the raw context override is ignored. An empty env and empty
// context default to the current environment (the kube context kubectl points at), never the
// human's pin, so the agent does not silently ride a human's sticky selection.
type EnvResolver func(env, context string) (Resolved, error)

// ClientForEnv resolves a control-plane client for a resolved target's cluster (its kube
// context) and control-plane namespace (which burrowd). The MCP server holds one factory
// instead of one client (ADR-0005, ADR-0036); burrow-mcp builds it to cache a client per
// target.
type ClientForEnv func(r Resolved) (*client.Client, error)

// EnvHandle is one entry in the local Burrow config the agent can target: a name resolving to
// a kube context and an app namespace, with Current marking the one the human's selector points
// at (ADR-0036). It backs burrow_environments, the agent's discovery tool.
type EnvHandle struct {
	Name      string
	Context   string
	Namespace string
	Current   bool
}

// EnvLister lists the local Burrow config handles the agent can target (ADR-0036). It reads the
// client-side config and contacts no cluster, so burrow_environments is a pure, local listing.
type EnvLister func() ([]EnvHandle, error)

// LocalConfigResolver builds the EnvResolver that resolves a tool's env handle through the local
// Burrow config (~/.burrow/config or $BURROW_CONFIG), the same selector state the `burrow env`
// CLI reads (ADR-0036). kubeconfigPath is honored for the default (follow the current kube
// context) path; the handle path needs no kubeconfig. burrow-mcp runs locally (the agent
// launches it), so it can read the config the human edits.
func LocalConfigResolver(kubeconfigPath string) EnvResolver {
	return func(env, contextOverride string) (Resolved, error) {
		cfg, err := localconfig.Load()
		if err != nil {
			return Resolved{}, err
		}
		switch {
		case env != "":
			// A named handle is the normal path and fully determines the target. Resolve it as
			// if pinned so the control-plane-namespace default and the not-registered error are
			// shared with the CLI; this path reads no kubeconfig. A raw context override is
			// ignored here: env wins.
			sel := *cfg
			sel.Current = env
			r, err := localconfig.Resolve(&sel, kubeconfigPath)
			if err != nil {
				return Resolved{}, err
			}
			return fromLocalconfig(r), nil
		case contextOverride != "":
			// Raw escape hatch: target a kube context directly, with no handle, so burrowd's
			// default app namespace and the default control-plane namespace apply.
			return Resolved{Context: contextOverride, ControlPlaneNamespace: localconfig.DefaultControlPlaneNamespace}, nil
		default:
			// Default: follow the current kube context, not the human's pin. The agent never
			// rides a human's sticky selection (ADR-0036), so the pin is cleared before
			// resolving; the result is still echoed so a defaulted target stays legible.
			sel := *cfg
			sel.Current = ""
			r, err := localconfig.Resolve(&sel, kubeconfigPath)
			if err != nil {
				return Resolved{}, err
			}
			return fromLocalconfig(r), nil
		}
	}
}

// LocalConfigEnvLister builds the EnvLister that lists the local Burrow config handles, marking
// the one the human's selector currently resolves to (ADR-0036). kubeconfigPath is honored when
// the selector is following the current kube context; a config with no current context still
// lists its handles (the current marker is simply absent).
func LocalConfigEnvLister(kubeconfigPath string) EnvLister {
	return func() ([]EnvHandle, error) {
		cfg, err := localconfig.Load()
		if err != nil {
			return nil, err
		}
		current := ""
		if r, err := localconfig.Resolve(cfg, kubeconfigPath); err == nil {
			current = r.Name
		}
		out := make([]EnvHandle, 0, len(cfg.Environments))
		for _, e := range cfg.Environments {
			out = append(out, EnvHandle{
				Name:      e.Name,
				Context:   e.Context,
				Namespace: e.AppNamespace,
				Current:   current != "" && e.Name == current,
			})
		}
		return out, nil
	}
}

func fromLocalconfig(r localconfig.Resolved) Resolved {
	return Resolved{
		Name:                  r.Name,
		Context:               r.Context,
		ControlPlaneNamespace: r.ControlPlaneNamespace,
		AppNamespace:          r.Namespace,
	}
}

// deps bundles the seams every tool handler shares: the env resolver, the per-target client
// factory, and the local-config env lister. It keeps each tool's constructor to a single
// argument.
type deps struct {
	resolve   EnvResolver
	clientFor ClientForEnv
	envs      EnvLister
}

// target resolves a tool's env/context arguments to a concrete environment and returns the
// control-plane client for it together with the resolved target (so the handler can route the
// call to the resolved app namespace and echo the environment in its result).
func (d deps) target(env, kubeContext string) (*client.Client, Resolved, error) {
	r, err := d.resolve(env, kubeContext)
	if err != nil {
		return nil, Resolved{}, err
	}
	c, err := d.clientFor(r)
	if err != nil {
		return nil, Resolved{}, err
	}
	return c, r, nil
}

// envArg is embedded first in every operating tool's input so the target environment is the
// prominent, leading argument (ADR-0036). Its single field is promoted into the tool's generated
// input schema as an optional "env" property: a handle name from the local Burrow config that
// resolves to a kube context and an app namespace. An environment name is non-secret selector
// metadata, not a credential. Use burrow_environments to discover the names.
type envArg struct {
	Env string `json:"env,omitempty" jsonschema:"optional: the target environment, by name from your Burrow config (e.g. nonprod or prod). It resolves locally to that environment's cluster and app namespace, so naming it points the operation at the right place. Omit to use the current environment. Use burrow_environments to list the names."`
}

// contextArg is embedded after envArg in every operating tool's input as the low-level escape
// hatch: it names a kube context directly, without a Burrow config handle. Its single field is
// promoted into the tool's generated input schema as an optional "context" property. A context
// name is a kubeconfig label, not a credential (ADR-0005, ADR-0036).
type contextArg struct {
	Context string `json:"context,omitempty" jsonschema:"optional: a low-level override that targets a kube context directly, without a Burrow config handle (e.g. do-nyc1-prod). The normal way to choose an environment is the env argument; when env is also set it wins."`
}

// envRef names the environment a tool acted in, echoed in a mutating tool's result so a
// defaulted target is legible to the agent and recorded in the audit trail (ADR-0036). Name is
// the handle, empty when a raw context was targeted; Context and Namespace are the resolved kube
// context and app namespace.
type envRef struct {
	Name      string `json:"name,omitempty"`
	Context   string `json:"context,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

func toEnvRef(r Resolved) envRef {
	return envRef{Name: r.Name, Context: r.Context, Namespace: r.AppNamespace}
}

// NewServer builds the Burrow MCP server: an agent-neutral surface (ADR-0003) exposing
// the control plane's operations as MCP tools. Each tool translates a call into a
// control-plane API call via the client and returns the structured result; a control
// plane error becomes a tool error the agent can read. The server holds no cluster
// credentials (ADR-0005). It targets one environment per call (ADR-0036): every operating tool
// takes an optional env (a handle in the local Burrow config) resolved through resolve to a
// cluster and app namespace, with context as a low-level raw override; burrow_environments lists
// the local handles the agent can name, and every mutating tool echoes the environment it acted
// in.
func NewServer(resolve EnvResolver, clientFor ClientForEnv, envs EnvLister, version string) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "burrow", Title: "Burrow", Version: version}, nil)
	d := deps{resolve: resolve, clientFor: clientFor, envs: envs}

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_deploy",
		Description: "Deploy an application to the cluster by container image reference. The image must already be pushed to a registry the cluster can pull from; only the reference and small metadata are sent, never code. Config is NOT passed here: an app's config is a separate, app-global store sourced at deploy time, so set any config vars the release needs with burrow_config_set BEFORE deploying — the new release then boots with it on first start. (burrow_config_set with no_restart=true followed by burrow_deploy is a single restart.) For SECRETS (DB URLs, API keys), do not put values in config and do not paste secret values into this conversation: ask the user to run `burrow app secret set <app> KEY=VALUE` themselves BEFORE deploying, then confirm the key with burrow_secret_list. Returns the new release and the release it superseded (the rollback handle). Pass env to target an environment by name from your Burrow config (such as staging or prod); it resolves locally to that environment's cluster and app namespace, so this is how you deploy the same app to staging versus prod. Omit env to use the current environment; pass context only as a low-level override to target a kube context directly. Use burrow_environments to list the environments you can name. The result echoes the environment it acted in.",
	}, deployTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_config_set",
		Description: "Set (upsert) a non-secret config var for an app (configuration set as an environment variable). The config store is the single source of truth, sourced into the workload at deploy time. By default the running app is rolled so it picks the change up; set no_restart=true to only persist it and let it land on the next deploy (so setting config then deploying is a single restart). For secrets, do not use config: config vars are non-secret.",
	}, configSetTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_config_list",
		Description: "List an app's non-secret config vars (the config store). Read-only.",
	}, configListTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_config_unset",
		Description: "Remove a non-secret config var from an app. By default the running app is rolled so it drops the value; set no_restart=true to only persist the removal and let it land on the next deploy.",
	}, configUnsetTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_secret_list",
		Description: "List the KEYS of an app's secret environment variables — never the values (secret values never travel over MCP; ADR-0029). Read-only. Use this to confirm a secret the app needs is present before deploying. To SET a secret value, there is no tool: NEVER ask the user to paste a secret value into this conversation (anything in the prompt is retained in context and re-sent on later tool calls). Instead, ask the user to run `burrow app secret set <app> KEY=VALUE` themselves at their terminal, then confirm with this list tool.",
	}, secretListTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_secret_unset",
		Description: "Remove a secret environment variable from an app by KEY (no value crosses MCP). By default the running app is rolled so it drops the value; set no_restart=true to only persist the removal and let it land on the next deploy. To SET a secret, ask the user to run `burrow app secret set <app> KEY=VALUE` themselves — never have them paste a secret value into this conversation.",
	}, secretUnsetTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_apps",
		Description: "List the applications Burrow manages and each one's running state (image, ready/desired replicas, availability), so you can discover what is deployed before operating on it. Read-only. Pass env to survey an environment by name from your Burrow config (such as staging or prod); it resolves locally to that environment's cluster and app namespace, so this is how you compare prod versus staging. Omit env for the current environment; pass context only to target a kube context directly.",
	}, appsTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_addons",
		Description: "List the backing-service add-ons installed on the cluster (logs, …), their mode (installed/connected), in-cluster endpoint, and the capabilities you can query. Read-only.",
	}, addonsTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_addon_install",
		Description: "Install a vetted, self-hostable backing service for a capability (e.g. logs → VictoriaLogs) and register it as queryable, in one step. Held for confirmation by a guardrail; set confirm=true ONLY after the user approves, never on your own.",
	}, addonInstallTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_addon_remove",
		Description: "Remove an installed add-on by name. Held for confirmation by a guardrail (removing a backing service can break dependent apps); set confirm=true ONLY after the user approves.",
	}, addonRemoveTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_addon_attach",
		Description: "Give an application its own database on the installed Postgres add-on and wire it in. You supply only the add-on type (\"postgres\") and the app name — NO secret. Burrow generates the database, role, and connection string server-side and writes it into the app's Secret as DATABASE_URL; the value is never returned to you or shown in this conversation. After attaching, write integration code that reads the DATABASE_URL environment variable. Re-attaching rotates the password. Returns only the app, the add-on, and the key name (DATABASE_URL) — never a connection string. Install the postgres add-on first with burrow_addon_install if it is not yet installed.",
	}, addonAttachTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_addon_backup",
		Description: "Back up an application's database on the installed Postgres add-on. You supply only the add-on type (\"postgres\") and the app name — NO secret. Burrow runs an in-cluster Job that dumps the database to a backup volume and records the backup; the database superuser password never crosses this tool or appears in the result. Returns the recorded backup (its id, the app, the on-volume path, the size, and the status) — never a connection string. Backup destroys nothing, so it is allowed. To RESTORE a backup (which overwrites live data) there is no tool: ask the user to run `burrow addon restore postgres <app> --backup <id>` themselves.",
	}, addonBackupTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_addon_backups",
		Description: "List the recorded database backups for the Postgres add-on (id, app, time, size, status), so you can see what restore points exist. Pass the add-on type (\"postgres\") and optionally an app to restrict the listing; omit the app to list every app's backups. Read-only — restoring is CLI-only.",
	}, addonBackupsTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_logs_query",
		Description: "Query the cluster's aggregated logs (the installed logs add-on) with a VictoriaLogs LogsQL query to investigate why an app is failing or slow — e.g. `error`, `level:error`, `panic AND web`. Returns recent matching records (most recent first). Needs a logs add-on installed first (burrow_addon_install with capability \"logs\").",
	}, logsQueryTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_metrics_query",
		Description: "Run an instant PromQL query against the cluster's connected metrics store (Prometheus or VictoriaMetrics) to answer how an app is performing — CPU, memory, request rate, error rate, latency. Examples: `up`, `rate(http_requests_total[5m])`, `sum(rate(http_requests_total{status=~\"5..\"}[5m]))`, `container_memory_usage_bytes`. Returns the matching samples (each with its labels and value). Needs a metrics add-on connected first (`burrow addon connect prometheus`).",
	}, metricsQueryTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_app_delete",
		Description: "Delete an application entirely: its workload, its routing (Service and Ingress), and its recorded release history, so it disappears from the apps listing and from status. This is destructive and irreversible. Held for confirmation by a guardrail by default; set confirm=true ONLY after the user explicitly approves, never on your own.",
	}, appDeleteTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_status",
		Description: "Report an application's status: its most recent release and the live workload state (desired/ready replicas, availability). Pass env to read an environment by name from your Burrow config (such as staging or prod); it resolves locally to that environment's cluster and app namespace, so this is how you check prod versus staging. Omit env for the current environment; pass context only to target a kube context directly.",
	}, statusTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_logs",
		Description: "Return recent log lines for an application's workload.",
	}, logsTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_rollback",
		Description: "Roll an application back to its previously running release by redeploying that release's image reference. Returns the new release and which release it restored. Allowed by default (rollback is a recovery action), but an operator may configure a guardrail to hold it for confirmation; when held, the error says so — ask the user, then retry with confirm set to true.",
	}, rollbackTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_scale",
		Description: "Change an application's replica count. A guardrail may refuse it (e.g. above the replica ceiling) or hold it for confirmation (e.g. scaling to zero); when held, the error says so — ask the user, then retry with confirm set to true.",
	}, scaleTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_expose",
		Description: "Make a deployed application reachable from outside the cluster at a hostname, by creating a Service and an Ingress. Public exposure is held for confirmation by a guardrail by default; when held, the error says so — ask the user, then retry with confirm set to true. Reachability also needs an ingress controller and DNS pointing the host at the cluster.",
	}, exposeTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_unexpose",
		Description: "Remove an application's exposure (its Service and Ingress). Does not affect the running workload.",
	}, unexposeTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_reachability",
		Description: "Report whether an application is reachable at its hostname, link by link: deployed and ready, exposed, given an external address by an ingress controller, a TLS certificate when one was requested, and DNS pointing the host at that address. Returns a plain one-line summary plus the full chain, so you can tell the user exactly which link is missing and what to do. After deploying, exposing (burrow_expose), and pointing DNS at the cluster, call this with wait set to true to poll until the app is live and get its URL; when the app converges, reachable is true and url is the live address, and if it returns blocked_on that names the one link to fix. Read-only.",
	}, reachabilityTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_domain_add",
		Description: "Point a hostname at the cluster by creating or updating a DNS record at a configured provider (e.g. DigitalOcean or Cloudflare). Give the cluster's external address (the IP or hostname from burrow_reachability once the app is exposed); an IPv4 address becomes an A record, a hostname a CNAME. A guardrail holds public DNS writes for confirmation by default; when held, the error says so — ask the user, then retry with confirm set to true. The provider must already be configured by the operator.",
	}, domainAddTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_domain_remove",
		Description: "Remove the DNS record a configured provider holds for a hostname. Deleting a public DNS record is held for confirmation by a guardrail by default; when held, the error says so — ask the user, then retry with confirm set to true.",
	}, domainRemoveTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_providers",
		Description: "List the configured cloud providers and the capabilities each serves (e.g. dns), so you know which provider name to pass for an operation like burrow_domain_add. Read-only: provider credentials are configured by the operator via the CLI, never by an agent.",
	}, providersTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_guard",
		Description: "List the control-plane guardrails and their current dispositions (allow, confirm, or deny), so you can tell in advance whether an operation will be allowed, held for the user's confirmation, or denied. Read-only: guardrail policy is changed only by the operator via the CLI, never by an agent. Pass env to read a specific environment's guardrails by name (default the current one); each environment has its own policy, so prod can be locked down while staging stays permissive. Pass context only to target a kube context directly.",
	}, guardTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_cluster",
		Description: "See what this cluster can do before you change anything (ADR-0034): a neutral, read-only report of its capabilities, read live. Tells you whether an ingress controller is installed and which IngressClass to use, whether there is a default StorageClass (and its name) for persistent volumes, whether Service type=LoadBalancer is likely supported (inferred from the detected cloud provider) or the cluster is NodePort-only, whether cert-manager is installed (for TLS), the cloud provider, and whether a DNS provider is configured. Use it to survey a cluster and explain its state — and to know whether an operation like exposing an app or requesting a certificate will work — before doing anything. When an ingress controller or cert-manager is missing, the remediation is not an agent action: recommend the human run `burrow system ingress install` (it installs whichever pieces are missing, with a cost-aware LoadBalancer-vs-NodePort choice). Read-only: it changes nothing and returns no secret. Pass env to survey a specific environment by name (default the current one); this is how you compare prod versus staging. Pass context only to target a kube context directly.",
	}, clusterTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_audit",
		Description: "Review the control plane's append-only audit log (ADR-0027): the durable record of the guarded, mutating operations that ran and the guardrail outcome of each — allowed, held (confirmation required, not executed), denied, executed (allowed, or confirmed and carried out), or failed. Use it to answer \"what did the agent do,\" \"what was held or denied,\" and to show that a dangerous action asked first. Newest first; optionally filter by app/host/add-on target, operation (e.g. deploy, rollback, app_delete), outcome, and limit. Read-only — the log has no write or alter path. Args are redacted at the source to KEY NAMES and safe metadata (image reference, replica count, env/secret key names) — never an env value, token, or secret.",
	}, auditTool(d))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_environments",
		Description: "List the environments you can target: the handles in your local Burrow config (~/.burrow/config), each a name resolving to a kube context and an app namespace, with the current one marked. Pass one of these names as the env argument on another tool to operate that environment, so you can deploy to staging and gate prod by naming the environment. Read-only: it reads the local config and contacts no cluster.",
	}, environmentsTool(d))

	return s
}

// Serve runs the Burrow MCP server over stdio until the client disconnects. It targets one
// environment per call: env handles are resolved through resolve, clients are built per target
// through clientFor, and the local handles are listed via envs (ADR-0036).
func Serve(ctx context.Context, resolve EnvResolver, clientFor ClientForEnv, envs EnvLister, version string) error {
	return NewServer(resolve, clientFor, envs, version).Run(ctx, &sdk.StdioTransport{})
}

type deployInput struct {
	envArg
	contextArg
	App         string   `json:"app" jsonschema:"the application name (a DNS-1123 label)"`
	Image       string   `json:"image" jsonschema:"the pullable container image reference to deploy, e.g. registry.example.com/app:1.2.3"`
	Command     []string `json:"command,omitempty" jsonschema:"optional command override for the container"`
	MetricsPort int32    `json:"metrics_port,omitempty" jsonschema:"optional: annotate the pod so the metrics add-on scrapes /metrics on this port"`
	Replicas    int32    `json:"replicas" jsonschema:"desired number of replicas"`
	Confirm     bool     `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed an operation a guardrail held for confirmation; do not self-confirm"`
}

type appInput struct {
	envArg
	contextArg
	App string `json:"app" jsonschema:"the application name"`
}

type logsInput struct {
	envArg
	contextArg
	App  string `json:"app" jsonschema:"the application name"`
	Tail int    `json:"tail,omitempty" jsonschema:"maximum number of recent log lines to return"`
}

type scaleInput struct {
	envArg
	contextArg
	App      string `json:"app" jsonschema:"the application name"`
	Replicas int32  `json:"replicas" jsonschema:"desired number of replicas"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed an operation a guardrail held for confirmation (e.g. scaling to zero); do not self-confirm"`
}

// deployOutput echoes the environment the deploy acted in alongside the release result, so a
// defaulted target is legible to the agent and the audit trail (ADR-0036).
type deployOutput struct {
	Environment envRef `json:"environment"`
	client.DeployResult
}

func deployTool(d deps) sdk.ToolHandlerFor[deployInput, deployOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in deployInput) (*sdk.CallToolResult, deployOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, deployOutput{}, err
		}
		res, err := c.Deploy(ctx, in.App, client.DeployRequest{Env: r.AppNamespace, Image: in.Image, Command: in.Command, MetricsPort: in.MetricsPort, Replicas: in.Replicas, Confirm: in.Confirm})
		if err != nil {
			return nil, deployOutput{}, err
		}
		return nil, deployOutput{Environment: toEnvRef(r), DeployResult: res}, nil
	}
}

type configSetInput struct {
	envArg
	contextArg
	App       string `json:"app" jsonschema:"the application name"`
	Key       string `json:"key" jsonschema:"the config var name (e.g. LOG_LEVEL)"`
	Value     string `json:"value" jsonschema:"the value to set"`
	NoRestart bool   `json:"no_restart,omitempty" jsonschema:"true to persist without rolling the running app; the change lands on the next deploy"`
}

type configUnsetInput struct {
	envArg
	contextArg
	App       string `json:"app" jsonschema:"the application name"`
	Key       string `json:"key" jsonschema:"the config var name to remove"`
	NoRestart bool   `json:"no_restart,omitempty" jsonschema:"true to persist the removal without rolling the running app; the change lands on the next deploy"`
}

// keyAck is a small structured ack for a config or secret key mutation. It echoes the
// environment the mutation acted in (ADR-0036).
type keyAck struct {
	Environment envRef `json:"environment"`
	App         string `json:"app"`
	Key         string `json:"key"`
}

type configOutput struct {
	Config map[string]string `json:"config"`
}

func configSetTool(d deps) sdk.ToolHandlerFor[configSetInput, keyAck] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in configSetInput) (*sdk.CallToolResult, keyAck, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, keyAck{}, err
		}
		if err := c.SetConfig(ctx, in.App, r.AppNamespace, in.Key, in.Value, in.NoRestart); err != nil {
			return nil, keyAck{}, err
		}
		return nil, keyAck{Environment: toEnvRef(r), App: in.App, Key: in.Key}, nil
	}
}

func configUnsetTool(d deps) sdk.ToolHandlerFor[configUnsetInput, keyAck] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in configUnsetInput) (*sdk.CallToolResult, keyAck, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, keyAck{}, err
		}
		if err := c.UnsetConfig(ctx, in.App, r.AppNamespace, in.Key, in.NoRestart); err != nil {
			return nil, keyAck{}, err
		}
		return nil, keyAck{Environment: toEnvRef(r), App: in.App, Key: in.Key}, nil
	}
}

func configListTool(d deps) sdk.ToolHandlerFor[appInput, configOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, configOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, configOutput{}, err
		}
		cfg, err := c.Config(ctx, in.App, r.AppNamespace)
		if err != nil {
			return nil, configOutput{}, err
		}
		return nil, configOutput{Config: cfg}, nil
	}
}

type secretUnsetInput struct {
	envArg
	contextArg
	App       string `json:"app" jsonschema:"the application name"`
	Key       string `json:"key" jsonschema:"the secret environment variable name to remove (the KEY, not a value)"`
	NoRestart bool   `json:"no_restart,omitempty" jsonschema:"true to persist the removal without rolling the running app; the change lands on the next deploy"`
}

// secretsOutput is an app's secret KEYS only — never the values (ADR-0028/0004).
type secretsOutput struct {
	Keys []string `json:"keys"`
}

func secretListTool(d deps) sdk.ToolHandlerFor[appInput, secretsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, secretsOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, secretsOutput{}, err
		}
		keys, err := c.Secrets(ctx, in.App, r.AppNamespace)
		if err != nil {
			return nil, secretsOutput{}, err
		}
		return nil, secretsOutput{Keys: keys}, nil
	}
}

func secretUnsetTool(d deps) sdk.ToolHandlerFor[secretUnsetInput, keyAck] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in secretUnsetInput) (*sdk.CallToolResult, keyAck, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, keyAck{}, err
		}
		if err := c.UnsetSecret(ctx, in.App, r.AppNamespace, in.Key, in.NoRestart); err != nil {
			return nil, keyAck{}, err
		}
		return nil, keyAck{Environment: toEnvRef(r), App: in.App, Key: in.Key}, nil
	}
}

func statusTool(d deps) sdk.ToolHandlerFor[appInput, client.StatusResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, client.StatusResult, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, client.StatusResult{}, err
		}
		res, err := c.Status(ctx, in.App, r.AppNamespace)
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

func logsTool(d deps) sdk.ToolHandlerFor[logsInput, logsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in logsInput) (*sdk.CallToolResult, logsOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, logsOutput{}, err
		}
		lines, err := c.Logs(ctx, in.App, r.AppNamespace, in.Tail)
		if err != nil {
			return nil, logsOutput{}, err
		}
		return nil, logsOutput{Lines: lines}, nil
	}
}

type rollbackInput struct {
	envArg
	contextArg
	App     string `json:"app" jsonschema:"the application name"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed a rollback that an operator's guardrail held for confirmation; do not self-confirm"`
}

// rollbackOutput echoes the environment the rollback acted in alongside the result (ADR-0036).
type rollbackOutput struct {
	Environment envRef `json:"environment"`
	client.RollbackResult
}

func rollbackTool(d deps) sdk.ToolHandlerFor[rollbackInput, rollbackOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in rollbackInput) (*sdk.CallToolResult, rollbackOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, rollbackOutput{}, err
		}
		res, err := c.Rollback(ctx, in.App, r.AppNamespace, in.Confirm)
		if err != nil {
			return nil, rollbackOutput{}, err
		}
		return nil, rollbackOutput{Environment: toEnvRef(r), RollbackResult: res}, nil
	}
}

// scaleOutput echoes the environment the scale acted in alongside the result (ADR-0036).
type scaleOutput struct {
	Environment envRef `json:"environment"`
	client.ScaleResult
}

func scaleTool(d deps) sdk.ToolHandlerFor[scaleInput, scaleOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in scaleInput) (*sdk.CallToolResult, scaleOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, scaleOutput{}, err
		}
		res, err := c.Scale(ctx, in.App, r.AppNamespace, in.Replicas, in.Confirm)
		if err != nil {
			return nil, scaleOutput{}, err
		}
		return nil, scaleOutput{Environment: toEnvRef(r), ScaleResult: res}, nil
	}
}

type exposeInput struct {
	envArg
	contextArg
	App     string `json:"app" jsonschema:"the application name"`
	Host    string `json:"host" jsonschema:"the external hostname to route to the app, e.g. app.example.com"`
	Port    int32  `json:"port" jsonschema:"the app's container port to forward to"`
	TLS     bool   `json:"tls,omitempty" jsonschema:"request an HTTPS certificate for the host via cert-manager"`
	Issuer  string `json:"issuer,omitempty" jsonschema:"the cert-manager ClusterIssuer to use when tls is set (e.g. letsencrypt)"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed exposing the app to the public internet; do not self-confirm"`
}

// exposeOutput echoes the environment the expose acted in alongside the result (ADR-0036).
type exposeOutput struct {
	Environment envRef `json:"environment"`
	client.ExposeResult
}

func exposeTool(d deps) sdk.ToolHandlerFor[exposeInput, exposeOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in exposeInput) (*sdk.CallToolResult, exposeOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, exposeOutput{}, err
		}
		res, err := c.Expose(ctx, in.App, r.AppNamespace, in.Host, in.Port, in.TLS, in.Issuer, in.Confirm)
		if err != nil {
			return nil, exposeOutput{}, err
		}
		return nil, exposeOutput{Environment: toEnvRef(r), ExposeResult: res}, nil
	}
}

// unexposeOutput is a small structured ack for the unexpose tool, echoing the environment it
// acted in (ADR-0036).
type unexposeOutput struct {
	Environment envRef `json:"environment"`
	App         string `json:"app"`
}

func unexposeTool(d deps) sdk.ToolHandlerFor[appInput, unexposeOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, unexposeOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, unexposeOutput{}, err
		}
		if err := c.Unexpose(ctx, in.App, r.AppNamespace); err != nil {
			return nil, unexposeOutput{}, err
		}
		return nil, unexposeOutput{Environment: toEnvRef(r), App: in.App}, nil
	}
}

type reachabilityInput struct {
	envArg
	contextArg
	App  string `json:"app" jsonschema:"the application name"`
	Wait bool   `json:"wait,omitempty" jsonschema:"poll until the app is live (reachable) or a timeout, instead of a single point-in-time check; use after deploying, exposing, and pointing DNS to confirm the app is live and get its URL"`
}

// reachabilityWaitTimeout bounds how long the burrow_reachability tool polls in wait mode before
// returning the last verdict. The control-plane engine stays point-in-time; this wait lives in
// the thin client layer (ADR-0034 slice 3).
const reachabilityWaitTimeout = 3 * time.Minute

func reachabilityTool(d deps) sdk.ToolHandlerFor[reachabilityInput, client.ReachabilityResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in reachabilityInput) (*sdk.CallToolResult, client.ReachabilityResult, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, client.ReachabilityResult{}, err
		}
		reach := func(ctx context.Context, app string) (client.ReachabilityResult, error) {
			return c.Reachability(ctx, app, r.AppNamespace)
		}
		if in.Wait {
			reach = func(ctx context.Context, app string) (client.ReachabilityResult, error) {
				return c.WaitReachable(ctx, app, r.AppNamespace, reachabilityWaitTimeout, nil)
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
	envArg
	contextArg
	Host     string `json:"host" jsonschema:"the hostname to point at the cluster, e.g. app.example.com"`
	Provider string `json:"provider,omitempty" jsonschema:"the configured DNS provider to write the record at (its name from burrow_providers); omit to use the only one configured"`
	Address  string `json:"address,omitempty" jsonschema:"the cluster's external IPv4 address or hostname to point at; omit if you set app instead"`
	App      string `json:"app,omitempty" jsonschema:"an exposed app whose external address to point at, instead of address — the control plane reads it from the app's ingress"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed writing the public DNS record; do not self-confirm"`
}

// domainOutput echoes the environment a DNS mutation acted in alongside the result (ADR-0036).
type domainOutput struct {
	Environment envRef `json:"environment"`
	client.DomainResult
}

func domainAddTool(d deps) sdk.ToolHandlerFor[domainAddInput, domainOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in domainAddInput) (*sdk.CallToolResult, domainOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, domainOutput{}, err
		}
		res, err := c.AddDomain(ctx, in.Host, in.Provider, in.Address, in.App, in.Confirm)
		if err != nil {
			return nil, domainOutput{}, err
		}
		return nil, domainOutput{Environment: toEnvRef(r), DomainResult: res}, nil
	}
}

type domainRemoveInput struct {
	envArg
	contextArg
	Host     string `json:"host" jsonschema:"the hostname whose DNS record to remove"`
	Provider string `json:"provider,omitempty" jsonschema:"the configured DNS provider holding the record; omit to use the only one configured"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed deleting the public DNS record; do not self-confirm"`
}

func domainRemoveTool(d deps) sdk.ToolHandlerFor[domainRemoveInput, domainOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in domainRemoveInput) (*sdk.CallToolResult, domainOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, domainOutput{}, err
		}
		res, err := c.RemoveDomain(ctx, in.Host, in.Provider, in.Confirm)
		if err != nil {
			return nil, domainOutput{}, err
		}
		return nil, domainOutput{Environment: toEnvRef(r), DomainResult: res}, nil
	}
}

// providersInput carries only the target environment: listing providers takes no other
// arguments.
type providersInput struct {
	envArg
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

// appsInput carries only the target environment: listing apps takes no other arguments.
type appsInput struct {
	envArg
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

func appsTool(d deps) sdk.ToolHandlerFor[appsInput, appsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appsInput) (*sdk.CallToolResult, appsOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, appsOutput{}, err
		}
		apps, err := c.Apps(ctx, r.AppNamespace)
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

// addonsInput carries only the target environment: listing add-ons takes no other arguments.
type addonsInput struct {
	envArg
	contextArg
}

type addonsOutput struct {
	Addons []addonItem `json:"addons"`
}

func addonsTool(d deps) sdk.ToolHandlerFor[addonsInput, addonsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addonsInput) (*sdk.CallToolResult, addonsOutput, error) {
		c, _, err := d.target(in.Env, in.Context)
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
	envArg
	contextArg
	Capability string `json:"capability" jsonschema:"the capability to install a vetted backing service for, e.g. logs"`
	Confirm    bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed an operation a guardrail held for confirmation; do not self-confirm"`
}

// addonInstallOutput echoes the environment the install acted in alongside the add-on (ADR-0036).
type addonInstallOutput struct {
	Environment envRef `json:"environment"`
	addonItem
}

func addonInstallTool(d deps) sdk.ToolHandlerFor[addonInstallInput, addonInstallOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addonInstallInput) (*sdk.CallToolResult, addonInstallOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, addonInstallOutput{}, err
		}
		a, err := c.InstallAddon(ctx, in.Capability, in.Confirm)
		if err != nil {
			return nil, addonInstallOutput{}, err
		}
		return nil, addonInstallOutput{Environment: toEnvRef(r), addonItem: toAddonItem(a)}, nil
	}
}

type appDeleteInput struct {
	envArg
	contextArg
	App     string `json:"app" jsonschema:"the application name to delete (a DNS-1123 label)"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed this destructive delete; do not self-confirm"`
}

type appDeleteOutput struct {
	Environment envRef `json:"environment"`
	Deleted     string `json:"deleted"`
}

func appDeleteTool(d deps) sdk.ToolHandlerFor[appDeleteInput, appDeleteOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appDeleteInput) (*sdk.CallToolResult, appDeleteOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, appDeleteOutput{}, err
		}
		if err := c.DeleteApp(ctx, in.App, r.AppNamespace, in.Confirm); err != nil {
			return nil, appDeleteOutput{}, err
		}
		return nil, appDeleteOutput{Environment: toEnvRef(r), Deleted: in.App}, nil
	}
}

type addonRemoveInput struct {
	envArg
	contextArg
	Name    string `json:"name" jsonschema:"the add-on instance name to remove"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed; do not self-confirm"`
}

type addonRemoveOutput struct {
	Environment envRef `json:"environment"`
	Removed     string `json:"removed"`
}

// addonAttachInput carries only the add-on type and app name — never a secret (ADR-0031).
type addonAttachInput struct {
	envArg
	contextArg
	Addon string `json:"addon" jsonschema:"the add-on type to attach, e.g. postgres"`
	App   string `json:"app" jsonschema:"the application name to give a database (a DNS-1123 label)"`
}

// addonAttachOutput is the non-secret ack: the environment, the app, the add-on, and the KEY the
// connection string was written under — never the value (ADR-0031, ADR-0036).
type addonAttachOutput struct {
	Environment envRef `json:"environment"`
	App         string `json:"app"`
	Addon       string `json:"addon"`
	SecretKey   string `json:"secret_key"`
}

func addonAttachTool(d deps) sdk.ToolHandlerFor[addonAttachInput, addonAttachOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addonAttachInput) (*sdk.CallToolResult, addonAttachOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, addonAttachOutput{}, err
		}
		res, err := c.AttachAddon(ctx, in.Addon, in.App)
		if err != nil {
			return nil, addonAttachOutput{}, err
		}
		return nil, addonAttachOutput{Environment: toEnvRef(r), App: res.App, Addon: res.Addon, SecretKey: res.SecretKey}, nil
	}
}

// addonBackupInput carries only the add-on type and app name — never a secret (ADR-0032).
type addonBackupInput struct {
	envArg
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

// addonBackupOutput echoes the environment the backup ran in alongside the recorded backup
// (ADR-0036).
type addonBackupOutput struct {
	Environment envRef `json:"environment"`
	backupItem
}

func addonBackupTool(d deps) sdk.ToolHandlerFor[addonBackupInput, addonBackupOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addonBackupInput) (*sdk.CallToolResult, addonBackupOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, addonBackupOutput{}, err
		}
		res, err := c.BackupAddon(ctx, in.Addon, in.App)
		if err != nil {
			return nil, addonBackupOutput{}, err
		}
		return nil, addonBackupOutput{Environment: toEnvRef(r), backupItem: toBackupItem(res.Backup)}, nil
	}
}

// addonBackupsInput carries the add-on type and an optional app filter — never a secret.
type addonBackupsInput struct {
	envArg
	contextArg
	Addon string `json:"addon" jsonschema:"the add-on type to list backups for, e.g. postgres"`
	App   string `json:"app,omitempty" jsonschema:"optional: restrict to one app; omit to list every app's backups"`
}

type addonBackupsOutput struct {
	Backups []backupItem `json:"backups"`
}

func addonBackupsTool(d deps) sdk.ToolHandlerFor[addonBackupsInput, addonBackupsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addonBackupsInput) (*sdk.CallToolResult, addonBackupsOutput, error) {
		c, _, err := d.target(in.Env, in.Context)
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

func addonRemoveTool(d deps) sdk.ToolHandlerFor[addonRemoveInput, addonRemoveOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addonRemoveInput) (*sdk.CallToolResult, addonRemoveOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, addonRemoveOutput{}, err
		}
		if err := c.RemoveAddon(ctx, in.Name, in.Confirm); err != nil {
			return nil, addonRemoveOutput{}, err
		}
		return nil, addonRemoveOutput{Environment: toEnvRef(r), Removed: in.Name}, nil
	}
}

type logsQueryInput struct {
	envArg
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

func logsQueryTool(d deps) sdk.ToolHandlerFor[logsQueryInput, logsQueryOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in logsQueryInput) (*sdk.CallToolResult, logsQueryOutput, error) {
		c, _, err := d.target(in.Env, in.Context)
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
	envArg
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

func metricsQueryTool(d deps) sdk.ToolHandlerFor[metricsQueryInput, metricsQueryOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in metricsQueryInput) (*sdk.CallToolResult, metricsQueryOutput, error) {
		c, _, err := d.target(in.Env, in.Context)
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

func providersTool(d deps) sdk.ToolHandlerFor[providersInput, providersOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in providersInput) (*sdk.CallToolResult, providersOutput, error) {
		c, _, err := d.target(in.Env, in.Context)
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

// guardInput carries only the target environment: listing guardrails takes no other arguments.
type guardInput struct {
	envArg
	contextArg
}

type guardOutput struct {
	Guardrails []client.Guardrail `json:"guardrails"`
}

func guardTool(d deps) sdk.ToolHandlerFor[guardInput, guardOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in guardInput) (*sdk.CallToolResult, guardOutput, error) {
		c, r, err := d.target(in.Env, in.Context)
		if err != nil {
			return nil, guardOutput{}, err
		}
		gs, err := c.Guardrails(ctx, r.AppNamespace)
		if err != nil {
			return nil, guardOutput{}, err
		}
		return nil, guardOutput{Guardrails: gs}, nil
	}
}

// clusterInput carries only the target environment: reading cluster capabilities takes no other
// arguments.
type clusterInput struct {
	envArg
	contextArg
}

func clusterTool(d deps) sdk.ToolHandlerFor[clusterInput, client.ClusterCapabilities] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in clusterInput) (*sdk.CallToolResult, client.ClusterCapabilities, error) {
		c, _, err := d.target(in.Env, in.Context)
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
	envArg
	contextArg
	App       string `json:"app,omitempty" jsonschema:"optional: filter to one app/host/add-on target"`
	Operation string `json:"operation,omitempty" jsonschema:"optional: filter to one operation, e.g. deploy, rollback, app_delete"`
	Outcome   string `json:"outcome,omitempty" jsonschema:"optional: filter to one outcome — allowed, held, denied, executed, or failed"`
	Limit     int    `json:"limit,omitempty" jsonschema:"optional: maximum rows to return (default 200), newest first"`
}

type auditOutput struct {
	Entries []client.AuditEntry `json:"entries"`
}

func auditTool(d deps) sdk.ToolHandlerFor[auditInput, auditOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in auditInput) (*sdk.CallToolResult, auditOutput, error) {
		c, _, err := d.target(in.Env, in.Context)
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

// environmentsInput has no fields: burrow_environments lists the local Burrow config handles the
// agent can target and takes no arguments, not even an env (it lists the environments to name).
type environmentsInput struct{}

// environmentInfo is the agent's view of one local environment handle (ADR-0036): the name, the
// kube context and app namespace it resolves to, and whether it is the current selection.
type environmentInfo struct {
	Name      string `json:"name"`
	Context   string `json:"context"`
	Namespace string `json:"namespace"`
	Current   bool   `json:"current"`
}

type environmentsOutput struct {
	Environments []environmentInfo `json:"environments"`
}

func environmentsTool(d deps) sdk.ToolHandlerFor[environmentsInput, environmentsOutput] {
	return func(_ context.Context, _ *sdk.CallToolRequest, _ environmentsInput) (*sdk.CallToolResult, environmentsOutput, error) {
		hs, err := d.envs()
		if err != nil {
			return nil, environmentsOutput{}, err
		}
		out := environmentsOutput{Environments: make([]environmentInfo, 0, len(hs))}
		for _, h := range hs {
			out.Environments = append(out.Environments, environmentInfo{Name: h.Name, Context: h.Context, Namespace: h.Namespace, Current: h.Current})
		}
		return nil, out, nil
	}
}
