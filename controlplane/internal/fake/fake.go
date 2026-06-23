// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

// Package fake provides in-memory implementations of the control-plane seams
// (controlplane.Clock, Kubernetes, Registry, Database) for tests. They are
// deterministic and controllable: time is explicit, state is inspectable, and errors
// can be injected per operation so engine logic can be exercised against absence and
// failure without a real cluster, registry, or database (ADR-0010).
//
// The fakes are concurrency-safe so later fault-injection tests can drive them from
// multiple goroutines under the race detector. This package is source-available under
// FSL-1.1-ALv2 (see LICENSING.md and ADR-0001); it lives under controlplane/internal
// so it is importable only by the control plane it supports.
package fake

import "github.com/burrow-cloud/burrow/controlplane"

// Op names a seam method for error injection. Pass one to a fake's SetError to make
// that method return the given error until it is cleared (SetError(op, nil)).
type Op string

const (
	OpApply         Op = "ApplyDeployment"
	OpStatus        Op = "DeploymentStatus"
	OpScale         Op = "ScaleDeployment"
	OpLogs          Op = "Logs"
	OpDelete        Op = "DeleteDeployment"
	OpResolve       Op = "Resolve"
	OpSaveRelease   Op = "SaveRelease"
	OpRelease       Op = "Release"
	OpLatestRelease Op = "LatestRelease"
	OpReleases      Op = "Releases"
)

// cloneRelease returns a deep copy of r so a fake never aliases a caller's Env or
// Command slices/maps — matching a real database, which serializes its records.
func cloneRelease(r controlplane.Release) controlplane.Release {
	if r.Env != nil {
		m := make(map[string]string, len(r.Env))
		for k, v := range r.Env {
			m[k] = v
		}
		r.Env = m
	}
	if r.Command != nil {
		c := make([]string, len(r.Command))
		copy(c, r.Command)
		r.Command = c
	}
	return r
}
