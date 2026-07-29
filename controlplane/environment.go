// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultEnvironment is the name of the environment install creates: `prod`, mapped to the app
// namespace burrowd runs against (BURROW_NAMESPACE). It is the environment an operation that names
// none resolves to, and — since a fresh install has exactly one — the only one a single-environment
// self-hoster ever sees (ADR-0067 §2–§3, superseding ADR-0035 phase 2's implicit `default`).
//
// THE NAME IS A GUARDRAIL DECISION, NOT NAMING TASTE. ADR-0065 §3 makes app.delete and dns.delete
// deny-by-default and expects the operator to relax them per environment — allow in development,
// confirm in staging, deny in production. That gradient needs an environment whose name says what it
// is. An environment called `default` invites `guard set --env default app.delete allow` as the
// obvious way to make the friction stop, and the operator has then relaxed PRODUCTION without ever
// typing the word. `prod` makes the same command read as what it is. A self-hoster's single
// environment IS production: the absence of a staging environment does not make it not production
// (ADR-0067 §2).
//
// The NAME and the NAMESPACE are separate values, which is what keeps an install predating this from
// migrating: it gains an environment named `prod` pointing at the namespace its apps are already in
// (`burrow-apps`), not at `burrow-apps-prod`, and nothing moves (ADR-0067 §3). Resource names follow
// the same rule one level down — AddonInstanceName gives the DEFAULT environment the unqualified
// name (`burrow-postgres`) by switching on THIS CONSTANT rather than on its value, so changing the
// value from `default` to `prod` renamed no instance, no volume, and no Secret.
//
// It is reserved: `burrow env add prod` is rejected because install already created it.
const DefaultEnvironment = "prod"

// retiredDefaultEnvironment is the name the first environment carried before ADR-0067 §2 — a
// synthesized `default` that was never registered. It survives only as a reserved word: migration
// 00018 rewrote the stored name to DefaultEnvironment everywhere, and re-admitting `default` as a
// user-chosen environment would resurrect exactly the "relax production without typing the word"
// confusion the rename removed. Nothing resolves through it.
const retiredDefaultEnvironment = "default"

// Environment is a named app environment for namespace-per-environment (ADR-0035 phase 2): one
// cluster, several app namespaces, one per environment. Name is the operator-facing handle (a
// DNS-1123 label); Namespace is the Kubernetes namespace the environment's apps deploy into.
// Default marks the environment install created (`prod`), the one an operation naming none resolves
// to (ADR-0067 §2).
type Environment struct {
	// Name is the environment handle, a DNS-1123 label (e.g. "prod", "staging").
	Name string `json:"name"`
	// Namespace is the Kubernetes namespace this environment's apps deploy into.
	Namespace string `json:"namespace"`
	// Default reports whether this is the default environment — `prod`, created at install and
	// mapped to the app namespace burrowd runs against (ADR-0067 §2–§3). Environments added later
	// are never default.
	Default bool `json:"default"`
	// CreatedAt is when the environment was registered, read from the injected clock. It is the
	// zero time when the default environment is synthesized because its registry row is missing.
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// AmbiguousEnvironmentError reports that a mutating operation arrived with no environment named while
// more than one environment is registered, so burrowd refuses to pick one rather than silently
// defaulting to `prod`, the environment install created (ADR-0047 §1). It is a structured outcome,
// not a system failure: the request was understood, but its target is ambiguous — an unanswered
// question, not a held operation. It lists the registered environments (`prod` first, then the ones
// added later) with their namespaces so the caller re-issues the operation naming an explicit
// environment, rather than letting a state change land on the default by accident. The check is on
// registration, not reachability: ambiguity is a static fact about how many environments exist, with
// no network probe (ADR-0047 §1). Callers distinguish it with AsAmbiguousEnvironment; the HTTP API
// maps it to a 4xx with the machine-readable "ambiguous_environment" code.
type AmbiguousEnvironmentError struct {
	// Environments are the registered environments the caller must choose among (`prod` first,
	// then the ones added later in name order, as ListEnvironments returns them).
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
// it, mirroring AsGuardrail and AsMissingPrerequisites so a front end (the HTTP API, and through it
// the `burrow` CLI and `burrow-agent`) can surface the structured refusal without parsing prose.
func AsAmbiguousEnvironment(err error) (*AmbiguousEnvironmentError, bool) {
	var a *AmbiguousEnvironmentError
	if errors.As(err, &a) {
		return a, true
	}
	return nil, false
}

// validateEnvironmentName reports whether name is a usable environment handle for `burrow env add`:
// a non-empty, DNS-1123-label-safe lowercase token that is neither `prod` (install already created
// it — ADR-0067 §2) nor the retired `default` (the name `prod` replaced). It mirrors the app-name
// validation so an environment name is always a valid Kubernetes namespace component.
func validateEnvironmentName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("environment name is empty")
	case name == DefaultEnvironment:
		return fmt.Errorf("environment %q already exists: install creates it, mapped to the app namespace (ADR-0067 §2)", name)
	case name == retiredDefaultEnvironment:
		return fmt.Errorf("environment name %q is retired: the environment it named is now called %q (ADR-0067 §2)", name, DefaultEnvironment)
	case len(name) > maxNameLen:
		return fmt.Errorf("environment name %q is longer than %d characters", name, maxNameLen)
	case !dns1123Label.MatchString(name):
		return fmt.Errorf("environment name %q is not a valid DNS-1123 label", name)
	}
	return nil
}
