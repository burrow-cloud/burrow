// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/connect"
)

// kubeconfigTransport reaches the in-cluster control plane through the Kubernetes API-server
// service proxy, authenticated by the developer's kubeconfig, with the burrowd API token read
// from the install Secret (ADR-0014). connect.Client puts the X-Burrow-Token RoundTripper into
// the proxy http.Client, so requests carry the token on the wire (ADR-0015). It is the default
// open-source transport (ADR-0045).
type kubeconfigTransport struct {
	opts connect.Options
}

// Connect resolves the token from the install Secret and returns a proxy-routed client.
func (t kubeconfigTransport) Connect(ctx context.Context) (*client.Client, error) {
	return connect.Client(ctx, t.opts)
}

// directTransport talks to a control-plane URL directly (e.g. an ingress) with an API token,
// selected by --control-plane/--token (or BURROW_CONTROL_PLANE_URL/BURROW_API_TOKEN). client.NewClient
// wires the X-Burrow-Token RoundTripper, so the direct path sends the same header as the proxy
// path (ADR-0015, ADR-0045).
type directTransport struct {
	baseURL string
	token   string
}

// Connect returns a client for the configured URL and token.
func (t directTransport) Connect(_ context.Context) (*client.Client, error) {
	return client.NewClient(t.baseURL, t.token), nil
}
