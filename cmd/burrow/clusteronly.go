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
// Deliberately NOT refused HERE:
//
//   - Installing and the rest of the cluster-lifecycle surface (`cluster install`, `cluster upgrade`,
//     `cluster bootstrap`, `join`, `cluster ingress/registry/postgres/metrics install`). ADR-0078 §3:
//     install continues to act on a kubeconfig context, since installing into Burrow Cloud is not a
//     thing that can be asked for, and that stays true — a person with the managed product selected
//     can still install a cluster. None of them routes through the paths guarded here.
//
//     They are not unguarded, though: they answer to their own rule, in lifecycleContext
//     (clustercontext.go), which refuses unless the cluster is one somebody named (cloud ADR-0038
//     §1). What differs is the shape of the answer. This file's refusal is about the target KIND, so
//     the recovery is to switch target; theirs is about a cluster having been NAMED at all, so a
//     `--context` satisfies it whatever the active target is.
//
//     How each names its cluster differs, and the difference is not tidy: `cluster install` takes a
//     positional `<context>`, `cluster bootstrap` acts on the k3s kubeconfig it just wrote, and
//     `join` acts on the kubeconfig it is recording access into. `cluster upgrade` and the
//     provisioners take a `--context` flag, and otherwise follow the active target when it is a
//     cluster. The first three need no flag because each already names its cluster in the only way
//     that makes sense for what it does.
//   - `burrow auth ...`. It is how a person sees which target is active and changes it, so a refusal
//     there would leave someone with the managed product selected and no way to read or leave that
//     state. It reads and writes the local config only and touches no cluster.
//   - The reads that call commonOpts.readClient instead: `guard list`, `cluster config list`,
//     `audit`, `failures`, `env list`, and — added by the sweep of the whole surface — `addon list`,
//     `addon logs` and `addon metrics`. They were refused here because they shared a connection
//     path with the writes beside them, not because they needed a cluster — each is a route the
//     managed control plane answers, and the refusal was the client not knowing it existed rather
//     than the route being absent (cloud issue #202). They act on either kind of target now.
//
//     A read moves there on that basis alone: the route answers for a tenant, and what it returns is
//     the tenant's. `cluster` and `cluster capacity` therefore stay refused although their routes
//     answer, because what they describe is the operator's cluster — its nodes, its headroom, its
//     top consumers across every tenant — and what a tenant should see of that is a decision nobody
//     has taken.
//   - `addon attach`, on commonOpts.changeClient. A command that CHANGES a tenant needs more than a
//     route that answers: it needs the product to have said a tenant may. For attach it has —
//     provisioning a tenant's database over POST /v1/addons/attach is what the managed product does,
//     and it is the route every attach on the platform has gone through (cloud issue #215). The rest
//     of the add-on writes stay here because that second question is still open for them.
//
//     Worth being exact about what the refusal was worth, because it is easy to read it as a control
//     it never was: `tenantguard.PersonPolicy` allows every guardrail on a person's credential, so
//     each command refused here would have been served to the same person's bearer token over plain
//     HTTP (cloud issue #208). These refusals are a convention about what the product offers, not a
//     boundary — and for attach, they were also inverted, since a tenant's `burrow-agent` could
//     attach a database their own `burrow` refused. Where an attach genuinely needs holding, that is
//     a guardrail in the control plane, which binds every caller (ADR-0095).
//
//     `addon backups` and `addon backup-health` are reads whose routes answer too, and they stay
//     refused for a third reason that is neither of those: on the managed product the backups are
//     the PLATFORM's, taken of an instance the tenant does not operate, and none of them is recorded
//     in the tenant's own registry. Both commands would therefore report, accurately and uselessly,
//     that a tenant has no backups — `backups` while pointing at `burrow addon backup`, which
//     refuses there. What a managed tenant should be told about the backups of their database is a
//     product statement, and it is not one a client change can make.
//
//     `addon sql` is a read in the sense that matters to a person and is NOT one here. It opens a
//     SQL connection from burrowd, and the managed control plane does not run in the fleet its
//     tenants' databases are in and may not dial into one (cloud ADR-0036 §1). Its route is not
//     absent and the tenant guardrail policy holds it at `confirm`, so what it needs is an execution
//     path inside the fleet, not a different client.

// refuseCloudTarget reports why a cluster-only command cannot run, or nil when it can. It reads the
// local config and nothing else: no kubeconfig, no network, no credential.
//
// A config that will not load is not this function's error to raise. Every caller is about to load it
// again, or to open a connection that will fail on the same problem and say so far better; failing
// here would only replace a legible message with a vaguer one. Silence on that path also keeps the
// cluster path exactly as it was, which is the point — this changes what happens with the managed
// product selected and nothing else.
func refuseCloudTarget() error {
	target, ok := selectedCloudTarget()
	if !ok {
		return nil
	}
	return errCloudTargetHasNoCluster(target)
}

// selectedCloudTarget returns the active target when it is the managed product, and false otherwise —
// including when the config cannot be read, for the reason refuseCloudTarget documents: with an
// unreadable config nothing can tell there is a target at all.
//
// It is separate from refuseCloudTarget because refusing is not the only correct response. `burrow
// version` REPORTS rather than acts, so the honest thing there is to say what the line is about and
// which target it does not apply to, not to fail (cloud#209). Both need the same fact, and one place
// to get it is what keeps the two from disagreeing about when the managed product is selected.
func selectedCloudTarget() (localconfig.Target, bool) {
	cfg, err := localconfig.Load()
	if err != nil {
		return localconfig.Target{}, false
	}
	target, selected, err := cfg.ActiveTarget()
	if err != nil || !selected || target.Kind != localconfig.TargetKindCloud {
		return localconfig.Target{}, false
	}
	return target, true
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
