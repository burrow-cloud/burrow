// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"text/template"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The ingress stack Burrow installs when a cluster has none. Pinned to specific upstream
// releases so a cluster always gets a known-good set; bump these deliberately. The
// ingress-nginx "cloud" manifest provisions a LoadBalancer Service (the right default for a
// managed cluster like DigitalOcean); a bare-metal/hostPort variant can come later behind a
// flag (ADR-0018).
const (
	ingressNginxManifest = "https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.11.3/deploy/static/provider/cloud/deploy.yaml"
	certManagerManifest  = "https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml"

	// Let's Encrypt ACME directories. Staging has high rate limits but issues untrusted
	// certificates — use it to validate the flow without burning the production quota.
	acmeProductionURL = "https://acme-v02.api.letsencrypt.org/directory"
	acmeStagingURL    = "https://acme-staging-v02.api.letsencrypt.org/directory"

	// defaultIssuerName matches `burrow app publish --tls-issuer`'s default, so an exposed app's
	// cert-manager annotation points at the issuer this command creates.
	defaultIssuerName = "letsencrypt"
)

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

// ingressOptions are the inputs to `burrow system ingress install`.
type ingressOptions struct {
	email      string
	issuerName string
	staging    bool
	kubeconfig string
	dryRun     bool
	wait       bool
	verbose    bool
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
		Long: "ingress provisions the pieces that make apps reachable over HTTPS — the\n" +
			"ingress-nginx controller, cert-manager, and a Let's Encrypt issuer — installing only\n" +
			"what the cluster does not already have. It is a one-time setup an operator runs with\n" +
			"their kubeconfig, not an agent operation.",
	}

	o := ingressOptions{}
	install := &cobra.Command{
		Use:   "install",
		Short: "Install ingress-nginx, cert-manager, and a Let's Encrypt issuer (whichever are missing)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runIngressInstall(cmd.Context(), o, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	install.Flags().StringVar(&o.email, "email", "", "ACME registration email for Let's Encrypt (recommended: receives expiry notices)")
	install.Flags().StringVar(&o.issuerName, "issuer-name", defaultIssuerName, "name of the ClusterIssuer to create")
	install.Flags().BoolVar(&o.staging, "staging", false, "use the Let's Encrypt staging environment (untrusted certs, high rate limits) to test the flow")
	install.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	install.Flags().BoolVar(&o.dryRun, "dry-run", false, "print what would be installed instead of applying it")
	install.Flags().BoolVar(&o.wait, "wait", true, "wait for cert-manager to become ready before creating the issuer")
	install.Flags().BoolVar(&o.verbose, "verbose", false, "show every resource kubectl applies instead of a summary")

	parent.AddCommand(install)
	return parent
}

func runIngressInstall(ctx context.Context, o ingressOptions, stdout, stderr io.Writer) error {
	if o.issuerName == "" {
		o.issuerName = defaultIssuerName
	}
	issuer, err := renderIssuer(o)
	if err != nil {
		return err
	}

	if o.dryRun {
		fmt.Fprintf(stdout, "ingress install would, against your current cluster:\n")
		fmt.Fprintf(stdout, "  - install ingress-nginx if absent:  kubectl apply -f %s\n", ingressNginxManifest)
		fmt.Fprintf(stdout, "  - install cert-manager if absent:   kubectl apply -f %s\n", certManagerManifest)
		fmt.Fprintf(stdout, "  - apply this ClusterIssuer (%s):\n\n%s\n", o.acmeServer(), indent(issuer))
		return nil
	}

	cs, err := clientset(o.kubeconfig)
	if err != nil {
		return err
	}

	// Ingress controller: install only if absent.
	hasNginx, err := ingressControllerPresent(ctx, cs)
	if err != nil {
		return err
	}
	if hasNginx {
		fmt.Fprintln(stdout, "ingress-nginx already present — using it.")
	} else {
		fmt.Fprintln(stdout, "Installing ingress-nginx...")
		if err := kubectlApplyURL(ctx, o.kubeconfig, ingressNginxManifest, o.verbose, stdout, stderr); err != nil {
			return err
		}
		if o.wait {
			if err := waitForDeployment(ctx, cs, "ingress-nginx", "ingress-nginx-controller", "ingress controller", stdout, 3*time.Minute); err != nil {
				return err
			}
		}
	}

	// cert-manager: install only if absent.
	hasCertManager, err := certManagerPresent(ctx, cs)
	if err != nil {
		return err
	}
	if hasCertManager {
		fmt.Fprintln(stdout, "cert-manager already present — using it.")
	} else {
		fmt.Fprintln(stdout, "Installing cert-manager...")
		if err := kubectlApplyURL(ctx, o.kubeconfig, certManagerManifest, o.verbose, stdout, stderr); err != nil {
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

// ingressControllerPresent reports whether an ingress-nginx controller is already installed,
// detected by the "nginx" IngressClass (what `expose` annotates against) or the controller
// Deployment in the ingress-nginx namespace.
func ingressControllerPresent(ctx context.Context, cs kubernetes.Interface) (bool, error) {
	_, err := cs.NetworkingV1().IngressClasses().Get(ctx, "nginx", metav1.GetOptions{})
	if err == nil {
		return true, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("checking for an ingress class: %w", err)
	}
	_, err = cs.AppsV1().Deployments("ingress-nginx").Get(ctx, "ingress-nginx-controller", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking for the ingress-nginx controller: %w", err)
	}
	return true, nil
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

// kubectlApplyURL applies a manifest from a URL, summarizing what changed (or streaming it
// with verbose), like the in-cluster install manifests.
func kubectlApplyURL(ctx context.Context, kubeconfig, url string, verbose bool, stdout, stderr io.Writer) error {
	return applyAndSummarize(ctx, applyArgs(kubeconfig, url), "", verbose, stdout, stderr)
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
		if err := kubectlApply(ctx, kubeconfig, issuer, verbose, stdout, attemptStderr); err == nil {
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
