// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewServer builds the Burrow MCP server: an agent-neutral surface (ADR-0003) exposing
// the control plane's operations as MCP tools. Each tool translates a call into a
// control-plane API call via the client and returns the structured result; a control
// plane error becomes a tool error the agent can read. The server holds no cluster
// credentials (ADR-0005).
func NewServer(client *Client, version string) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "burrow", Title: "Burrow", Version: version}, nil)

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_deploy",
		Description: "Deploy an application to the cluster by container image reference. The image must already be pushed to a registry the cluster can pull from; only the reference and small metadata are sent, never code. Returns the new release and the release it superseded (the rollback handle).",
	}, deployTool(client))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_status",
		Description: "Report an application's status: its most recent release and the live workload state (desired/ready replicas, availability).",
	}, statusTool(client))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_logs",
		Description: "Return recent log lines for an application's workload.",
	}, logsTool(client))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_rollback",
		Description: "Roll an application back to its previously running release by redeploying that release's image reference. Returns the new release and which release it restored.",
	}, rollbackTool(client))

	sdk.AddTool(s, &sdk.Tool{
		Name:        "burrow_scale",
		Description: "Change an application's replica count. May be refused by policy (e.g. scaling to zero or above the replica ceiling).",
	}, scaleTool(client))

	return s
}

// Serve runs the Burrow MCP server over stdio until the client disconnects.
func Serve(ctx context.Context, client *Client, version string) error {
	return NewServer(client, version).Run(ctx, &sdk.StdioTransport{})
}

type deployInput struct {
	App      string            `json:"app" jsonschema:"the application name (a DNS-1123 label)"`
	Image    string            `json:"image" jsonschema:"the pullable container image reference to deploy, e.g. registry.example.com/app:1.2.3"`
	Env      map[string]string `json:"env,omitempty" jsonschema:"environment variables to set on the workload"`
	Command  []string          `json:"command,omitempty" jsonschema:"optional command override for the container"`
	Replicas int32             `json:"replicas" jsonschema:"desired number of replicas"`
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
}

func deployTool(c *Client) sdk.ToolHandlerFor[deployInput, DeployResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in deployInput) (*sdk.CallToolResult, DeployResult, error) {
		res, err := c.Deploy(ctx, in.App, DeployRequest{Image: in.Image, Env: in.Env, Command: in.Command, Replicas: in.Replicas})
		if err != nil {
			return nil, DeployResult{}, err
		}
		return nil, res, nil
	}
}

func statusTool(c *Client) sdk.ToolHandlerFor[appInput, StatusResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, StatusResult, error) {
		res, err := c.Status(ctx, in.App)
		if err != nil {
			return nil, StatusResult{}, err
		}
		return nil, res, nil
	}
}

// logsOutput wraps the lines so the tool has a structured object output.
type logsOutput struct {
	Lines []LogLine `json:"lines"`
}

func logsTool(c *Client) sdk.ToolHandlerFor[logsInput, logsOutput] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in logsInput) (*sdk.CallToolResult, logsOutput, error) {
		lines, err := c.Logs(ctx, in.App, in.Tail)
		if err != nil {
			return nil, logsOutput{}, err
		}
		return nil, logsOutput{Lines: lines}, nil
	}
}

func rollbackTool(c *Client) sdk.ToolHandlerFor[appInput, RollbackResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in appInput) (*sdk.CallToolResult, RollbackResult, error) {
		res, err := c.Rollback(ctx, in.App)
		if err != nil {
			return nil, RollbackResult{}, err
		}
		return nil, res, nil
	}
}

func scaleTool(c *Client) sdk.ToolHandlerFor[scaleInput, ScaleResult] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in scaleInput) (*sdk.CallToolResult, ScaleResult, error) {
		res, err := c.Scale(ctx, in.App, in.Replicas)
		if err != nil {
			return nil, ScaleResult{}, err
		}
		return nil, res, nil
	}
}
