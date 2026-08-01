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
// kubeconfig context and installs nothing, so nothing in this file applies a manifest, mints a
// credential, or contacts a cluster's control plane.

// errCloudSignInUnavailable is what selecting the managed product yields in this build. Signing in
// to Burrow Cloud is a device flow decided by cloud ADR-0028 and built in the managed product's own
// repository; the open-source CLI carries the target model, the picker and the local state, and
// deliberately does not carry a stub that pretends to sign in (ADR-0009).
var errCloudSignInUnavailable = errors.New(
	"signing in to " + localconfig.CloudEndpoint + " is not available in this build.\n" +
		"Burrow Cloud is the managed product and its sign-in ships with it; this open-source CLI\n" +
		"knows the target but has no way to authenticate to it yet, so nothing was recorded.\n\n" +
		"To run Burrow on your own Kubernetes cluster instead, choose \"Other\" at the prompt, or:\n" +
		"  burrow auth login --context <kube-context>")

// cloudSignInFn signs in to the managed product and returns the target to record. It is the seam
// the managed product's own build replaces with cloud ADR-0028's device flow; the open-source
// default authenticates nothing, makes no network call, and says so. A target is recorded only if
// this returns successfully, so a failed sign-in never leaves a target nothing can reach.
var cloudSignInFn = func(ctx context.Context, out io.Writer) (localconfig.Target, error) {
	return localconfig.Target{}, errCloudSignInUnavailable
}

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
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuthStatus(cmd.OutOrStdout(), authStatusOpts{})
		},
	}
	cmd.AddCommand(newAuthLoginCmd(), newAuthStatusCmd(), newAuthSwitchCmd())
	return cmd
}

// --- login ---

// authLoginOpts are the inputs to a login: the non-interactive selections (--cloud / --context) and
// the kubeconfig the context list is read from.
type authLoginOpts struct {
	cloud       bool
	kubeContext string
	kubeconfig  string
}

func newAuthLoginCmd() *cobra.Command {
	o := authLoginOpts{}
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Choose where you use Burrow and make it the active target",
		Long: "login asks where you use Burrow and records the answer as your active target.\n\n" +
			"Pressing return selects " + localconfig.CloudEndpoint + ", the managed product. Choosing \"Other\"\n" +
			"lists the contexts already in your kubeconfig, so you pick a cluster by a name you recognise\n" +
			"rather than typing a server URL. Only the context NAME is stored; your credential stays in the\n" +
			"kubeconfig.\n\n" +
			"It installs nothing and contacts no cluster. Pass --cloud or --context to select without a\n" +
			"prompt.\n\n" +
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
	return cmd
}

func runAuthLogin(ctx context.Context, o authLoginOpts, in io.Reader, out io.Writer) error {
	if o.cloud && o.kubeContext != "" {
		return errors.New("--cloud and --context select different targets; pass one of them")
	}
	interactive := stdinIsTerminal(in)
	p := newPrompter(in, out)

	target, err := chooseTarget(ctx, o, p, out, interactive)
	if err != nil {
		return err
	}

	cfg, err := localconfig.Load()
	if err != nil {
		return err
	}
	if err := cfg.SetTarget(target); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n%s Signed in to %s: %s.\n", okMark(out), target.Name, target.Describe())
	fmt.Fprintln(out, "It is now your active target. See `burrow auth status`, or change it with `burrow auth switch <name>`.")

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
		return cloudSignInFn(ctx, out)
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
		return cloudSignInFn(ctx, out)
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
type authStatusResult struct {
	Targets      []authTargetStatus `json:"targets"`
	Active       string             `json:"active"`
	AgentWired   bool               `json:"agentWired"`
	AgentPresent bool               `json:"agentPresent"`
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
	writeAgentStatusLine(out, r)
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
