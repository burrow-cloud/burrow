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

// dnsProviderTypesList returns the supported provider types that can serve DNS as a
// comma-separated list for user-facing guidance, sourced from the in-process provider registry
// (ADR-0023) so it stays in step with the vendors Burrow actually supports.
func dnsProviderTypesList() string {
	var names []string
	for _, t := range SupportedProviderTypes() {
		for _, c := range t.Capabilities() {
			if c == CapabilityDNS {
				names = append(names, string(t))
				break
			}
		}
	}
	return strings.Join(names, ", ")
}

// missingDNSProviderPrerequisite builds the DNS-provider prerequisite for host: no provider is
// configured to automate the record. It names the supported provider types so the agent can ask
// the user which vendor hosts the domain and offer to configure a supported one, instead of
// defaulting to a manual registrar step; it still falls back to pointing DNS by hand when the
// vendor is unsupported (ADR-0018, ADR-0023). This string surfaces to users, so it stays plain
// (no em-dashes).
func missingDNSProviderPrerequisite(host string) Prerequisite {
	return Prerequisite{
		Name:   "DNS provider",
		Detail: "no DNS provider is configured; Burrow can automate the record for a supported provider (" + dnsProviderTypesList() + ")",
		Fix:    fmt.Sprintf("ask the user which hosts %s, then have them run \"burrow config provider add <type>\". If their provider is not supported, point %s at the ingress address manually.", host, host),
	}
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
