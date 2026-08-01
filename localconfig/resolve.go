// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package localconfig

import (
	"fmt"

	"k8s.io/client-go/tools/clientcmd"
)

// Mode is how the active target was selected: pinned to a named handle, or following the
// current kube context.
type Mode string

const (
	// ModePinned means a handle was pinned with `burrow env use`.
	ModePinned Mode = "pinned"
	// ModeFollowing means the target tracks the current kube context (the default).
	ModeFollowing Mode = "following"
	// ModeTargeted means an ADR-0078 target chosen with `burrow auth login` decided the cluster,
	// and no pinned handle inside that cluster narrowed it further.
	ModeTargeted Mode = "targeted"
)

// Resolved is the concrete target a command will act against, derived from the config and
// the kubeconfig. Namespace is the app namespace (for display); empty means the caller falls
// back to the burrowd default app namespace. Env is the burrowd-registered environment NAME to
// send with the operation (empty means the cluster's default namespace and global guardrails);
// it is what burrowd resolves, not a raw namespace. In follow mode Name (and Env) are empty
// when the current context matches no registered handle (an "unregistered" current context).
// AgentKubeconfig/AgentContext carry the resolved handle's scoped, burrowd-only credential
// (ADR-0038) so the operate path can default to it; both are empty when the handle records none.
// Target names the ADR-0078 target that decided the cluster, and is empty when no target is
// selected (the pre-ADR-0078 behaviour, and still the default).
type Resolved struct {
	Name                  string
	Context               string
	Namespace             string
	ControlPlaneNamespace string
	Env                   string
	Mode                  Mode
	Target                string
	AgentKubeconfig       string
	AgentContext          string
}

// Resolve decides which environment a command targets (ADR-0036, ADR-0078).
//
// When an ADR-0078 target is selected it decides the CLUSTER first, since that is what the target
// says: a Kubernetes target resolves to its kubeconfig context, and a pinned handle applies only
// when it is a handle inside that same cluster (a pin for a different cluster is not a narrowing of
// this one). A Burrow Cloud target has no kubeconfig to resolve and is reported as such.
//
// With no target selected the behaviour is exactly as before. When a handle is pinned (cfg.Current
// set), it resolves to that handle, erroring clearly if the pinned name is not registered.
// Otherwise it follows the kubeconfig's current context: the target is that context, its namespace
// (so kubens moves Burrow too; empty when the context sets none, leaving the burrowd default to
// apply), and the default control-plane namespace. If the current context matches a registered
// handle by context name, that handle's Name and Env (the burrowd env name to send) are surfaced;
// otherwise both are empty.
func Resolve(cfg *Config, kubeconfigPath string) (Resolved, error) {
	target, hasTarget, err := cfg.ActiveTarget()
	if err != nil {
		return Resolved{}, err
	}
	if hasTarget {
		return resolveWithTarget(cfg, target, kubeconfigPath)
	}
	return resolveWithoutTarget(cfg, kubeconfigPath)
}

// resolveWithTarget resolves against the selected ADR-0078 target. A Burrow Cloud target carries no
// kubeconfig context, so a command that reaches the cluster through one is told plainly which
// target is active and how to switch, instead of silently acting on some other cluster.
func resolveWithTarget(cfg *Config, target Target, kubeconfigPath string) (Resolved, error) {
	if target.Kind != TargetKindKubernetes {
		return Resolved{}, fmt.Errorf(
			"localconfig: the active target %q is %s, which this command reaches through a kubeconfig and cannot; switch to a cluster target with \"burrow auth switch <name>\" (see \"burrow auth status\")",
			target.Name, target.Describe())
	}

	namespace, found, err := contextNamespace(kubeconfigPath, target.Context)
	if err != nil {
		return Resolved{}, err
	}
	if !found {
		return Resolved{}, fmt.Errorf(
			"localconfig: the active target %q names kube context %q, which is not in your kubeconfig; the kubeconfig may have moved or the context may have been renamed. Point at it again with \"burrow auth login\", or pick another target with \"burrow auth switch <name>\"",
			target.Name, target.Context)
	}

	resolved := Resolved{
		Context:               target.Context,
		Namespace:             namespace,
		ControlPlaneNamespace: DefaultControlPlaneNamespace,
		Mode:                  ModeTargeted,
		Target:                target.Name,
	}
	// A pinned handle narrows the target only when it is a handle in the SAME cluster; a pin left
	// over from another cluster is not a narrowing of this one and is skipped rather than silently
	// redirecting the command out of the target.
	if cfg != nil && cfg.Current != "" {
		env, ok := cfg.Lookup(cfg.Current)
		if !ok {
			return Resolved{}, errUnknownPin(cfg.Current)
		}
		if env.Context == target.Context {
			return fromHandle(env, ModePinned, target.Name), nil
		}
	}
	if cfg != nil {
		if env, ok := cfg.LookupByContext(target.Context); ok {
			resolved.Name = env.Name
			resolved.Env = env.Env
			resolved.AgentKubeconfig = env.AgentKubeconfig
			resolved.AgentContext = env.AgentContext
		}
	}
	return resolved, nil
}

// resolveWithoutTarget is the pre-ADR-0078 resolution, unchanged: a pinned handle, else the
// kubeconfig's current context.
func resolveWithoutTarget(cfg *Config, kubeconfigPath string) (Resolved, error) {
	if cfg != nil && cfg.Current != "" {
		env, ok := cfg.Lookup(cfg.Current)
		if !ok {
			return Resolved{}, errUnknownPin(cfg.Current)
		}
		return fromHandle(env, ModePinned, ""), nil
	}

	context, namespace, err := currentContext(kubeconfigPath)
	if err != nil {
		return Resolved{}, err
	}
	resolved := Resolved{
		Context:               context,
		Namespace:             namespace,
		ControlPlaneNamespace: DefaultControlPlaneNamespace,
		Mode:                  ModeFollowing,
	}
	if cfg != nil {
		if env, ok := cfg.LookupByContext(context); ok {
			resolved.Name = env.Name
			resolved.Env = env.Env
			resolved.AgentKubeconfig = env.AgentKubeconfig
			resolved.AgentContext = env.AgentContext
		}
	}
	return resolved, nil
}

// fromHandle renders a registered handle as the resolved selection, carrying the target that led to
// it (empty when none is selected). It is shared by the pinned paths so they cannot drift.
func fromHandle(env Environment, mode Mode, target string) Resolved {
	return Resolved{
		Name:                  env.Name,
		Context:               env.Context,
		Namespace:             env.AppNamespace,
		ControlPlaneNamespace: env.controlPlaneNamespaceOrDefault(),
		Env:                   env.Env,
		Mode:                  mode,
		Target:                target,
		AgentKubeconfig:       env.AgentKubeconfig,
		AgentContext:          env.AgentContext,
	}
}

// errUnknownPin is the message for a pinned handle name that is not registered.
func errUnknownPin(name string) error {
	return fmt.Errorf(
		"localconfig: pinned environment %q is not in the config; pin a registered environment with \"burrow env use <name>\" or return to following the kube context with \"burrow env follow\"",
		name)
}

// contextNamespace reads one named kubeconfig context's namespace and reports whether the context
// exists at all, honoring an explicit path otherwise the ambient $KUBECONFIG / ~/.kube/config. It is
// how a Kubernetes target is checked against the kubeconfig it names, so a target that has gone
// stale is caught where it is resolved rather than at connect time.
func contextNamespace(kubeconfigPath, name string) (namespace string, found bool, err error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		rules.ExplicitPath = kubeconfigPath
	}
	cfg, err := rules.Load()
	if err != nil {
		return "", false, fmt.Errorf("localconfig: loading kubeconfig: %w", err)
	}
	c, ok := cfg.Contexts[name]
	if !ok {
		return "", false, nil
	}
	return c.Namespace, true, nil
}

// currentContext reads the kubeconfig's current context and its namespace, honoring an
// explicit path otherwise the ambient $KUBECONFIG / ~/.kube/config, the same way
// connect.Contexts does. A context that sets no namespace yields an empty namespace.
func currentContext(kubeconfigPath string) (context, namespace string, err error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		rules.ExplicitPath = kubeconfigPath
	}
	cfg, err := rules.Load()
	if err != nil {
		return "", "", fmt.Errorf("localconfig: loading kubeconfig: %w", err)
	}
	context = cfg.CurrentContext
	if c := cfg.Contexts[context]; c != nil {
		namespace = c.Namespace
	}
	return context, namespace, nil
}

// Render formats a resolved target for display on a command, so the target is never
// ambiguous (ADR-0036, ADR-0078). Examples:
//
//	nonprod (context "do-nyc1-nonprod", namespace "team-x")
//	following kubectl: do-nyc1-dev (unregistered)
//	target "do-nyc1" (no environment registered)
func (r Resolved) Render() string {
	if r.Mode == ModeFollowing && r.Name == "" {
		if r.Context == "" {
			return "no current kube context"
		}
		return fmt.Sprintf("following kubectl: %s (unregistered)", r.Context)
	}
	if r.Mode == ModeTargeted && r.Name == "" {
		// A kubeconfig target is named after its context, so naming both would stutter.
		if r.Target == r.Context {
			return fmt.Sprintf("target %q (no environment registered)", r.Target)
		}
		return fmt.Sprintf("target %q: context %q (no environment registered)", r.Target, r.Context)
	}
	out := fmt.Sprintf("%s (context %q", r.Name, r.Context)
	if r.Namespace != "" {
		out += fmt.Sprintf(", namespace %q", r.Namespace)
	}
	out += ")"
	switch {
	case r.Mode == ModeFollowing:
		out += " (following kubectl)"
	case r.Target != "":
		out += fmt.Sprintf(" (target %q)", r.Target)
	}
	return out
}
