// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package targetname names the target a command acted on, so a command that changes something can
// say where it changed it ([ADR-0078](../../docs/adr/0078-the-cli-points-at-a-target.md) §4).
//
// The failure the target model introduces is acting on the WRONG target — deploying to a cluster
// while believing you were deploying to the managed product, or the reverse. It cannot be prevented
// by design, because both are legitimate things to do and neither CLI can know which was meant. The
// only control left is making it immediately visible: the recoverable form of that mistake is
// noticing in the same breath, and the unrecoverable form is hearing about it later from somebody
// else.
//
// Two properties follow from that and are the reason this is a package rather than a helper in one
// binary. First, the name shown is the one the person chose in the picker — the same string
// `burrow auth status` prints — because a server URL or an internal identifier is not what they
// would recognise. Where no target was chosen, the environment handle registered for the cluster
// that was reached is the next-best name a person recognises, and a kube context is named only when
// Burrow has no name of its own for what it reached. Second, it names what the command ACTUALLY
// reached: a recorded target is named only when that target is what decided the connection, and a
// handle only when it is the handle for that very context. Naming a target a command did not reach
// would manufacture the exact mistake this exists to catch.
//
// Both `burrow` and `burrow-agent` render it, so the shape lives here rather than twice.
package targetname

import (
	"fmt"

	"github.com/burrow-cloud/burrow/localconfig"
)

// KindNone is the kind reported when no configured target decided where the command went: the CLI
// followed the ambient kubeconfig, which ADR-0078 §1 preserves, or a per-invocation flag overrode
// the selection. It is deliberately not an empty string, so a reader of the JSON never has to
// distinguish "no target" from "this field was not written".
const KindNone = "none"

// Named is the target a command acted on. It is emitted as the `target` member of a mutating
// command's JSON result, so an agent composing a result can say where the change happened, and
// rendered by Clause for the human line.
//
// Name is empty unless a configured target decided the connection; Context is the kube context that
// was actually reached; Endpoint is set only when a --control-plane URL bypassed the target model
// entirely. Detail is the sentence form, and for a configured target it is exactly what
// `burrow auth status` prints for that target, so the two can never drift into describing the same
// target differently.
type Named struct {
	Name     string `json:"name,omitempty"`
	Kind     string `json:"kind"`
	Context  string `json:"context,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Override bool   `json:"override,omitempty"`
	Detail   string `json:"detail"`

	// handle is the environment handle registered for the context that was reached, when no target
	// decided it. It is what the human clause says — "on prod" — so a command names the cluster the
	// way the rest of the CLI does instead of falling back to Kubernetes vocabulary for somebody who
	// has never run `burrow auth login`.
	//
	// It is unexported, and so absent from the JSON, deliberately: the `target` member answers what a
	// TARGET decided, a shape agents already parse, and a handle is not a target. What the handle
	// names is already in the JSON as `context`, which is the same cluster by construction — the
	// handle is matched to that context and nothing else.
	handle string
}

// For names what an invocation acted on, given the kube context it connected to and whether a
// per-invocation --context overrode the selection.
//
// A configured target is named only when it is a Kubernetes target whose context IS the one that was
// reached. That covers the ordinary case (the target decided the context) and deliberately excludes
// two others: a command that resolves through the kubeconfig without consulting the target at all,
// and a Burrow Cloud target, which has no kube context and cannot be what a kubeconfig connection
// reached. In both, naming the recorded target would assert something untrue about where the change
// landed.
//
// An override wins over any recorded target, because ADR-0078 §4 keeps --context working as a
// per-invocation override and what the person overrode it TO is what was acted on.
//
// With no target deciding, the environment handle registered for that same context supplies the
// name for the human clause. It is bound by the same rule as the target: the handle must be the one
// registered for the context that was reached, so the name can never describe a different cluster.
// An overridden invocation takes no handle name — the person named a cluster for this one command,
// and what they typed is what the line should reflect back.
func For(cfg *localconfig.Config, kubeContext string, override bool) Named {
	if !override {
		if t, ok, err := cfg.ActiveTarget(); err == nil && ok &&
			t.Kind == localconfig.TargetKindKubernetes && t.Context == kubeContext {
			return Named{Name: t.Name, Kind: string(t.Kind), Context: t.Context, Detail: t.Describe()}
		}
	}
	n := Named{Kind: KindNone, Context: kubeContext, Override: override}
	if !override && cfg != nil && kubeContext != "" {
		if env, ok := cfg.LookupByContext(kubeContext); ok {
			n.handle = env.Name
		}
	}
	switch {
	case kubeContext == "":
		n.Detail = "no target is selected and no kube context could be determined"
	case override:
		n.Detail = fmt.Sprintf("kube context %q, chosen for this command with --context", kubeContext)
	default:
		n.Detail = fmt.Sprintf("no target is selected; following kube context %q", kubeContext)
	}
	return n
}

// ForHandle names an environment handle a command reached through the handle's OWN scoped credential
// (ADR-0038), with no kube context involved in getting there.
//
// It exists for one case: the handle records a context the kubeconfig no longer holds, and the
// command connected anyway on the credential, which carries the API server and the CA (issue #488).
// For is no use there, because the only context it could be given is the stale one — and naming a
// cluster that does not exist as the one a change landed on is exactly issue #473. The handle is the
// true name of where the command went, and it is the name the rest of the CLI uses for it anyway.
func ForHandle(name string) Named {
	return Named{
		Kind:   KindNone,
		handle: name,
		Detail: fmt.Sprintf("the %q environment, reached with its own scoped credential", name),
	}
}

// ForCloud names a Burrow Cloud target a command acted through. It is a separate constructor from
// For because there is no kube context to check the target against: what decided the connection is
// the target itself, so the name is the one the person picked and the detail is the same sentence
// `burrow auth status` prints. The endpoint is recorded too — it is where the change landed, and it
// is not a credential; the token that goes with it is never rendered.
func ForCloud(t localconfig.Target) Named {
	return Named{
		Name:     t.Name,
		Kind:     string(localconfig.TargetKindCloud),
		Endpoint: t.Endpoint,
		Detail:   t.Describe(),
	}
}

// ForControlPlane names a control plane addressed directly by URL with --control-plane. That flag
// bypasses the target model rather than selecting within it, so there is no picker name to show and
// the URL is the honest answer. A URL is not a credential; the token that goes with it is never
// rendered here or anywhere else.
func ForControlPlane(url string) Named {
	return Named{
		Kind:     KindNone,
		Endpoint: url,
		Override: true,
		Detail:   "the control plane at " + url + ", chosen for this command with --control-plane",
	}
}

// Clause renders the trailing phrase a changing command appends to what it did — "guardrail
// app.deploy set to allow on prod" — so the place arrives in the same breath as the change.
//
// It names the place the way the rest of the CLI names it, and in the same words: the target or
// handle you picked, plain, with no quotes and no Kubernetes vocabulary, exactly as the targeting
// line `burrow app deploy` prints reads (issue #465). A kube context appears only when Burrow has no
// name of its own for where the command went, and then it is the answer rather than a mechanism
// leaking out.
//
// It no longer trails "(no target selected)". That is a statement about a thing the reader has not
// done, it is true on every mutating command until they do it, and it reads as a fault when it is
// not one. `burrow auth status` says it once, to somebody asking; a line reporting a change that
// succeeded is not the place to raise it.
func (n Named) Clause() string {
	switch {
	case n.Name != "":
		return "on " + n.Name
	case n.Endpoint != "":
		return "on the control plane at " + n.Endpoint
	case n.Context != "" && n.Override:
		return fmt.Sprintf("on kube context %q (--context override)", n.Context)
	case n.handle != "":
		return "on " + n.handle
	case n.Context != "":
		return fmt.Sprintf("on kube context %q", n.Context)
	default:
		return "on an unnamed target (none is selected and no kube context could be determined)"
	}
}

// Decided reports whether anything actually chose where this invocation went: a target, an
// environment handle, a --context override, or a --control-plane URL. It is false only for somebody
// with no target and no registered handle, who is following whatever context kubectl points at —
// for whom a clause on a line that never carried one would say nothing but that they have not done
// a thing nobody asked them to do.
func (n Named) Decided() bool {
	return n.Name != "" || n.handle != "" || n.Override || n.Endpoint != ""
}
