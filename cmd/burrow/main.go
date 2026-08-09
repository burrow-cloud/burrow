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
	"github.com/burrow-cloud/burrow/internal/clustercred"
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
// back by emitChange so a changing command names it (ADR-0078 §4). announced records that the
// targeting line has already been printed for this invocation, so the result does not answer the
// same question a second time (actedon.go). kubeContext/installID/contextResolved memoise what the
// privileged path resolved (clustercontext.go), so a command that resolves the cluster more than
// once gets one answer; notedStale does the same for the stale-handle note, which describes
// configuration that cannot change mid-invocation and so is worth saying exactly once. All of them
// are per-invocation state on a per-command struct, not global state: newRootCmd builds a fresh
// commonOpts for every command on every run, which is what keeps two commands in one process from
// seeing each other's target.
type commonOpts struct {
	controlPlane    string
	token           string
	kubeconfig      string
	context         string
	namespace       string
	env             string
	json            bool
	acted           targetname.Named
	announced       bool
	kubeContext     string
	installID       string
	contextResolved bool
	notedStale      bool
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
	flags.StringVar(&o.context, "context", "", "kubeconfig context to target (default: the active target, else the current context); selects which cluster's burrowd to operate")
	flags.BoolVar(&o.json, "json", false, "print the raw JSON result")
}

// client returns a control-plane client for a command that does not target an app — guard, cluster
// config, add-ons, credentials, audit, failures. It resolves the CLUSTER through the selected target
// (clustercontext.go) and deliberately does not resolve the active environment handle, so a pinned
// handle never silently redirects a cluster-setup or policy command (ADR-0036). Per-app commands use
// resolveAndConnect instead.
//
// stderr is where a divergence between the cluster reached and the target selected is reported; it
// is a parameter rather than a field so the note lands on the command's own error stream and never
// on stdout, where it would corrupt a --json result.
//
// The managed product is refused outright rather than resolved, because these commands act on a
// cluster and a managed tenant has none (clusteronly.go).
func (o *commonOpts) client(ctx context.Context, stderr io.Writer) (*client.Client, error) {
	if err := o.requireCluster(); err != nil {
		return nil, err
	}
	return o.clusterClient(ctx, stderr)
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

// readClient is client's sibling for a command that READS something the control plane answers over
// the same API on EITHER kind of target. A cluster target resolves exactly as it does in client —
// the cluster the selected target names, with no environment handle consulted — and the managed
// product, which client refuses because it has no cluster, is connected to at the endpoint the
// target names.
//
// The distinction it draws is between a route and a cluster. `burrow guard list`, `burrow cluster
// config list`, `burrow audit`, `burrow failures`, `burrow addon list`, `burrow addon logs` and
// `burrow addon metrics` never needed a cluster; they needed burrowd, and a managed tenant has one
// of those (cloud issue #202). Refusing them was the client not knowing the endpoint
// existed, not the endpoint being absent.
//
// A read moves here when the managed product actually serves its route. The verb is the transport's,
// not the test: `logs query` and `metrics query` are POSTs because a query does not fit in a path,
// and they change nothing. A command that CHANGES a tenant needs an answer to a further question —
// whether a tenant may — and moves to changeClient once that answer exists, or keeps calling client
// while it does not (clusteronly.go).
func (o *commonOpts) readClient(ctx context.Context, stderr io.Writer) (*client.Client, error) {
	return o.eitherTargetClient(ctx, stderr)
}

// changeClient is readClient for a command that CHANGES something, and it is a strictly higher bar:
// the route answering is necessary and not sufficient, because every person-side refusal in this file
// is a client-side convention rather than an enforced boundary. `tenantguard.PersonPolicy` in the
// managed control plane allows every guardrail on a person's credential, so the routes the CLI
// refuses would be served to the same person's bearer token by anything that spoke HTTP (cloud issue
// #208). What the refusal buys is therefore that the product has not said yes yet — not protection.
//
// So a command moves here when the product HAS said yes, and `addon attach` is the first: the managed
// product provisions a tenant's database through POST /v1/addons/attach, that route is what every
// attach on the platform has gone through, and the result is the `DATABASE_URL` a tenant's app reads
// (cloud issue #215). Refusing it in the client left a tenant with a documented command that could
// not do the documented thing, and left their AGENT — the surface ADR-0065 keeps deliberately
// narrower than the human's — able to attach a database the human's own CLI would not.
//
// Whether an attach should then be held for a human is a separate question with its own answer
// (ADR-0095): the guardrail belongs in the control plane, where it binds every caller, rather than in
// a client that only binds the one that happens to be this binary.
//
// The rest of the add-on writes — `install`, `connect`, `remove`, `backup`, `restore` — stay on
// client. Each is a question about what a managed tenant may do to an instance the platform operates
// and pays for, and none has been answered.
func (o *commonOpts) changeClient(ctx context.Context, stderr io.Writer) (*client.Client, error) {
	return o.eitherTargetClient(ctx, stderr)
}

// eitherTargetClient is the shared body of readClient and changeClient: the managed product is
// connected to at the endpoint the target names, and anything else resolves through client exactly as
// it did. Which commands may call it is the whole of the difference between the two, and it is
// documented on them rather than decided here.
func (o *commonOpts) eitherTargetClient(ctx context.Context, stderr io.Writer) (*client.Client, error) {
	tgt, cloud, err := o.cloudTargetIfSelected()
	if err != nil {
		return nil, err
	}
	if cloud {
		return o.connect(ctx, tgt)
	}
	return o.client(ctx, stderr)
}

// cloudTargetIfSelected builds the managed-product target when that is what is active, and reports
// false when it is not. It reads the local config and nothing else: ResolveOperate returns a cloud
// target before it opens a kubeconfig, so nothing on this path touches one.
//
// A config that will not load reports false rather than failing, the same tolerance refuseCloudTarget
// applies and for the same reason: the cluster path is about to load it again, and failing here would
// replace a legible message with a vaguer one.
func (o *commonOpts) cloudTargetIfSelected() (target, bool, error) {
	// --control-plane names the control plane outright, so no target is consulted for it here any
	// more than in requireCluster or resolveTarget.
	if o.controlPlane != "" {
		return target{}, false, nil
	}
	cfg, err := localconfig.Load()
	if err != nil {
		return target{}, false, nil
	}
	resolved, err := localconfig.ResolveOperate(cfg, o.kubeconfig)
	if err != nil || !resolved.Cloud() {
		return target{}, false, nil
	}
	tgt, err := o.cloudTarget(cfg, resolved)
	if err != nil {
		return target{}, false, err
	}
	return tgt, true, nil
}

// clusterClient is client without the cluster-only check, for the one caller that has already
// established it is acting on a cluster: `burrow cluster registry install`, whose DNS follow-on step
// talks to the burrowd it just installed alongside. Nothing else should use it.
func (o *commonOpts) clusterClient(ctx context.Context, stderr io.Writer) (*client.Client, error) {
	kubeContext, err := o.clusterContext(stderr)
	if err != nil {
		return nil, err
	}
	o.acted = o.namePrivilegedTarget(kubeContext)
	// The install id rides along now that the target decides this path too (ADR-0084 §5). Before, a
	// privileged command consulted no target and so had no id to send; sending none against a cluster
	// rebuilt under a reused context name is the bare 401 from an unexpected cluster that §5 exists to
	// turn into a message naming the cause.
	return o.connect(ctx, target{context: kubeContext, controlPlaneNamespace: o.namespace, installID: o.installID})
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
	// cloudEndpoint is set when the active target is the managed product (ADR-0078 §1): the endpoint
	// the target names, in place of the kube context there is none of. It is empty for every cluster
	// target, so the kubeconfig path is reached exactly as it was. cloudBaseURLFor turns it into the
	// origin to call, which for the managed product is not the same host.
	cloudEndpoint string
	// flagOverride records that a flag on this invocation named something other than what would
	// otherwise have been resolved: --context or --env on the resolved path, --env on the
	// --control-plane one. It is the one thing a READ-ONLY command still says out loud, because the
	// person asked for a target other than the active one and seeing that reflected is the point of
	// having asked.
	flagOverride bool
	// stale is the pinned handle whose recorded kube context is no longer in the kubeconfig, set only
	// when this invocation is connecting with the handle's scoped credential instead (issue #488). It
	// rides on the target so the note is printed where the command's other one-line notes are, on its
	// own stderr, rather than from inside resolution — localconfig reports conditions and commands
	// speak. It is nil whenever there is nothing to say.
	stale *localconfig.ContextMissingError
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
		// An explicit --env names itself here for the same reason it does below: the person asked for
		// an environment other than the one this invocation would otherwise use, and a read that
		// swallowed that would leave them guessing whether the flag took.
		return target{env: o.env, display: display, flagOverride: o.env != ""}, nil
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
		flagOverride:          o.context != "" || o.env != "",
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
	// A pinned handle whose recorded kube context has been renamed away resolved anyway, because its
	// scoped credential reaches the cluster without one (issue #488). Which of the three things that
	// means depends on what this invocation is actually connecting with:
	//
	//   - The scoped credential: the recorded name is read by nothing, so the command carries on and
	//     says the name is stale once. It is also NOT what was acted on, so the target is named from
	//     the handle — naming a context that no longer exists is issue #473 all over again.
	//   - An explicit --context: the person named the cluster for this command, and that is where it
	//     goes. The pin decided nothing, so there is nothing to say about it.
	//   - An explicit --kubeconfig with no --context: this turns the scoped credential off and sends
	//     the connection through the recorded context, which is precisely the name that kubeconfig
	//     does not have. Nothing is left to dial with, so the refusal stands.
	if resolved.ContextStale != nil {
		switch {
		case tgt.agentKubeconfig != "":
			tgt.stale = resolved.ContextStale
			o.acted = targetname.ForHandle(resolved.Name)
		case o.context == "":
			return target{}, resolved.ContextStale
		}
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
	return target{env: o.env, display: "targeting " + resolved.Render(), cloudEndpoint: resolved.Endpoint}, nil
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

// targetLine renders the one-line target shown on stderr before a command that CHANGES something,
// so a pin or a context switch is never silent (ADR-0036, ADR-0078 §1). It names the target and
// nothing else — "targeting prod" — because the kube context and namespace behind that name are
// Burrow's own resolution detail; `burrow auth status` reports them for anyone who wants them.
//
// An explicit --context or --env is the exception, and the only one. There the person asked for
// something other than the active target, so the line names the exact override instead: saying more
// is right precisely because they did something unusual, and it is the one line a read-only command
// prints too.
func targetLine(resolved localconfig.Resolved, ctxOverride, envOverride, finalContext, finalEnv string) string {
	if ctxOverride == "" && envOverride == "" {
		if resolved.Name == "" && resolved.Target == "" && resolved.Context == "" {
			// Nothing resolved, so there is nothing to say the command is targeting; Render's own
			// wording ("no current kube context") is the whole line.
			return resolved.Render()
		}
		return "targeting " + resolved.Render()
	}
	s := fmt.Sprintf("targeting context %q", finalContext)
	if finalEnv != "" {
		s += fmt.Sprintf(", env %q", finalEnv)
	}
	s += " (flag override)"
	return s
}

// resolveAndConnect resolves the active target (ADR-0036), names it on stderr so it never pollutes
// stdout or a --json result, connects to the resolved cluster, and returns the client and the
// burrowd env NAME to send with the operation.
//
// It is for the commands that CHANGE something. ADR-0078 §1 scopes the rule to those: the failure
// it guards against is acting on the wrong target, and a listing that acts on nothing pays a tax it
// does not owe — which is how the target line came to be the only line an empty read printed. A
// read-only command calls resolveAndConnectRead instead.
func (o *commonOpts) resolveAndConnect(ctx context.Context, stderr io.Writer) (*client.Client, string, error) {
	return o.resolveConnect(ctx, stderr, true)
}

// resolveAndConnectRead is resolveAndConnect for a command that only READS. It prints no target
// line: the result itself says which environment it describes, and a banner in front of a listing
// is noise. An explicit --context or --env still prints, because the person asked for a target
// other than the active one and should see that reflected.
//
// Which commands are which is not a judgement call made here. It is the classification every
// command already carries in changeguard_test.go: classChanges names its target, everything else
// does not.
func (o *commonOpts) resolveAndConnectRead(ctx context.Context, stderr io.Writer) (*client.Client, string, error) {
	return o.resolveConnect(ctx, stderr, false)
}

// resolveConnect is the shared body of the two: resolve, name the target if this invocation should,
// then connect.
func (o *commonOpts) resolveConnect(ctx context.Context, stderr io.Writer, changing bool) (*client.Client, string, error) {
	tgt, err := o.resolveTarget()
	if err != nil {
		return nil, "", err
	}
	// Printing it here is what lets the result line stay quiet about it: this line names the
	// environment (ADR-0036, #460), and a result that appended the target again would answer the
	// same question twice, in two vocabularies. It is recorded rather than inferred so the rule
	// holds for every command on this path without each one having to know it (actedon.go).
	if changing || tgt.flagOverride {
		fmt.Fprintln(stderr, tgt.display)
		o.announced = true
	}
	o.noteStaleHandleContext(tgt, stderr)
	tgt = applyScopedFallback(tgt, stderr)
	c, err := o.connect(ctx, tgt)
	if err != nil {
		return nil, "", err
	}
	return c, tgt.env, nil
}

// noteStaleHandleContext says that the pinned handle records a kube context the kubeconfig no longer
// holds, on an invocation that carried on regardless because the handle's scoped credential reaches
// the cluster without one (issue #488).
//
// It is a note and not a refusal because nothing on this path reads the recorded name — but it is
// said, rather than left silent, because the name is still wrong and two other things do read it:
// follow mode matches a handle to its context BY NAME, and `burrow env list` marks the row. Somebody
// who never sees this hears about it the first time they unpin.
//
// Once per invocation. A command that resolves more than once would otherwise repeat a line about a
// piece of configuration that has not changed in between.
func (o *commonOpts) noteStaleHandleContext(tgt target, stderr io.Writer) {
	if tgt.stale == nil || o.notedStale {
		return
	}
	o.notedStale = true
	fmt.Fprintf(stderr,
		"burrow: the pinned environment %q records kube context %q, which is no longer in your kubeconfig; continuing on its scoped credential, which needs none. Re-point the handle with \"burrow env use %s --context <context>\".\n",
		tgt.stale.Name, tgt.stale.Context, tgt.stale.Name)
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
		fmt.Fprintf(stderr, "burrow: scoped agent kubeconfig %s is missing; using the ambient kubeconfig (run \"burrow cluster install\" to re-create it)\n", tgt.agentKubeconfig)
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
//
// Each arm forwards the target's install id (ADR-0084 §5). On the --control-plane arm that is
// always empty today, and honestly so: naming a URL and a token bypasses target resolution
// entirely, so there is no recorded id to forward. The field is passed rather than omitted so the
// id travels the moment that path resolves a target, instead of the omission having to be noticed.
func (o *commonOpts) transport(tgt target) (client.Transport, error) {
	if o.controlPlane != "" {
		if o.token == "" {
			return nil, errors.New("--token (or BURROW_API_TOKEN) is required with --control-plane")
		}
		return client.DirectTransport{BaseURL: o.controlPlane, Token: o.token, Name: client.ClientNameCLI, Version: cliVersion(), InstallID: tgt.installID}, nil
	}
	if tgt.cloudEndpoint != "" {
		return cloudcred.Transport(cloudBaseURLFor(tgt.cloudEndpoint), cloudcred.KindCLI, client.ClientNameCLI, cliVersion())
	}
	return connect.KubeconfigTransport{Options: o.connectOptions(tgt)}, nil
}

// cloudBaseURLFor is the origin to call for a target's endpoint. The known managed endpoint goes
// through cloudBaseURL, the same variable the sign-in flow uses, so one seam points every call at
// the managed product and a test can redirect all of them at once.
//
// The comparison is against CloudEndpoint, the IDENTITY, because that is what a recorded target's
// endpoint field holds — including every target written before the console host existed. What comes
// back is the console (CloudAPIEndpoint) via cloudBaseURL, which is the whole reason this mapping is
// a function rather than a "https://" prefix.
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
		// The person's own credential for this install when they have signed in to it, and empty
		// otherwise — which falls back to the install's shared token and is what keeps an install
		// nobody has signed in to behaving exactly as it did (ADR-0084 §1).
		//
		// It is keyed by the INSTALL, not by the context: the credential belongs to the Burrow that
		// issued it, so a renamed context still finds it and a cluster rebuilt under a reused name
		// does not present the previous install's token to the new one. A target with no install id
		// looks nothing up, which is correct — there is no install for a credential to belong to.
		Token: clustercred.Token(clustercred.KindCLI, tgt.installID),
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
