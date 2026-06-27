// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/burrow-cloud/burrow/client"
)

// NewServer builds the Burrow MCP server: an agent-neutral surface (ADR-0003) exposing
// the control plane's operations as MCP tools. Each tool translates a call into a
// control-plane API call via the client and returns the structured result; a control
// plane error becomes a tool error the agent can read. The server holds no cluster
// credentials (ADR-0005).
func NewServer(c *client.Client, version string) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "burrow", Title: "Burrow", Version: version}, nil)

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_deploy",
		Description: "Deploy an application to the cluster by container image reference. The image must already be pushed to a registry the cluster can pull from; only the reference and small metadata are sent, never code. Returns the new release and the release it superseded (the rollback handle).",
	}, deployTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_apps",
		Description: "List the applications Burrow manages and each one's running state (image, ready/desired replicas, availability), so you can discover what is deployed before operating on it. Read-only.",
	}, appsTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_addons",
		Description: "List the backing-service add-ons installed on the cluster (logs, …), their mode (installed/connected), in-cluster endpoint, and the capabilities you can query. Read-only.",
	}, addonsTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_addon_install",
		Description: "Install a vetted, self-hostable backing service for a capability (e.g. logs → VictoriaLogs) and register it as queryable, in one step. Held for confirmation by a guardrail; set confirm=true ONLY after the user approves, never on your own.",
	}, addonInstallTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_addon_remove",
		Description: "Remove an installed add-on by name. Held for confirmation by a guardrail (removing a backing service can break dependent apps); set confirm=true ONLY after the user approves.",
	}, addonRemoveTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_logs_query",
		Description: "Query the cluster's aggregated logs (the installed logs add-on) with a VictoriaLogs LogsQL query to investigate why an app is failing or slow — e.g. `error`, `level:error`, `panic AND web`. Returns recent matching records (most recent first). Needs a logs add-on installed first (burrow_addon_install with capability \"logs\").",
	}, logsQueryTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_metrics_query",
		Description: "Run an instant PromQL query against the cluster's connected metrics store (Prometheus or VictoriaMetrics) to answer how an app is performing — CPU, memory, request rate, error rate, latency. Examples: `up`, `rate(http_requests_total[5m])`, `sum(rate(http_requests_total{status=~\"5..\"}[5m]))`, `container_memory_usage_bytes`. Returns the matching samples (each with its labels and value). Needs a metrics add-on connected first (`burrow addon connect prometheus`).",
	}, metricsQueryTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_app_delete",
		Description: "Delete an application entirely: its workload, its routing (Service and Ingress), and its recorded release history, so it disappears from the apps listing and from status. This is destructive and irreversible. Held for confirmation by a guardrail by default; set confirm=true ONLY after the user explicitly approves, never on your own.",
	}, appDeleteTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_status",
		Description: "Report an application's status: its most recent release and the live workload state (desired/ready replicas, availability).",
	}, statusTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_logs",
		Description: "Return recent log lines for an application's workload.",
	}, logsTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_rollback",
		Description: "Roll an application back to its previously running release by redeploying that release's image reference. Returns the new release and which release it restored.",
	}, rollbackTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_scale",
		Description: "Change an application's replica count. A guardrail may refuse it (e.g. above the replica ceiling) or hold it for confirmation (e.g. scaling to zero); when held, the error says so — ask the user, then retry with confirm set to true.",
	}, scaleTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_expose",
		Description: "Make a deployed application reachable from outside the cluster at a hostname, by creating a Service and an Ingress. Public exposure is held for confirmation by a guardrail by default; when held, the error says so — ask the user, then retry with confirm set to true. Reachability also needs an ingress controller and DNS pointing the host at the cluster.",
	}, exposeTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_unexpose",
		Description: "Remove an application's exposure (its Service and Ingress). Does not affect the running workload.",
	}, unexposeTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_reachability",
		Description: "Report whether an application is reachable at its hostname, link by link: deployed and ready, exposed, given an external address by an ingress controller, and DNS pointing the host at that address. Returns a plain one-line summary plus the full chain, so you can tell the user exactly which link is missing and what to do. Read-only.",
	}, reachabilityTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_domain_add",
		Description: "Point a hostname at the cluster by creating or updating a DNS record at a configured provider (e.g. DigitalOcean or Cloudflare). Give the cluster's external address (the IP or hostname from burrow_reachability once the app is exposed); an IPv4 address becomes an A record, a hostname a CNAME. A guardrail holds public DNS writes for confirmation by default; when held, the error says so — ask the user, then retry with confirm set to true. The provider must already be configured by the operator.",
	}, domainAddTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_domain_remove",
		Description: "Remove the DNS record a configured provider holds for a hostname. Deleting a public DNS record is held for confirmation by a guardrail by default; when held, the error says so — ask the user, then retry with confirm set to true.",
	}, domainRemoveTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_providers",
		Description: "List the configured cloud providers and the capabilities each serves (e.g. dns), so you know which provider name to pass for an operation like burrow_domain_add. Read-only: provider credentials are configured by the operator via the CLI, never by an agent.",
	}, providersTool(c))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_guard",
		Description: "List the control-plane guardrails and their current dispositions (allow, confirm, or deny), so you can tell in advance whether an operation will be allowed, held for the user's confirmation, or denied. Read-only: guardrail policy is changed only by the operator via the CLI, never by an agent.",
	}, guardTool(c))

	return s
}

// Serve runs the Burrow MCP server over stdio until the client disconnects.
func Serve(ctx context.Context, c *client.Client, version string) error {
	return NewServer(c, version).Run(ctx, &sdk.StdioTransport{})
}

type deployInput struct {
	App         string            `json:"app" jsonschema:"the application name (a DNS-1123 label)"`
	Image       string            `json:"image" jsonschema:"the pullable container image reference to deploy, e.g. registry.example.com/app:1.2.3"`
	Env         map[string]string `json:"env,omitempty" jsonschema:"environment variables to set on the workload"`
	Command     []string          `json:"command,omitempty" jsonschema:"optional command override for the container"`
	MetricsPort int32             `json:"metrics_port,omitempty" jsonschema:"optional: annotate the pod so the metrics add-on scrapes /metrics on this port"`
	Replicas    int32             `json:"replicas" jsonschema:"desired number of replicas"`
	Confirm     bool              `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed an operation a guardrail held for confirmation; do not self-confirm"`
}

type appInput struct {
	App string `json:"app" jsonschema:"the application name"`
}

type logsInput struct {
	App  string `json:"app" jsonschema:"the application name"`
	Tail int    `json:"tail,omitempty" jsonschema:"maximum number of recent log lines to return"`
}

type scaleInput struct {
	App      string `json:"app" jsonschema:"the application name"`
	Replicas int32  `json:"replicas" jsonschema:"desired number of replicas"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed an operation a guardrail held for confirmation (e.g. scaling to zero); do not self-confirm"`
}

func deployTool(c *client.Client) sdk.ToolHandlerFor[deployInput, client.DeployResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in deployInput) (*sdk.CallToolResult, client.DeployResult, error) {
		res, err := c.Deploy(ctx, in.App, client.DeployRequest{Image: in.Image, Env: in.Env, Command: in.Command, MetricsPort: in.MetricsPort, Replicas: in.Replicas, Confirm: in.Confirm})
		if err != nil {
			return nil, client.DeployResult{}, err
		}
		return nil, res, nil
	}
}

func statusTool(c *client.Client) sdk.ToolHandlerFor[appInput, client.StatusResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, client.StatusResult, error) {
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

func logsTool(c *client.Client) sdk.ToolHandlerFor[logsInput, logsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in logsInput) (*sdk.CallToolResult, logsOutput, error) {
		lines, err := c.Logs(ctx, in.App, in.Tail)
		if err != nil {
			return nil, logsOutput{}, err
		}
		return nil, logsOutput{Lines: lines}, nil
	}
}

func rollbackTool(c *client.Client) sdk.ToolHandlerFor[appInput, client.RollbackResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, client.RollbackResult, error) {
		res, err := c.Rollback(ctx, in.App)
		if err != nil {
			return nil, client.RollbackResult{}, err
		}
		return nil, res, nil
	}
}

func scaleTool(c *client.Client) sdk.ToolHandlerFor[scaleInput, client.ScaleResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in scaleInput) (*sdk.CallToolResult, client.ScaleResult, error) {
		res, err := c.Scale(ctx, in.App, in.Replicas, in.Confirm)
		if err != nil {
			return nil, client.ScaleResult{}, err
		}
		return nil, res, nil
	}
}

type exposeInput struct {
	App     string `json:"app" jsonschema:"the application name"`
	Host    string `json:"host" jsonschema:"the external hostname to route to the app, e.g. app.example.com"`
	Port    int32  `json:"port" jsonschema:"the app's container port to forward to"`
	TLS     bool   `json:"tls,omitempty" jsonschema:"request an HTTPS certificate for the host via cert-manager"`
	Issuer  string `json:"issuer,omitempty" jsonschema:"the cert-manager ClusterIssuer to use when tls is set (e.g. letsencrypt)"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed exposing the app to the public internet; do not self-confirm"`
}

func exposeTool(c *client.Client) sdk.ToolHandlerFor[exposeInput, client.ExposeResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in exposeInput) (*sdk.CallToolResult, client.ExposeResult, error) {
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

func unexposeTool(c *client.Client) sdk.ToolHandlerFor[appInput, unexposeOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, unexposeOutput, error) {
		if err := c.Unexpose(ctx, in.App); err != nil {
			return nil, unexposeOutput{}, err
		}
		return nil, unexposeOutput{App: in.App}, nil
	}
}

func reachabilityTool(c *client.Client) sdk.ToolHandlerFor[appInput, client.ReachabilityResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, client.ReachabilityResult, error) {
		res, err := c.Reachability(ctx, in.App)
		if err != nil {
			return nil, client.ReachabilityResult{}, err
		}
		return nil, res, nil
	}
}

type domainAddInput struct {
	Host     string `json:"host" jsonschema:"the hostname to point at the cluster, e.g. app.example.com"`
	Provider string `json:"provider,omitempty" jsonschema:"the configured DNS provider to write the record at (its name from burrow_providers); omit to use the only one configured"`
	Address  string `json:"address,omitempty" jsonschema:"the cluster's external IPv4 address or hostname to point at; omit if you set app instead"`
	App      string `json:"app,omitempty" jsonschema:"an exposed app whose external address to point at, instead of address — the control plane reads it from the app's ingress"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed writing the public DNS record; do not self-confirm"`
}

func domainAddTool(c *client.Client) sdk.ToolHandlerFor[domainAddInput, client.DomainResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in domainAddInput) (*sdk.CallToolResult, client.DomainResult, error) {
		res, err := c.AddDomain(ctx, in.Host, in.Provider, in.Address, in.App, in.Confirm)
		if err != nil {
			return nil, client.DomainResult{}, err
		}
		return nil, res, nil
	}
}

type domainRemoveInput struct {
	Host     string `json:"host" jsonschema:"the hostname whose DNS record to remove"`
	Provider string `json:"provider,omitempty" jsonschema:"the configured DNS provider holding the record; omit to use the only one configured"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed deleting the public DNS record; do not self-confirm"`
}

func domainRemoveTool(c *client.Client) sdk.ToolHandlerFor[domainRemoveInput, client.DomainResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in domainRemoveInput) (*sdk.CallToolResult, client.DomainResult, error) {
		res, err := c.RemoveDomain(ctx, in.Host, in.Provider, in.Confirm)
		if err != nil {
			return nil, client.DomainResult{}, err
		}
		return nil, res, nil
	}
}

// providersInput has no fields: listing providers takes no arguments.
type providersInput struct{}

// providerInfo is the non-secret view of a configured provider the agent sees.
type providerInfo struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Capabilities []string `json:"capabilities"`
}

type providersOutput struct {
	Providers []providerInfo `json:"providers"`
}

// appsInput has no fields: listing apps takes no arguments.
type appsInput struct{}

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

func appsTool(c *client.Client) sdk.ToolHandlerFor[appsInput, appsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, _ appsInput) (*sdk.CallToolResult, appsOutput, error) {
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

type addonsInput struct{}

type addonsOutput struct {
	Addons []addonItem `json:"addons"`
}

func addonsTool(c *client.Client) sdk.ToolHandlerFor[addonsInput, addonsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, _ addonsInput) (*sdk.CallToolResult, addonsOutput, error) {
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
	Capability string `json:"capability" jsonschema:"the capability to install a vetted backing service for, e.g. logs"`
	Confirm    bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed an operation a guardrail held for confirmation; do not self-confirm"`
}

func addonInstallTool(c *client.Client) sdk.ToolHandlerFor[addonInstallInput, addonItem] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addonInstallInput) (*sdk.CallToolResult, addonItem, error) {
		a, err := c.InstallAddon(ctx, in.Capability, in.Confirm)
		if err != nil {
			return nil, addonItem{}, err
		}
		return nil, toAddonItem(a), nil
	}
}

type appDeleteInput struct {
	App     string `json:"app" jsonschema:"the application name to delete (a DNS-1123 label)"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed this destructive delete; do not self-confirm"`
}

type appDeleteOutput struct {
	Deleted string `json:"deleted"`
}

func appDeleteTool(c *client.Client) sdk.ToolHandlerFor[appDeleteInput, appDeleteOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appDeleteInput) (*sdk.CallToolResult, appDeleteOutput, error) {
		if err := c.DeleteApp(ctx, in.App, in.Confirm); err != nil {
			return nil, appDeleteOutput{}, err
		}
		return nil, appDeleteOutput{Deleted: in.App}, nil
	}
}

type addonRemoveInput struct {
	Name    string `json:"name" jsonschema:"the add-on instance name to remove"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed; do not self-confirm"`
}

type addonRemoveOutput struct {
	Removed string `json:"removed"`
}

func addonRemoveTool(c *client.Client) sdk.ToolHandlerFor[addonRemoveInput, addonRemoveOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addonRemoveInput) (*sdk.CallToolResult, addonRemoveOutput, error) {
		if err := c.RemoveAddon(ctx, in.Name, in.Confirm); err != nil {
			return nil, addonRemoveOutput{}, err
		}
		return nil, addonRemoveOutput{Removed: in.Name}, nil
	}
}

type logsQueryInput struct {
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

func logsQueryTool(c *client.Client) sdk.ToolHandlerFor[logsQueryInput, logsQueryOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in logsQueryInput) (*sdk.CallToolResult, logsQueryOutput, error) {
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

func metricsQueryTool(c *client.Client) sdk.ToolHandlerFor[metricsQueryInput, metricsQueryOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in metricsQueryInput) (*sdk.CallToolResult, metricsQueryOutput, error) {
		ss, err := c.QueryMetrics(ctx, in.Query, in.Backend)
		if err != nil {
			return nil, metricsQueryOutput{}, err
		}
		out := metricsQueryOutput{Samples: make([]metricSample, 0, len(ss))}
		for _, s := range ss {
			out.Samples = append(out.Samples, metricSample{Labels: s.Labels, Value: s.Value, Time: s.Time})
		}
		return nil, out, nil
	}
}

func providersTool(c *client.Client) sdk.ToolHandlerFor[providersInput, providersOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, _ providersInput) (*sdk.CallToolResult, providersOutput, error) {
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

// guardInput has no fields: listing guardrails takes no arguments.
type guardInput struct{}

type guardOutput struct {
	Guardrails []client.Guardrail `json:"guardrails"`
}

func guardTool(c *client.Client) sdk.ToolHandlerFor[guardInput, guardOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, _ guardInput) (*sdk.CallToolResult, guardOutput, error) {
		gs, err := c.Guardrails(ctx)
		if err != nil {
			return nil, guardOutput{}, err
		}
		return nil, guardOutput{Guardrails: gs}, nil
	}
}
