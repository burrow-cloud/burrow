// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package connect

import (
	"fmt"
	"sort"

	"k8s.io/client-go/tools/clientcmd"
)

// Context is a kubeconfig context: a cluster, each running its own burrowd, so the contexts
// are the environments a developer or agent can target (ADR-0035 phase 1). The CLI's global
// --context flag and the MCP per-call context argument both name one of these.
type Context struct {
	Name    string // the kubeconfig context name
	Cluster string // the cluster the context points at
	Current bool   // whether this is the kubeconfig's current context
}

// Contexts loads the kubeconfig (honoring an explicit path, otherwise the ambient KUBECONFIG /
// ~/.kube/config) and returns its contexts sorted by name, marking the current one. It needs no
// control-plane connection: the kubeconfig itself is the registry of environments (ADR-0035
// phase 1), so both `burrow context list` and the MCP burrow_environments tool read it through
// this one helper rather than duplicating the logic.
func Contexts(kubeconfig string) ([]Context, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("connect: loading kubeconfig: %w", err)
	}
	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Context, 0, len(names))
	for _, name := range names {
		out = append(out, Context{
			Name:    name,
			Cluster: cfg.Contexts[name].Cluster,
			Current: name == cfg.CurrentContext,
		})
	}
	return out, nil
}

// TargetContextName returns the name of the kubeconfig context an operation will target: the
// override when non-empty, otherwise the kubeconfig's current context. It reads the kubeconfig
// the same way Contexts does (honoring an explicit path, otherwise the ambient KUBECONFIG /
// ~/.kube/config) and needs no control-plane connection, so a command can name the context it is
// about to probe even before reaching the cluster.
func TargetContextName(kubeconfig, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := rules.Load()
	if err != nil {
		return "", fmt.Errorf("connect: loading kubeconfig: %w", err)
	}
	return cfg.CurrentContext, nil
}
