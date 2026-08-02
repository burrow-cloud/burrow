// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package localconfig

import "fmt"

// Cluster is which CLUSTER a privileged command acts on: the guardrail, cluster-setting, add-on,
// credential and audit commands that reach a cluster without being scoped to one application
// ([ADR-0084](../docs/adr/0084-everyone-who-uses-burrow-carries-their-own-token.md) §4).
//
// It is a third entry point beside Resolve and ResolveOperate because the question is a different
// one, and answering it with either of those would be wrong in a way that is hard to see. Those two
// resolve an ENVIRONMENT: they fold in a pinned handle, its namespaces, and the burrowd environment
// name to send with the operation. A privileged command is not scoped to an environment, and
// [ADR-0036](../docs/adr/0036-environment-selection.md) is deliberate that a pinned handle must
// never redirect one — pinning `staging` is a statement about which apps are being operated, not
// about which cluster is being administered. So this asks only "which cluster", and a pin is never
// consulted.
//
// Context is the kubeconfig context to connect to, and it is EMPTY when no target is selected. That
// is the pre-ADR-0078 world, and an empty context is how the caller keeps reaching it exactly as it
// did: through the kubeconfig's current context, chosen by whatever `kubectl config use-context` was
// last run on.
//
// Target names the selected target and is empty when none is, Kind is that target's kind, and
// Endpoint is set only for a Burrow Cloud target — which names no cluster at all, so its Context is
// empty for a reason the caller has to decide what to do about.
//
// InstallID is the install the target was pointed at (ADR-0084 §5), sent on every request so a
// control plane that turns out to be a different install refuses rather than acts. It travels with
// the context because the two answer the halves of one question: the context is how the request gets
// there, and the id is what says it arrived. It is empty when no target is selected, when the target
// predates install ids, or when it is the managed product.
type Cluster struct {
	Context   string
	Target    string
	Kind      TargetKind
	Endpoint  string
	InstallID string
}

// Selected reports whether a configured target decided this cluster. When it is false the caller is
// in the pre-ADR-0078 world and follows the kubeconfig, which stays the default.
func (c Cluster) Selected() bool { return c.Target != "" }

// Cloud reports whether the selected target is the managed product. It names no cluster, so a caller
// that can only act on one has to either refuse or say out loud which cluster it fell back to.
func (c Cluster) Cloud() bool { return c.Kind == TargetKindCloud }

// ResolveCluster decides which cluster a privileged command acts on, from the selected target alone.
//
// Three outcomes, and each is a different thing for the caller to do:
//
//   - No target selected. The zero Cluster, with no error: the caller follows the kubeconfig's
//     current context, exactly as every command did before targets existed. This is still the
//     default and nothing about it changes.
//   - A Kubernetes target. Its context name, checked against the kubeconfig first so a target that
//     has gone stale is caught here — where the message can name the target and the context — rather
//     than at connect time, where it surfaces as a kubeconfig error about a cluster the reader did
//     not know they were reaching for.
//   - A Burrow Cloud target. No context, and Cloud reports true. There is no cluster to name.
//
// A pinned environment handle is not consulted in any of the three; see Cluster.
func ResolveCluster(cfg *Config, kubeconfigPath string) (Cluster, error) {
	target, selected, err := cfg.ActiveTarget()
	if err != nil {
		return Cluster{}, err
	}
	if !selected {
		return Cluster{}, nil
	}
	switch target.Kind {
	case TargetKindCloud:
		return Cluster{Target: target.Name, Kind: target.Kind, Endpoint: target.Endpoint}, nil
	case TargetKindKubernetes:
	default:
		return Cluster{}, fmt.Errorf(
			"localconfig: the active target %q is %s, which this command reaches through a kubeconfig and cannot; switch to a cluster target with \"burrow auth switch <name>\" (see \"burrow auth status\")",
			target.Name, target.Describe())
	}
	if _, found, err := contextNamespace(kubeconfigPath, target.Context); err != nil {
		return Cluster{}, err
	} else if !found {
		return Cluster{}, errTargetContextMissing(target.Name, target.Context)
	}
	return Cluster{Context: target.Context, Target: target.Name, Kind: target.Kind, InstallID: target.InstallID}, nil
}
