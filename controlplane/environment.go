// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"fmt"
	"time"
)

// DefaultEnvironment is the name of the implicit environment that always exists: the app namespace
// burrowd already runs against (BURROW_NAMESPACE), behaving exactly like before environments were
// introduced (ADR-0035 phase 2). It is reserved — it cannot be created with `burrow env add`,
// because it is synthesized rather than stored — and it is always listed first.
const DefaultEnvironment = "default"

// Environment is a named app environment for namespace-per-environment (ADR-0035 phase 2): one
// cluster, several app namespaces, one per environment. Name is the operator-facing handle (a
// DNS-1123 label); Namespace is the Kubernetes namespace the environment's apps deploy into.
// Default marks the implicit `default` environment, which is synthesized from the engine's app
// namespace rather than registered.
type Environment struct {
	// Name is the environment handle, a DNS-1123 label (e.g. "staging", "prod").
	Name string `json:"name"`
	// Namespace is the Kubernetes namespace this environment's apps deploy into.
	Namespace string `json:"namespace"`
	// Default reports whether this is the implicit `default` environment (the app namespace
	// burrowd already runs against). Registered environments are never default.
	Default bool `json:"default"`
	// CreatedAt is when the environment was registered, read from the injected clock. It is the
	// zero time for the synthesized default environment.
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// validateEnvironmentName reports whether name is a usable environment handle: a non-empty,
// DNS-1123-label-safe lowercase token that is not the reserved `default` (which is implicit and
// cannot be created). It mirrors the app-name validation so an environment name is always a valid
// Kubernetes namespace component.
func validateEnvironmentName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("environment name is empty")
	case name == DefaultEnvironment:
		return fmt.Errorf("environment name %q is reserved (the implicit default environment)", name)
	case len(name) > maxNameLen:
		return fmt.Errorf("environment name %q is longer than %d characters", name, maxNameLen)
	case !dns1123Label.MatchString(name):
		return fmt.Errorf("environment name %q is not a valid DNS-1123 label", name)
	}
	return nil
}
