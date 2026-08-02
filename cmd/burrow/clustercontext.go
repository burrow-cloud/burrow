// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"io"

	"github.com/spf13/pflag"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/localconfig"
)

// Which cluster a privileged command acts on
// ([ADR-0084](../../docs/adr/0084-everyone-who-uses-burrow-carries-their-own-token.md) §4).
//
// A privileged command is one that reaches a cluster without being scoped to an application:
// `guard`, `cluster` and `cluster config`, `addon`, `env add`, `config provider`,
// `config registry`, `app domain`, `audit`, `failures`. They used to build their connection from
// the raw `--context` flag, which is empty unless somebody types it, so they fell through to the
// kubeconfig's current context and the selected target was never consulted. A person therefore kept
// `kubectx` and `burrow auth switch` in agreement by hand and had no way to tell which of the two a
// given command had obeyed, because the output did not say. Every command now resolves through the
// selected target instead.
//
// The precedence is the whole design, and it is this, highest first:
//
//  1. `--control-plane` names a control plane outright, so no target is consulted for it. Handled
//     by the callers, which return before reaching here.
//  2. `--context` — a person naming a cluster for this one invocation. An EXPLICIT choice keeps
//     winning over the selected target; that is somebody being deliberate, which is the opposite of
//     the problem. ADR-0078 §4 keeps the flag working as a per-invocation override and this is it.
//  3. The selected ADR-0078 target, when it is a Kubernetes target. This is the arm that was
//     missing.
//  4. The kubeconfig's current context — reached only when NO target is selected. That is the
//     pre-target world, it is still the default, and nothing about it changes.
//
// What stops is the IMPLICIT fall-through: (4) is no longer reachable while a target is selected.
// And when an explicit flag does send a command somewhere the selected target does not name, that is
// said out loud rather than left to be discovered, because a silent divergence is the failure this
// whole record is about.

// clusterContext resolves the kube context a privileged command acts on, per the precedence above,
// and returns the empty string when nothing has decided one — meaning the kubeconfig's current
// context, which is what connect already does with an empty context.
//
// The result is memoised on the per-invocation commonOpts so a command that resolves the cluster
// more than once (`env add` applies manifests, calls the API, then records a handle; `addon install`
// stages RBAC before it calls the API) gets one answer and prints any divergence note once. It is
// per-invocation state on a per-command struct, not global state, for the same reason `acted` is.
func (o *commonOpts) clusterContext(stderr io.Writer) (string, error) {
	if o.contextResolved {
		return o.kubeContext, nil
	}
	// installID is left empty on every path but the one that resolves a target below. An explicit
	// --context is a deliberate choice of a different cluster from the one the target names, so the
	// target's id no longer describes what is on the other end; carrying it would refuse the override
	// on the grounds that it is an override, which is the rule resolveTarget already applies on the
	// per-app path.
	// An explicit flag wins, and nothing about the target is allowed to fail the invocation from
	// here on. Overriding a target whose context has gone stale is a legitimate way to keep working,
	// and so is running with a ~/.burrow/config that will not parse; failing on the target nobody
	// asked this command to use would take a working escape hatch away. A config that cannot be read
	// costs the note below and nothing else.
	if o.context != "" {
		o.kubeContext, o.contextResolved = o.context, true
		if o.controlPlane == "" {
			if cfg, err := localconfig.Load(); err == nil {
				noteContextOverride(cfg, o.context, stderr)
			}
		}
		return o.kubeContext, nil
	}
	// --control-plane names the control plane outright, so no target is consulted for it — the same
	// early return requireCluster and resolveTarget make. It matters beyond tidiness: this path never
	// opens a kubeconfig, so resolving a target here would let a target whose context has been
	// renamed away break a scripted or CI invocation that has no kubeconfig to speak of.
	if o.controlPlane != "" {
		o.contextResolved = true
		return "", nil
	}
	cfg, err := localconfig.Load()
	if err != nil {
		return "", err
	}
	cluster, err := localconfig.ResolveCluster(cfg, o.kubeconfig)
	if err != nil {
		return "", err
	}
	o.kubeContext, o.contextResolved, o.installID = cluster.Context, true, cluster.InstallID
	return o.kubeContext, nil
}

// noteContextOverride says so when `--context` sends a command to a cluster the active target does
// not name. The override is honoured either way — it is deliberate — but a change landing somewhere
// other than the target on screen in `burrow auth status` is exactly the thing that should be
// noticed in the same breath rather than discovered later from somebody else.
//
// It stays quiet when the flag names the target's own context, which is a person confirming the
// selection rather than leaving it, and when no target is selected at all.
func noteContextOverride(cfg *localconfig.Config, kubeContext string, stderr io.Writer) {
	target, selected, err := cfg.ActiveTarget()
	if err != nil || !selected {
		return
	}
	if target.Kind != localconfig.TargetKindKubernetes || target.Context == kubeContext {
		return
	}
	fmt.Fprintf(stderr, "burrow: acting on kube context %q; the active target %q names kube context %q (--context overrides it).\n",
		kubeContext, target.Name, target.Context)
}

// noteLifecycleContext says which cluster a CLUSTER-LIFECYCLE command is about to act on, when that
// is not the cluster the active target names.
//
// The lifecycle commands — `cluster install`, `cluster upgrade`, `cluster bootstrap`, `join`, and
// the `cluster ingress` / `cluster registry` / `cluster postgres` provisioners — deliberately do NOT
// follow the target. [ADR-0078](../../docs/adr/0078-the-cli-points-at-a-target.md) §3 settles that:
// they act on a kubeconfig context, because installing into the managed product is not a thing that
// can be asked for, and an installer that redirected itself to whatever was last selected would be
// the more dangerous of the two behaviours. What was missing is not target-following; it is that
// several of them had no `--context` flag at all, so there was no per-invocation way to point them
// anywhere, and they never said which cluster they had picked.
//
// So they gained the flag, and they say so here when the cluster they are acting on is not the one
// the target names. Silent when no target is selected (the pre-target world, unchanged) and when the
// target names this very context.
func noteLifecycleContext(kubeconfig, kubeContext string, stderr io.Writer) {
	cfg, err := localconfig.Load()
	if err != nil {
		return
	}
	target, selected, err := cfg.ActiveTarget()
	if err != nil || !selected {
		return
	}
	// Resolve the flag (or its absence) to the context this command will really reach, so the note
	// names a context rather than an empty string. A kubeconfig that cannot be read is not worth
	// failing on here: the command itself is about to fail on it and will say so far better.
	acting, err := connect.TargetContextName(kubeconfig, kubeContext)
	if err != nil || acting == "" {
		return
	}
	switch {
	case target.Kind == localconfig.TargetKindCloud:
		fmt.Fprintf(stderr, "burrow: acting on kube context %q. The active target %q is %s, which has no cluster of its own; installing and provisioning always act on a kubeconfig context.\n",
			acting, target.Name, target.Describe())
	case target.Kind == localconfig.TargetKindKubernetes && target.Context != acting:
		fmt.Fprintf(stderr, "burrow: acting on kube context %q, which is not what the active target %q names (kube context %q). Installing and provisioning act on a kubeconfig context rather than the target; pass --context to name one.\n",
			acting, target.Name, target.Context)
	}
}

// bindLifecycleContext registers `--context` on a cluster-lifecycle command. The flag's help says
// what the default is, because the default is the interesting part: these commands follow the
// kubeconfig and not the target, so a person with a target selected needs to know that naming a
// context is how they point one somewhere else.
func bindLifecycleContext(flags *pflag.FlagSet, kubeContext *string) {
	flags.StringVar(kubeContext, "context", "", "kubeconfig context to act on (default: the kubeconfig's current context, not the active target)")
}
