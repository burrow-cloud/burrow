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
	App      string            `json:"app" jsonschema:"the application name (a DNS-1123 label)"`
	Image    string            `json:"image" jsonschema:"the pullable container image reference to deploy, e.g. registry.example.com/app:1.2.3"`
	Env      map[string]string `json:"env,omitempty" jsonschema:"environment variables to set on the workload"`
	Command  []string          `json:"command,omitempty" jsonschema:"optional command override for the container"`
	Replicas int32             `json:"replicas" jsonschema:"desired number of replicas"`
	Confirm  bool              `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed an operation a guardrail held for confirmation; do not self-confirm"`
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
		res, err := c.Deploy(ctx, in.App, client.DeployRequest{Image: in.Image, Env: in.Env, Command: in.Command, Replicas: in.Replicas, Confirm: in.Confirm})
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
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true ONLY after the user has explicitly confirmed exposing the app to the public internet; do not self-confirm"`
}

func exposeTool(c *client.Client) sdk.ToolHandlerFor[exposeInput, client.ExposeResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in exposeInput) (*sdk.CallToolResult, client.ExposeResult, error) {
		res, err := c.Expose(ctx, in.App, in.Host, in.Port, in.Confirm)
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
