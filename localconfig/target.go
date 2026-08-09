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

// The managed product is NAMED by one host and ADDRESSED at another, and the two constants below
// exist because collapsing them into one is what broke sign-in: the apex serves the marketing
// website and answers every API path with a 404, so a device flow aimed at it can never start.
//
// CloudEndpoint is the IDENTITY. It is the target's Name, the `endpoint` field written into
// ~/.burrow/config, and the credential filename (internal/cloudcred.File). It is keyed to the
// product rather than to wherever its API happens to be served, so moving the API does not strand
// the credentials and targets already on disk — which is why this value must not change.
//
// CloudAPIEndpoint is the ADDRESS. It is the console: the host the managed control plane actually
// answers on, and the only one anything connects to or points a person's browser at. It is derived
// from CloudEndpoint so the two cannot drift apart.
//
// The rule for choosing between them: prose that NAMES the product takes CloudEndpoint; anything
// somebody or something CONNECTS TO takes CloudAPIEndpoint.
const (
	CloudEndpoint    = "burrow-cloud.dev"
	CloudAPIEndpoint = "console." + CloudEndpoint
)

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
	// this field existed has none, `burrow auth login` learns one only when it can claim the install
	// it reached (ADR-0084 §1) and records none otherwise, and a target with no id is served exactly
	// as it was — the check is a refinement of an existing relationship, not a new precondition on it.
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
// carries none, and one written by `burrow auth login` carries none whenever that sign-in could not
// claim the install it reached. Requiring it here would turn every config already on disk into a
// load error — the one outcome the whole design is arranged to avoid.
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

// targetsContext reports whether any recorded Kubernetes target already reaches a kube context. It
// asks by CONTEXT rather than by name because that is the question — whether this cluster is already
// registered — and a target's name is only conventionally the context it names, so a hand-edited or
// renamed entry would otherwise be duplicated rather than recognised.
func (c *Config) targetsContext(context string) bool {
	for _, t := range c.Targets {
		if t.Kind == TargetKindKubernetes && t.Context == context {
			return true
		}
	}
	return false
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
// Replacing the entry wholesale CLEARS any install id the OLD entry carried, and that is correct
// rather than an oversight. Re-pointing at a context is exactly what somebody does after rebuilding
// the cluster behind it, so carrying the old id forward would preserve a mismatch through the act
// meant to resolve it. What survives is whatever t itself carries: `burrow auth login` sets the id
// on the target it passes here when its sign-in learned one from the install that answered
// (ADR-0084 §1), and passes none when it did not — leaving the target unchecked until an install or
// a join records one, which is the state every target was in before ids existed, and is served.
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

// RegisterTarget records a target WITHOUT making it active, and reports whether it was new. The
// caller Saves.
//
// It is the other half of SetTarget, and the distinction is the point. `burrow auth login` is a
// person saying "point me here", so it selects. An install is a person putting a control plane into
// a cluster, which says nothing about where their next command should go — they may well have
// deliberately selected something else, and re-pointing them would be the same silent redirection
// this whole area exists to stop, just aimed the other way.
//
// Registering is what was missing. Nothing an install did wrote a target, so a self-hosted person
// had none: signing in to the managed product then left them with exactly one target and `burrow
// auth switch` listing nothing to go back to
// ([cloud#201](https://github.com/burrow-cloud/cloud/issues/201)). An install now leaves a target
// behind, so the way back exists before it is needed.
//
// A target already recorded under this name is left exactly as it is, and false is returned. That
// keeps a re-run install idempotent and, more importantly, preserves whatever the existing entry
// carries — an install id recorded by a previous run is a fact about the cluster, and quietly
// discarding it here would re-open the mismatch ADR-0084 §5 exists to catch.
func (c *Config) RegisterTarget(t Target) (bool, error) {
	if err := t.validate(); err != nil {
		return false, fmt.Errorf("localconfig: %w", err)
	}
	if _, ok := c.LookupTarget(t.Name); ok {
		return false, nil
	}
	c.Targets = append(c.Targets, t)
	return true, nil
}

// registerEnvironmentTargets derives a Kubernetes target for every registered environment handle
// whose kube context no target names yet. It never touches CurrentTarget.
//
// This holds the model's invariant rather than migrating anything: a target is a cluster, an
// environment is one of possibly several environments UNDER a target, so an environment with no
// target above it is not an old shape to reconcile — it is a config that does not typecheck. Deriving
// the missing target makes the state unrepresentable instead of leaving every reader to cope with
// it, which is what produced cloud#201: `burrow auth status` listed targets, the person's own
// install was not one, and the recovery command the CLI printed named something that did not exist.
//
// It runs on load and writes nothing by itself. Nothing is selected, so a person who has never run
// `burrow auth login` still follows their kubeconfig exactly as before; what changes is that their
// cluster is now something `burrow auth status` can show and `burrow auth switch` can reach.
func (c *Config) registerEnvironmentTargets() {
	for _, e := range c.Environments {
		if e.Context == "" {
			continue
		}
		if c.targetsContext(e.Context) {
			continue
		}
		target := KubernetesTarget(e.Context)
		if _, ok := c.LookupTarget(target.Name); ok {
			continue
		}
		// The handle knows which install is behind the context (ADR-0084 §5) and the derived target
		// reaches the same one, so the check applies to it from the moment it exists rather than
		// waiting for the next install to record it a second time.
		target.InstallID = e.InstallID
		c.Targets = append(c.Targets, target)
	}
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
