// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package mcp

import (
	"fmt"

	"github.com/burrow-cloud/burrow/localconfig"
)

// selector resolves an agent's env/context tool arguments through the local handle config
// (localconfig) and lists the handles for burrow_environments (ADR-0036 slice 5b). It reads the
// handle config ($BURROW_CONFIG, else ~/.burrow/config) on each call so it always reflects the
// human's current handles. kubeconfig is the path used to find the current kube context when marking
// which handle is current in a listing (empty means the ambient kubeconfig); it never selects an
// agent's target, since the agent targets explicitly and never rides the human's pin or ambient
// context.
type selector struct {
	kubeconfig string
}

// target is where a tool call routes: the kube context to connect to (which cluster's burrowd) and
// the burrowd-registered environment NAME to send with the operation, plus the local handle name
// that selected them (for the result echo). An empty context means the current kube context; an
// empty env means the cluster's default environment.
type target struct {
	name    string
	context string
	env     string
}

// resolve turns a tool call's env and context arguments into the target it acts against (ADR-0036
// slice 5b).
//
// The env argument is a LOCAL HANDLE NAME: when set, it is looked up in the handle config and the
// call routes to the handle's kube context while sending the handle's Env value (the
// burrowd-registered environment NAME, empty for the cluster default). An unknown handle name is a
// clear error. burrowd resolves an env NAME (not a namespace) and errors on an unknown one, so this
// always sends the NAME.
//
// The context argument is a low-level raw kube-context override for targeting a context that has no
// handle. Precedence: the env handle wins when both are given; context applies only when env is
// empty. Omitting both follows the current kube context (empty context) with the default
// environment (empty env).
func (s selector) resolve(envHandle, contextOverride string) (target, error) {
	if envHandle != "" {
		cfg, err := localconfig.Load()
		if err != nil {
			return target{}, err
		}
		env, ok := cfg.Lookup(envHandle)
		if !ok {
			return target{}, fmt.Errorf(
				"environment %q is not in the config; run \"burrow env add %s\" to register it, or \"burrow env list\" to see the registered environments",
				envHandle, envHandle)
		}
		return target{name: env.Name, context: env.Context, env: env.Env}, nil
	}
	return target{context: contextOverride}, nil
}

// actedIn is the environment a mutating tool operated against, echoed in its result so the target is
// legible to the agent and to anyone reviewing the audit trail (ADR-0036). Name is the local handle
// that selected it (empty when the call named a raw context or defaulted to the current one);
// Context is the kube context the call routed to (empty means the current context); Env is the
// burrowd-registered environment NAME sent with the operation (empty means the default environment).
type actedIn struct {
	Name    string `json:"name,omitempty"`
	Context string `json:"context,omitempty"`
	Env     string `json:"env,omitempty"`
}

// targeted is embedded in every environment-scoped mutating tool's result to echo the environment
// it acted in (ADR-0036). Its single field is promoted into the tool's generated output schema as an
// "environment" property.
type targeted struct {
	Environment actedIn `json:"environment"`
}

// echo renders the target as the environment a result reports it acted in.
func (t target) echo() targeted {
	return targeted{Environment: actedIn{Name: t.name, Context: t.context, Env: t.env}}
}

// environmentInfo is the agent's view of one local environment handle (ADR-0036): the handle name,
// the kube context it targets, the app namespace (for display), the burrowd-registered environment
// NAME it sends with each operation (empty for the cluster default), and whether it is the current
// selection. It comes from the local handle config, not the burrowd registry.
type environmentInfo struct {
	Name      string `json:"name"`
	Context   string `json:"context"`
	Namespace string `json:"namespace,omitempty"`
	Env       string `json:"env,omitempty"`
	Current   bool   `json:"current"`
}

// list returns the local environment handles for burrow_environments, marking the current selection
// (the pinned handle, or the one matching the current kube context in follow mode). A handle config
// that cannot resolve a current selection (e.g. no kube context) still lists every handle, with none
// marked current.
func (s selector) list() ([]environmentInfo, error) {
	cfg, err := localconfig.Load()
	if err != nil {
		return nil, err
	}
	var currentName string
	if resolved, err := localconfig.Resolve(cfg, s.kubeconfig); err == nil {
		currentName = resolved.Name
	}
	out := make([]environmentInfo, 0, len(cfg.Environments))
	for _, e := range cfg.Environments {
		out = append(out, environmentInfo{
			Name:      e.Name,
			Context:   e.Context,
			Namespace: e.AppNamespace,
			Env:       e.Env,
			Current:   e.Name != "" && e.Name == currentName,
		})
	}
	return out, nil
}
