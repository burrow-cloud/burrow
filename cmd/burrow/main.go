// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Command burrow is the Burrow CLI: the human-facing way to operate Burrow. It calls the
// same control-plane API the agent's control channel does (ADR-0002) — deploy by image
// reference, status, logs, rollback, scale — and can build and push an image first (the
// client-side build path, ADR-0008). Like `burrow-agent` it carries no orchestration logic and
// no cluster credentials, only the control-plane API token (ADR-0005, ADR-0049). Its command
// surface is built with Cobra (ADR-0019).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/internal/cloudcred"
	"github.com/burrow-cloud/burrow/internal/targetname"
	"github.com/burrow-cloud/burrow/localconfig"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		// errArgsReported is already printed (the friendly message + the command's usage), so
		// don't print it again — just exit non-zero.
		if !errors.Is(err, errArgsReported) {
			fmt.Fprintln(os.Stderr, "burrow:", err)
		}
		os.Exit(1)
	}
}

// errArgsReported is returned by exactArgs after it has printed a helpful message naming the
// expected arguments and the command's usage; main exits non-zero without reprinting it.
var errArgsReported = errors.New("invalid arguments (already reported)")

// exactArgs requires exactly n positional arguments. On a mismatch it prints a plain message
// naming the arguments the command expects — drawn from its Use line, e.g. "<app>" — followed
// by the command's usage, so a user who forgets an argument sees what to pass instead of
// Cobra's bare "accepts 1 arg(s), received 0".
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == n {
			return nil
		}
		w := cmd.ErrOrStderr()
		expected := usageArgs(cmd.Use)
		if len(args) < n {
			fmt.Fprintf(w, "%s needs %s.\n\n", cmd.CommandPath(), expected)
		} else {
			fmt.Fprintf(w, "%s takes only %s.\n\n", cmd.CommandPath(), expected)
		}
		fmt.Fprint(w, cmd.UsageString())
		return errArgsReported
	}
}

// usageArgs extracts the argument placeholders from a command's Use line — the <...> / [...]
// tokens after the command name, e.g. "scale <app> <replicas>" -> "<app> <replicas>".
func usageArgs(use string) string {
	fields := strings.Fields(use)
	var parts []string
	for _, f := range fields[1:] { // skip the command name itself
		if strings.HasPrefix(f, "<") || strings.HasPrefix(f, "[") {
			parts = append(parts, f)
		}
	}
	if len(parts) == 0 {
		return "different arguments"
	}
	return strings.Join(parts, " ")
}

// run builds the root command and executes it with args. It is the single entry point the
// tests drive; building a fresh command tree each call keeps flag defaults (read from the
// environment) and output writers isolated per invocation.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return root.ExecuteContext(ctx)
}

func newRootCmd() *cobra.Command {
	// List commands in the order they are added (golden-path order within each group, ADR-0037)
	// rather than Cobra's default alphabetical sort.
	cobra.EnableCommandSorting = false
	root := &cobra.Command{
		Use:           "burrow",
		Short:         rootShortDesc,
		Long:          rootLongDesc,
		SilenceUsage:  true,
		SilenceErrors: true,
		// RunE handles only a truly bare `burrow` (no subcommand): Cobra rejects an unknown
		// subcommand before RunE, and `-h`/`--help` short-circuits to help before RunE, so neither
		// reaches here. A first-run user (no ~/.burrow/config) gets just the install banner instead
		// of the full command wall; once set up, bare `burrow` falls through to the grouped help, the
		// same as `burrow -h` (ADR-0037).
		RunE: func(cmd *cobra.Command, _ []string) error {
			if exists, err := localconfig.Exists(); err == nil && !exists {
				fmt.Fprint(cmd.OutOrStdout(), firstRunBanner)
				return nil
			}
			return cmd.Help()
		},
	}
	// Render help in kubectl's order (description, examples, commands, flags, then a single Usage
	// line at the bottom) on the root; subcommands inherit the templates.
	applyHelpLayout(root)
	// The completion command stays visible so a user can discover it (ADR-0037); Cobra's built-in
	// covers bash, zsh, fish, and PowerShell.
	addGroups(root)
	root.AddCommand(
		// Get started: install, point at a cluster, configure credentials (ADR-0037). install and
		// upgrade now live under `burrow cluster` (the cluster-lifecycle surface); the top-level
		// `burrow install` / `burrow upgrade` stay as deprecated, hidden aliases so existing muscle
		// memory and scripts keep working (they print a one-line migration hint).
		// auth leads the golden path: it is where a person says which Burrow they talk to, and it
		// is the first command anyone runs (ADR-0078).
		grouped(newAuthCmd(), groupGetStarted),
		grouped(newJoinCmd(), groupGetStarted),
		newInstallAliasCmd(),
		newUpgradeAliasCmd(),
		// A hidden stub that points an old guide's `burrow mcp ...` at `burrow agent <tool> install`
		// (ADR-0062 §2). Temporary: remove it, with mcp_removed.go, after the next release.
		newMcpRemovedCmd(),
		grouped(newAgentCmd(), groupGetStarted),
		grouped(newClusterCmd(), groupGetStarted),
		grouped(newConfigCmd(), groupGetStarted),
		// Environments: select which cluster/namespace commands target.
		grouped(newEnvCmd(), groupEnvironments),
		// Operate: act on deployed applications and their add-ons.
		grouped(newAppCmd(), groupOperate),
		grouped(newAddonCmd(), groupOperate),
		// The cluster-wide failure listing (ADR-0074 §8). It sits under Operate rather than Govern
		// because it is what someone runs when something is wrong, alongside the app they are
		// operating — and it is top level rather than under `app` because its whole point is that it
		// spans every object Burrow manages, not one app.
		grouped(newFailuresCmd(), groupOperate),
		// Govern: guardrail policy and the audit trail.
		grouped(newGuardCmd(), groupGovern),
		grouped(newAuditCmd(), groupGovern),
		// version (and the auto-generated completion/help) fall in the default group.
		newVersionCmd(),
	)
	return root
}

// commonOpts holds the configuration the control-plane operations share.
//
// acted is the target this invocation reached, recorded when the connection is resolved and read
// back by emitChange so a changing command names it (ADR-0078 §4). It is per-invocation state on a
// per-command struct, not global state: newRootCmd builds a fresh commonOpts for every command on
// every run, which is what keeps two commands in one process from seeing each other's target.
type commonOpts struct {
	controlPlane string
	token        string
	kubeconfig   string
	context      string
	namespace    string
	env          string
	json         bool
	acted        targetname.Named
}

// bindCommon registers the shared flags on the flag set, defaulting from the environment.
func bindCommon(flags *pflag.FlagSet, o *commonOpts) {
	bindClientFlags(flags, o)
	flags.StringVar(&o.namespace, "namespace", connect.DefaultNamespace, "namespace Burrow is installed in")
}

// bindEnv registers the --env flag selecting which namespace-per-environment to operate in (ADR-0035
// phase 2b). It is added only to the per-app operation commands; an empty value means the default
// environment. The flag is distinct from --context (which selects the cluster) and from `burrow app
// config` (an app's environment variables).
func bindEnv(flags *pflag.FlagSet, o *commonOpts) {
	flags.StringVar(&o.env, "env", "", "environment to operate in (default: the default environment)")
}

// bindClientFlags registers the control-plane connection flags without --namespace, so a command
// that needs --namespace for a different meaning (e.g. `env add`, where it is the environment's
// namespace) can bind the control-plane namespace under its own flag name.
func bindClientFlags(flags *pflag.FlagSet, o *commonOpts) {
	flags.StringVar(&o.controlPlane, "control-plane", os.Getenv("BURROW_CONTROL_PLANE_URL"), "control-plane API base URL (default: auto-connect via kubeconfig)")
	flags.StringVar(&o.token, "token", os.Getenv("BURROW_API_TOKEN"), "control-plane API token (default: read from the install Secret)")
	flags.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig for auto-connect (default: ambient)")
	flags.StringVar(&o.context, "context", "", "kubeconfig context to target (default: current context); selects which cluster's burrowd to operate")
	flags.BoolVar(&o.json, "json", false, "print the raw JSON result")
}

// client returns a control-plane client for the raw connection flags (--context, --namespace),
// without resolving the active environment handle. Commands that do not target an app
// (install, env add, guard, audit, addon) use it so a pinned handle never silently redirects a
// cluster-setup or policy command. Per-app commands use resolveAndConnect instead (ADR-0036).
//
// Because this path reaches a cluster and never consults the selected target, it refuses outright
// while the managed product is selected rather than acting on whatever the kubeconfig points at
// (clusteronly.go).
func (o *commonOpts) client(ctx context.Context) (*client.Client, error) {
	if err := o.requireCluster(); err != nil {
		return nil, err
	}
	return o.clusterClient(ctx)
}

// requireCluster refuses this invocation when it needs a cluster and the active target is the
// managed product (clusteronly.go). --control-plane names the control plane outright, so no target
// is consulted for it — here or in resolveTarget, which returns early on it for the same reason.
// A command that writes to a cluster before it connects calls this itself, first.
func (o *commonOpts) requireCluster() error {
	if o.controlPlane != "" {
		return nil
	}
	return refuseCloudTarget()
}

// clusterClient is client without the cluster-only check, for the one caller that has already
// established it is acting on a cluster: `burrow cluster registry install`, whose DNS follow-on step
// talks to the burrowd it just installed alongside. Nothing else should use it.
func (o *commonOpts) clusterClient(ctx context.Context) (*client.Client, error) {
	o.acted = o.namePrivilegedTarget()
	return o.connect(ctx, target{context: o.context, controlPlaneNamespace: o.namespace})
}

// target is the resolved target a per-app command acts against (ADR-0036 slice 5a): the kube
// context to connect to, the control-plane namespace burrowd runs in, the burrowd-registered
// environment NAME to send with the operation, and a human display string. resolveTarget folds
// the pinned/followed handle together with the --context/--env/--namespace overrides to build it.
type target struct {
	context               string
	controlPlaneNamespace string
	env                   string
	display               string
	// agentKubeconfig/agentContext carry the resolved handle's scoped, burrowd-only credential
	// (ADR-0038 phase 2). resolveTarget sets them only for the operate path and only when the user
	// overrode neither --kubeconfig nor --context nor --control-plane, so connect defaults to the
	// scoped credential there; they stay empty for the privileged path (client) and for any explicit
	// override, which both keep the ambient/admin kubeconfig.
	agentKubeconfig string
	agentContext    string
	// cloudEndpoint is set when the active target is the managed product (ADR-0078 §1): the host its
	// control plane answers on, in place of the kube context there is none of. It is empty for every
	// cluster target, so the kubeconfig path is reached exactly as it was.
	cloudEndpoint string
	// installID is the Burrow install the selected target was pointed at (ADR-0084 §5), sent on every
	// request so a control plane that is a different install refuses rather than acts. It comes from
	// the target and nowhere else: a context name is how the request gets there, and this is what
	// says it arrived. Empty when no target is selected, when the target predates install ids, or
	// when it is the managed product — all of which are served exactly as they are today.
	installID string
}

// resolveTarget decides which cluster + environment a per-app command targets (ADR-0036). With
// --control-plane it talks to that URL directly and sends the raw --env, unchanged. Otherwise it
// resolves the active handle (the pinned one, or the current kube context in follow mode) and
// applies the flag overrides: --context replaces the kube context, --env replaces the burrowd env
// name, an explicit --namespace replaces the control-plane namespace. The env value is always a
// registered env NAME (or empty for the cluster's default namespace and global guardrails), never
// a raw namespace, because burrowd resolves a NAME and errors on an unknown one.
func (o *commonOpts) resolveTarget() (target, error) {
	if o.controlPlane != "" {
		display := fmt.Sprintf("targeting control plane at %s", o.controlPlane)
		if o.env != "" {
			display += fmt.Sprintf(" (env %q)", o.env)
		}
		o.acted = targetname.ForControlPlane(o.controlPlane)
		return target{env: o.env, display: display}, nil
	}
	cfg, err := localconfig.Load()
	if err != nil {
		return target{}, err
	}
	// ResolveOperate rather than Resolve: an ordinary app command works against either kind of target,
	// because the control-plane API it calls is the same one whether it is reached through a cluster's
	// API server or over HTTPS at the managed product (ADR-0078 §1).
	resolved, err := localconfig.ResolveOperate(cfg, o.kubeconfig)
	if err != nil {
		return target{}, err
	}
	if resolved.Cloud() {
		return o.cloudTarget(cfg, resolved)
	}
	kubeContext := resolved.Context
	if o.context != "" {
		kubeContext = o.context
	}
	env := resolved.Env
	if o.env != "" {
		env = o.env
	}
	cpn := resolved.ControlPlaneNamespace
	if o.namespace != "" && o.namespace != connect.DefaultNamespace {
		cpn = o.namespace
	}
	if cpn == "" {
		cpn = connect.DefaultNamespace
	}
	tgt := target{
		context:               kubeContext,
		controlPlaneNamespace: cpn,
		env:                   env,
		display:               targetLine(resolved, o.context, o.env, kubeContext, env),
		installID:             resolved.InstallID,
	}
	// An explicit --context is a deliberate choice of a different cluster from the one the target
	// names, so the target's install id no longer describes what is on the other end and is dropped.
	// Carrying it would refuse the override on the grounds that it is an override.
	if o.context != "" {
		tgt.installID = ""
	}
	// Name the target from the context this command will actually connect to, so an override is
	// named as what it was overridden TO and an unselected target reads as the kubeconfig it followed
	// (ADR-0078 §4).
	o.acted = targetname.For(cfg, kubeContext, o.context != "")
	// Default the operate path to the scoped, burrowd-only agent credential (ADR-0038 phase 2) when
	// the resolved handle carries one and the user overrode neither the kubeconfig nor the context.
	// An explicit --kubeconfig or --context is a deliberate choice of the ambient/admin path, so it
	// leaves the agent fields unset; --control-plane returns earlier and never reaches here.
	if o.kubeconfig == "" && o.context == "" && resolved.AgentKubeconfig != "" {
		tgt.agentKubeconfig = resolved.AgentKubeconfig
		tgt.agentContext = resolved.AgentContext
	}
	return tgt, nil
}

// cloudTarget builds the target for a selected Burrow Cloud target. There is no cluster to resolve:
// the endpoint is the whole address, --env is sent as written (the managed control plane resolves
// environment names the same way a self-hosted one does), and the credential is picked up later, at
// connect time, so nothing reads a token to decide where a command is going.
func (o *commonOpts) cloudTarget(cfg *localconfig.Config, resolved localconfig.Resolved) (target, error) {
	if flag := o.kubeOnlyFlag(); flag != "" {
		return target{}, fmt.Errorf(
			"%s picks a cluster out of your kubeconfig, and the active target %q is the managed product at %s, which has no cluster of its own. Drop the flag, or switch to a cluster target with \"burrow auth switch <name>\"",
			flag, resolved.Target, resolved.Endpoint)
	}
	t, ok := cfg.LookupTarget(resolved.Target)
	if !ok { // unreachable: ActiveTarget resolved it out of this same config
		return target{}, fmt.Errorf("the active target %q is not in the targets list; choose one with \"burrow auth switch <name>\"", resolved.Target)
	}
	o.acted = targetname.ForCloud(t)
	return target{env: o.env, display: resolved.Render(), cloudEndpoint: resolved.Endpoint}, nil
}

// kubeOnlyFlag names the kubeconfig-shaped flag this invocation set, or "" if none did. Those flags
// describe a cluster, so with the managed product selected they cannot be honoured — and a command
// that quietly ignored one would run somewhere the person did not ask for. Naming the flag is the
// difference between an error a reader can act on and a kubeconfig failure that sends them hunting
// for a cluster problem they do not have.
func (o *commonOpts) kubeOnlyFlag() string {
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

// targetLine renders the one-line target shown on stderr before a per-app operation so a context
// switch or a pin is never silent (ADR-0036). With no flag overrides it uses the resolved handle's
// own description (pinned, or following the current kube context); an explicit --context or --env
// names the exact override target instead.
func targetLine(resolved localconfig.Resolved, ctxOverride, envOverride, finalContext, finalEnv string) string {
	if ctxOverride == "" && envOverride == "" {
		// ModeTargeted needs no "targeting" prefix: Render already leads with the target it resolved
		// through, and prefixing it would stutter ("targeting target ...").
		if resolved.Mode == localconfig.ModePinned {
			return "targeting " + resolved.Render()
		}
		return resolved.Render()
	}
	s := fmt.Sprintf("targeting context %q", finalContext)
	if finalEnv != "" {
		s += fmt.Sprintf(", env %q", finalEnv)
	}
	s += " (flag override)"
	return s
}

// resolveAndConnect resolves the active target (ADR-0036), prints it to stderr so it never
// pollutes stdout or a --json result, connects to the resolved cluster, and returns the client and
// the burrowd env NAME to send with the operation.
func (o *commonOpts) resolveAndConnect(ctx context.Context, stderr io.Writer) (*client.Client, string, error) {
	tgt, err := o.resolveTarget()
	if err != nil {
		return nil, "", err
	}
	fmt.Fprintln(stderr, tgt.display)
	tgt = applyScopedFallback(tgt, stderr)
	c, err := o.connect(ctx, tgt)
	if err != nil {
		return nil, "", err
	}
	return c, tgt.env, nil
}

// applyScopedFallback keeps the resolved scoped agent credential only when its kubeconfig file is
// present. If a handle recorded one but the file is gone (deleted, or a config synced to a machine
// that never minted it), it clears the agent fields so connect falls back to the ambient kubeconfig
// and prints a brief one-line note to stderr rather than failing hard (ADR-0038 phase 2; `burrow
// install` re-creates the credential). It is a no-op when no scoped credential was resolved.
func applyScopedFallback(tgt target, stderr io.Writer) target {
	if tgt.agentKubeconfig == "" {
		return tgt
	}
	if _, err := os.Stat(tgt.agentKubeconfig); err != nil {
		fmt.Fprintf(stderr, "burrow: scoped agent kubeconfig %s is missing; using the ambient kubeconfig (run \"burrow install\" to re-create it)\n", tgt.agentKubeconfig)
		tgt.agentKubeconfig = ""
		tgt.agentContext = ""
	}
	return tgt
}

// connect builds a control-plane client for a target. With --control-plane set it talks to that
// URL directly (e.g. an ingress) using --token. Otherwise it auto-connects through the Kubernetes
// API-server proxy, reading the token from the install Secret in the target's control-plane
// namespace, so a developer with kubectl access configures nothing (ADR-0014). The operate path
// defaults to the scoped, burrowd-only agent credential when the target carries one (ADR-0038
// phase 2); the privileged path and any explicit override keep the ambient/admin kubeconfig.
func (o *commonOpts) connect(ctx context.Context, tgt target) (*client.Client, error) {
	tr, err := o.transport(tgt)
	if err != nil {
		return nil, err
	}
	return tr.Connect(ctx)
}

// transport selects the control-plane transport for a target (ADR-0045). With --control-plane set
// it returns a directTransport for that URL and --token; otherwise the default kubeconfig API-server
// proxy transport, carrying the scoped-credential and namespace choices connectOptions resolves. The
// direct-vs-kubeconfig branch stays here so command files stay unchanged; a private module can plug a
// different Transport in without touching them.
func (o *commonOpts) transport(tgt target) (client.Transport, error) {
	if o.controlPlane != "" {
		if o.token == "" {
			return nil, errors.New("--token (or BURROW_API_TOKEN) is required with --control-plane")
		}
		return client.DirectTransport{BaseURL: o.controlPlane, Token: o.token, Name: client.ClientNameCLI, Version: cliVersion()}, nil
	}
	if tgt.cloudEndpoint != "" {
		return cloudcred.Transport(cloudBaseURLFor(tgt.cloudEndpoint), cloudcred.KindCLI, client.ClientNameCLI, cliVersion())
	}
	return connect.KubeconfigTransport{Options: o.connectOptions(tgt)}, nil
}

// cloudBaseURLFor is the origin to call for a target's endpoint. The known managed endpoint goes
// through cloudBaseURL, the same variable the sign-in flow uses, so one seam points every call at
// the managed product and a test can redirect all of them at once.
func cloudBaseURLFor(endpoint string) string {
	if endpoint == localconfig.CloudEndpoint {
		return cloudBaseURL
	}
	return "https://" + endpoint
}

// connectOptions builds the auto-connect options for a target. It uses the scoped, burrowd-only
// agent kubeconfig recorded on the target when present (ADR-0038 phase 2), overriding both the
// kubeconfig path and the context with the scoped credential's own; otherwise it uses --kubeconfig
// (empty means ambient) and the target's context. resolveTarget sets the agent fields only for the
// operate path with no --kubeconfig/--context override, so the privileged path (client) and any
// explicit override fall through to the ambient/admin kubeconfig here.
func (o *commonOpts) connectOptions(tgt target) connect.Options {
	kubeconfig := o.kubeconfig
	kubeContext := tgt.context
	if tgt.agentKubeconfig != "" {
		kubeconfig = tgt.agentKubeconfig
		kubeContext = tgt.agentContext
	}
	return connect.Options{
		Kubeconfig:    kubeconfig,
		Context:       kubeContext,
		Namespace:     tgt.controlPlaneNamespace,
		ClientName:    client.ClientNameCLI,
		ClientVersion: cliVersion(),
		InstallID:     tgt.installID,
	}
}

// emit prints v as indented JSON when asJSON, otherwise the human-readable line.
func emit(w io.Writer, asJSON bool, v any, human string) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	fmt.Fprintln(w, human)
	return nil
}

// kvFlag collects repeated KEY=VALUE flags into a map. It satisfies pflag.Value.
type kvFlag struct{ m map[string]string }

func (f *kvFlag) String() string { return "" }

func (f *kvFlag) Type() string { return "KEY=VALUE" }

func (f *kvFlag) Set(s string) error {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return fmt.Errorf("expected KEY=VALUE, got %q", s)
	}
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.m[s[:i]] = s[i+1:]
	return nil
}
