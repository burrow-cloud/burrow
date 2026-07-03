// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/internal/jointoken"
)

// k3sKubeconfigPath is where k3s writes the cluster's admin kubeconfig. `burrow cluster bootstrap`
// runs on the VPS, so it reads this local file directly to deploy burrowd and to assemble the join
// token (ADR-0044).
const k3sKubeconfigPath = "/etc/rancher/k3s/k3s.yaml"

// k3sJoinContextName is the kube-context/cluster/environment name the join token records the
// bootstrapped cluster under on the laptop. A stable, recognizable default; the user can rename the
// environment afterwards (`burrow env rename`).
const k3sJoinContextName = "burrow-vps"

// ipifyURL is the default public-IP echo service the auto-detector queries: it returns the caller's
// public IPv4 as a bare string. It is only reached when --public-ip is not given.
const ipifyURL = "https://api.ipify.org"

// k3sInstallScriptURL is the upstream k3s install script the real installer pipes to a shell.
const k3sInstallScriptURL = "https://get.k3s.io"

// bootstrapArgs are the resolved inputs to a `burrow cluster bootstrap` run on the VPS.
type bootstrapArgs struct {
	publicIP     string
	kubeconfig   string
	environment  string
	namespace    string
	appNamespace string
	image        string
	wait         bool
}

// publicIPDetector resolves the VPS's own public IP when --public-ip is not given. It is a seam: the
// real implementation queries an external echo service, and tests substitute a fake so no network
// call is made.
type publicIPDetector interface {
	DetectPublicIP(ctx context.Context) (string, error)
}

// newIPDetector builds the public-IP detector. It is a package var so a test can substitute a fake
// detector without reaching the network; the real detector queries the ipify echo service.
var newIPDetector = func() publicIPDetector {
	return echoIPDetector{url: ipifyURL, client: http.DefaultClient}
}

// echoIPDetector resolves the public IP by GETting an echo service that returns the caller's public
// IP as a bare string (e.g. ipify).
type echoIPDetector struct {
	url    string
	client *http.Client
}

// DetectPublicIP GETs the echo URL and returns the trimmed body as the public IP.
func (e echoIPDetector) DetectPublicIP(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.url, nil)
	if err != nil {
		return "", fmt.Errorf("building the public-IP request: %w", err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("querying %s for the public IP: %w", e.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("querying %s for the public IP: unexpected status %s", e.url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return "", fmt.Errorf("reading the public-IP response: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// k3sInstaller installs k3s on the local box and reports/awaits its API. It is a seam: the real
// implementation shells out to the upstream installer and probes the local API, and tests substitute
// a fake so the flow neither installs k3s nor sleeps.
type k3sInstaller interface {
	// Running reports whether k3s is already installed and its API answering, so bootstrap is
	// idempotent and skips the install.
	Running(ctx context.Context) (bool, error)
	// Install runs the upstream k3s installer with cmd's flags.
	Install(ctx context.Context, cmd k3sInstallCommand) error
	// WaitForAPI blocks until the freshly installed k3s API server answers, or ctx is done.
	WaitForAPI(ctx context.Context) error
}

// newK3sInstaller builds the k3s installer for the given local admin kubeconfig path. It is a package
// var so a test can substitute a fake; the real installer shells out and probes the local API.
var newK3sInstaller = func(kubeconfigPath string, stdout, stderr io.Writer) k3sInstaller {
	return &execK3sInstaller{kubeconfigPath: kubeconfigPath, run: execRunner(stdout, stderr)}
}

// k3sInstallCommand is the k3s install invocation built as an inspectable value, so a unit test can
// assert its flags without shelling out. The real installer pipes the upstream script to a shell and
// passes Args to it; Args are the k3s server flags (ADR-0044): the TLS SAN and external IP the
// laptop connects through, a world-readable kubeconfig, and traefik disabled (Burrow manages ingress
// with ingress-nginx). servicelb is deliberately left enabled — it is the free single-node
// LoadBalancer (ADR-0043).
type k3sInstallCommand struct {
	PublicIP string
	Args     []string
}

// buildK3sInstallCommand builds the k3s install command for the resolved public IP. The critical
// flags (ADR-0044):
//   - --tls-san <ip> so the API-server certificate is valid for the public IP the laptop reaches;
//   - --node-external-ip <ip> so the node advertises the public IP (servicelb hands it out as the
//     free LoadBalancer address);
//   - --write-kubeconfig-mode 0644 so the bootstrap can read the admin kubeconfig to assemble the
//     join token;
//   - --disable traefik because Burrow provisions ingress-nginx (`burrow cluster ingress install`),
//     and two ingress controllers contending for :80/:443 is the misconfiguration to avoid.
//
// servicelb is NOT disabled: on a single node it makes the node's own IP a free LoadBalancer IP.
func buildK3sInstallCommand(publicIP string) k3sInstallCommand {
	return k3sInstallCommand{
		PublicIP: publicIP,
		Args: []string{
			"server",
			"--tls-san", publicIP,
			"--node-external-ip", publicIP,
			"--write-kubeconfig-mode", "0644",
			"--disable", "traefik",
		},
	}
}

// execK3sInstaller is the real k3s installer: it shells out to the upstream install script and probes
// the local API through the admin kubeconfig k3s writes.
type execK3sInstaller struct {
	kubeconfigPath string
	run            runner
}

// Running reports whether k3s is already installed and answering: the admin kubeconfig exists and a
// version probe against the local API succeeds. A missing kubeconfig, or one whose API does not
// answer, reports not-running so the (idempotent) install proceeds.
func (i *execK3sInstaller) Running(ctx context.Context) (bool, error) {
	if _, err := os.Stat(i.kubeconfigPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("checking for an existing k3s install: %w", err)
	}
	return i.probeAPI(ctx) == nil, nil
}

// Install pipes the upstream k3s install script to a shell with the server flags. The pipeline needs
// a shell, so it runs `sh -c "curl -sfL <script> | sh -s - <args>"`; the args are Burrow-controlled
// (validated IP and fixed flags), so a plain space-join is safe.
func (i *execK3sInstaller) Install(ctx context.Context, cmd k3sInstallCommand) error {
	pipeline := fmt.Sprintf("curl -sfL %s | sh -s - %s", k3sInstallScriptURL, strings.Join(cmd.Args, " "))
	if err := i.run(ctx, "sh", "-c", pipeline); err != nil {
		return fmt.Errorf("installing k3s: %w", err)
	}
	return nil
}

// WaitForAPI polls the local k3s API through the admin kubeconfig until a version probe succeeds or
// the deadline passes. Bounded so a wedged install fails clearly instead of hanging forever.
func (i *execK3sInstaller) WaitForAPI(ctx context.Context) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if err := i.probeAPI(ctx); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("k3s API did not become reachable within 2m; check `systemctl status k3s`")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// probeAPI asks the k3s API for its version through the local admin kubeconfig; a nil error means the
// API is answering.
func (i *execK3sInstaller) probeAPI(_ context.Context) error {
	cs, err := clientsetForContext(i.kubeconfigPath, "")
	if err != nil {
		return err
	}
	if _, err := cs.Discovery().ServerVersion(); err != nil {
		return err
	}
	return nil
}

// newBootstrapCmd is the on-VPS bootstrap (ADR-0044): run once on a bare box, it turns it into a
// Burrow cluster and prints a `burrow join <token>` to run on the laptop. It resolves the public IP,
// installs k3s with the SANs and external IP the laptop needs, deploys burrowd (reusing the `burrow
// install` path, which also mints the scoped agent credential, ADR-0038), and encodes the admin
// kubeconfig — rewritten to the public IP — as the join token.
func newBootstrapCmd() *cobra.Command {
	a := bootstrapArgs{}
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Turn this bare VPS into a Burrow cluster (run on the VPS)",
		Long: "bootstrap turns the bare VPS it runs on into a complete single-node Burrow cluster\n" +
			"(ADR-0044). It resolves the box's public IP, installs k3s (with the TLS SAN and external\n" +
			"IP your laptop connects through, and traefik disabled so Burrow's ingress-nginx owns\n" +
			"ingress), deploys the Burrow control plane, and prints a `burrow join <token>` line to run\n" +
			"on your laptop.\n\n" +
			"Run this once, on the VPS, over your own SSH session — Burrow never SSHes anywhere. After\n" +
			"it prints the join token, every operation runs from your laptop.\n\n" +
			"The printed token is admin-grade: treat it like a kubeconfig (never paste it into agent\n" +
			"chat). Make sure your provider firewall allows inbound :6443, :80, and :443.",
		Example: "  # On the VPS, letting Burrow detect the public IP\n" +
			"  burrow cluster bootstrap\n\n" +
			"  # On the VPS, naming the public IP explicitly\n" +
			"  burrow cluster bootstrap --public-ip 203.0.113.10",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBootstrap(cmd.Context(), a, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&a.publicIP, "public-ip", "", "the VPS's public IP (default: auto-detected)")
	cmd.Flags().StringVar(&a.kubeconfig, "kubeconfig", k3sKubeconfigPath, "path to the local k3s admin kubeconfig")
	cmd.Flags().StringVar(&a.environment, "environment", "", "name for this environment (default: a generated adjective-animal name)")
	cmd.Flags().StringVar(&a.namespace, "namespace", connect.DefaultNamespace, "namespace to install the control plane into")
	cmd.Flags().StringVar(&a.appNamespace, "app-namespace", connect.DefaultAppNamespace, "namespace to deploy applications into")
	cmd.Flags().StringVar(&a.image, "burrowd-image", defaultBurrowdImage(), "burrowd container image to deploy (must be pullable by the cluster)")
	cmd.Flags().BoolVar(&a.wait, "wait", true, "wait for the control plane to become ready before printing the join token")
	return cmd
}

// runBootstrap runs the full bootstrap flow (ADR-0044): resolve the public IP, install k3s (unless
// already running), deploy burrowd by reusing the install path, and print the join token.
func runBootstrap(ctx context.Context, a bootstrapArgs, stdout, stderr io.Writer) error {
	ip, err := resolvePublicIP(ctx, a.publicIP, newIPDetector())
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Public IP: %s\n", ip)

	installer := newK3sInstaller(a.kubeconfig, stdout, stderr)
	if err := ensureK3sInstalled(ctx, installer, buildK3sInstallCommand(ip), stdout); err != nil {
		return err
	}

	// Deploy burrowd by REUSING the `burrow install` path against the local k3s admin kubeconfig. This
	// applies the control-plane manifests and, per ADR-0038, mints the scoped burrow-agent credential
	// that the laptop's `burrow join` later reads — so there is no duplicate install logic here.
	if err := installBurrowdOnK3s(ctx, a, stdout, stderr); err != nil {
		return err
	}

	// Emit the join token: the local k3s admin kubeconfig with its server rewritten to the public IP,
	// encoded via the ADR-0044 codec.
	token, err := assembleJoinToken(a.kubeconfig, ip, a.namespace, k3sJoinContextName)
	if err != nil {
		return err
	}
	printJoinInstructions(stdout, token)
	return nil
}

// ensureK3sInstalled installs k3s unless it is already running (idempotent), waiting for the API
// after a fresh install.
func ensureK3sInstalled(ctx context.Context, inst k3sInstaller, cmd k3sInstallCommand, stdout io.Writer) error {
	running, err := inst.Running(ctx)
	if err != nil {
		return err
	}
	if running {
		fmt.Fprintln(stdout, "k3s is already installed and its API is answering; skipping the install.")
		return nil
	}
	fmt.Fprintf(stdout, "Installing k3s (tls-san %s, node-external-ip %s, traefik disabled)...\n", cmd.PublicIP, cmd.PublicIP)
	if err := inst.Install(ctx, cmd); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Waiting for the k3s API to answer...")
	if err := inst.WaitForAPI(ctx); err != nil {
		return err
	}
	return nil
}

// installBurrowdOnK3s deploys the control plane into the local k3s by reusing runInstall (the same
// path `burrow install` drives), targeting the k3s admin kubeconfig's current context.
func installBurrowdOnK3s(ctx context.Context, a bootstrapArgs, stdout, stderr io.Writer) error {
	kubeContext, err := currentKubeContext(a.kubeconfig)
	if err != nil {
		return err
	}
	return runInstall(ctx, installArgs{
		kubeContext:  kubeContext,
		environment:  a.environment,
		namespace:    a.namespace,
		appNamespace: a.appNamespace,
		image:        a.image,
		kubeconfig:   a.kubeconfig,
		wait:         a.wait,
	}, stdout, stderr)
}

// resolvePublicIP resolves the VPS's public IP: the explicit flag if given, otherwise the detector.
// Either way the result must be a public IP; a private/loopback/link-local address or a detection
// failure is a clear stop that tells the user to pass --public-ip.
func resolvePublicIP(ctx context.Context, explicit string, d publicIPDetector) (string, error) {
	if explicit != "" {
		ip := strings.TrimSpace(explicit)
		if !isPublicIP(ip) {
			return "", fmt.Errorf("--public-ip %q is not a public IP address; pass the VPS's public IP", explicit)
		}
		return ip, nil
	}
	ip, err := d.DetectPublicIP(ctx)
	if err != nil {
		return "", fmt.Errorf("could not determine the public IP automatically; pass --public-ip <ip>: %w", err)
	}
	if !isPublicIP(ip) {
		return "", fmt.Errorf("detected address %q is not a public IP; pass --public-ip <ip>", ip)
	}
	return ip, nil
}

// isPublicIP reports whether s parses as a globally routable IP: not loopback, private (RFC1918 /
// fc00::/7), link-local, multicast, or unspecified.
func isPublicIP(s string) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	return true
}

// currentKubeContext returns the current-context name recorded in the kubeconfig at path. k3s names
// it "default"; reading it rather than hard-coding keeps the reuse correct if that ever changes.
func currentKubeContext(path string) (string, error) {
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the k3s admin kubeconfig %s: %w", path, err)
	}
	if cfg.CurrentContext == "" {
		return "", fmt.Errorf("k3s admin kubeconfig %s has no current context", path)
	}
	return cfg.CurrentContext, nil
}

// assembleJoinToken reads the local k3s admin kubeconfig, rewrites its server from the loopback
// address k3s writes (https://127.0.0.1:6443) to the public IP the laptop connects through, and
// encodes the cluster CA and admin client cert+key into a one-line join token (ADR-0044). The token
// is admin-grade; see the jointoken package doc.
func assembleJoinToken(kubeconfigPath, publicIP, namespace, contextName string) (string, error) {
	cfg, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return "", fmt.Errorf("reading the k3s admin kubeconfig %s: %w", kubeconfigPath, err)
	}
	kctx := cfg.Contexts[cfg.CurrentContext]
	if kctx == nil {
		return "", fmt.Errorf("k3s admin kubeconfig %s has no current context", kubeconfigPath)
	}
	cluster := cfg.Clusters[kctx.Cluster]
	auth := cfg.AuthInfos[kctx.AuthInfo]
	if cluster == nil || auth == nil {
		return "", fmt.Errorf("k3s admin kubeconfig %s is missing the cluster or admin credential for context %q", kubeconfigPath, cfg.CurrentContext)
	}

	caData, err := inlineOrFile(cluster.CertificateAuthorityData, cluster.CertificateAuthority)
	if err != nil {
		return "", fmt.Errorf("reading the cluster CA: %w", err)
	}
	certData, err := inlineOrFile(auth.ClientCertificateData, auth.ClientCertificate)
	if err != nil {
		return "", fmt.Errorf("reading the admin client certificate: %w", err)
	}
	keyData, err := inlineOrFile(auth.ClientKeyData, auth.ClientKey)
	if err != nil {
		return "", fmt.Errorf("reading the admin client key: %w", err)
	}

	server, err := rewriteServerHost(cluster.Server, publicIP)
	if err != nil {
		return "", err
	}

	return jointoken.Encode(jointoken.Token{
		Server:                   server,
		CertificateAuthorityData: caData,
		ClientCertificateData:    certData,
		ClientKeyData:            keyData,
		Namespace:                namespace,
		ContextName:              contextName,
	})
}

// inlineOrFile returns inline kubeconfig data when present, else reads the referenced file, else nil.
// k3s inlines everything, so the inline path is the norm; the file fallback keeps assembly correct
// for a kubeconfig that references cert/key/CA files instead.
func inlineOrFile(data []byte, path string) ([]byte, error) {
	if len(data) > 0 {
		return data, nil
	}
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return b, nil
}

// rewriteServerHost replaces the host of a kube API server URL with the public IP, preserving the
// port (k3s serves the API on :6443). This is the trivial kubeconfig rewrite ADR-0044 reimplements
// rather than depend on k3sup for.
func rewriteServerHost(server, publicIP string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("parsing the k3s API server URL %q: %w", server, err)
	}
	port := u.Port()
	if port == "" {
		port = "6443"
	}
	u.Host = net.JoinHostPort(publicIP, port)
	return u.String(), nil
}

// printJoinInstructions prints the copy-to-laptop join line, the admin-grade warning, the laptop next
// steps, and the firewall reminder.
func printJoinInstructions(stdout io.Writer, token string) {
	fmt.Fprintf(stdout, "\n%s Burrow is bootstrapped. Run this on your laptop to finish:\n\n", okMark(stdout))
	fmt.Fprintf(stdout, "  burrow join %s\n\n", token)
	fmt.Fprintln(stdout, "This token is ADMIN-grade — treat it like a kubeconfig: copy it over a private channel,")
	fmt.Fprintln(stdout, "do not paste it into agent chat, and do not commit it.")
	fmt.Fprintln(stdout, "\nOn your laptop:")
	fmt.Fprintln(stdout, "  brew install burrow")
	fmt.Fprintln(stdout, "  burrow join <the token above>")
	fmt.Fprintln(stdout, "\nEnsure your provider firewall allows inbound :6443 (API server, reached from your laptop),")
	fmt.Fprintln(stdout, ":80 and :443 (public traffic).")
}
