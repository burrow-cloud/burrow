// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"

	"github.com/burrow-cloud/burrow/localconfig"
)

// Some commands can only reach a cluster: they connect through a kubeconfig, so a Burrow Cloud
// target — which has no cluster of its own — is not something they can act through. While a cluster
// was the only kind of target, that cost nothing. Once a Burrow Cloud target could be operated
// through, it became a trap: someone signs in to the managed product, deploys through it, then runs
// `burrow guard set app.delete deny` and it lands on whatever `kubectl config current-context`
// happens to be — a cluster they may not have thought about in a week, with no error and no clue.
//
// So those commands refuse while the managed product is selected.
//
// WHICH cluster they use when a CLUSTER target is selected is a separate question, and it is now
// answered: the target decides, for every one of them (ADR-0084 §4, clustercontext.go). This file
// covers only the target kind that names no cluster at all.
//
// Deliberately NOT refused:
//
//   - Installing and the rest of the cluster-lifecycle surface (`cluster install`, `cluster upgrade`,
//     `cluster bootstrap`, `join`, `cluster ingress/registry/postgres install`). ADR-0078 §3: install
//     continues to act on a kubeconfig context, since installing into Burrow Cloud is not a thing
//     that can be asked for. None of them routes through the paths guarded here, and none of them
//     follows the target.
//
//     How each names its cluster differs, and the difference is not tidy: `cluster install` takes a
//     positional `<context>`, `cluster bootstrap` acts on the k3s kubeconfig it just wrote, and
//     `join` acts on the kubeconfig it is recording access into. `cluster upgrade` and the three
//     provisioners take a `--context` flag and say which context they are acting on whenever the
//     active target names another (clustercontext.go). The first three are left alone because each
//     already names its cluster in the only way that makes sense for what it does.
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
