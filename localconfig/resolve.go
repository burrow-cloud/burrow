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
)

// Resolved is the concrete target a command will act against, derived from the config and
// the kubeconfig. Namespace is the app namespace (for display); empty means the caller falls
// back to the burrowd default app namespace. Env is the burrowd-registered environment NAME to
// send with the operation (empty means the cluster's default namespace and global guardrails);
// it is what burrowd resolves, not a raw namespace. In follow mode Name (and Env) are empty
// when the current context matches no registered handle (an "unregistered" current context).
type Resolved struct {
	Name                  string
	Context               string
	Namespace             string
	ControlPlaneNamespace string
	Env                   string
	Mode                  Mode
}

// Resolve decides which environment a command targets (ADR-0036).
//
// When a handle is pinned (cfg.Current set), it resolves to that handle, erroring clearly if
// the pinned name is not registered. Otherwise it follows the kubeconfig's current context:
// the target is that context, its namespace (so kubens moves Burrow too; empty when the
// context sets none, leaving the burrowd default to apply), and the default control-plane
// namespace. If the current context matches a registered handle by context name, that
// handle's Name and Env (the burrowd env name to send) are surfaced; otherwise both are empty.
func Resolve(cfg *Config, kubeconfigPath string) (Resolved, error) {
	if cfg != nil && cfg.Current != "" {
		env, ok := cfg.Lookup(cfg.Current)
		if !ok {
			return Resolved{}, fmt.Errorf(
				"localconfig: pinned environment %q is not in the config; pin a registered environment with \"burrow env use <name>\" or return to following the kube context with \"burrow env follow\"",
				cfg.Current)
		}
		return Resolved{
			Name:                  env.Name,
			Context:               env.Context,
			Namespace:             env.AppNamespace,
			ControlPlaneNamespace: env.controlPlaneNamespaceOrDefault(),
			Env:                   env.Env,
			Mode:                  ModePinned,
		}, nil
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
		if env, ok := cfg.lookupByContext(context); ok {
			resolved.Name = env.Name
			resolved.Env = env.Env
		}
	}
	return resolved, nil
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
// ambiguous (ADR-0036). Examples:
//
//	nonprod (context "do-nyc1-nonprod", namespace "team-x")
//	following kubectl: do-nyc1-dev (unregistered)
func (r Resolved) Render() string {
	if r.Mode == ModeFollowing && r.Name == "" {
		if r.Context == "" {
			return "no current kube context"
		}
		return fmt.Sprintf("following kubectl: %s (unregistered)", r.Context)
	}
	out := fmt.Sprintf("%s (context %q", r.Name, r.Context)
	if r.Namespace != "" {
		out += fmt.Sprintf(", namespace %q", r.Namespace)
	}
	out += ")"
	if r.Mode == ModeFollowing {
		out += " (following kubectl)"
	}
	return out
}
