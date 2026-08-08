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
//  1. `--context` — a person naming a cluster for this one invocation. An EXPLICIT choice keeps
//     winning over the selected target; that is somebody being deliberate, which is the opposite of
//     the problem. ADR-0078 §4 keeps the flag working as a per-invocation override and this is it.
//  2. `--control-plane` names a control plane outright, so no target is consulted for it.
//  3. The selected ADR-0078 target, when it is a Kubernetes target. This is the arm that was
//     missing.
//  4. The kubeconfig's current context — reached only when NO target is selected. That is the
//     pre-target world, it is still the default, and nothing about it changes.
//
// (1) and (2) never both decide anything, since --control-plane needs no kube context and reads
// none: they are ordered this way only so that a --context passed alongside it still reaches the
// kubeconfig-side work a few commands do before they connect.
//
// What stops is the IMPLICIT fall-through: (4) is no longer reachable while a target is selected.
// And when an explicit flag does send a command somewhere the selected target does not name, that is
// said out loud rather than left to be discovered, because a silent divergence is the failure this
// whole record is about.
//
// The CLUSTER-LIFECYCLE commands resolve separately, in lifecycleContext below, and the difference
// between the two resolutions is exactly (4): a lifecycle command has no fall-through at all, not
// even the pre-target one, because the privileged operation it is about to perform is one nobody
// should get on a cluster they did not name (cloud ADR-0038 §1).

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
	// A config that will not load leaves the kubeconfig deciding, and says so once.
	//
	// Failing here would be a regression for somebody who has never selected a target: this path
	// did not read ~/.burrow/config at all before, so a malformed one broke nothing, and refusing
	// `guard list` over a file the command has no use for is not an improvement. It is also the same
	// tolerance refuseCloudTarget already applies — with an unreadable config nothing can tell there
	// is a target at all, so the whole target model is inert either way, and pretending otherwise
	// here would only produce a worse message than the one `burrow auth status` gives.
	//
	// The note is the difference between falling back and falling back SILENTLY, which is the thing
	// this file exists to stop. It fires only on a file that exists and will not parse: a missing
	// config is the ordinary first-run state and loads as empty.
	cfg, err := localconfig.Load()
	if err != nil {
		fmt.Fprintf(stderr, "burrow: %v\nburrow: following the kubeconfig instead; run \"burrow auth status\" to see the targeting state.\n", err)
		o.contextResolved = true
		return "", nil
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

// lifecycleContext decides which cluster a CLUSTER-LIFECYCLE command acts on, or refuses to let it
// run (cloud ADR-0038 §1).
//
// The lifecycle set is the commands that reach a cluster through a kubeconfig alone and take
// `--context` to say which: `cluster upgrade`, and the `cluster ingress` / `cluster registry` /
// `cluster postgres` / `cluster metrics` provisioners. `cluster install`, `cluster bootstrap` and
// `join` are outside it, not by exemption but because none of them CAN run without naming a
// cluster — install takes a required positional `<context>` and lists the kubeconfig's contexts when
// it is absent, bootstrap acts on the k3s admin kubeconfig it just wrote, and join on the kubeconfig
// carried in the token it was handed. Binding the flag is therefore the marker of the set, and
// bindLifecycleContext is where it is stamped.
//
// Two arms decide it, and the missing third is the change:
//
//  1. `--context`, a person naming a cluster for this one invocation. It wins over the active target
//     exactly as it does on the privileged path above, and it is honoured whatever kind the target
//     is: installing a cluster while the managed product is selected stays legal, because a person
//     using both is an ordinary state and the flag says which cluster they mean.
//  2. The active target, when it is a cluster target. A cluster target names a context, which is a
//     name somebody configured, so acting on it is acting on a cluster somebody chose.
//
// The arm that is gone is the one these commands used to end on: the kubeconfig's current context,
// taken whenever neither of the two above had spoken. They warned about it and proceeded, and the
// warning was the wrong instrument — it goes to stderr, it is read after the fact if at all, and
// never in a script, while the consequence is an install or an upgrade on a cluster nobody named. So
// the fall-through is a refusal now, and it names the cluster it would have acted on, which is the
// difference between a message and a fix.
//
// Scripts that relied on the ambient context break, and naming a context repairs them. That is the
// accepted cost of the rule rather than an oversight in it: it is the class of breakage that is
// better loud.
//
// The returned context is never empty. That matters mechanically as well as in principle — an empty
// context is precisely what connect reads as "the kubeconfig's current context", so returning one
// would reinstate the fall-through underneath the rule.
func lifecycleContext(kubeconfig, kubeContext string, stderr io.Writer) (string, error) {
	// An explicit flag wins, and nothing about the target may fail the invocation from here on — the
	// same tolerance clusterContext applies, for the same reasons. Overriding a target whose context
	// has gone stale is a legitimate way to keep working, and so is running with a ~/.burrow/config
	// that will not parse; a config that cannot be read costs the divergence note and nothing else.
	if kubeContext != "" {
		if cfg, err := localconfig.Load(); err == nil {
			noteContextOverride(cfg, kubeContext, stderr)
		}
		return kubeContext, nil
	}
	// With no flag the config is the only thing left that can name a cluster, so a config that will
	// not load is not something to fall back FROM here: falling back is the behaviour being removed,
	// and the file's unreadability is exactly why nothing can name a cluster. The privileged path
	// tolerates the same file because it has a default to fall back to and this does not.
	cfg, err := localconfig.Load()
	if err != nil {
		return "", errUnnamedLifecycleCluster(kubeconfig, fmt.Sprintf("the local config could not be read (%v)", err))
	}
	target, selected, err := cfg.ActiveTarget()
	switch {
	case err != nil:
		return "", errUnnamedLifecycleCluster(kubeconfig, fmt.Sprintf("the active target could not be read (%v)", err))
	case !selected:
		return "", errUnnamedLifecycleCluster(kubeconfig, "no target is selected")
	case target.Kind != localconfig.TargetKindKubernetes:
		return "", errUnnamedLifecycleCluster(kubeconfig,
			fmt.Sprintf("the active target %q is %s, which has no cluster of its own", target.Name, target.Describe()))
	}
	// The target names a context and ResolveCluster checks it against the kubeconfig, so a target
	// that has gone stale is caught here — where the message names the target and the context —
	// rather than at connect time, as a kubeconfig error about a cluster the reader did not know they
	// were reaching for.
	cluster, err := localconfig.ResolveCluster(cfg, kubeconfig)
	if err != nil {
		return "", err
	}
	return cluster.Context, nil
}

// errUnnamedLifecycleCluster is the refusal. Its job is to be actionable on one read: what the
// command needed, why nothing supplied it, which cluster it would have acted on under the old rule,
// and the two ways to name one.
//
// Naming the would-be cluster is the part that turns a refusal into a fix. It is almost always the
// cluster the person meant — it is the one they would have got — so the correction is a copy of the
// name out of the message, rather than a hunt through `kubectl config get-contexts` for whichever
// entry was current.
//
// A kubeconfig with no current context, or none at all, leaves nothing to name; the message says the
// same thing without that clause rather than inventing a cluster.
func errUnnamedLifecycleCluster(kubeconfig, because string) error {
	const switchTarget = `select a cluster target with "burrow auth switch <name>" (see "burrow auth status")`
	would, err := connect.TargetContextName(kubeconfig, "")
	if err != nil || would == "" {
		return fmt.Errorf("this command acts on a cluster and nothing names one: %s. Name a cluster with --context <name>, or %s", because, switchTarget)
	}
	return fmt.Errorf("this command acts on a cluster and nothing names one: %s. It would have acted on kube context %q, the kubeconfig's current context, which is no longer enough on its own. Act on that one with --context %s, or %s",
		because, would, would, switchTarget)
}

// lifecycleContextFlag is the annotation bindLifecycleContext stamps on the `--context` flag it
// registers, which is what makes the lifecycle set enumerable rather than a list somebody maintains.
// A test walks the command tree for it and asserts the rule over every command that carries it, so a
// provisioner added later is covered from the moment it binds the flag.
const lifecycleContextFlag = "burrow_lifecycle_context"

// bindLifecycleContext registers `--context` on a cluster-lifecycle command. The flag's help states
// the default because the default is the interesting part: with no cluster target active there is
// none, and the flag is the whole way to run the command at all.
func bindLifecycleContext(flags *pflag.FlagSet, kubeContext *string) {
	flags.StringVar(kubeContext, "context", "", "kubeconfig context to act on (default: the active cluster target's; required when no cluster target is active)")
	// SetAnnotation fails only on a flag that is not registered, which the line above just did.
	_ = flags.SetAnnotation("context", lifecycleContextFlag, []string{"true"})
}
