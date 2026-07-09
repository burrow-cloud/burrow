// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package agentconn resolves the credential-free, agent-layer control-plane client the scoped
// agent binary uses (ADR-0005, ADR-0038). It is module-private shared connection logic: the same
// building blocks burrow-mcp uses (connect.KubeconfigTransport, client.NewClientVersion, and the
// localconfig scoped-credential lookup), lifted out so a second thin client — burrow-agent — reaches
// burrowd over one seam without duplicating the precedence or the fail-closed rules.
//
// Like burrow-mcp it holds no cluster-operating credentials of its own: it reaches the in-cluster
// control plane through the scoped, burrowd-only agent kubeconfig `burrow install` mints (ADR-0038)
// and the Kubernetes API-server proxy (ADR-0014), and it fails closed — a handle that records a
// scoped credential whose file is missing is an error, never a silent escalation to the
// ambient/admin kubeconfig. In strict mode a context with no scoped credential is an error too. The
// package is binary-neutral: its messages name no particular environment variable.
package agentconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/localconfig"
)

// Config configures the agent-layer client factory. ControlPlaneURL (with Token) names a single
// control plane directly and outranks everything; Kubeconfig is an explicit kubeconfig used for every
// context; Namespace is the control-plane namespace burrowd runs in; Strict refuses the ambient
// fallback for a context with no scoped credential; Version is forwarded as X-Burrow-Client-Version
// (ADR-0039).
type Config struct {
	ControlPlaneURL string
	Token           string
	Kubeconfig      string
	Namespace       string
	Strict          bool
	Version         string
}

// ClientForContext resolves a control-plane client for a kubeconfig context (which cluster's burrowd
// a call targets; ADR-0035, ADR-0036). An empty context means the current kubeconfig context.
type ClientForContext = func(kubeContext string) (*client.Client, error)

// NewFactory builds the per-context control-plane client factory. Its credential precedence is,
// highest first:
//
//  1. ControlPlaneURL — a direct URL (e.g. an ingress) with Token; it names exactly one control
//     plane, so the per-call context does not apply and every call uses it. An empty Token is an
//     error.
//  2. Kubeconfig — an explicit kubeconfig used for every context.
//  3. the scoped, burrowd-only agent kubeconfig recorded for the handle whose context matches the
//     requested one (ADR-0038), so the agent's reachable credential is confined to burrowd.
//  4. the ambient kubeconfig — the fallback for a context with no matching handle or scoped
//     credential.
//
// Like burrow-mcp it fails closed (ADR-0038): step 4 is refused when the handle records a scoped
// credential whose file is missing (always an error, never a silent escalation to admin), and in
// strict mode the ambient fallback is refused entirely — only the explicit escape hatches (steps 1
// and 2) remain. The proxy-path factory is concurrency-safe and caches one client per context.
func NewFactory(ctx context.Context, cfg Config, stderr io.Writer) (ClientForContext, error) {
	if cfg.ControlPlaneURL != "" {
		if cfg.Token == "" {
			return nil, errors.New("a control-plane token is required with a control-plane URL")
		}
		c := client.NewClientVersion(cfg.ControlPlaneURL, cfg.Token, cfg.Version)
		return func(string) (*client.Client, error) { return c, nil }, nil
	}

	var mu sync.Mutex
	cache := map[string]*client.Client{}
	return func(kubeContext string) (*client.Client, error) {
		mu.Lock()
		defer mu.Unlock()
		if c, ok := cache[kubeContext]; ok {
			return c, nil
		}
		opts, err := ConnectOptions(kubeContext, cfg.Kubeconfig, cfg.Namespace, cfg.Strict, stderr)
		if err != nil {
			return nil, err
		}
		opts.ClientVersion = cfg.Version // ADR-0039: forward this binary's version as X-Burrow-Client-Version
		// Route through the same kubeconfig transport the CLI and MCP server use, so all three share
		// one seam (ADR-0045). The transport stays credential-free here: it reads the burrowd API token
		// from the install Secret over the human's proxy, holding no cluster-operating credential of
		// its own (ADR-0005).
		c, err := connect.KubeconfigTransport{Options: opts}.Connect(ctx)
		if err != nil {
			return nil, err
		}
		cache[kubeContext] = c
		return c, nil
	}, nil
}

// ConnectOptions builds the auto-connect options for a requested kube context, applying the
// explicit-kubeconfig > scoped-per-handle > ambient precedence (a direct control-plane URL is
// handled earlier, in NewFactory). An explicit kubeconfig is the operator's deliberate choice and is
// used unchanged (allowed even in strict mode); otherwise it defaults to the scoped, burrowd-only
// agent kubeconfig recorded for the matching handle (ADR-0038). It fails closed: a recorded scoped
// credential whose file is missing is always an error, and in strict mode a handle with no scoped
// credential is an error too, rather than escalating to the ambient/admin kubeconfig. It is exported
// so the resolver is unit-testable.
func ConnectOptions(kubeContext, kubeconfig, namespace string, strict bool, stderr io.Writer) (connect.Options, error) {
	opts := connect.Options{Kubeconfig: kubeconfig, Namespace: namespace, Context: kubeContext}
	if kubeconfig != "" {
		return opts, nil
	}
	agentKubeconfig, agentContext, err := scopedAgentKubeconfig(kubeContext, strict, stderr)
	if err != nil {
		return connect.Options{}, err
	}
	if agentKubeconfig != "" {
		opts.Kubeconfig = agentKubeconfig
		opts.Context = agentContext
	}
	return opts, nil
}

// scopedAgentKubeconfig resolves the scoped, burrowd-only kubeconfig for a requested kube context
// (ADR-0038). It finds the local handle whose context matches and returns its recorded
// AgentKubeconfig/AgentContext when the file is present. It fails closed in two cases:
//
//   - A handle that records a scoped credential whose file is missing is ALWAYS an error (even in
//     non-strict mode): a handle that declares a scoped credential and then can't find it is a
//     misconfiguration, never a reason to silently escalate to the ambient/admin kubeconfig.
//   - In strict mode, a context with no scoped credential at all — an empty or unregistered context,
//     or a handle installed before the scoped credential existed — is an error too, so the ambient
//     fallback is refused. The message names no environment variable: this package is binary-neutral.
//
// Otherwise, for a context with no scoped credential it returns "", "", nil to signal the non-strict
// ambient fallback (backward compatibility for pre-scoped-credential clusters).
func scopedAgentKubeconfig(kubeContext string, strict bool, _ io.Writer) (kubeconfig, kubeContextOut string, err error) {
	env, found := lookupByContext(kubeContext)
	if !found || env.AgentKubeconfig == "" {
		if strict {
			return "", "", fmt.Errorf("strict mode is set but no scoped agent credential is available for context %q; run \"burrow install\" to mint one", kubeContext)
		}
		return "", "", nil
	}
	if _, err := os.Stat(env.AgentKubeconfig); err != nil {
		return "", "", fmt.Errorf("scoped agent kubeconfig %q for environment %q is missing; run \"burrow upgrade\" (or \"burrow install\") to re-mint it. Refusing to fall back to the ambient kubeconfig.", env.AgentKubeconfig, env.Name)
	}
	return env.AgentKubeconfig, env.AgentContext, nil
}

// lookupByContext loads the local handle config and returns the environment registered for a kube
// context, and whether one exists. An empty context or a load failure reports no match, which in
// non-strict mode means the ambient fallback and in strict mode is refused by the caller.
func lookupByContext(kubeContext string) (localconfig.Environment, bool) {
	if kubeContext == "" {
		return localconfig.Environment{}, false
	}
	cfg, err := localconfig.Load()
	if err != nil {
		return localconfig.Environment{}, false
	}
	return cfg.LookupByContext(kubeContext)
}
