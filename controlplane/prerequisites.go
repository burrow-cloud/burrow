// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"errors"
	"fmt"
	"strings"
)

// Prerequisite names one cluster prerequisite a public, optionally TLS-terminated exposure needs
// but which is missing, paired with the exact burrow command that provisions it (ADR-0006,
// ADR-0034). It is the structured unit an agent reads to fix the cluster's setup without inspecting
// it directly with kubectl.
type Prerequisite struct {
	// Name is the missing piece (e.g. "ingress controller", "cert-manager", "DNS provider").
	Name string `json:"name"`
	// Detail says what is missing and why the exposure needs it.
	Detail string `json:"detail"`
	// Fix is the burrow command that provisions it.
	Fix string `json:"fix"`
}

// MissingPrerequisitesError reports that an expose request cannot be satisfied because the cluster
// is not set up for the public, optionally TLS-terminated reachability it asked for. It is a
// structured outcome, not a system failure: the request was understood, but the cluster lacks one
// or more prerequisites. It enumerates each missing prerequisite and the burrow command that
// resolves it, so an agent can guide the user back onto Burrow's path rather than falling back to
// raw kubectl to diagnose the cluster (ADR-0006). Callers distinguish it with AsMissingPrerequisites.
type MissingPrerequisitesError struct {
	// Host is the hostname the exposure targeted.
	Host string
	// TLS reports whether the exposure asked for HTTPS (so TLS prerequisites applied).
	TLS bool
	// Missing enumerates the prerequisites that are absent, each with its fix.
	Missing []Prerequisite
}

func (e *MissingPrerequisitesError) Error() string {
	var b strings.Builder
	b.WriteString("the cluster is not set up for ")
	if e.TLS {
		b.WriteString("public HTTPS")
	} else {
		b.WriteString("public reachability")
	}
	if e.Host != "" {
		fmt.Fprintf(&b, " at %s", e.Host)
	}
	b.WriteString("; missing prerequisites:")
	for _, p := range e.Missing {
		fmt.Fprintf(&b, "\n  - %s: %s; %s", p.Name, p.Detail, p.Fix)
	}
	return b.String()
}

// AsMissingPrerequisites reports whether err is (or wraps) a MissingPrerequisitesError and returns
// it, mirroring AsGuardrail so a front end (the HTTP API, the MCP server) can surface the structured
// checklist without parsing prose.
func AsMissingPrerequisites(err error) (*MissingPrerequisitesError, bool) {
	var m *MissingPrerequisitesError
	if errors.As(err, &m) {
		return m, true
	}
	return nil, false
}
