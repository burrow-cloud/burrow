// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package localconfig

import (
	"fmt"

	"k8s.io/client-go/tools/clientcmd"
)

// Mode is how the active target was selected: pinned to a named handle, or following the
// current kube context.
type Mode string

const (
	// ModePinned means a handle was pinned with `burrow env use`.
	ModePinned Mode = "pinned"
	// ModeFollowing means the target tracks the current kube context (the default).
	ModeFollowing Mode = "following"
	// ModeTargeted means an ADR-0078 target chosen with `burrow auth login` decided the cluster,
	// and no pinned handle inside that cluster narrowed it further.
	ModeTargeted Mode = "targeted"
)

// Resolved is the concrete target a command will act against, derived from the config and
// the kubeconfig. Namespace is the app namespace (for display); empty means the caller falls
// back to the burrowd default app namespace. Env is the burrowd-registered environment NAME to
// send with the operation (empty means the cluster's default namespace and global guardrails);
// it is what burrowd resolves, not a raw namespace. In follow mode Name (and Env) are empty
// when the current context matches no registered handle (an "unregistered" current context).
// AgentKubeconfig/AgentContext carry the resolved handle's scoped, burrowd-only credential
// (ADR-0038) so the operate path can default to it; both are empty when the handle records none.
// Target names the ADR-0078 target that decided the cluster, and is empty when no target is
// selected (the pre-ADR-0078 behaviour, and still the default). Kind is that target's kind, and
// Endpoint is set only for a Burrow Cloud target: the endpoint the target NAMES, in place of the
// kube context a cluster target resolves to. For the managed product that is CloudEndpoint, the
// identity, and the caller turns it into an origin — a step that exists because the identity and the
// API's address are two different hosts (see CloudAPIEndpoint).
type Resolved struct {
	Name                  string
	Context               string
	Namespace             string
	ControlPlaneNamespace string
	Env                   string
	Mode                  Mode
	Target                string
	Kind                  TargetKind
	Endpoint              string
	AgentKubeconfig       string
	AgentContext          string
	// InstallID is the install this resolution expects to be talking to (ADR-0084 §5). It comes from
	// the selected target when there is one and from the registered handle otherwise, so the check
	// covers both the CLI's targeted path and the handle-based path `burrow-agent` resolves through.
	// A context that matches neither carries none, and sends no header.
	InstallID string
	// ContextStale is set when the PINNED handle this resolution came from records a kube context the
	// kubeconfig no longer holds, and the resolution proceeded anyway because the handle carries a
	// scoped credential that reaches the cluster without one (issue #488). It is nil on every other
	// resolution, including the one that refuses.
	//
	// It is data handed back rather than a message written out for the same reason ContextMissingError
	// is a type: this package resolves and reports, and the commands decide what to say. The caller is
	// expected to say it once — the recorded name is wrong and correcting it is one command — and then
	// carry on, since the credential is what the connection is made with either way.
	ContextStale *ContextMissingError
}

// Cloud reports whether this resolution is the managed product, which is reached over HTTPS with the
// credential sign-in stored rather than through a kubeconfig. It is the one branch a caller needs:
// everything else about a cloud resolution — no context, no namespace, no scoped kubeconfig — falls
// out of there being no cluster.
func (r Resolved) Cloud() bool { return r.Kind == TargetKindCloud }

// Resolve decides which environment a command targets (ADR-0036, ADR-0078).
//
// When an ADR-0078 target is selected it decides the CLUSTER first, since that is what the target
// says: a Kubernetes target resolves to its kubeconfig context, and a pinned handle applies only
// when it is a handle inside that same cluster (a pin for a different cluster is not a narrowing of
// this one). A Burrow Cloud target has no kubeconfig to resolve and is reported as such.
//
// With no target selected the behaviour is as it was. When a handle is pinned (cfg.Current set), it
// resolves to that handle, erroring clearly if the pinned name is not registered — or if the context
// it records is no longer in the kubeconfig AND the handle has no scoped credential to reach the
// cluster with, which is the check a renamed context needs and the one that was missing. A handle
// that does carry one resolves, and reports the stale name on ContextStale instead.
// Otherwise it follows the kubeconfig's current context: the target is that context, its namespace
// (so kubens moves Burrow too; empty when the context sets none, leaving the burrowd default to
// apply), and the default control-plane namespace. If the current context matches a registered
// handle by context name, that handle's Name and Env (the burrowd env name to send) are surfaced;
// otherwise both are empty.
func Resolve(cfg *Config, kubeconfigPath string) (Resolved, error) {
	resolved, err := ResolveOperate(cfg, kubeconfigPath)
	if err != nil {
		return Resolved{}, err
	}
	if resolved.Cloud() {
		return Resolved{}, errNeedsKubeconfig(resolved.Target, resolved.Endpoint)
	}
	return resolved, nil
}

// ResolveOperate is Resolve for the ordinary application-facing commands — deploy, status, logs and
// the rest — which can act through EITHER kind of target, because the control plane they call is the
// same API whether it is reached through a cluster's API server or over HTTPS at the managed
// product (ADR-0078 §1).
//
// It is a separate entry point rather than a change to Resolve on purpose. A caller that genuinely
// needs a cluster — anything that reads a kubeconfig, mints a scoped credential, or installs
// something — keeps getting the refusal it gets today, so forgetting to opt in fails safe with a
// legible message instead of silently acting on whatever cluster the kubeconfig happens to point at.
func ResolveOperate(cfg *Config, kubeconfigPath string) (Resolved, error) {
	target, hasTarget, err := cfg.ActiveTarget()
	if err != nil {
		return Resolved{}, err
	}
	if hasTarget {
		return resolveWithTarget(cfg, target, kubeconfigPath)
	}
	return resolveWithoutTarget(cfg, kubeconfigPath)
}

// errNeedsKubeconfig is what a command that can only reach a cluster says when the active target is
// the managed product. It names the target, says why this particular command cannot use it, and
// gives the one command that changes the answer — rather than failing somewhere further down with a
// kubeconfig error that sends the reader looking for a cluster problem they do not have.
func errNeedsKubeconfig(name, endpoint string) error {
	return fmt.Errorf(
		"localconfig: the active target %q is the managed product at %s, which this command cannot use: it works through a cluster's kubeconfig, and a Burrow Cloud tenant has no cluster of its own. Switch to a cluster target with \"burrow auth switch <name>\" (see \"burrow auth status\")",
		name, endpoint)
}

// resolveWithTarget resolves against the selected ADR-0078 target. A Burrow Cloud target carries no
// kubeconfig context: it resolves to the endpoint it names, and the caller decides whether that is
// something it can act through.
func resolveWithTarget(cfg *Config, target Target, kubeconfigPath string) (Resolved, error) {
	if target.Kind == TargetKindCloud {
		return Resolved{
			Mode:     ModeTargeted,
			Target:   target.Name,
			Kind:     target.Kind,
			Endpoint: target.Endpoint,
		}, nil
	}
	if target.Kind != TargetKindKubernetes {
		return Resolved{}, fmt.Errorf(
			"localconfig: the active target %q is %s, which this command reaches through a kubeconfig and cannot; switch to a cluster target with \"burrow auth switch <name>\" (see \"burrow auth status\")",
			target.Name, target.Describe())
	}

	namespace, found, err := contextNamespace(kubeconfigPath, target.Context)
	if err != nil {
		return Resolved{}, err
	}
	if !found {
		return Resolved{}, errTargetContextMissing(target.Name, target.Context)
	}

	resolved := Resolved{
		Context:               target.Context,
		Namespace:             namespace,
		ControlPlaneNamespace: DefaultControlPlaneNamespace,
		Mode:                  ModeTargeted,
		Target:                target.Name,
		Kind:                  target.Kind,
		InstallID:             target.InstallID,
	}
	// A pinned handle narrows the target only when it is a handle in the SAME cluster; a pin left
	// over from another cluster is not a narrowing of this one and is skipped rather than silently
	// redirecting the command out of the target.
	if cfg != nil && cfg.Current != "" {
		env, ok := cfg.Lookup(cfg.Current)
		if !ok {
			return Resolved{}, errUnknownPin(cfg.Current)
		}
		if env.Context == target.Context {
			pinned := fromHandle(env, ModePinned, target.Name)
			pinned.Kind = target.Kind
			// The handle narrows WHICH ENVIRONMENT inside the cluster the target names; it does not
			// change which install that is, so the target's id survives the narrowing. The handle's
			// own id stands in when the target has none — they name the same cluster, and the more
			// recently recorded of the two is the one that knows what is actually installed there.
			if target.InstallID != "" {
				pinned.InstallID = target.InstallID
			}
			return pinned, nil
		}
	}
	if cfg != nil {
		if env, ok := cfg.LookupByContext(target.Context); ok {
			resolved.Name = env.Name
			resolved.Env = env.Env
			resolved.AgentKubeconfig = env.AgentKubeconfig
			resolved.AgentContext = env.AgentContext
			// The target's id wins; the handle's stands in when the target has none. A target
			// written by `burrow auth login` carries no id (that command contacts no cluster), so
			// without this the CLI would go unchecked on a cluster the handle knows perfectly well.
			if resolved.InstallID == "" {
				resolved.InstallID = env.InstallID
			}
		}
	}
	return resolved, nil
}

// resolveWithoutTarget is the pre-ADR-0078 resolution, unchanged: a pinned handle, else the
// kubeconfig's current context.
func resolveWithoutTarget(cfg *Config, kubeconfigPath string) (Resolved, error) {
	if cfg != nil && cfg.Current != "" {
		env, ok := cfg.Lookup(cfg.Current)
		if !ok {
			return Resolved{}, errUnknownPin(cfg.Current)
		}
		// The handle's context is checked against the kubeconfig for the same reason a target's is,
		// and it was the one pinned path that did not check (issue #473). Follow mode needs no check:
		// its context comes FROM the kubeconfig.
		if pinnedContextMissing(kubeconfigPath, env.Context) {
			stale := errPinnedContextMissing(env.Name, env.Context)
			// A handle carrying a scoped, burrowd-only credential (ADR-0038) does not dial through the
			// recorded context at all: the credential holds the API server, the CA and the token, and
			// the connect path overrides both the kubeconfig and the context with it. Refusing there
			// takes working access away over a label — and ADR-0084 §5 is explicit that a kube context
			// name is a label rather than an identity, so it can be renamed under a handle that still
			// reaches its cluster perfectly well (issue #488). The staleness is real and is handed back
			// to be said out loud; what changes is that it is no longer fatal.
			//
			// A handle with no usable credential has nothing else to dial with, so for it the refusal
			// stands exactly as it was.
			if !scopedCredentialUsable(env.AgentKubeconfig) {
				return Resolved{}, stale
			}
			resolved := fromHandle(env, ModePinned, "")
			resolved.ContextStale = stale
			return resolved, nil
		}
		return fromHandle(env, ModePinned, ""), nil
	}

	context, namespace, err := currentContext(kubeconfigPath)
	if err != nil {
		return Resolved{}, err
	}
	resolved := Resolved{
		Context:               context,
		Namespace:             namespace,
		ControlPlaneNamespace: DefaultControlPlaneNamespace,
		Mode:                  ModeFollowing,
	}
	if cfg != nil {
		if env, ok := cfg.LookupByContext(context); ok {
			resolved.Name = env.Name
			resolved.Env = env.Env
			resolved.AgentKubeconfig = env.AgentKubeconfig
			resolved.AgentContext = env.AgentContext
			resolved.InstallID = env.InstallID
		}
	}
	return resolved, nil
}

// fromHandle renders a registered handle as the resolved selection, carrying the target that led to
// it (empty when none is selected). It is shared by the pinned paths so they cannot drift.
func fromHandle(env Environment, mode Mode, target string) Resolved {
	return Resolved{
		Name:                  env.Name,
		Context:               env.Context,
		Namespace:             env.AppNamespace,
		ControlPlaneNamespace: env.controlPlaneNamespaceOrDefault(),
		Env:                   env.Env,
		Mode:                  mode,
		Target:                target,
		AgentKubeconfig:       env.AgentKubeconfig,
		AgentContext:          env.AgentContext,
		InstallID:             env.InstallID,
	}
}

// ContextMissingError reports that a target or an environment handle names a kube context the
// kubeconfig no longer holds. It is a type rather than a formatted string so the commands that
// REPORT on the configuration — `burrow env list`, `burrow auth status` — can recognise it and mark
// the stale entry, instead of failing in place of the command that would have failed.
//
// Name is the target or handle, Context the name it records, and Pinned distinguishes the two
// wordings: the fixes differ, and an error that names the wrong command is barely better than none.
type ContextMissingError struct {
	Name    string
	Context string
	Pinned  bool
}

func (e *ContextMissingError) Error() string {
	if e.Pinned {
		return fmt.Sprintf(
			"localconfig: the pinned environment %q names kube context %q, which is not in your kubeconfig; the kubeconfig may have moved or the context may have been renamed. Re-point the handle with \"burrow env use %s --context <context>\", or follow your current kube context again with \"burrow env follow\"",
			e.Name, e.Context, e.Name)
	}
	return fmt.Sprintf(
		"localconfig: the active target %q names kube context %q, which is not in your kubeconfig; the kubeconfig may have moved or the context may have been renamed. Point at it again with \"burrow auth login\", or pick another target with \"burrow auth switch <name>\"",
		e.Name, e.Context)
}

// errTargetContextMissing is what a selected Kubernetes target says when the kube context it names
// is not in the kubeconfig. It names both, says the two things that usually cause it, and gives the
// two commands that fix it. Every resolution path shares it, so a stale target reads the same way
// whether an app command or a privileged one tripped over it.
func errTargetContextMissing(name, kubeContext string) error {
	return &ContextMissingError{Name: name, Context: kubeContext}
}

// errPinnedContextMissing is the same for a PINNED environment handle, and it exists because a
// handle recording a context that has since been renamed is not a harmless stale string.
//
// What it used to do was worse than failing. The handle is where the pinned path gets its context
// from, and nothing checked it, so the name flowed into the display while the connection was made
// some other way — through the scoped, burrowd-only credential the handle also carries (ADR-0038),
// which holds the cluster's address and CA and needs no kubeconfig context at all. The command
// succeeded, on the right cluster, and reported a cluster that does not exist (issue #473). The
// mechanism built to say which cluster you acted on answered with one you did not.
//
// It is also not only a display problem. A handle is matched to its context by NAME in follow mode,
// so once the context is renamed the handle stops matching, and commands quietly resolve to the
// cluster's default environment rather than the one the handle names. Both failures are silent, and
// both are fixed by the same one-line correction — which is why this says how.
//
// It is the refusal only where the handle has nothing else to dial with. Where it carries a scoped
// credential the pinned path proceeds and reports this on Resolved.ContextStale instead (issue #488):
// neither failure above applies there, since the pinned path finds its handle by the handle's own
// name and the display already names the environment rather than the context.
func errPinnedContextMissing(name, kubeContext string) *ContextMissingError {
	return &ContextMissingError{Name: name, Context: kubeContext, Pinned: true}
}

// scopedCredentialUsable reports whether a handle's recorded scoped kubeconfig (ADR-0038) is on disk
// and usable, which is what decides whether a stale kube context name is fatal: a credential that
// loads makes the recorded name a label nothing reads, and a credential that does not leaves the
// kubeconfig context as the only way to reach the cluster.
//
// It requires a context in the file and not merely a file that parses. An empty or truncated
// kubeconfig loads without error and dials nothing, and treating that as a credential would trade a
// message naming the handle and its fix for a connection error naming neither.
func scopedCredentialUsable(path string) bool {
	if path == "" {
		return false
	}
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil || cfg == nil {
		return false
	}
	return len(cfg.Contexts) > 0
}

// errUnknownPin is the message for a pinned handle name that is not registered.
func errUnknownPin(name string) error {
	return fmt.Errorf(
		"localconfig: pinned environment %q is not in the config; pin a registered environment with \"burrow env use <name>\" or return to following the kube context with \"burrow env follow\"",
		name)
}

// pinnedContextMissing reports whether a pinned handle names a kube context the kubeconfig does not
// have, which is the state issue #473 leaves undetected.
//
// Two situations are deliberately NOT missing, and both are the difference between a check and an
// obstruction:
//
//   - A kubeconfig that cannot be read. The pinned path did not open one at all before this check
//     existed, and a handle carrying a scoped, burrowd-only credential (ADR-0038) reaches its
//     cluster without one. Refusing on a file this resolution has no other use for would take
//     working access away over a file that is not in the path.
//   - A kubeconfig with no contexts. With no view of what exists, "missing" is not something this
//     can know, and asserting it would send somebody re-pointing a handle that is fine.
//     `burrow auth status` applies the same tolerance when it marks a target's context missing.
func pinnedContextMissing(kubeconfigPath, name string) bool {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		rules.ExplicitPath = kubeconfigPath
	}
	cfg, err := rules.Load()
	if err != nil || len(cfg.Contexts) == 0 {
		return false
	}
	_, ok := cfg.Contexts[name]
	return !ok
}

// contextNamespace reads one named kubeconfig context's namespace and reports whether the context
// exists at all, honoring an explicit path otherwise the ambient $KUBECONFIG / ~/.kube/config. It is
// how a Kubernetes target is checked against the kubeconfig it names, so a target that has gone
// stale is caught where it is resolved rather than at connect time.
func contextNamespace(kubeconfigPath, name string) (namespace string, found bool, err error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		rules.ExplicitPath = kubeconfigPath
	}
	cfg, err := rules.Load()
	if err != nil {
		return "", false, fmt.Errorf("localconfig: loading kubeconfig: %w", err)
	}
	c, ok := cfg.Contexts[name]
	if !ok {
		return "", false, nil
	}
	return c.Namespace, true, nil
}

// currentContext reads the kubeconfig's current context and its namespace, honoring an
// explicit path otherwise the ambient $KUBECONFIG / ~/.kube/config, the same way
// connect.Contexts does. A context that sets no namespace yields an empty namespace.
func currentContext(kubeconfigPath string) (context, namespace string, err error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		rules.ExplicitPath = kubeconfigPath
	}
	cfg, err := rules.Load()
	if err != nil {
		return "", "", fmt.Errorf("localconfig: loading kubeconfig: %w", err)
	}
	context = cfg.CurrentContext
	if c := cfg.Contexts[context]; c != nil {
		namespace = c.Namespace
	}
	return context, namespace, nil
}

// Render names a resolved target for display on a command, so the target is never ambiguous
// (ADR-0036, ADR-0078). Examples:
//
//	prod
//	"do-nyc1" (no environment registered)
//	kube context "do-nyc1-dev" (no environment registered)
//	"burrow-cloud.dev" (the managed product)
//
// It names the environment and nothing else. The kube context and the namespace are how Burrow
// FOUND that environment rather than what it is, and they are exactly the Kubernetes vocabulary the
// rest of the CLI keeps out of sight; `burrow auth status` reports them for anyone who wants them.
//
// The remaining branches have no environment name to give, so each says what it did resolve through
// — the selected target, or the kube context being followed — and says plainly that no environment
// is registered for it. Naming the context there is not the mechanism leaking back in: it is the
// only name the resolution has.
func (r Resolved) Render() string {
	switch {
	case r.Cloud():
		// The target is named after the endpoint, so naming both would stutter; what a reader needs
		// from this line is that the command is going to the managed product and not to a cluster.
		return fmt.Sprintf("%q (the managed product)", r.Target)
	case r.Name != "":
		return r.Name
	case r.Mode == ModeTargeted:
		return fmt.Sprintf("%q (no environment registered)", r.Target)
	case r.Context != "":
		return fmt.Sprintf("kube context %q (no environment registered)", r.Context)
	default:
		return "no current kube context"
	}
}
