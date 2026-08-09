// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/localconfig"
)

// `burrow auth` is how a person chooses WHERE Burrow is (ADR-0078). Until now the CLI decided what
// to talk to by reading whatever kubeconfig context happened to be current, which is not written
// down anywhere and cannot describe the managed product at all, because a managed tenant owns no
// cluster.
//
// A target is either the managed product or a Kubernetes cluster the person holds a context for.
// `auth login` asks which and records the selection; `auth status` says what is configured and which
// is active; `auth switch` changes the active one without re-authenticating. The existing --context
// flag keeps working as a per-invocation override for a Kubernetes target.
//
// Authenticating is NOT installing (ADR-0078 §3). Install is a one-time, cluster-admin act on a
// cluster; auth is per-person and repeatable. The second person to use a cluster brings their own
// kubeconfig context and installs nothing, so nothing here applies a manifest or contacts a
// cluster's control plane. Signing in to the managed product does obtain a credential for the
// person, from the managed product, over cloud ADR-0028's device flow — that is authentication, not
// installation, and it touches no cluster either.

// Selecting burrow-cloud.dev runs cloud ADR-0028's device flow, in cloudlogin.go. It is a real
// client in this binary and not a seam a managed build fills: one binary, two targets, which is
// ADR-0078's whole premise.

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Choose which Burrow you talk to, and prove who you are",
		Long: "auth chooses the TARGET your commands act on: either Burrow Cloud, or a Kubernetes cluster\n" +
			"you have a kubeconfig context for.\n\n" +
			"Authenticating is not installing. Install puts a control plane into a cluster once, and needs\n" +
			"cluster-admin; auth is per-person and repeatable, so the second person to use a cluster brings\n" +
			"their own kubeconfig context and installs nothing.\n\n" +
			"A Kubernetes target records the context NAME and never a copy of your credential, so rotating\n" +
			"your kubeconfig, re-issuing a certificate, or letting a provider CLI manage it all keep working.",
		// A bare `burrow auth` describes its subcommands and stops. It used to run `auth status`,
		// which loads the kubeconfig, resolves the active environment, and reports on an unwired
		// coding agent — checks and warnings nobody asked for from a group name. The status is worth
		// having and is unchanged; it belongs to the verb that names it.
		//
		// The RunE is kept, rather than dropped for Cobra's help-by-default, so that NoArgs is
		// reached at all: Cobra returns help (and exit 0) from a group with no RunE BEFORE it
		// validates args, which would make `auth bogus` look like it quietly worked. With one,
		// `auth bogus` is rejected by name. `burrow-agent addon` carries a RunE for the same reason.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newAuthLoginCmd(), newAuthInviteCmd(), newAuthStatusCmd(), newAuthSwitchCmd())
	return cmd
}

// --- login ---

// authLoginOpts are the inputs to a login: the non-interactive selections (--cloud / --context) and
// the kubeconfig the context list is read from.
type authLoginOpts struct {
	cloud       bool
	kubeContext string
	kubeconfig  string
	// name is the principal to record for a cluster sign-in (ADR-0084 §1). It is the handle an audit
	// row and a listing read as, not an authentication of anybody — burrowd authenticates the token
	// it issues, never this string — so it defaults to the environment's idea of who is at the
	// terminal and is worth overriding only when that is wrong.
	name string
	// invite is an invitation an admin of the install issued (ADR-0084 §2): the second person's way
	// in, exchanged here for a credential of their own. It replaces the claim rather than adding to
	// it — a claim is how the FIRST person on an install gets a credential, and everybody after them
	// is invited.
	invite string
}

func newAuthLoginCmd() *cobra.Command {
	o := authLoginOpts{}
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Choose where you use Burrow and make it the active target",
		Long: "login asks where you use Burrow and records the answer as your active target.\n\n" +
			"Pressing return selects " + localconfig.CloudEndpoint + ", the managed product: your browser opens\n" +
			"on an approval page, you check the code there matches the one printed here, and you approve. Two\n" +
			"credentials are issued — yours and burrow-agent's — and both are written to files only you can\n" +
			"read. Neither is displayed. With no browser to open, the URL is printed to visit yourself.\n\n" +
			"Choosing \"Other\" lists the contexts already in your kubeconfig, so you pick a cluster by a name\n" +
			"you recognise rather than typing a server URL. Only the context NAME is stored; nothing about\n" +
			"that path needs an account.\n\n" +
			"For a cluster it then asks that Burrow for a credential of your own, using your kubeconfig once\n" +
			"to prove you are an operator of it. Afterwards your kubeconfig is how the request GETS there and\n" +
			"the credential is what says who is asking. If the cluster is unreachable, has no Burrow in it, or\n" +
			"already has an admin, it says so and your commands keep using the install's shared token.\n\n" +
			"If somebody has invited you to their Burrow, pass their invitation with --invite and the cluster\n" +
			"with --context. That exchanges the invitation for a credential of your own, created on this\n" +
			"machine, and the invitation is spent. You need no cluster admin and no shared token for it.\n\n" +
			"It installs nothing. Pass --cloud or --context to select without a prompt.\n\n" +
			"Afterwards it offers to restrict an installed coding agent to burrow-agent, Burrow's scoped\n" +
			"control channel. Declining is fine and leaves a working setup; `burrow agent claude install`\n" +
			"does it later.",
		Example: "  # Ask where you use Burrow\n" +
			"  burrow auth login\n\n" +
			"  # Point at a cluster you already have a context for, without a prompt\n" +
			"  burrow auth login --context do-nyc1-cluster",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuthLogin(cmd.Context(), o, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&o.cloud, "cloud", false, "select "+localconfig.CloudEndpoint+" without prompting")
	cmd.Flags().StringVar(&o.kubeContext, "context", "", "select this kubeconfig context as the target without prompting")
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig the contexts are read from (default: ambient)")
	cmd.Flags().StringVar(&o.name, "name", "", "the name to record for you on this install's audit trail (default: your username)")
	cmd.Flags().StringVar(&o.invite, "invite", "", "an invitation from an admin of the cluster, exchanged here for a credential of your own")
	return cmd
}

func runAuthLogin(ctx context.Context, o authLoginOpts, in io.Reader, out io.Writer) error {
	if o.cloud && o.kubeContext != "" {
		return errors.New("--cloud and --context select different targets; pass one of them")
	}
	if err := o.checkInvite(); err != nil {
		return err
	}
	interactive := stdinIsTerminal(in)
	p := newPrompter(in, out)

	target, err := chooseTarget(ctx, o, p, out, interactive)
	if err != nil {
		return err
	}

	// A cluster target now asks that Burrow for a credential of the person's own (ADR-0084 §1). It
	// runs BEFORE the target is written so the install id it learns is recorded on the same entry:
	// SetTarget replaces an entry wholesale, so an id set afterwards would have to be a second write
	// of a target that had already been saved without one.
	var signIn signInResult
	switch {
	case target.Kind != localconfig.TargetKindKubernetes:
		// Burrow Cloud signs in through its own device flow, which issued the credentials already.
	case o.invite != "":
		// An invitation that cannot be exchanged fails the command and records nothing. There is no
		// fallback for the person holding one: they were given access so that they would NOT need
		// the install's shared token, so a recorded target they cannot authenticate to is a target
		// that answers every command with a 401 (ADR-0084 §2).
		signIn, err = acceptInvitation(ctx, o.kubeconfig, o.invite, &target)
		if err != nil {
			return err
		}
	default:
		signIn = signInToCluster(ctx, o.kubeconfig, o.principalName(), &target)
	}

	cfg, err := localconfig.Load()
	if err != nil {
		return err
	}
	// What this is about to REPLACE, read before it is replaced. Selecting a target re-points every
	// subsequent command, and until now that happened in silence: somebody with a cluster of their
	// own who signed in to the managed product watched `burrow app list` go from their apps to "No
	// apps deployed", with nothing on screen connecting the two (cloud#201).
	previous, hadPrevious, _ := cfg.ActiveTarget()

	if err := cfg.SetTarget(target); err != nil {
		return err
	}
	// Every local record naming this kube context reaches the install that just answered, so they all
	// learn its id — a handle left behind would make the mismatch check fire on which record happened
	// to be selected rather than on the cluster having actually changed (ADR-0084 §5).
	if target.InstallID != "" {
		cfg.SetInstallID(target.Context, target.InstallID)
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n%s Signed in to %s: %s.\n", okMark(out), target.Name, target.Describe())
	// A credential that WAS issued is part of what just succeeded; one that was not is a caveat about
	// a setup that works anyway. Marking them the same would either dress a caveat as an achievement
	// or make signing in successfully read like something went wrong.
	switch {
	case signIn.Issued:
		fmt.Fprintf(out, "%s %s\n", okMark(out), signIn.Line)
	case signIn.Line != "":
		fmt.Fprintf(out, "%s%s\n", note(out), signIn.Line)
	}
	fmt.Fprintln(out, "It is now your active target. See `burrow auth status`, or change it with `burrow auth switch <name>`.")
	if hadPrevious && previous.Name != target.Name {
		fmt.Fprintf(out, "\nYour commands were going to %s. Send them back there with:  burrow auth switch %s\n",
			previous.Describe(), previous.Name)
	}

	// The moment a person's setup finishes is when they are asked about their agent. It never fails
	// the login: the target is recorded above and stays recorded.
	offerAgentWiring(p, out, interactive)
	return nil
}

// chooseTarget resolves which target the login is for: the explicit flags, else the picker. It
// returns only a target that is real — a Kubernetes context that exists in the kubeconfig, or a
// completed cloud sign-in — so the caller records nothing it cannot act on.
func chooseTarget(ctx context.Context, o authLoginOpts, p *prompter, out io.Writer, interactive bool) (localconfig.Target, error) {
	switch {
	case o.cloud:
		return cloudSignIn(ctx, out, interactive)
	case o.kubeContext != "":
		return kubernetesTarget(o.kubeconfig, o.kubeContext)
	case !interactive:
		return localconfig.Target{}, errors.New(
			"`burrow auth login` asks where you use Burrow, so it needs a terminal.\n" +
				"Select without prompting instead: `burrow auth login --cloud`, or\n" +
				"`burrow auth login --context <kube-context>`")
	}

	choice, err := pickWhere(p)
	if err != nil {
		return localconfig.Target{}, err
	}
	if choice == whereCloud {
		return cloudSignIn(ctx, out, interactive)
	}
	return pickKubeContext(o.kubeconfig, p)
}

// where is the answer to the first question: the managed product, or a cluster of your own.
type where int

const (
	whereCloud where = iota
	whereOther
)

// pickWhere asks ADR-0078 §2's opening question. The managed product is the first item and the
// default, so someone who came for it reaches it by pressing return.
//
// The two entries carry no explanatory text on purpose. A person with no cluster and no interest in
// one must never be shown a Kubernetes concept, and "a Kubernetes cluster you have a kubeconfig for"
// hung beside "Other" is exactly that: a term they would have to read past to answer a question they
// have already answered. Everything Kubernetes stays behind the choice of Other, where the context
// list explains itself.
func pickWhere(p *prompter) (where, error) {
	fmt.Fprintf(p.w, "Where do you use Burrow?\n\n  1) %s\n  2) Other\n\n", localconfig.CloudEndpoint)

	n, err := pickIndex(p, "Select [1]: ", 2, 1)
	if err != nil {
		return whereCloud, err
	}
	if n == 1 {
		return whereCloud, nil
	}
	return whereOther, nil
}

// pickKubeContext lists the kubeconfig contexts and asks which one, so a cluster is picked by a name
// the person already recognises. With no kubeconfig it stops with errNoKubeconfigTarget rather than
// asking for a server URL.
func pickKubeContext(kubeconfig string, p *prompter) (localconfig.Target, error) {
	contexts, err := listContexts(kubeconfig)
	if err != nil || len(contexts) == 0 {
		return localconfig.Target{}, errNoKubeconfigTarget()
	}

	fmt.Fprint(p.w, "\nWhich cluster? These are the contexts in your kubeconfig:\n\n")
	def := 1
	tw := tabwriter.NewWriter(p.w, 0, 0, 3, ' ', 0)
	for i, c := range contexts {
		if c.Current {
			def = i + 1
			fmt.Fprintf(tw, "  %d) %s\t%s\t(current)\n", i+1, c.Name, c.Cluster)
			continue
		}
		fmt.Fprintf(tw, "  %d) %s\t%s\n", i+1, c.Name, c.Cluster)
	}
	_ = tw.Flush()
	fmt.Fprintln(p.w)

	n, err := pickIndex(p, fmt.Sprintf("Select [%d]: ", def), len(contexts), def)
	if err != nil {
		return localconfig.Target{}, err
	}
	return localconfig.KubernetesTarget(contexts[n-1].Name), nil
}

// pickAttempts bounds re-prompting after an unreadable answer, so a mistyped entry gets another go
// but a stream of junk cannot spin forever.
const pickAttempts = 3

// pickIndex reads a 1-based selection, returning def for an empty line (pressing return) and
// re-asking a bounded number of times on anything out of range.
func pickIndex(p *prompter, prompt string, count, def int) (int, error) {
	for attempt := 0; attempt < pickAttempts; attempt++ {
		answer, err := p.line(prompt)
		if err != nil {
			return 0, err
		}
		if answer == "" {
			return def, nil
		}
		if n, convErr := strconv.Atoi(answer); convErr == nil && n >= 1 && n <= count {
			return n, nil
		}
		fmt.Fprintf(p.w, "Enter a number between 1 and %d, or press return for %d.\n", count, def)
	}
	return 0, fmt.Errorf("no selection was made after %d attempts", pickAttempts)
}

// kubernetesTarget builds a Kubernetes target for a named context, checking it is actually in the
// kubeconfig first so a typo is caught here rather than at the next command that tries to connect.
func kubernetesTarget(kubeconfig, name string) (localconfig.Target, error) {
	contexts, err := listContexts(kubeconfig)
	if err != nil || len(contexts) == 0 {
		return localconfig.Target{}, errNoKubeconfigTarget()
	}
	if !contextExists(contexts, name) {
		return localconfig.Target{}, fmt.Errorf("context %q is not in your kubeconfig; available: %s", name, contextNames(contexts))
	}
	return localconfig.KubernetesTarget(name), nil
}

// errNoKubeconfigTarget is the clear stop when a Kubernetes target is asked for and there is no
// kubeconfig: it names what is missing, and that the managed product needs none of it. It is
// deliberately not a prompt for a server URL and not a degraded mode — a Kubernetes target requires
// a configured Kubernetes client, and a person who has none needs to hear that rather than be walked
// further into a flow that cannot complete (ADR-0078 §2).
func errNoKubeconfigTarget() error {
	return errors.New("no kubeconfig was found, so there is no cluster to point Burrow at.\n" +
		"A Kubernetes target needs a configured Kubernetes client: set $KUBECONFIG, or create\n" +
		"~/.kube/config. Burrow Cloud needs none of that, and is the first entry in the picker.")
}

// --- status ---

type authStatusOpts struct {
	kubeconfig string
	json       bool
}

func newAuthStatusCmd() *cobra.Command {
	o := authStatusOpts{}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the configured targets and which one is active",
		Long: "status lists the targets you have configured, says which is active and what each one is,\n" +
			"and flags a target whose kubeconfig context has gone missing.\n\n" +
			"It reads local state only: no cluster is contacted and nothing is changed. It also reports a\n" +
			"coding agent that is installed but not yet restricted to burrow-agent, since this is where a\n" +
			"person looks when asking whether their setup is right.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuthStatus(cmd.OutOrStdout(), o)
		},
	}
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig the contexts are checked against (default: ambient)")
	cmd.Flags().BoolVar(&o.json, "json", false, "print the raw JSON result")
	return cmd
}

// authTargetStatus is one target's row: what it is, whether it is active, and whether the kubeconfig
// still holds the context it names.
type authTargetStatus struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Detail  string `json:"detail"`
	Active  bool   `json:"active"`
	Missing bool   `json:"missing,omitempty"`
}

// authStatusResult is the JSON shape of `burrow auth status`.
//
// Environment, Context and Namespace describe where commands go right now: the environment the
// active target resolves to, and the cluster coordinates behind it. This is where the kube context
// and the app namespace are reported, because the target line a command prints names the
// environment alone. They are worth knowing; they are not worth printing unbidden on every command,
// so they are here, for somebody who asked. All three are absent for the managed product, which has
// no cluster.
//
// ContextMissing marks the one case where Context is a name and not a location: a pinned handle
// records a kube context the kubeconfig no longer holds, and commands reach the cluster with the
// handle's scoped credential instead (issue #488). Reporting the recorded name unqualified would
// name a cluster that does not exist, which is the failure this whole surface exists to catch.
type authStatusResult struct {
	Targets        []authTargetStatus `json:"targets"`
	Active         string             `json:"active"`
	Environment    string             `json:"environment,omitempty"`
	Context        string             `json:"context,omitempty"`
	ContextMissing bool               `json:"contextMissing,omitempty"`
	Namespace      string             `json:"namespace,omitempty"`
	AgentWired     bool               `json:"agentWired"`
	AgentPresent   bool               `json:"agentPresent"`
}

func runAuthStatus(out io.Writer, o authStatusOpts) error {
	cfg, err := localconfig.Load()
	if err != nil {
		return err
	}
	// Read the kubeconfig once so every Kubernetes target is checked against the same view of it. A
	// kubeconfig that cannot be read is not fatal here: status is what a person runs to find out
	// what is wrong, so it reports the targets and marks the contexts unverifiable.
	known := map[string]bool{}
	if contexts, cerr := listContexts(o.kubeconfig); cerr == nil {
		for _, c := range contexts {
			known[c.Name] = true
		}
	}

	result := authStatusResult{
		Active:       cfg.CurrentTarget,
		AgentPresent: claudeCodeDetected(),
	}
	// Where commands actually go, resolved the same way they resolve it. A resolution error is
	// swallowed on purpose: status is what somebody runs to find out what is wrong, and it reports
	// the targets either way rather than failing in place of the command that would have failed.
	if resolved, rerr := localconfig.ResolveOperate(cfg, o.kubeconfig); rerr == nil {
		result.Environment, result.Context, result.Namespace = resolved.Name, resolved.Context, resolved.Namespace
		result.ContextMissing = resolved.ContextStale != nil
	}
	result.AgentWired = result.AgentPresent && claudeCodeWired()
	for _, t := range cfg.Targets {
		row := authTargetStatus{
			Name:   t.Name,
			Kind:   string(t.Kind),
			Detail: t.Describe(),
			Active: t.Name == cfg.CurrentTarget,
		}
		if t.Kind == localconfig.TargetKindKubernetes && len(known) > 0 && !known[t.Context] {
			row.Missing = true
			row.Detail += " (not in your kubeconfig)"
		}
		result.Targets = append(result.Targets, row)
	}

	if o.json {
		return emit(out, true, result, "")
	}
	writeAuthStatus(out, result)
	return nil
}

// writeAuthStatus renders the human form: the targets as a table with the active one marked, a note
// for any target whose context has gone missing, and the agent line.
func writeAuthStatus(out io.Writer, r authStatusResult) {
	if len(r.Targets) == 0 {
		fmt.Fprintln(out, "No target is configured, so commands follow your current kube context.")
		fmt.Fprintln(out, "Choose where you use Burrow:  burrow auth login")
		writeActiveEnvironment(out, r)
		writeAgentStatusLine(out, r)
		return
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTIVE\tTARGET\tKIND\tDETAIL")
	for _, t := range r.Targets {
		marker := ""
		if t.Active {
			marker = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", marker, t.Name, t.Kind, t.Detail)
	}
	_ = tw.Flush()

	switch {
	case r.Active == "":
		fmt.Fprintln(out, "\nNo target is active, so commands follow your current kube context. Pick one with `burrow auth switch <name>`.")
	default:
		fmt.Fprintln(out, "\n* the active target. Change it with `burrow auth switch <name>`, or add another with `burrow auth login`.")
	}
	for _, t := range r.Targets {
		if t.Missing {
			fmt.Fprintf(out, "\nTarget %q names a kube context that is not in your kubeconfig. The kubeconfig may have\n"+
				"moved or the context may have been renamed; point at it again with `burrow auth login`.\n", t.Name)
		}
	}
	writeActiveEnvironment(out, r)
	writeAgentStatusLine(out, r)
}

// writeActiveEnvironment prints where commands go right now: the environment they resolve to, and
// the kube context and app namespace behind it.
//
// This is the home of the cluster coordinates. A command's target line names the environment and
// stops there, because that is the answer to "where is this going" and the rest is how Burrow found
// it. The rest is still worth knowing when something looks wrong, so it is reported HERE, to
// somebody who asked for it, instead of on every command to somebody who did not.
//
// Nothing is printed for the managed product: it has no cluster, and the targets table above
// already names the endpoint.
func writeActiveEnvironment(out io.Writer, r authStatusResult) {
	if r.Context == "" {
		return
	}
	fmt.Fprintln(out)
	if r.Environment != "" {
		fmt.Fprintf(out, "Commands target the %q environment.\n", r.Environment)
	} else {
		fmt.Fprintln(out, "No environment is registered for the context commands resolve to.")
	}
	namespace := r.Namespace
	if namespace == "" {
		namespace = "(the install's default)"
	}
	kubeContext := r.Context
	if r.ContextMissing {
		kubeContext += "  (not in your kubeconfig)"
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  kube context\t%s\n", kubeContext)
	fmt.Fprintf(tw, "  app namespace\t%s\n", namespace)
	_ = tw.Flush()
	if r.ContextMissing {
		fmt.Fprintf(out, "\nThe %q handle records a kube context that is not in your kubeconfig. Commands still reach\n"+
			"the cluster with the handle's scoped credential, which needs no context, but the recorded name\n"+
			"is wrong; re-point it with `burrow env use %s --context <context>`.\n", r.Environment, r.Environment)
	}
}

// writeAgentStatusLine adds the one actionable line about an unwired coding agent. It appears here
// and not on every command: a caveat is skimmed, and someone who installed on Monday and signed in
// on Friday has seen the pointer once, days earlier, in a wall of text.
func writeAgentStatusLine(out io.Writer, r authStatusResult) {
	if r.AgentPresent && !r.AgentWired {
		fmt.Fprintf(out, "\n%s%s\n", note(out), agentUnwiredStatusLine)
	}
}

// --- switch ---

func newAuthSwitchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "switch [name]",
		Short: "Make an already-configured target the active one",
		Long: "switch changes which configured target your commands act on, without re-authenticating.\n" +
			"Run it with no name to list what is configured.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return runAuthSwitch(name, cmd.OutOrStdout())
		},
	}
	return cmd
}

func runAuthSwitch(name string, out io.Writer) error {
	cfg, err := localconfig.Load()
	if err != nil {
		return err
	}
	if name == "" {
		if len(cfg.Targets) == 0 {
			return errors.New("no targets are configured; choose where you use Burrow with `burrow auth login`")
		}
		fmt.Fprintf(out, "Configured targets: %s\n", cfg.TargetNames())
		fmt.Fprintln(out, "Name one to make it active:  burrow auth switch <name>")
		return nil
	}
	if err := cfg.SwitchTarget(name); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	target, _ := cfg.LookupTarget(name)
	fmt.Fprintf(out, "Now targeting %s: %s.\n", target.Name, target.Describe())
	return nil
}
