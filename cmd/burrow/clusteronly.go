// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"

	"github.com/burrow-cloud/burrow/localconfig"
)

// Some commands can only reach a cluster. They connect with the raw --context/--namespace, or with a
// kubeconfig clientset, and never consult the selected target (ADR-0078 §4 leaves open whether they
// should follow one; issue #429 decides that separately). While a cluster was the only kind of
// target, that cost nothing. Now that a Burrow Cloud target can be operated through, it is a trap: someone
// signs in to the managed product, deploys through it, then runs `burrow guard set app.delete deny`
// and it lands on whatever `kubectl config current-context` happens to be — a cluster they may not
// have thought about in a week, with no error and no clue.
//
// So those commands refuse while the managed product is selected. Refusing is not the answer to
// "which cluster should they use" — that question is still open — but it is the answer to "may they
// silently use one nobody chose", and that one is not.
//
// Deliberately NOT refused:
//
//   - Installing and the rest of the cluster-lifecycle surface (`cluster install`, `cluster upgrade`,
//     `cluster bootstrap`, `join`, `cluster ingress/registry/postgres install`). ADR-0078 §3: install
//     continues to act on a kubeconfig context, since installing into Burrow Cloud is not a thing
//     that can be asked for. They name the cluster they act on rather than inheriting one, and none
//     of them routes through the paths guarded here.
//   - `burrow auth ...`. It is how a person sees which target is active and changes it, so a refusal
//     there would leave someone with the managed product selected and no way to read or leave that
//     state. It reads and writes the local config only and touches no cluster.

// refuseCloudTarget reports why a cluster-only command cannot run, or nil when it can. It reads the
// local config and nothing else: no kubeconfig, no network, no credential.
//
// A config that will not load is not this function's error to raise. Every caller is about to load it
// again, or to open a connection that will fail on the same problem and say so far better; failing
// here would only replace a legible message with a vaguer one. Silence on that path also keeps the
// cluster path exactly as it was, which is the point — this changes what happens with the managed
// product selected and nothing else.
func refuseCloudTarget() error {
	cfg, err := localconfig.Load()
	if err != nil {
		return nil
	}
	target, selected, err := cfg.ActiveTarget()
	if err != nil || !selected || target.Kind != localconfig.TargetKindCloud {
		return nil
	}
	return errCloudTargetHasNoCluster(target)
}

// errCloudTargetHasNoCluster names the active target, says plainly why this command cannot use it,
// and gives the one command that changes the answer. It is worded to match the refusal the
// kubeconfig-shaped flags already produce (commonOpts.cloudTarget), because it is the same fact:
// the managed product has no cluster of its own.
func errCloudTargetHasNoCluster(target localconfig.Target) error {
	return fmt.Errorf(
		"this command reaches a cluster, and the active target %q is %s, which has no cluster of its own. Switch to a cluster target with \"burrow auth switch <name>\" (see \"burrow auth status\")",
		target.Name, target.Describe())
}
