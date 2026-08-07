// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Command burrow-agent is the coding agent's control channel to Burrow: a capability-reduced,
// JSON-first command-line surface the agent invokes directly (ADR-0049). It carries the operate-verbs
// — the read-only siblings plus the mutating compute verbs (deploy, build, rollback, scale, autoscale, run) —
// while the ADMIN verbs (install, bootstrap, cluster setup, guard set, credential writes) are
// STRUCTURALLY ABSENT, not compiled into this binary, a stronger boundary than a runtime deny list.
// It authenticates to the control plane with the scoped control-plane credential (ADR-0038) and holds
// no cluster credentials (ADR-0005): in self-host it reaches the in-cluster control plane through the
// scoped, burrowd-only agent kubeconfig `burrow cluster install` mints and the Kubernetes API-server proxy
// (ADR-0014). Every command prints its result as indented JSON so the agent can pipe, grep, and jq it;
// a mutating verb prints a structured outcome envelope (executed, held_for_confirmation, denied, or
// error) the agent can branch on and relay to the human (ADR-0020). Setting
// BURROW_AGENT_REQUIRE_SCOPED refuses the ambient-kubeconfig fallback entirely.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/spf13/pflag"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/internal/agentconn"
	"github.com/burrow-cloud/burrow/internal/cloudcred"
	"github.com/burrow-cloud/burrow/internal/targetname"
	"github.com/burrow-cloud/burrow/localconfig"
)

// version is the Burrow release version this binary reports to the control plane (ADR-0039), sent as
// X-Burrow-Client-Version through agentconn. It is stamped at build time with
// `-ldflags "-X main.version=<tag>"` (goreleaser injects the release tag on a tagged build) and is
// EMPTY for a local `go build`, a `go install …@version`, or a test binary — exactly like the `burrow`
// CLI's own `version` var. It must not carry a hard-coded release default: an unstamped build that
// claims a real old tag is refused by the skew gate as an ancient release, when the truth is that it
// is a source build with nothing to upgrade to.
var version string

// agentVersion returns this binary's release version: the ldflags-stamped tag for a release build,
// otherwise the main-module version from the build info — set when it is installed with
// `go install …@version` or a Go pseudo-version for a local source build — or "dev" for an
// unversioned build. It mirrors the `burrow` CLI's cliVersion() so both binaries report a source
// build the same way, and so the ADR-0039 skew gate exempts both alike: a pseudo-version or "dev" is
// served rather than refused, because there is nothing for the user to upgrade to.
func agentVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		// A mutating verb that resolved to a non-executed outcome (held, denied, or an operation
		// error) has already printed its JSON envelope to stdout and returns an *exitError carrying
		// only the process exit code — no stderr line, so the JSON stays the sole machine-readable
		// output. Every other error is a genuine failure printed for a human.
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		// A read-only verb surfaces its error here rather than through an outcome envelope, so the
		// version-skew retarget has to run on this path too (issue #308).
		fmt.Fprintln(os.Stderr, "burrow-agent:", retargetTooOld(err))
		os.Exit(1)
	}
}

// run builds the root command and executes it with args. It is the single entry point the tests
// drive; building a fresh command tree each call keeps flag defaults (read from the environment) and
// output writers isolated per invocation.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return root.ExecuteContext(ctx)
}

// connOpts holds the control-plane connection flags every command binds.
type connOpts struct {
	controlPlane string
	token        string
	kubeconfig   string
	context      string
	namespace    string
	env          string
}

// bindConn registers the shared connection flags, defaulting from the environment.
func bindConn(flags *pflag.FlagSet, o *connOpts) {
	flags.StringVar(&o.controlPlane, "control-plane", os.Getenv("BURROW_CONTROL_PLANE_URL"), "control-plane API base URL (default: auto-connect via kubeconfig)")
	flags.StringVar(&o.token, "token", os.Getenv("BURROW_API_TOKEN"), "control-plane API token (default: read from the install Secret)")
	flags.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig for auto-connect (default: ambient)")
	flags.StringVar(&o.context, "context", "", "kubeconfig context to target (default: current context)")
	flags.StringVar(&o.namespace, "namespace", connect.DefaultNamespace, "namespace Burrow is installed in")
}

// bindEnv registers the --env flag naming the environment to operate in (ADR-0035 phase 2b). It is
// bound only on the env-scoped commands; an empty value means the default environment.
func bindEnv(flags *pflag.FlagSet, o *connOpts) {
	flags.StringVar(&o.env, "env", "", "environment to operate in (default: the default environment)")
}

// resolve builds a control-plane client, the environment name to send with the operation, and the
// name of the target it reached, applying the shared agentconn resolver's precedence (ADR-0038,
// ADR-0049). With --control-plane it talks to that URL directly and sends the raw --env. Otherwise it
// resolves the active handle (the pinned one, or the current kube context in follow mode) and applies
// the flag overrides, defaulting to the scoped, burrowd-only agent credential. Strict mode
// (BURROW_AGENT_REQUIRE_SCOPED) refuses the ambient fallback.
//
// The target it returns is what a mutating verb puts in its outcome envelope, so an agent composing
// a result can say WHERE the change happened rather than only what happened (ADR-0078 §4). It names
// what this invocation actually reached, which is why it is built here and not from the config alone
// (nameTarget).
func (o *connOpts) resolve(ctx context.Context, stderr io.Writer) (*client.Client, string, targetname.Named, error) {
	strict := truthy(os.Getenv("BURROW_AGENT_REQUIRE_SCOPED"))
	if o.controlPlane != "" {
		factory, err := agentconn.NewFactory(ctx, agentconn.Config{
			ControlPlaneURL: o.controlPlane,
			Token:           o.token,
			Name:            client.ClientNameAgent,
			Version:         agentVersion(),
		}, stderr)
		if err != nil {
			return nil, "", targetname.Named{}, err
		}
		c, err := factory("")
		if err != nil {
			return nil, "", targetname.Named{}, err
		}
		return c, o.env, targetname.ForControlPlane(o.controlPlane), nil
	}

	cfg, err := localconfig.Load()
	if err != nil {
		return nil, "", targetname.Named{}, err
	}
	// ResolveOperate rather than Resolve: the operate-verbs this binary carries work against either
	// kind of target, because the control-plane API is the same one whether it is reached through a
	// cluster's API server or over HTTPS at the managed product (ADR-0078 §1).
	resolved, err := localconfig.ResolveOperate(cfg, o.kubeconfig)
	if err != nil {
		return nil, "", targetname.Named{}, err
	}
	if resolved.Cloud() {
		return o.resolveCloud(ctx, cfg, resolved)
	}
	kubeContext := resolved.Context
	if o.context != "" {
		kubeContext = o.context
	}
	envName := resolved.Env
	if o.env != "" {
		envName = o.env
	}
	cpNamespace := resolved.ControlPlaneNamespace
	if o.namespace != "" && o.namespace != connect.DefaultNamespace {
		cpNamespace = o.namespace
	}
	factory, err := agentconn.NewFactory(ctx, agentconn.Config{
		ControlPlaneURL: o.controlPlane,
		Token:           o.token,
		Kubeconfig:      o.kubeconfig,
		Namespace:       cpNamespace,
		Strict:          strict,
		Name:            client.ClientNameAgent,
		Version:         agentVersion(),
	}, stderr)
	if err != nil {
		return nil, "", targetname.Named{}, err
	}
	c, err := factory(kubeContext)
	if err != nil {
		return nil, "", targetname.Named{}, err
	}
	return c, envName, o.nameTarget(cfg, resolved, kubeContext), nil
}

// nameTarget names what this invocation acted on, for the `target` member of the outcome envelope
// (ADR-0078 §4). It is built from the resolution and the context actually connected to, not from the
// config alone, so an override is named as what it was overridden TO and an unselected target reads
// as the kubeconfig it followed.
//
// A PINNED handle whose recorded kube context has been renamed away is the exception (issue #488).
// The resolution proceeds there because the handle's scoped credential (ADR-0038) carries the API
// server, the CA and the token and reaches the cluster with no context at all — so the recorded name
// decided nothing, and naming it would put a cluster that does not exist in the envelope, which is
// issue #473 in the machine-readable form an agent actually reads. targetname.ForHandle names the
// environment and no context, so the stale string cannot reach any field of the JSON.
//
// It applies only where the scoped credential is what the connection is made with — neither
// --kubeconfig nor --context set. With --context the person named the cluster for this one command
// and that is what was reached; with --kubeconfig the connection is sent through the recorded
// context, which is the very name that kubeconfig does not have, so it fails at connect and there is
// no envelope to name anything in.
//
// Nothing else is said about the staleness, deliberately. The `burrow` CLI notes it on stderr, which
// is not this binary's idiom: it prints one JSON document and, on a mutating verb, nothing else
// (ADR-0049 §1). The envelope has no channel for it either — outcome, operation, result, code,
// message, confirm_required, hint and target are the whole contract, and each describes the
// OPERATION (ADR-0020). A field carrying the stale name would also be one the agent cannot act on:
// re-pointing a handle is `burrow env use`, a config write structurally absent from this binary
// (ADR-0049 §2a), so relaying it would be noise in front of a fix only the person can apply — and
// their own surfaces already report it, `burrow auth status` on its contextMissing member and
// `burrow env list` on the row. What the envelope owes the agent is where the operation landed, and
// ForHandle's detail — the environment, reached with its own scoped credential — is exactly that.
func (o *connOpts) nameTarget(cfg *localconfig.Config, resolved localconfig.Resolved, kubeContext string) targetname.Named {
	if resolved.ContextStale != nil && o.kubeconfig == "" && o.context == "" {
		return targetname.ForHandle(resolved.Name)
	}
	return targetname.For(cfg, kubeContext, o.context != "")
}

// cloudBaseURL is the origin the managed product answers on, and the one seam a test redirects so no
// test reaches a real burrow-cloud.dev. It is a variable for that reason only; nothing changes it at
// runtime.
var cloudBaseURL = "https://" + localconfig.CloudEndpoint

// resolveCloud connects through a selected Burrow Cloud target. There is no cluster to resolve: the
// endpoint is the whole address, --env is sent as written, and the credential is the AGENT's — never
// the person's, which is the entire point of sign-in issuing two (cloud ADR-0028 §2). Revoking the
// agent's credential must stop the agent without signing the person's terminal out.
//
// The kubeconfig-shaped flags describe a cluster, so they cannot be honoured here and are refused by
// name rather than ignored: an agent that silently dropped --context would act somewhere nobody
// asked for.
func (o *connOpts) resolveCloud(ctx context.Context, cfg *localconfig.Config, resolved localconfig.Resolved) (*client.Client, string, targetname.Named, error) {
	if flag := o.kubeOnlyFlag(); flag != "" {
		return nil, "", targetname.Named{}, fmt.Errorf(
			"%s picks a cluster out of a kubeconfig, and the active target %q is the managed product at %s, which has no cluster of its own; drop the flag, or switch to a cluster target with \"burrow auth switch <name>\"",
			flag, resolved.Target, resolved.Endpoint)
	}
	t, ok := cfg.LookupTarget(resolved.Target)
	if !ok { // unreachable: ActiveTarget resolved it out of this same config
		return nil, "", targetname.Named{}, fmt.Errorf("the active target %q is not in the targets list", resolved.Target)
	}
	base := cloudBaseURL
	if resolved.Endpoint != localconfig.CloudEndpoint {
		base = "https://" + resolved.Endpoint
	}
	tr, err := cloudcred.Transport(base, cloudcred.KindAgent, client.ClientNameAgent, agentVersion())
	if err != nil {
		return nil, "", targetname.Named{}, err
	}
	c, err := tr.Connect(ctx)
	if err != nil {
		return nil, "", targetname.Named{}, err
	}
	return c, o.env, targetname.ForCloud(t), nil
}

// kubeOnlyFlag names the kubeconfig-shaped flag this invocation set, or "" if none did.
func (o *connOpts) kubeOnlyFlag() string {
	switch {
	case o.kubeconfig != "":
		return "--kubeconfig"
	case o.context != "":
		return "--context"
	case o.namespace != "" && o.namespace != connect.DefaultNamespace:
		return "--namespace"
	default:
		return ""
	}
}

// truthy reports whether an environment-variable value enables a flag: 1, true, or yes
// (case-insensitive, surrounding whitespace ignored). Empty, 0, or anything else is off.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// emitJSON writes v as indented JSON — the only output mode in Phase 1, so the agent can pipe, grep,
// and jq every result (ADR-0049).
func emitJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
