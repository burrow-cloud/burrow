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
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("control plane: %s (%s, http %d)", e.Message, e.Code, e.StatusCode)
	}
	return fmt.Sprintf("control plane: %s (http %d)", e.Message, e.StatusCode)
}

// The DTOs below mirror the control-plane API's JSON shapes (snake_case).

type DeployRequest struct {
	Image    string            `json:"image"`
	Env      map[string]string `json:"env,omitempty"`
	Command  []string          `json:"command,omitempty"`
	Replicas int32             `json:"replicas"`
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

type LogLine struct {
	Pod       string    `json:"pod"`
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
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

func (c *Client) Scale(ctx context.Context, app string, replicas int32) (ScaleResult, error) {
	var out ScaleResult
	err := c.do(ctx, http.MethodPost, c.appPath(app, "scale"), map[string]int32{"replicas": replicas}, &out)
	return out, err
}

func (c *Client) appPath(app, verb string) string {
	return "/v1/apps/" + url.PathEscape(app) + "/" + verb
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
	// Send the token both ways: Authorization for the direct/ingress path, and
	// X-Burrow-Token for the Kubernetes API-server proxy path, where the kubeconfig
	// transport owns the Authorization header (ADR-0014).
	req.Header.Set("Authorization", "Bearer "+c.token)
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
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Error
		if msg == "" {
			if msg = strings.TrimSpace(string(data)); msg == "" {
				msg = resp.Status
			}
		}
		return &APIError{StatusCode: resp.StatusCode, Code: e.Code, Message: msg}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}
