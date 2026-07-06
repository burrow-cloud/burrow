// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"errors"
	"fmt"
	"strings"
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

// AmbiguousEnvironmentError reports that a mutating operation arrived with no environment named while
// more than one environment is registered, so burrowd refuses to pick one rather than silently
// defaulting to the implicit `default` environment (ADR-0047 §1). It is a structured outcome, not a
// system failure: the request was understood, but its target is ambiguous — an unanswered question,
// not a held operation. It lists the registered environments (the implicit default first, then the
// named ones) with their namespaces so the caller re-issues the operation naming an explicit
// environment, rather than letting a state change land on the default by accident. The check is on
// registration, not reachability: ambiguity is a static fact about how many environments exist, with
// no network probe (ADR-0047 §1). Callers distinguish it with AsAmbiguousEnvironment; the HTTP API
// maps it to a 4xx with the machine-readable "ambiguous_environment" code.
type AmbiguousEnvironmentError struct {
	// Environments are the registered environments the caller must choose among (the implicit
	// `default` first, then the named ones in name order, as ListEnvironments returns them).
	Environments []Environment
}

func (e *AmbiguousEnvironmentError) Error() string {
	listed := make([]string, 0, len(e.Environments))
	example := ""
	for _, env := range e.Environments {
		listed = append(listed, fmt.Sprintf("%s (namespace %s)", env.Name, env.Namespace))
		if example == "" && !env.Default {
			example = env.Name
		}
	}
	if example == "" && len(e.Environments) > 0 {
		example = e.Environments[0].Name
	}
	return fmt.Sprintf(
		"this operation changes state and more than one environment is registered — %s. Name the target environment (e.g. env: %s); Burrow will not choose an environment for a mutating operation.",
		strings.Join(listed, ", "), example)
}

// AsAmbiguousEnvironment reports whether err is (or wraps) an AmbiguousEnvironmentError and returns
// it, mirroring AsGuardrail and AsMissingPrerequisites so a front end (the HTTP API, the MCP server)
// can surface the structured refusal without parsing prose.
func AsAmbiguousEnvironment(err error) (*AmbiguousEnvironmentError, bool) {
	var a *AmbiguousEnvironmentError
	if errors.As(err, &a) {
		return a, true
	}
	return nil, false
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
