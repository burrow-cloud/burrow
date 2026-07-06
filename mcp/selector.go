// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package mcp

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/localconfig"
)

// selector resolves an agent's env/context tool arguments through the local handle config
// (localconfig) and lists the handles for burrow_environments (ADR-0036 slice 5b). It reads the
// handle config ($BURROW_CONFIG, else ~/.burrow/config) on each call so it always reflects the
// human's current handles. kubeconfig is the path used to find the current kube context when marking
// which handle is current in a listing (empty means the ambient kubeconfig).
//
// The agent never rides the human's PIN: resolve does not consult cfg.Current, so an agent's target
// is never the human's CLI selection (ADR-0047 §5). But for a read-only survey (ADR-0047 §3) and the
// unambiguous single-environment case (ADR-0047 §2) an empty env/context call DOES default to the
// current kube context, so the agent can look before it acts and the common single-environment
// self-hoster is unaffected. A MUTATING call with an ambiguous target — no env/context and more than
// one environment registered — is refused instead (resolveMutating, ADR-0047 §1). So a read-only
// tool always echoes the environment it read (§3) while a mutating one is forced to name it.
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

// resolveMutating is resolve with the ADR-0047 forcing function for a state-changing operation: when
// the call names no target — neither env nor context — and more than one environment handle is
// registered, it refuses with a structured, alternatives-listing error instead of routing the change
// to whatever kube context is current. With zero or one handle registered there is no ambiguity, so it
// behaves exactly like resolve. The check is on registration, not reachability (ADR-0047 §1): the
// agent must name the target before a change lands, so a deploy meant for one environment never
// silently lands on another.
func (s selector) resolveMutating(envHandle, contextOverride string) (target, error) {
	if envHandle == "" && contextOverride == "" {
		cfg, err := localconfig.Load()
		if err != nil {
			return target{}, err
		}
		if len(cfg.Environments) > 1 {
			return target{}, ambiguousEnvError(cfg.Environments)
		}
	}
	return s.resolve(envHandle, contextOverride)
}

// ambiguousEnvError is the refusal resolveMutating returns when a mutating call named no target and
// more than one environment is registered (ADR-0047 §1). It names the handles and their clusters so
// the agent re-issues the call with an explicit env, rather than letting a change land on whichever
// cluster happens to be current; burrow_environments carries the full detail. The listing is sorted so
// the message is deterministic.
func ambiguousEnvError(envs []localconfig.Environment) error {
	listed := make([]string, 0, len(envs))
	names := make([]string, 0, len(envs))
	for _, e := range envs {
		listed = append(listed, fmt.Sprintf("%s (context %s)", e.Name, e.Context))
		names = append(names, e.Name)
	}
	sort.Strings(listed)
	sort.Strings(names)
	return fmt.Errorf(
		"this operation changes state and more than one environment is registered — %s. Name the target with the env argument (e.g. env: %s); Burrow will not choose an environment for a mutating operation. Use burrow_environments to see each handle's cluster and namespace.",
		strings.Join(listed, ", "), names[0])
}

// enrichUnreachable makes a per-app tool's client-resolution failure actionable when its cause is
// the target's control plane being unreachable (a connect.UnreachableError): it appends the OTHER
// registered local environment handles — each rendered "name (context <context>)" — so the agent
// can tell the human where the real target might be, e.g. "prod is unreachable; other registered
// environments: staging (context ...)" (ADR-0047 §4).
//
// It is keyed strictly on the unreachable classification. Any other error — an unknown-handle
// refusal, a "burrowd not installed" NotFound, a control-plane 4xx — already carries its own
// actionable message and is returned unchanged, so the enrichment never clobbers it. Consistent
// with the ADR-0047 §1 registration-not-reachability posture, the alternatives are drawn from what
// is REGISTERED and are never probed: naming them costs no network, and they may themselves be down.
// Burrow never switches targets, retries elsewhere, or auto-fails-over — the operation still stops;
// the enrichment only names where a human might redirect. With no other handle registered (or a
// config that cannot be read) the original error is returned unchanged, since there is nothing to
// suggest.
func (s selector) enrichUnreachable(tgt target, err error) error {
	var unreachable *connect.UnreachableError
	if !errors.As(err, &unreachable) {
		return err
	}
	cfg, cfgErr := localconfig.Load()
	if cfgErr != nil {
		return err
	}
	others := make([]string, 0, len(cfg.Environments))
	for _, e := range cfg.Environments {
		// Exclude the environment that just failed, identified by whichever axis named it (its
		// handle name, or a raw context override); everything else is a genuine alternative.
		if tgt.name != "" && e.Name == tgt.name {
			continue
		}
		if tgt.context != "" && e.Context == tgt.context {
			continue
		}
		others = append(others, fmt.Sprintf("%s (context %s)", e.Name, e.Context))
	}
	if len(others) == 0 {
		return err
	}
	sort.Strings(others)
	return fmt.Errorf(
		"%w — other registered environments: %s. Burrow did not switch targets or retry elsewhere; choosing a different environment is a deliberate act that needs the user's go-ahead. Use burrow_environments for each handle's cluster and namespace.",
		err, strings.Join(others, ", "))
}

// actedIn is the environment a tool operated in — the one a mutating tool acted against or a
// read-only tool read from — echoed in its result so the target is legible to the agent and to
// anyone reviewing the audit trail (ADR-0036, ADR-0047 §3). Name is the local handle that selected
// it (empty when the call named a raw context or defaulted to the current one); Context is the kube
// context the call routed to (empty means the current context); Env is the burrowd-registered
// environment NAME sent with the operation (empty means the default environment).
type actedIn struct {
	Name    string `json:"name,omitempty"`
	Context string `json:"context,omitempty"`
	Env     string `json:"env,omitempty"`
}

// targeted is embedded in every environment-scoped tool's result — mutating tools echo the
// environment they acted in (ADR-0036), and read-only per-app tools echo the environment they read
// so a survey never silently conflates two (ADR-0047 §3). Its single field is promoted into the
// tool's generated output schema as an "environment" property.
type targeted struct {
	Environment actedIn `json:"environment"`
}

// echo renders the target as the environment a result reports it acted in or read from.
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
