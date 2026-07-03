// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/template"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// The ingress stack Burrow installs when a cluster has none. Pinned to specific upstream
// releases so a cluster always gets a known-good set; bump these deliberately. Two
// ingress-nginx variants at the same pinned controller version: the "cloud" manifest
// provisions a LoadBalancer Service (a billable cloud load balancer, the right default for a
// managed cluster like DigitalOcean); the "baremetal" manifest provisions a NodePort Service
// (no cloud load balancer, no extra charge) for bare-metal or cost-sensitive clusters. The
// --expose flag (ADR-0034 slice 2) picks between them.
const (
	ingressNginxManifest          = "https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.11.3/deploy/static/provider/cloud/deploy.yaml"
	ingressNginxBaremetalManifest = "https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.11.3/deploy/static/provider/baremetal/deploy.yaml"
	certManagerManifest           = "https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml"

	// Let's Encrypt ACME directories. Staging has high rate limits but issues untrusted
	// certificates; use it to validate the flow without burning the production quota.
	acmeProductionURL = "https://acme-v02.api.letsencrypt.org/directory"
	acmeStagingURL    = "https://acme-staging-v02.api.letsencrypt.org/directory"

	// defaultIssuerName matches `burrow app publish --tls-issuer`'s default, so an exposed app's
	// cert-manager annotation points at the issuer this command creates.
	defaultIssuerName = "letsencrypt"
)

// The values of the --expose flag (ADR-0034 slice 2): which ingress-nginx Service the
// controller gets. "auto" runs the slice-1 capability detection and picks loadbalancer on a
// known cloud provider, nodeport on bare-metal / no-LB-support clusters.
const (
	exposeAuto         = "auto"
	exposeLoadBalancer = "loadbalancer"
	exposeNodePort     = "nodeport"
)

// detectCapabilities is the capability-detection seam used to resolve --expose auto, defaulting
// to the production read-only probe (ADR-0034 slice 1). It is a package var so tests can inject
// a fake detector; the production detector runs over the kubeconfig clientset.
var detectCapabilities func(context.Context, kubernetes.Interface) (controlplane.ClusterCapabilities, error) = kube.DetectCapabilities

// issuerTemplate renders a Let's Encrypt ClusterIssuer with an HTTP-01 solver via the
// ingress-nginx class. HTTP-01 needs only that the host's DNS already points at the
// controller; a DNS-01 solver (which issues before the host is public) is a later addition
// once the provider token is wired into cert-manager (ADR-0018).
var issuerTemplate = template.Must(template.New("issuer").Parse(`apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: {{.IssuerName}}
spec:
  acme:
    server: {{.ACMEServer}}
{{- if .Email}}
    email: {{.Email}}
{{- end}}
    privateKeySecretRef:
      name: {{.IssuerName}}-account-key
    solvers:
      - http01:
          ingress:
            class: nginx
`))

// ingressOptions are the inputs to `burrow cluster ingress install`.
type ingressOptions struct {
	email      string
	issuerName string
	staging    bool
	kubeconfig string
	expose     string
	approve    bool
	dryRun     bool
	wait       bool
	verbose    bool
}

// validateExpose checks the --expose value, treating an empty value as auto.
func (o ingressOptions) validateExpose() error {
	switch o.expose {
	case "", exposeAuto, exposeLoadBalancer, exposeNodePort:
		return nil
	default:
		return fmt.Errorf("invalid --expose %q: want loadbalancer, nodeport, or auto", o.expose)
	}
}

func (o ingressOptions) acmeServer() string {
	if o.staging {
		return acmeStagingURL
	}
	return acmeProductionURL
}

// newIngressCmd is a setup command (not part of `burrow install`): it provisions the
// ingress-nginx controller, cert-manager, and a Let's Encrypt ClusterIssuer, acting with the
// developer's kubeconfig (ADR-0018). It detects an existing controller or cert-manager and
// uses it rather than installing a second one. The agent never runs this — installing a
// cluster-wide controller is privileged setup; the agent detects its absence via the
// reachability surface and tells the human to run it.
func newIngressCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:   "ingress",
		Short: "Set up cluster ingress and TLS (install)",
		Long: "ingress provisions the pieces that make apps reachable over HTTPS (the\n" +
			"ingress-nginx controller, cert-manager, and a Let's Encrypt issuer), installing only\n" +
			"what the cluster does not already have. It is a one-time setup an operator runs with\n" +
			"their kubeconfig, not an agent operation.",
	}

	o := ingressOptions{}
	install := &cobra.Command{
		Use:   "install",
		Short: "Install ingress-nginx, cert-manager, and a Let's Encrypt issuer (whichever are missing)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runIngressInstall(cmd.Context(), o, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	install.Flags().StringVar(&o.email, "email", "", "ACME registration email for Let's Encrypt (recommended: receives expiry notices)")
	install.Flags().StringVar(&o.issuerName, "issuer-name", defaultIssuerName, "name of the ClusterIssuer to create")
	install.Flags().BoolVar(&o.staging, "staging", false, "use the Let's Encrypt staging environment (untrusted certs, high rate limits) to test the flow")
	install.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	install.Flags().StringVar(&o.expose, "expose", exposeAuto, "how to expose the controller: loadbalancer (billable cloud LB), nodeport (free, point DNS at a node IP), or auto (detect from the provider)")
	install.Flags().BoolVar(&o.approve, "approve", false, "approve installing a billable LoadBalancer (required to install the loadbalancer path non-interactively); the free nodeport path needs no approval. The plan and cost notice always print. No shorthand: a cost approval should not be a single keystroke.")
	install.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the plan (including the cost notice) instead of applying it")
	install.Flags().BoolVar(&o.wait, "wait", true, "wait for cert-manager to become ready before creating the issuer")
	install.Flags().BoolVar(&o.verbose, "verbose", false, "show every resource burrow applies instead of a summary")

	parent.AddCommand(install)
	return parent
}

func runIngressInstall(ctx context.Context, o ingressOptions, stdin io.Reader, stdout, stderr io.Writer) error {
	if o.issuerName == "" {
		o.issuerName = defaultIssuerName
	}
	if err := o.validateExpose(); err != nil {
		return err
	}
	issuer, err := renderIssuer(o)
	if err != nil {
		return err
	}

	// dry-run prints the plan and the cost notice without contacting the cluster, so an operator
	// can review what an install would do (including the billable-resource warning) before running
	// it. auto is left unresolved here: picking loadbalancer vs nodeport needs the live capability
	// probe, which only runs on the real apply.
	if o.dryRun {
		writeIngressDryRunPlan(o, issuer, stdout)
		return nil
	}

	cs, err := clientset(o.kubeconfig)
	if err != nil {
		return err
	}

	// Resolve --expose (auto runs the slice-1 capability probe), then read what is already present
	// so the plan only lists the missing pieces (detect-and-skip).
	expose, err := resolveExpose(ctx, o.expose, cs)
	if err != nil {
		return err
	}
	manifest := ingressManifestFor(expose)

	hasNginx, err := ingressControllerPresent(ctx, cs)
	if err != nil {
		return err
	}
	hasCertManager, err := certManagerPresent(ctx, cs)
	if err != nil {
		return err
	}

	// Always print the plan with the cost/SPOF notice before any write (ADR-0034 slice 2: nothing
	// cluster-wide is installed without the operator seeing what it costs), then gate the write on
	// the resolved mode. The billable loadbalancer path requires approval (interactive prompt, or
	// --approve non-interactively); the free nodeport path proceeds after the plan, no approval needed.
	writeIngressPlan(stdout, o, expose, manifest, hasNginx, hasCertManager)
	ok, err := confirmInstall(o, expose, stdin, stdout)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(stdout, "Aborted. Nothing was changed.")
		return nil
	}

	// Ingress controller: install only if absent.
	if hasNginx {
		fmt.Fprintln(stdout, "ingress-nginx already present, using it.")
	} else {
		fmt.Fprintf(stdout, "Installing ingress-nginx (%s)...\n", manifestVariantLabel(expose))
		if err := serverSideApplyURL(ctx, o.kubeconfig, manifest, o.verbose, stdout, stderr); err != nil {
			return err
		}
		if o.wait {
			if err := waitForDeployment(ctx, cs, "ingress-nginx", "ingress-nginx-controller", "ingress controller", stdout, 3*time.Minute); err != nil {
				return err
			}
		}
	}

	// cert-manager: install only if absent.
	if hasCertManager {
		fmt.Fprintln(stdout, "cert-manager already present, using it.")
	} else {
		fmt.Fprintln(stdout, "Installing cert-manager...")
		if err := serverSideApplyURL(ctx, o.kubeconfig, certManagerManifest, o.verbose, stdout, stderr); err != nil {
			return err
		}
	}

	// The ClusterIssuer is a cert-manager CRD served by its webhook, so it cannot be applied
	// until the webhook is up — wait for it (whether we just installed cert-manager or it was
	// already here) and retry, since the webhook briefly rejects calls right after readiness.
	if o.wait {
		if err := waitForDeployment(ctx, cs, "cert-manager", "cert-manager-webhook", "cert-manager webhook", stdout, 3*time.Minute); err != nil {
			return err
		}
	}
	if o.email == "" {
		fmt.Fprintln(stderr, "note: no --email given; Let's Encrypt expiry notices will not be sent.")
	}
	fmt.Fprintf(stdout, "Creating ClusterIssuer %q...\n", o.issuerName)
	if err := applyIssuer(ctx, o.kubeconfig, issuer, o.verbose, stdout, stderr); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "\nIngress and TLS are set up. Expose an app and request a certificate:\n"+
		"  burrow app publish <app> --host <name> --port <n> --tls\n"+
		"The controller's external address can take a few minutes; check `burrow app reachability <app>`.\n")
	return nil
}

// resolveExpose turns the --expose value into a concrete mode. auto runs the slice-1 capability
// probe and picks loadbalancer when LoadBalancer support is inferred (a known cloud provider) or
// nodeport otherwise. The explicit modes need no probe. The cost notice is provider-agnostic, so
// resolveExpose does not surface the detected provider name (provider detection from node labels
// is best-effort and a confidently-wrong name would be worse than a general note).
func resolveExpose(ctx context.Context, mode string, cs kubernetes.Interface) (string, error) {
	switch mode {
	case exposeNodePort:
		return exposeNodePort, nil
	case exposeLoadBalancer:
		return exposeLoadBalancer, nil
	default: // auto (or empty)
		caps, err := detectCapabilities(ctx, cs)
		if err != nil {
			return "", fmt.Errorf("detecting capabilities to pick an expose mode (pass --expose loadbalancer or --expose nodeport to skip detection): %w", err)
		}
		if caps.LoadBalancer.Supported {
			return exposeLoadBalancer, nil
		}
		return exposeNodePort, nil
	}
}

// ingressManifestFor returns the pinned ingress-nginx manifest for the resolved expose mode: the
// baremetal (NodePort) manifest for nodeport, the cloud (LoadBalancer) manifest otherwise.
func ingressManifestFor(expose string) string {
	if expose == exposeNodePort {
		return ingressNginxBaremetalManifest
	}
	return ingressNginxManifest
}

// manifestVariantLabel describes the chosen ingress-nginx Service for plan and progress output.
func manifestVariantLabel(expose string) string {
	if expose == exposeNodePort {
		return "baremetal, NodePort Service"
	}
	return "cloud, LoadBalancer Service"
}

// costNotice explains the LoadBalancer path so the operator understands the tradeoff, not just the
// price: a LoadBalancer is billable but highly available, because a cloud load balancer spreads
// traffic across the nodes and the site survives a worker-node failure. It points to the free
// nodeport alternative for cost-sensitive clusters (ADR-0034 slice 2).
func costNotice() string {
	return "Note: a LoadBalancer is billable (a cloud load balancer, priced by your provider, for " +
		"example roughly a low-double-digit dollars per month on DigitalOcean) but it spreads traffic " +
		"across your nodes, so the site stays reachable when a worker node fails. Choose it for high " +
		"availability, or --expose nodeport to avoid the cost."
}

// nodePortNotice explains the nodeport path so the operator understands the tradeoff: it is free,
// but it points DNS at a single node's IP, which makes that node a single point of failure.
func nodePortNotice() string {
	return "Note: NodePort is free. It points your DNS at a single worker node's IP address, which " +
		"makes that node a single point of failure: if it goes down, the site becomes unreachable."
}

// writeIngressPlan prints the live install plan: only the missing pieces (detect-and-skip), and
// the cost notice (loadbalancer) or the node-IP note (nodeport).
func writeIngressPlan(w io.Writer, o ingressOptions, expose, manifest string, hasNginx, hasCertManager bool) {
	fmt.Fprintf(w, "Plan (expose: %s). Against your current cluster, ingress install will:\n", expose)
	if hasNginx {
		fmt.Fprintln(w, "  - ingress-nginx: already present, skip.")
	} else {
		fmt.Fprintf(w, "  - install ingress-nginx (%s): apply %s\n", manifestVariantLabel(expose), manifest)
	}
	if hasCertManager {
		fmt.Fprintln(w, "  - cert-manager: already present, skip.")
	} else {
		fmt.Fprintf(w, "  - install cert-manager: apply %s\n", certManagerManifest)
	}
	fmt.Fprintf(w, "  - apply a Let's Encrypt ClusterIssuer %q (%s).\n\n", o.issuerName, o.acmeServer())
	if expose == exposeLoadBalancer {
		fmt.Fprintln(w, costNotice())
	} else {
		fmt.Fprintln(w, nodePortNotice())
	}
	fmt.Fprintln(w)
}

// writeIngressDryRunPlan prints the plan without contacting the cluster. The conditional installs
// stay "if absent" (no live detect-and-skip), and auto is reported as resolved at apply time. The
// cost notice is shown for loadbalancer (and the auto case, which may resolve to it); nodeport
// shows the node-IP note instead.
func writeIngressDryRunPlan(o ingressOptions, issuer string, w io.Writer) {
	expose := o.expose
	if expose == "" {
		expose = exposeAuto
	}
	fmt.Fprintf(w, "Plan (expose: %s, dry run). Against your current cluster, ingress install would:\n", expose)
	switch expose {
	case exposeNodePort:
		fmt.Fprintf(w, "  - install ingress-nginx if absent (baremetal, NodePort Service): apply %s\n", ingressNginxBaremetalManifest)
	case exposeLoadBalancer:
		fmt.Fprintf(w, "  - install ingress-nginx if absent (cloud, LoadBalancer Service): apply %s\n", ingressNginxManifest)
	default: // auto
		fmt.Fprintf(w, "  - install ingress-nginx if absent (auto: cloud/LoadBalancer on a known provider, else baremetal/NodePort): apply %s\n", ingressNginxManifest)
	}
	fmt.Fprintf(w, "  - install cert-manager if absent: apply %s\n", certManagerManifest)
	fmt.Fprintf(w, "  - apply this ClusterIssuer (%s):\n\n%s\n\n", o.acmeServer(), indent(issuer))
	switch expose {
	case exposeNodePort:
		fmt.Fprintln(w, nodePortNotice())
	default: // loadbalancer, or auto (which may resolve to loadbalancer at apply time)
		fmt.Fprintln(w, costNotice())
	}
}

// confirmInstall gates the install after the plan is printed (ADR-0034 slice 2), branching on the
// resolved expose mode. Only the billable loadbalancer path is gated: --approve is an explicit
// approval and proceeds without a prompt, otherwise an interactive terminal prompts (default No)
// and with no terminal and no --approve it refuses rather than hang or apply, so a billable cloud
// load balancer is never provisioned non-interactively without explicit approval. The free nodeport
// path has no cost to approve, so it proceeds after the plan and node-IP notice — no prompt, no
// --approve needed, interactive or not (a passed --approve is a harmless no-op there).
func confirmInstall(o ingressOptions, expose string, in io.Reader, out io.Writer) (bool, error) {
	if expose != exposeLoadBalancer {
		return true, nil
	}
	if o.approve {
		return true, nil
	}
	if !stdinIsTerminal(in) {
		return false, errors.New("installing the loadbalancer path creates a billable cloud load " +
			"balancer; re-run with --approve to confirm non-interactively (or --expose nodeport for " +
			"the free option, or --dry-run to preview)")
	}
	return confirmProceed(in, out)
}

// confirmProceed prompts for confirmation and reports whether the operator typed yes. Anything but
// y / yes (case-insensitive) is a no, including an empty line or EOF (the [y/N] default).
func confirmProceed(in io.Reader, out io.Writer) (bool, error) {
	fmt.Fprint(out, "Proceed? [y/N]: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// renderIssuer renders the ClusterIssuer manifest for the options.
func renderIssuer(o ingressOptions) (string, error) {
	var sb strings.Builder
	data := struct {
		IssuerName string
		ACMEServer string
		Email      string
	}{IssuerName: o.issuerName, ACMEServer: o.acmeServer(), Email: o.email}
	if err := issuerTemplate.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("rendering ClusterIssuer: %w", err)
	}
	return sb.String(), nil
}

// ingressNginxControllerSelector matches the ingress-nginx controller Deployment by its standard
// recommended labels — the same selector the control-plane capability survey uses (ADR-0034,
// controlplane/kube detectIngress). The running controller, not merely an IngressClass, is what
// routes traffic and runs the admission webhook, so its readiness is the real "already installed"
// signal.
const ingressNginxControllerSelector = "app.kubernetes.io/name=ingress-nginx,app.kubernetes.io/component=controller"

// ingressControllerPresent reports whether a running ingress-nginx controller is already installed,
// detected by an ingress-nginx controller Deployment with at least one ready replica (listed across
// all namespaces so it is found wherever the release lives, the conventional "ingress-nginx"
// namespace or another).
//
// It deliberately does NOT key off the "nginx" IngressClass. An IngressClass is cluster-scoped and
// OUTLIVES the controller that created it: delete the ingress-nginx release and its namespace and the
// class is left orphaned, routing nothing. Keying off the class made a leftover orphan a false
// positive that made install skip, leaving a cluster with no controller. Requiring controller
// readiness (matching the capability survey) makes install proceed instead, and the manifest apply
// then adopts the orphan class via force-conflicts (see applyOne) — no kubectl deletion required.
func ingressControllerPresent(ctx context.Context, cs kubernetes.Interface) (bool, error) {
	deps, err := cs.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: ingressNginxControllerSelector,
	})
	if err != nil {
		return false, fmt.Errorf("checking for the ingress-nginx controller: %w", err)
	}
	for i := range deps.Items {
		if deps.Items[i].Status.ReadyReplicas > 0 {
			return true, nil
		}
	}
	return false, nil
}

// certManagerPresent reports whether cert-manager is already installed, detected by its
// controller Deployment in the cert-manager namespace.
func certManagerPresent(ctx context.Context, cs kubernetes.Interface) (bool, error) {
	_, err := cs.AppsV1().Deployments("cert-manager").Get(ctx, "cert-manager", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking for cert-manager: %w", err)
	}
	return true, nil
}

// applyIssuer applies the ClusterIssuer, retrying briefly: just after cert-manager reports
// ready its validating webhook can still reject the call ("failed calling webhook") for a few
// seconds, and the CRD may take a moment to register. Those rejections are expected, so each
// attempt's stderr is buffered and surfaced only if verbose or if every attempt fails.
func applyIssuer(ctx context.Context, kubeconfig, issuer string, verbose bool, stdout, stderr io.Writer) error {
	var lastErr error
	var lastStderr bytes.Buffer
	for attempt := 1; attempt <= 6; attempt++ {
		attemptStderr := io.Writer(stderr)
		if !verbose {
			lastStderr.Reset()
			attemptStderr = &lastStderr
		}
		if err := applyFn(ctx, kubeconfig, "", issuer, verbose, stdout, attemptStderr); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(5 * time.Second)
	}
	if !verbose {
		fmt.Fprint(stderr, lastStderr.String())
	}
	return fmt.Errorf("creating the ClusterIssuer (cert-manager not accepting it yet): %w", lastErr)
}

// indent prefixes each line with two spaces, for readable dry-run output.
func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}
