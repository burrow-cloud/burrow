// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package localconfig

import (
	"fmt"
	"sort"
)

// A target is where the control plane is (ADR-0078 §1). There are two kinds and deliberately not
// three: the managed product, or a Kubernetes cluster the person holds a kubeconfig context for. A
// managed control plane operated on somebody else's cluster is a shape the roadmap keeps open and is
// not modelled here, because inventing a kind for it now would be inventing the product it is a
// target for.
const (
	// TargetKindCloud is the managed product. The credential is a token obtained by signing in, and
	// it is never written here (cloud ADR-0028 owns it).
	TargetKindCloud TargetKind = "burrow-cloud"
	// TargetKindKubernetes is a cluster with Burrow installed in it. The credential is the
	// kubeconfig the person already has, used exactly as ADR-0014 already uses it.
	TargetKindKubernetes TargetKind = "kubernetes"
)

// CloudEndpoint is the managed product's host, and the name its target carries.
const CloudEndpoint = "burrow-cloud.dev"

// TargetKind distinguishes the two kinds of target (ADR-0078 §1).
type TargetKind string

// Target is a control plane the CLI can point at, recorded in ~/.burrow/config.
//
// A Kubernetes target stores the kubeconfig context NAME and NEVER a copy of the credential
// (ADR-0078 §1). The kubeconfig stays the single source of truth, so rotating it, re-issuing a
// certificate, or having a cloud provider's CLI manage it all keep working with nothing here going
// stale. A copied credential is a credential nobody remembers to rotate.
//
// A Burrow Cloud target likewise stores no token: only the endpoint it signs in to. Where the token
// lives is cloud ADR-0028's decision, not this file's.
type Target struct {
	Name     string     `yaml:"name"`
	Kind     TargetKind `yaml:"kind"`
	Context  string     `yaml:"context,omitempty"`  // Kubernetes only: the kubeconfig context name
	Endpoint string     `yaml:"endpoint,omitempty"` // Burrow Cloud only: the host signed in to
	// InstallID is the id of the Burrow install this target was pointed at (ADR-0084 §5). The context
	// name says HOW TO GET THERE; the id says WHETHER YOU ARRIVED, and the two answer different
	// questions because a context name is a label rather than an identity. It is user-controlled, it
	// is reusable, and providers generate it deterministically — `doctl kubernetes cluster kubeconfig
	// save` writes names like `do-nyc3-burrow`, so destroying a cluster and standing another one up
	// produces a byte-identical context name for an entirely different cluster. With only the name
	// recorded, the target follows it there and says nothing.
	//
	// It is not a secret and authorises nothing: it identifies an install so a mismatch can be named.
	// The CLI sends it on every request and the control plane refuses a request that names an install
	// it is not.
	//
	// It is optional, and validate deliberately does not require it. Every target recorded before
	// this field existed has none, targets are written by `burrow auth login`, which contacts no
	// cluster and so has nothing to learn an id from, and a target with no id is served exactly as it
	// was — the check is a refinement of an existing relationship, not a new precondition on it.
	InstallID string `yaml:"install_id,omitempty"`
}

// KubernetesTarget builds a target for a kubeconfig context, named after the context so a person
// selects a cluster by a name they already recognise.
func KubernetesTarget(context string) Target {
	return Target{Name: context, Kind: TargetKindKubernetes, Context: context}
}

// CloudTarget builds the target for the managed product.
func CloudTarget() Target {
	return Target{Name: CloudEndpoint, Kind: TargetKindCloud, Endpoint: CloudEndpoint}
}

// Describe renders what a target IS, for `burrow auth status` and for any message that has to name
// one.
func (t Target) Describe() string {
	switch t.Kind {
	case TargetKindCloud:
		return "the managed product at " + t.Endpoint
	case TargetKindKubernetes:
		return fmt.Sprintf("kube context %q", t.Context)
	default:
		return fmt.Sprintf("unknown target kind %q", t.Kind)
	}
}

// validate rejects a target the CLI cannot act on. It runs on every load, so a hand-edited or
// half-written entry produces a legible error naming the target and what is wrong with it, rather
// than a confusing failure several commands later (ADR-0078 "Consequences").
//
// InstallID is not checked. It is additive (ADR-0084 §5): every target written before it existed
// carries none, and one written by `burrow auth login` carries none either, because that command
// contacts no cluster and so has nothing to read an id from. Requiring it here would turn every
// config already on disk into a load error — the one outcome the whole design is arranged to avoid.
func (t Target) validate() error {
	if t.Name == "" {
		return fmt.Errorf("a target is missing its name")
	}
	switch t.Kind {
	case TargetKindKubernetes:
		if t.Context == "" {
			return fmt.Errorf("target %q is a %s target but names no kube context", t.Name, TargetKindKubernetes)
		}
	case TargetKindCloud:
		if t.Endpoint == "" {
			return fmt.Errorf("target %q is a %s target but names no endpoint", t.Name, TargetKindCloud)
		}
	case "":
		return fmt.Errorf("target %q is missing its kind (want %q or %q)", t.Name, TargetKindCloud, TargetKindKubernetes)
	default:
		return fmt.Errorf("target %q has unknown kind %q (want %q or %q)", t.Name, t.Kind, TargetKindCloud, TargetKindKubernetes)
	}
	return nil
}

// validateTargets checks every recorded target and that the active one is actually registered, so
// the whole targeting block is either coherent or reported in one legible error.
func (c *Config) validateTargets() error {
	seen := make(map[string]bool, len(c.Targets))
	for _, t := range c.Targets {
		if err := t.validate(); err != nil {
			return err
		}
		if seen[t.Name] {
			return fmt.Errorf("target %q is listed twice", t.Name)
		}
		seen[t.Name] = true
	}
	if c.CurrentTarget != "" && !seen[c.CurrentTarget] {
		return fmt.Errorf("the active target %q is not in the targets list (have: %s); choose one with \"burrow auth switch <name>\" or add one with \"burrow auth login\"",
			c.CurrentTarget, c.TargetNames())
	}
	return nil
}

// TargetNames returns the registered target names, sorted, joined for an error or a hint. It reads
// "none" when there are no targets, so a message never trails off into an empty list.
func (c *Config) TargetNames() string {
	if len(c.Targets) == 0 {
		return "none"
	}
	names := make([]string, 0, len(c.Targets))
	for _, t := range c.Targets {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

// LookupTarget returns the target with the given name, and whether it was found.
func (c *Config) LookupTarget(name string) (Target, bool) {
	for _, t := range c.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}

// ActiveTarget returns the target commands act against, and whether one is selected at all. No
// target selected is the pre-ADR-0078 world and is not an error: the CLI then behaves exactly as it
// did, following the kubeconfig. A CurrentTarget naming an unregistered target is an error, and is
// caught on load by validateTargets.
func (c *Config) ActiveTarget() (Target, bool, error) {
	if c == nil || c.CurrentTarget == "" {
		return Target{}, false, nil
	}
	t, ok := c.LookupTarget(c.CurrentTarget)
	if !ok {
		return Target{}, false, fmt.Errorf("localconfig: the active target %q is not in the targets list (have: %s); choose one with \"burrow auth switch <name>\"",
			c.CurrentTarget, c.TargetNames())
	}
	return t, true, nil
}

// SetTarget records a target and makes it active, replacing any existing entry with the same name
// (re-authenticating against a target you already have is an ordinary thing to do). The caller
// Saves.
//
// Replacing the entry wholesale CLEARS any install id it carried, and that is correct rather than an
// oversight. Re-pointing at a context is exactly what somebody does after rebuilding the cluster
// behind it, so carrying the old id forward would preserve a mismatch through the act meant to
// resolve it. This command contacts no cluster and cannot learn the new id, so it leaves the target
// unchecked until an install or a join records one — unchecked is the state every target was in
// before ids existed, and it is served.
func (c *Config) SetTarget(t Target) error {
	if err := t.validate(); err != nil {
		return fmt.Errorf("localconfig: %w", err)
	}
	for i := range c.Targets {
		if c.Targets[i].Name == t.Name {
			c.Targets[i] = t
			c.CurrentTarget = t.Name
			return nil
		}
	}
	c.Targets = append(c.Targets, t)
	c.CurrentTarget = t.Name
	return nil
}

// SetInstallID records the id of the Burrow install reachable through a kubeconfig context on every
// local record that names that context — Kubernetes targets and environment handles alike — and
// reports whether anything was updated. The caller Saves.
//
// Both, because the two are read by different callers and each one alone leaves a hole: the `burrow`
// CLI resolves through a target, and `burrow-agent` resolves through a handle. Recording only on the
// target would check the operator and exempt the agent, which is the thing actually deploying.
//
// It is addressed by CONTEXT rather than by name because that is what the writer knows. `burrow
// cluster install <context>`, its join path, and `burrow cluster upgrade` are told a context and
// learn an id from the cluster they just reached; which target or handle happens to point at that
// context is this file's business, not theirs. Nothing registered for the context is an ordinary
// outcome, not an error: installing does not require having run `burrow auth login` first, and an id
// with nowhere to be recorded is simply not recorded (ADR-0084 §5).
//
// More than one record may name the same context. All of them are updated, because they all reach
// the same install, and leaving one behind would make the mismatch fire on which record happened to
// be selected rather than on the cluster having actually changed.
func (c *Config) SetInstallID(context, installID string) bool {
	if context == "" || installID == "" {
		return false
	}
	updated := false
	for i := range c.Targets {
		if c.Targets[i].Kind == TargetKindKubernetes && c.Targets[i].Context == context {
			c.Targets[i].InstallID = installID
			updated = true
		}
	}
	for i := range c.Environments {
		if c.Environments[i].Context == context {
			c.Environments[i].InstallID = installID
			updated = true
		}
	}
	return updated
}

// SwitchTarget makes an already-recorded target active without re-authenticating (ADR-0078 §4). It
// errors, naming what is registered, when the name is not one of them. The caller Saves.
func (c *Config) SwitchTarget(name string) error {
	if _, ok := c.LookupTarget(name); !ok {
		return fmt.Errorf("localconfig: %q is not a configured target (have: %s); see \"burrow auth status\"", name, c.TargetNames())
	}
	c.CurrentTarget = name
	return nil
}
