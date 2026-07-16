// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	_ "embed"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
)

// defaultRegistryImage is the pinned Zot image (project-zot, Apache-2.0) the optional in-cluster
// registry runs (ADR-0053 §5). Pinned so the install is reproducible; bump deliberately.
const defaultRegistryImage = "ghcr.io/project-zot/zot-linux-amd64:v2.1.2"

// burrowdDeploymentName is the control-plane Deployment `burrow cluster registry install` patches to
// wire (and uninstall to unwire) the in-cluster registry as burrowd's default build push target.
const burrowdDeploymentName = "burrowd"

const (
	// buildRegistryEnv is the burrowd container env var naming the in-cluster registry's INTERNAL
	// push endpoint burrowd defaults an empty build target to (ADR-0053 §5). The build pushes here
	// in-cluster over plain HTTP. `burrow cluster registry install` sets it; uninstall removes it.
	buildRegistryEnv = "BURROW_BUILD_REGISTRY"
	// buildPublicRegistryEnv is the burrowd container env var naming the registry's PUBLIC ingress
	// hostname the resulting deploy references, so the node pulls through the ingress over TLS
	// (ADR-0054 §5). Distinct from buildRegistryEnv (the internal push endpoint). `burrow cluster
	// registry install --host` sets it; uninstall removes it. burrowd reads both at startup.
	buildPublicRegistryEnv = "BURROW_BUILD_PUBLIC_REGISTRY"
)

const (
	// registryName is the shared name of the registry's Deployment, Service, PVC, ConfigMap, and
	// Ingress.
	registryName = "burrow-registry"
	// registryConfigName is the Zot config ConfigMap.
	registryConfigName = "burrow-registry-config"
	// registryTLSSecretName is the Secret cert-manager writes the issued registry certificate into
	// (referenced by the Ingress tls block). Its presence is the "certificate ready" signal.
	registryTLSSecretName = "burrow-registry-tls"
	// registryAuthSecretName is the nginx basic-auth Secret guarding the public pull endpoint: it
	// holds the htpasswd `auth` entry the Ingress auth annotation points at, plus the generated
	// plaintext `password` so a re-install reuses the same credential rather than rotating it.
	registryAuthSecretName = "burrow-registry-auth"
	// registryPullUsername is the username of the generated pull credential — the internal push path
	// stays credential-free, only the external pull path is authenticated.
	registryPullUsername = "burrow"
)

// registryLabels label the registry's rendered and generated resources so they are recognizable as
// Burrow-managed.
var registryLabels = map[string]string{"app": registryName, "app.kubernetes.io/managed-by": "burrow"}

// clusterIssuerGVR is the cert-manager ClusterIssuer resource, checked via the dynamic client (it is
// a CRD, not a built-in kind).
var clusterIssuerGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"}

// registryManifest is the in-cluster registry manifest template (Zot's PVC, config, Deployment, a
// ClusterIP Service for in-cluster pushes, and an Ingress vhost for public pulls over TLS), embedded
// so `burrow cluster registry install` renders and applies it standalone — separate from the
// control-plane install manifests (ADR-0054). The basic-auth and pull Secrets carry credentials, so
// they are created through the typed client rather than rendered into this text.
//
//go:embed manifests/registry.yaml.tmpl
var registryManifest string

// registryTemplate parses the in-cluster registry manifest template once.
var registryTemplate = template.Must(template.New("registry").Parse(registryManifest))

// registryRenderOptions are the values rendered into the in-cluster registry manifest.
type registryRenderOptions struct {
	Namespace        string
	RegistryEndpoint string
	RegistryImage    string
	RegistryPort     int
	Host             string
	IssuerName       string
	TLSSecretName    string
	AuthSecretName   string
}

// renderRegistryManifest renders the in-cluster registry manifest for the control-plane namespace and
// the given public host, pinning the Zot image, the serve port, the cert-manager issuer, and the
// Ingress annotations (proxy-body-size 0 for large layers, and basic auth on the public endpoint).
func renderRegistryManifest(namespace, host string) (string, error) {
	var sb strings.Builder
	err := registryTemplate.Execute(&sb, registryRenderOptions{
		Namespace:        namespace,
		RegistryEndpoint: connect.RegistryEndpoint(namespace),
		RegistryImage:    defaultRegistryImage,
		RegistryPort:     connect.DefaultRegistryPort,
		Host:             host,
		IssuerName:       defaultIssuerName,
		TLSSecretName:    registryTLSSecretName,
		AuthSecretName:   registryAuthSecretName,
	})
	if err != nil {
		return "", fmt.Errorf("rendering the in-cluster registry manifest: %w", err)
	}
	return sb.String(), nil
}

// clusterRegistryClientset builds the Kubernetes clientset the cluster-registry subcommands act with.
// It is a package var so tests can substitute a fake; it defaults to the kubeconfig-driven clientset.
var clusterRegistryClientset = func(kubeconfig string) (kubernetes.Interface, error) {
	return clientset(kubeconfig)
}

// clusterIssuerPresentFn reports whether the named cert-manager ClusterIssuer exists. It is a package
// var so tests can substitute a recorder without a live cluster (a ClusterIssuer is a CRD the typed
// fake clientset does not serve); the real implementation reads it through the dynamic client.
var clusterIssuerPresentFn = clusterIssuerPresent

// clusterIssuerPresent reports whether the named ClusterIssuer exists, via the dynamic client.
func clusterIssuerPresent(ctx context.Context, kubeconfig, name string) (bool, error) {
	cfg, err := connect.RESTConfig(kubeconfig, "")
	if err != nil {
		return false, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return false, fmt.Errorf("building dynamic client: %w", err)
	}
	_, err = dyn.Resource(clusterIssuerGVR).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking for the %q ClusterIssuer: %w", name, err)
	}
	return true, nil
}

// clusterRegistryOptions are the inputs to the `burrow cluster registry` subcommands.
type clusterRegistryOptions struct {
	namespace    string
	appNamespace string
	kubeconfig   string
	host         string
	verbose      bool
}

// newClusterRegistryCmd is a setup command (not part of `burrow install`, ADR-0054): it installs,
// inspects, and removes the OPTIONAL in-cluster image registry (Zot) that gives the in-cluster build a
// zero-config push target (ADR-0053 §5). Like `burrow cluster ingress install`, it acts with the
// developer's kubeconfig — it is not an agent operation and does not route through burrowd's guarded
// API. It manages the registry that RUNS IN the cluster; for credentials to EXTERNAL registries (GHCR,
// Docker Hub, ...) the cluster pulls from, use `burrow config registry`.
//
// The registry is reachable the same way on any cluster (k3s or a managed cluster like DOKS): the
// build pushes to an internal ClusterIP Service in-cluster over plain HTTP, and nodes pull the image
// through the cluster ingress over TLS, the certificate issued by the existing Let's Encrypt
// `letsencrypt` ClusterIssuer. There is no node or containerd editing (ADR-0054 §5). Because the pull
// path is public, `install` requires a `--host` and depends on the ingress stack being present.
func newClusterRegistryCmd() *cobra.Command {
	o := clusterRegistryOptions{}
	parent := &cobra.Command{
		Use:   "registry",
		Short: "Manage the optional in-cluster image registry (status/install/uninstall)",
		Long: "registry manages the OPTIONAL in-cluster image registry that runs inside your cluster —\n" +
			"a lightweight Zot registry that gives the in-cluster build a zero-config push target so a\n" +
			"self-hosted user needs no external registry account. It is a one-time operator setup you run\n" +
			"with your kubeconfig, not an agent operation, and it is separate from `burrow install`, which\n" +
			"provisions only the control plane.\n\n" +
			"It is reachable the same way on any cluster: the build pushes to an internal Service in-cluster\n" +
			"over plain HTTP, and nodes pull through the cluster ingress over TLS. Because the pull path is\n" +
			"public it needs a hostname and the ingress stack — run `burrow cluster ingress install` first,\n" +
			"then `burrow cluster registry install --host registry.example.com`.\n\n" +
			"With no subcommand it reports whether the in-cluster registry is installed. External\n" +
			"registries (GHCR, Docker Hub, ...) remain fully supported and are the default; to give the\n" +
			"cluster credentials to pull from one of those, use `burrow config registry` instead — that\n" +
			"command manages pull credentials for external registries, this one manages the registry that\n" +
			"runs in your cluster.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClusterRegistryStatus(cmd.Context(), o, cmd.OutOrStdout())
		},
	}
	parent.PersistentFlags().StringVar(&o.namespace, "namespace", connect.DefaultNamespace, "control-plane namespace Burrow is installed in")
	parent.PersistentFlags().StringVar(&o.appNamespace, "app-namespace", "", "namespace apps deploy into (default: discovered from the install)")
	parent.PersistentFlags().StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")

	install := &cobra.Command{
		Use:   "install",
		Short: "Install the in-cluster registry behind the cluster ingress and wire it as the default build push target",
		Long: "install deploys the optional lightweight in-cluster registry (Zot) into the control-plane\n" +
			"namespace, exposes it for public pulls at --host through the cluster ingress with a\n" +
			"Let's Encrypt certificate, and wires it as burrowd's zero-config default build push target.\n" +
			"The build pushes to an internal Service in-cluster over plain HTTP; nodes pull the public host\n" +
			"over TLS with a generated credential. There is no node or containerd editing, so it works the\n" +
			"same on k3s and a managed cluster.\n\n" +
			"It depends on the ingress stack: run `burrow cluster ingress install` first. Known limitation:\n" +
			"the public pull path needs a hostname — a cluster with no domain cannot use the in-cluster\n" +
			"registry today (a future no-domain fallback via generic containerd certs.d is out of scope).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClusterRegistryInstall(cmd.Context(), o, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	install.Flags().StringVar(&o.host, "host", "", "public hostname the registry is pulled at, e.g. registry.example.com (required)")
	install.Flags().BoolVar(&o.verbose, "verbose", false, "show every resource burrow applies instead of a summary")

	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the in-cluster registry and unwire it from the build push target",
		Long: "uninstall removes the in-cluster registry Deployment, Service, Ingress, config, basic-auth\n" +
			"Secret, and its PersistentVolumeClaim, removes the generated pull credential from the app\n" +
			"namespace, and unsets burrowd's default build push target.\n\n" +
			"Residue to know about: deleting the PersistentVolumeClaim releases the volume, but whether\n" +
			"the underlying PersistentVolume (and the images stored on it) is reclaimed depends on the\n" +
			"StorageClass reclaim policy. The issued TLS certificate Secret is left in place. After\n" +
			"uninstall, a build that explicitly targets the in-cluster registry will fail until you\n" +
			"reinstall it or point the build at an external registry.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClusterRegistryUninstall(cmd.Context(), o, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}

	parent.AddCommand(install, uninstall)
	return parent
}

// inClusterRegistryPresent reports whether the in-cluster registry is installed, detected by its
// Deployment in the control-plane namespace.
func inClusterRegistryPresent(ctx context.Context, cs kubernetes.Interface, namespace string) (bool, error) {
	_, err := cs.AppsV1().Deployments(namespace).Get(ctx, registryName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking for the in-cluster registry: %w", err)
	}
	return true, nil
}

// runClusterRegistryStatus reports whether the in-cluster registry is installed. When it is, it prints
// the internal push endpoint, the public pull host, whether the TLS certificate has been issued, and
// whether the pull credential is present in the app namespace. When it is not, it prints the one-line
// hint to install it.
func runClusterRegistryStatus(ctx context.Context, o clusterRegistryOptions, stdout io.Writer) error {
	cs, err := clusterRegistryClientset(o.kubeconfig)
	if err != nil {
		return err
	}
	present, err := inClusterRegistryPresent(ctx, cs, o.namespace)
	if err != nil {
		return err
	}
	if !present {
		fmt.Fprintln(stdout, "In-cluster registry: not installed.")
		fmt.Fprintln(stdout, "Install it: burrow cluster ingress install && burrow cluster registry install --host registry.example.com")
		return nil
	}
	fmt.Fprintln(stdout, "In-cluster registry: installed.")
	fmt.Fprintf(stdout, "  Internal push endpoint:  %s (in-cluster, plain HTTP; the build pushes here)\n", connect.RegistryEndpoint(o.namespace))

	// The public host is recorded on burrowd's env at install; read it back rather than parsing the
	// Ingress. A missing burrowd or env leaves the host blank, reported honestly.
	host, _ := burrowdEnv(ctx, cs, o.namespace, buildPublicRegistryEnv)
	if host != "" {
		fmt.Fprintf(stdout, "  Public pull host:        https://%s (nodes pull through the ingress over TLS)\n", host)
	} else {
		fmt.Fprintln(stdout, "  Public pull host:        unknown (burrowd is not wired to a public host)")
	}

	// TLS readiness: cert-manager writes the issued certificate into the TLS Secret; its presence is
	// the "certificate ready" signal.
	tlsReady, err := secretPresent(ctx, cs, o.namespace, registryTLSSecretName)
	if err != nil {
		return err
	}
	if tlsReady {
		fmt.Fprintf(stdout, "  TLS certificate:         ready (%s)\n", registryTLSSecretName)
	} else {
		fmt.Fprintf(stdout, "  TLS certificate:         pending (cert-manager has not issued %s yet)\n", registryTLSSecretName)
	}

	// Pull credential: present when the app namespace's pull Secret has an entry for the public host.
	appNS, err := o.resolveAppNamespace(ctx, cs)
	if err != nil {
		fmt.Fprintf(stdout, "  Pull credential:         unknown (%v)\n", err)
		return nil
	}
	hosts, err := registryList(ctx, cs, appNS)
	if err != nil {
		return err
	}
	if host != "" && containsString(hosts, host) {
		fmt.Fprintf(stdout, "  Pull credential:         present (app namespace %s)\n", appNS)
	} else {
		fmt.Fprintf(stdout, "  Pull credential:         absent (app namespace %s)\n", appNS)
	}
	return nil
}

// runClusterRegistryInstall installs the in-cluster registry behind the cluster ingress: it verifies
// the ingress stack and the letsencrypt issuer are present, applies the registry manifest (internal
// Service + public Ingress with a Let's Encrypt certificate), generates a pull credential guarding the
// public endpoint and installs it in the app namespace, and wires burrowd's default build push target
// (internal endpoint) and public pull host (ADR-0054 §5).
func runClusterRegistryInstall(ctx context.Context, o clusterRegistryOptions, stdout, stderr io.Writer) error {
	if strings.TrimSpace(o.host) == "" {
		return fmt.Errorf("the in-cluster registry needs a public hostname for node pulls; pass --host <registry.example.com>")
	}
	cs, err := clusterRegistryClientset(o.kubeconfig)
	if err != nil {
		return err
	}

	// The registry depends on the ingress stack for its public TLS endpoint (ADR-0054): verify it is
	// present and point at `burrow cluster ingress install` if not, rather than deploying a registry
	// whose pull endpoint never gets a certificate.
	if err := verifyIngressStack(ctx, cs, o.kubeconfig); err != nil {
		return err
	}

	// The pull credential lands in the app namespace so app Pods inherit it; resolve it up front so a
	// missing burrowd is a clear stop before any write.
	appNS, err := o.resolveAppNamespace(ctx, cs)
	if err != nil {
		return err
	}

	manifests, err := renderRegistryManifest(o.namespace, o.host)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Installing the in-cluster registry:")
	if err := applyFn(ctx, o.kubeconfig, "", manifests, o.verbose, stdout, stderr); err != nil {
		return err
	}

	// Generate (or reuse) the pull credential: the basic-auth Secret backs the Ingress auth annotation
	// so the PUBLIC pull path is authenticated, while the INTERNAL push path stays credential-free.
	password, err := ensureRegistryCredential(ctx, cs, o.namespace, registryPullUsername)
	if err != nil {
		return err
	}
	// Install the same credential as a pull Secret in the app namespace and attach it to the default
	// ServiceAccount, so app Pods pull the public host. registryLogin is the ADR-0017 pull-secret path.
	if err := registryLogin(ctx, cs, appNS, o.host, registryPullUsername, password); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Installed a pull credential for %s in the app namespace (%s).\n", o.host, appNS)

	// Wire burrowd: the internal endpoint the build pushes to, and the public host the deploy
	// references so the node pulls through the ingress. burrowd is already running (install deployed
	// it), so patch the live Deployment's env; updating it rolls burrowd to pick the values up.
	if err := setBurrowdEnv(ctx, cs, o.namespace, buildRegistryEnv, connect.RegistryEndpoint(o.namespace)); err != nil {
		return err
	}
	if err := setBurrowdEnv(ctx, cs, o.namespace, buildPublicRegistryEnv, o.host); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Wired burrowd's build push endpoint to %s and its public pull host to %s.\n", connect.RegistryEndpoint(o.namespace), o.host)

	fmt.Fprintln(stdout, "\nDone. The in-cluster registry is installed. An in-cluster build with no explicit target")
	fmt.Fprintf(stdout, "now pushes to the internal endpoint and deploys by %s; external registries remain fully\n", o.host)
	fmt.Fprintln(stdout, "supported. The TLS certificate can take a few minutes to issue — check `burrow cluster registry`.")
	return nil
}

// runClusterRegistryUninstall removes the in-cluster registry: it deletes the registry resources,
// removes the generated pull credential from the app namespace, and unsets burrowd's default build
// push target and public pull host. Every step tolerates already-absent pieces so uninstall is
// idempotent.
func runClusterRegistryUninstall(ctx context.Context, o clusterRegistryOptions, stdout, stderr io.Writer) error {
	cs, err := clusterRegistryClientset(o.kubeconfig)
	if err != nil {
		return err
	}

	// Read the public host from burrowd's env before unwiring it, so the app-namespace pull credential
	// for that host can be removed. Best effort: a missing burrowd leaves the host blank.
	host, _ := burrowdEnv(ctx, cs, o.namespace, buildPublicRegistryEnv)

	if err := deleteRegistryResources(ctx, cs, o.namespace, stdout); err != nil {
		return err
	}

	// Remove the generated pull credential from the app namespace, best effort — an absent credential
	// or app namespace is nothing to undo, not a failure.
	if host != "" {
		if appNS, aerr := o.resolveAppNamespace(ctx, cs); aerr == nil {
			if lerr := registryLogout(ctx, cs, appNS, host); lerr == nil {
				fmt.Fprintf(stdout, "Removed the pull credential for %s from the app namespace (%s).\n", host, appNS)
			}
		}
	}

	if err := unsetBurrowdEnv(ctx, cs, o.namespace, buildRegistryEnv); err != nil {
		return err
	}
	if err := unsetBurrowdEnv(ctx, cs, o.namespace, buildPublicRegistryEnv); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Unset burrowd's default build push target and public pull host.")

	fmt.Fprintln(stdout, "\nDone. The in-cluster registry is removed. The PersistentVolume backing it and the issued")
	fmt.Fprintln(stdout, "TLS certificate Secret may linger depending on your StorageClass reclaim policy; a build that")
	fmt.Fprintln(stdout, "explicitly targets the in-cluster registry will now fail until you reinstall it or use an external one.")
	return nil
}

// verifyIngressStack checks that the pieces the registry's public TLS endpoint depends on are present:
// the ingress-nginx controller, cert-manager, and the letsencrypt ClusterIssuer `burrow cluster
// ingress install` creates. It returns a helpful error pointing at that command when a piece is
// missing, so the registry is never deployed with a pull endpoint that can never get a certificate.
func verifyIngressStack(ctx context.Context, cs kubernetes.Interface, kubeconfig string) error {
	hasNginx, err := ingressControllerPresent(ctx, cs)
	if err != nil {
		return err
	}
	if !hasNginx {
		return ingressStackMissingErr("the ingress-nginx controller")
	}
	hasCertManager, err := certManagerPresent(ctx, cs)
	if err != nil {
		return err
	}
	if !hasCertManager {
		return ingressStackMissingErr("cert-manager")
	}
	hasIssuer, err := clusterIssuerPresentFn(ctx, kubeconfig, defaultIssuerName)
	if err != nil {
		return err
	}
	if !hasIssuer {
		return ingressStackMissingErr(fmt.Sprintf("the %q ClusterIssuer", defaultIssuerName))
	}
	return nil
}

// ingressStackMissingErr names the missing ingress-stack piece and points at the command that
// provisions it.
func ingressStackMissingErr(what string) error {
	return fmt.Errorf("%s is not present, and the in-cluster registry needs the cluster ingress and TLS to expose its public pull endpoint; run `burrow cluster ingress install` first", what)
}

// deleteRegistryResources deletes the in-cluster registry's Deployment, Service, Ingress, config,
// basic-auth Secret, and PVC, ignoring anything already gone so uninstall is idempotent. The issued
// TLS Secret is left in place (documented residue).
func deleteRegistryResources(ctx context.Context, cs kubernetes.Interface, namespace string, stdout io.Writer) error {
	del := func(kind string, delete func() error) error {
		if err := delete(); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting the in-cluster registry %s: %w", kind, err)
		}
		return nil
	}
	steps := []struct {
		kind string
		fn   func() error
	}{
		{"Deployment", func() error {
			return cs.AppsV1().Deployments(namespace).Delete(ctx, registryName, metav1.DeleteOptions{})
		}},
		{"Service", func() error {
			return cs.CoreV1().Services(namespace).Delete(ctx, registryName, metav1.DeleteOptions{})
		}},
		{"Ingress", func() error {
			return cs.NetworkingV1().Ingresses(namespace).Delete(ctx, registryName, metav1.DeleteOptions{})
		}},
		{"ConfigMap", func() error {
			return cs.CoreV1().ConfigMaps(namespace).Delete(ctx, registryConfigName, metav1.DeleteOptions{})
		}},
		{"auth Secret", func() error {
			return cs.CoreV1().Secrets(namespace).Delete(ctx, registryAuthSecretName, metav1.DeleteOptions{})
		}},
		{"PersistentVolumeClaim", func() error {
			return cs.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, registryName, metav1.DeleteOptions{})
		}},
	}
	for _, s := range steps {
		if err := del(s.kind, s.fn); err != nil {
			return err
		}
	}
	fmt.Fprintln(stdout, "Removed the in-cluster registry Deployment, Service, Ingress, config, auth Secret, and volume claim.")
	return nil
}

// ensureRegistryCredential ensures the nginx basic-auth Secret guarding the public pull endpoint
// exists and returns its plaintext password. A re-install reuses the stored password (kept alongside
// the htpasswd entry) so pulls keep working rather than rotating the credential out from under them;
// the first install generates a strong random one.
func ensureRegistryCredential(ctx context.Context, cs kubernetes.Interface, namespace, user string) (string, error) {
	secrets := cs.CoreV1().Secrets(namespace)
	existing, getErr := secrets.Get(ctx, registryAuthSecretName, metav1.GetOptions{})
	create := apierrors.IsNotFound(getErr)
	switch {
	case getErr == nil:
		if pw := string(existing.Data["password"]); pw != "" {
			return pw, nil
		}
	case !create:
		return "", fmt.Errorf("reading the registry auth secret: %w", getErr)
	}

	password, err := generateRegistryPassword()
	if err != nil {
		return "", err
	}
	data := map[string][]byte{
		"auth":     []byte(htpasswdSHA(user, password)),
		"password": []byte(password),
	}
	if create {
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: registryAuthSecretName, Namespace: namespace, Labels: registryLabels},
			Data:       data,
		}, metav1.CreateOptions{})
	} else {
		existing.Data = data
		_, err = secrets.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return "", fmt.Errorf("writing the registry auth secret: %w", err)
	}
	return password, nil
}

// generateRegistryPassword returns a strong random URL-safe password for the generated pull
// credential.
func generateRegistryPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a registry password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// htpasswdSHA renders one htpasswd line in the `{SHA}` scheme nginx basic-auth understands:
// `user:{SHA}base64(sha1(password))`. SHA1 is unsalted, but the password is a long random value, so
// it only guards the public pull endpoint against unauthenticated access, not a weak human secret.
func htpasswdSHA(user, password string) string {
	sum := sha1.Sum([]byte(password))
	return user + ":{SHA}" + base64.StdEncoding.EncodeToString(sum[:])
}

// secretPresent reports whether the named Secret exists in the namespace.
func secretPresent(ctx context.Context, cs kubernetes.Interface, namespace, name string) (bool, error) {
	_, err := cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking for the %q secret: %w", name, err)
	}
	return true, nil
}

// resolveAppNamespace returns the namespace apps deploy into: the --app-namespace override, or the
// value read from the running burrowd Deployment. A missing burrowd surfaces as an error, so a
// half-install is a clear stop.
func (o clusterRegistryOptions) resolveAppNamespace(ctx context.Context, cs kubernetes.Interface) (string, error) {
	if o.appNamespace != "" {
		return o.appNamespace, nil
	}
	return appNamespaceOf(ctx, cs, o.namespace)
}

// setBurrowdEnv sets name=value on the running burrowd container so an in-cluster build picks it up
// (ADR-0053 §5, ADR-0054). It updates the value in place when already present, and updating the pod
// template rolls burrowd. A missing burrowd is a clear stop rather than a silent half-install.
func setBurrowdEnv(ctx context.Context, cs kubernetes.Interface, namespace, name, value string) error {
	dep, err := cs.AppsV1().Deployments(namespace).Get(ctx, burrowdDeploymentName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("burrowd is not installed in namespace %q; run `burrow install <context>` first", namespace)
	}
	if err != nil {
		return fmt.Errorf("reading the burrowd deployment: %w", err)
	}
	c := burrowdContainer(dep)
	if c == nil {
		return fmt.Errorf("the burrowd deployment in %q has no %q container to wire", namespace, burrowdDeploymentName)
	}
	for i := range c.Env {
		if c.Env[i].Name == name {
			if c.Env[i].Value == value {
				return nil
			}
			c.Env[i].Value = value
			return updateDeployment(ctx, cs, namespace, dep)
		}
	}
	c.Env = append(c.Env, corev1.EnvVar{Name: name, Value: value})
	return updateDeployment(ctx, cs, namespace, dep)
}

// unsetBurrowdEnv removes name from the burrowd container, reversing setBurrowdEnv. A missing burrowd
// or a value already absent is a no-op so uninstall is idempotent.
func unsetBurrowdEnv(ctx context.Context, cs kubernetes.Interface, namespace, name string) error {
	dep, err := cs.AppsV1().Deployments(namespace).Get(ctx, burrowdDeploymentName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the burrowd deployment: %w", err)
	}
	c := burrowdContainer(dep)
	if c == nil {
		return nil
	}
	for i := range c.Env {
		if c.Env[i].Name == name {
			c.Env = append(c.Env[:i], c.Env[i+1:]...)
			return updateDeployment(ctx, cs, namespace, dep)
		}
	}
	return nil
}

// burrowdContainer returns a pointer to the burrowd container in the Deployment's pod template, or nil
// when it is not found, so callers can edit its env in place.
func burrowdContainer(dep *appsv1.Deployment) *corev1.Container {
	for i := range dep.Spec.Template.Spec.Containers {
		if dep.Spec.Template.Spec.Containers[i].Name == burrowdDeploymentName {
			return &dep.Spec.Template.Spec.Containers[i]
		}
	}
	return nil
}

// updateDeployment writes the edited Deployment back.
func updateDeployment(ctx context.Context, cs kubernetes.Interface, namespace string, dep *appsv1.Deployment) error {
	if _, err := cs.AppsV1().Deployments(namespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating the burrowd deployment: %w", err)
	}
	return nil
}

// containsString reports whether s is in list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
