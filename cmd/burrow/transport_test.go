// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"testing"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/connect"
)

// TestTransportSelectionDirectURL confirms that --control-plane (with --token) selects the
// direct-URL transport carrying that URL and token, not the kubeconfig proxy path (ADR-0045).
func TestTransportSelectionDirectURL(t *testing.T) {
	o := &commonOpts{controlPlane: "https://cp.example", token: "tok"}
	tr, err := o.transport(target{})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	dt, ok := tr.(client.DirectTransport)
	if !ok {
		t.Fatalf("transport = %T, want client.DirectTransport", tr)
	}
	if dt.BaseURL != "https://cp.example" || dt.Token != "tok" {
		t.Errorf("DirectTransport = %+v, want the --control-plane URL and --token", dt)
	}
}

// TestTransportSelectionDirectURLRequiresToken confirms --control-plane without a token is an
// error rather than a silent unauthenticated connection.
func TestTransportSelectionDirectURLRequiresToken(t *testing.T) {
	o := &commonOpts{controlPlane: "https://cp.example"}
	if _, err := o.transport(target{}); err == nil {
		t.Fatal("transport with --control-plane and no --token should error")
	}
}

// TestTransportSelectionKubeconfig confirms that without --control-plane the default kubeconfig
// API-server proxy transport is selected, carrying the connect.Options connectOptions resolves
// for the target (namespace and any scoped credential) — ADR-0045.
func TestTransportSelectionKubeconfig(t *testing.T) {
	o := &commonOpts{}
	tgt := target{context: "prod", controlPlaneNamespace: "burrow"}
	tr, err := o.transport(tgt)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	kt, ok := tr.(connect.KubeconfigTransport)
	if !ok {
		t.Fatalf("transport = %T, want connect.KubeconfigTransport", tr)
	}
	if kt.Options.Context != "prod" || kt.Options.Namespace != "burrow" {
		t.Errorf("kubeconfig opts = %+v, want context prod namespace burrow", kt.Options)
	}
}
