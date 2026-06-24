// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"fmt"

	"github.com/burrow-cloud/burrow/controlplane"
)

// Until the real Kubernetes and registry adapters land (v0.1 Phase 5), burrowd wires
// these placeholders so it boots and serves the authenticated API with real persistence.
// Every cluster-touching operation returns controlplane.ErrNotImplemented, which the API
// reports honestly as 501 (ADR-0009) rather than pretending to succeed.

var (
	_ controlplane.Kubernetes = notImplementedKubernetes{}
	_ controlplane.Registry   = notImplementedRegistry{}
)

func errNotWired(what string) error {
	return fmt.Errorf("the %s adapter is not wired in this build (arrives in v0.1 Phase 5): %w", what, controlplane.ErrNotImplemented)
}

type notImplementedKubernetes struct{}

func (notImplementedKubernetes) ApplyWorkload(context.Context, controlplane.WorkloadSpec) error {
	return errNotWired("kubernetes")
}

func (notImplementedKubernetes) WorkloadStatus(context.Context, string) (controlplane.WorkloadStatus, error) {
	return controlplane.WorkloadStatus{}, errNotWired("kubernetes")
}

func (notImplementedKubernetes) ScaleWorkload(context.Context, string, int32) error {
	return errNotWired("kubernetes")
}

func (notImplementedKubernetes) Logs(context.Context, string, controlplane.LogOptions) ([]controlplane.LogLine, error) {
	return nil, errNotWired("kubernetes")
}

func (notImplementedKubernetes) DeleteWorkload(context.Context, string) error {
	return errNotWired("kubernetes")
}

type notImplementedRegistry struct{}

func (notImplementedRegistry) Resolve(context.Context, string) (controlplane.ImageInfo, error) {
	return controlplane.ImageInfo{}, errNotWired("registry")
}
