// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
)

// defaultRegistryImage is the pinned Zot image (project-zot, Apache-2.0) the optional in-cluster
// registry runs (ADR-0053 §5). Pinned so the install is reproducible; bump deliberately.
const defaultRegistryImage = "ghcr.io/project-zot/zot-linux-amd64:v2.1.2"

// burrowdDeploymentName is the control-plane Deployment `burrow cluster registry install` patches to
// wire (and uninstall to unwire) the in-cluster registry as burrowd's default build push target.
const burrowdDeploymentName = "burrowd"

// buildRegistryEnv is the burrowd container env var that names the in-cluster registry burrowd
// defaults an empty build target to (ADR-0053 §5). `burrow cluster registry install` sets it on the
// running burrowd Deployment; uninstall removes it. burrowd reads it at startup (cmd/burrowd/main.go).
const buildRegistryEnv = "BURROW_BUILD_REGISTRY"

// k3sRegistriesPath is where k3s reads its containerd registry configuration. `burrow cluster registry
// install` writes this on a k3s node so the node's containerd resolves the in-cluster registry name and
// pulls what a build pushes (ADR-0053 §5). k3s watches this file and reloads it without a restart.
const k3sRegistriesPath = "/etc/rancher/k3s/registries.yaml"

// registryManifest is the in-cluster registry manifest template (Zot's PVC, config, Deployment, and
// NodePort Service), embedded so `burrow cluster registry install` renders and applies it standalone —
// separate from the control-plane install manifests, because install provisions only the control plane
// (ADR-0054).
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
	RegistryNodePort int
}

// renderRegistryManifest renders the in-cluster registry manifest for the control-plane namespace,
// pinning the Zot image, the serve port, and the NodePort the node's containerd reaches it through.
func renderRegistryManifest(namespace string) (string, error) {
	var sb strings.Builder
	err := registryTemplate.Execute(&sb, registryRenderOptions{
		Namespace:        namespace,
		RegistryEndpoint: connect.RegistryEndpoint(namespace),
		RegistryImage:    defaultRegistryImage,
		RegistryPort:     connect.DefaultRegistryPort,
		RegistryNodePort: connect.DefaultRegistryNodePort,
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

// writeRegistriesConfigFn writes the k3s registries.yaml for the in-cluster registry. It is a package
// var so a test can substitute a recorder without touching the real /etc path; the real implementation
// writes the file (creating its directory).
var writeRegistriesConfigFn = writeRegistriesConfig

// removeRegistriesConfigFn removes the k3s registries.yaml on uninstall. A package var so a test can
// substitute a recorder; the real implementation deletes the file.
var removeRegistriesConfigFn = os.Remove

// k3sNodePresentFn reports whether this machine is a k3s node, so install/uninstall only touch the
// node's containerd registry config when there is one. A package var so tests need not touch /etc; the
// real check looks for the k3s registry-config directory.
var k3sNodePresentFn = k3sNodePresent

// k3sMirrorConfiguredFn reports whether the local k3s registries.yaml already mirrors the in-cluster
// registry into the node's containerd. A package var so status/uninstall tests need not touch /etc.
var k3sMirrorConfiguredFn = k3sMirrorConfigured

// k3sNodePresent reports whether this machine is a k3s node, by the presence of the directory k3s reads
// its registry configuration from. Off a k3s node (a laptop driving a remote cluster) there is no local
// containerd to wire, so the containerd step is skipped with a note rather than creating stray files.
func k3sNodePresent() bool {
	_, err := os.Stat(filepath.Dir(k3sRegistriesPath))
	return err == nil
}

// k3sMirrorConfigured reports whether the local k3s registries.yaml mirrors the in-cluster registry for
// the given control-plane namespace (its endpoint appears in the file). Best-effort: a missing or
// unreadable file reports not-configured.
func k3sMirrorConfigured(namespace string) bool {
	b, err := os.ReadFile(k3sRegistriesPath)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), connect.RegistryEndpoint(namespace))
}

// buildRegistriesConfig renders the k3s containerd registry configuration that lets the node pull
// images pushed to the in-cluster registry (ADR-0053 §5). It mirrors the in-cluster registry reference
// host (what a build pushes to and the deploy pulls by) to the pinned NodePort on localhost, and marks
// it insecure because the in-cluster registry serves plain HTTP — the traffic never leaves the node.
// The mirror host MUST equal connect.RegistryEndpoint(namespace) so the reference the deploy pins is
// exactly what containerd rewrites.
func buildRegistriesConfig(namespace string) string {
	host := connect.RegistryEndpoint(namespace)
	return fmt.Sprintf(`mirrors:
  "%s":
    endpoint:
      - "http://127.0.0.1:%d"
configs:
  "%s":
    tls:
      insecure_skip_verify: true
`, host, connect.DefaultRegistryNodePort, host)
}

// writeRegistriesConfig writes the rendered k3s registry configuration to path, creating the parent
// directory if k3s has not yet. k3s watches the file and reloads it, so no restart is needed.
func writeRegistriesConfig(namespace, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the k3s registry config directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(buildRegistriesConfig(namespace)), 0o644); err != nil {
		return fmt.Errorf("writing the k3s registry config %s: %w", path, err)
	}
	return nil
}

// clusterRegistryOptions are the inputs to the `burrow cluster registry` subcommands.
type clusterRegistryOptions struct {
	namespace  string
	kubeconfig string
	verbose    bool
}

// newClusterRegistryCmd is a setup command (not part of `burrow install`, ADR-0054): it installs,
// inspects, and removes the OPTIONAL in-cluster image registry (Zot) that gives the in-cluster build a
// zero-config push target (ADR-0053 §5). Like `burrow cluster ingress install`, it acts with the
// developer's kubeconfig — it is not an agent operation and does not route through burrowd's guarded
// API. It manages the registry that RUNS IN the cluster; for credentials to EXTERNAL registries (GHCR,
// Docker Hub, ...) the cluster pulls from, use `burrow config registry`.
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
	parent.PersistentFlags().StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")

	install := &cobra.Command{
		Use:   "install",
		Short: "Install the in-cluster registry and wire it as the default build push target",
		Long: "install deploys the optional lightweight in-cluster registry (Zot) into the control-plane\n" +
			"namespace, wires it as burrowd's zero-config default build push target, and — on a k3s node —\n" +
			"configures the node's containerd to pull from it. External registries stay fully supported;\n" +
			"this is simply a registry that happens to be local. Run it after `burrow install` when you\n" +
			"want the in-cluster build to need no external registry account.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClusterRegistryInstall(cmd.Context(), o, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	install.Flags().BoolVar(&o.verbose, "verbose", false, "show every resource burrow applies instead of a summary")

	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the in-cluster registry and unwire it from the build push target",
		Long: "uninstall removes the in-cluster registry Deployment, Service, config, and its\n" +
			"PersistentVolumeClaim, unsets burrowd's default build push target, and — on a k3s node —\n" +
			"removes the containerd registry mirror.\n\n" +
			"Residue to know about: deleting the PersistentVolumeClaim releases the volume, but whether\n" +
			"the underlying PersistentVolume (and the images stored on it) is reclaimed depends on the\n" +
			"StorageClass reclaim policy. After uninstall, a build that explicitly targets the in-cluster\n" +
			"registry will fail until you reinstall it or point the build at an external registry.",
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
	_, err := cs.AppsV1().Deployments(namespace).Get(ctx, "burrow-registry", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking for the in-cluster registry: %w", err)
	}
	return true, nil
}

// runClusterRegistryStatus reports whether the in-cluster registry is installed. When it is, it prints
// the in-cluster address a build pushes to and a deploy pulls by, the NodePort the node reaches it
// through, and whether this k3s node's containerd is wired to it. When it is not, it prints the
// one-line hint to install it.
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
		fmt.Fprintln(stdout, "Install it: burrow cluster registry install")
		return nil
	}
	endpoint := connect.RegistryEndpoint(o.namespace)
	fmt.Fprintln(stdout, "In-cluster registry: installed.")
	fmt.Fprintf(stdout, "  In-cluster address:  %s (a build pushes here; a deploy pulls by this reference)\n", endpoint)
	fmt.Fprintf(stdout, "  Node address:        http://127.0.0.1:%d (the NodePort the node's containerd reaches it at)\n", connect.DefaultRegistryNodePort)
	if k3sMirrorConfiguredFn(o.namespace) {
		fmt.Fprintf(stdout, "  k3s containerd:      wired (%s mirrors %s to the node port)\n", k3sRegistriesPath, endpoint)
	} else {
		fmt.Fprintf(stdout, "  k3s containerd:      not wired on this machine (%s has no mirror for %s)\n", k3sRegistriesPath, endpoint)
	}
	return nil
}

// runClusterRegistryInstall installs the in-cluster registry: it applies the registry manifest, wires
// it as burrowd's default build push target, and (on a k3s node) configures the node's containerd to
// pull from it (ADR-0053 §5).
func runClusterRegistryInstall(ctx context.Context, o clusterRegistryOptions, stdout, stderr io.Writer) error {
	cs, err := clusterRegistryClientset(o.kubeconfig)
	if err != nil {
		return err
	}

	manifests, err := renderRegistryManifest(o.namespace)
	if err != nil {
		return err
	}

	// Configure the node's containerd BEFORE the deploy on a k3s node, so the node can pull from the
	// registry as soon as it is up (k3s watches registries.yaml and reloads it, no restart). Off a k3s
	// node there is no local containerd to wire, so note what the node's runtime needs instead.
	if k3sNodePresentFn() {
		if err := writeRegistriesConfigFn(o.namespace, k3sRegistriesPath); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Configured k3s to pull from the in-cluster registry (%s).\n", k3sRegistriesPath)
	} else {
		fmt.Fprintf(stdout, "Note: this machine is not a k3s node, so the node's container runtime was not configured.\n"+
			"The node must mirror %s to http://127.0.0.1:%d for pulls to resolve (on k3s, run this on the node).\n",
			connect.RegistryEndpoint(o.namespace), connect.DefaultRegistryNodePort)
	}

	fmt.Fprintln(stdout, "Installing the in-cluster registry:")
	if err := applyFn(ctx, o.kubeconfig, "", manifests, o.verbose, stdout, stderr); err != nil {
		return err
	}

	// Wire burrowd's default build push target so an in-cluster build with no explicit target lands
	// here. burrowd is already running (install deployed it), so patch the live Deployment's env; a
	// missing burrowd is a clear stop rather than a silent half-install.
	if err := setBuildRegistryEnv(ctx, cs, o.namespace, connect.RegistryEndpoint(o.namespace)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Wired burrowd's default build push target to %s.\n", connect.RegistryEndpoint(o.namespace))

	fmt.Fprintln(stdout, "\nDone. The in-cluster registry is installed. An in-cluster build with no explicit")
	fmt.Fprintln(stdout, "target now pushes here; external registries remain fully supported.")
	return nil
}

// runClusterRegistryUninstall removes the in-cluster registry: it deletes the registry resources,
// unsets burrowd's default build push target, and (on a k3s node) removes the containerd mirror.
func runClusterRegistryUninstall(ctx context.Context, o clusterRegistryOptions, stdout, stderr io.Writer) error {
	cs, err := clusterRegistryClientset(o.kubeconfig)
	if err != nil {
		return err
	}

	if err := deleteRegistryResources(ctx, cs, o.namespace, stdout); err != nil {
		return err
	}

	if err := unsetBuildRegistryEnv(ctx, cs, o.namespace); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Unset burrowd's default build push target.")

	// Reverse the node's containerd wiring on a k3s node whose registries.yaml mirrors this registry.
	if k3sNodePresentFn() && k3sMirrorConfiguredFn(o.namespace) {
		if err := removeRegistriesConfigFn(k3sRegistriesPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing the k3s registry config %s: %w", k3sRegistriesPath, err)
		}
		fmt.Fprintf(stdout, "Removed the k3s containerd registry mirror (%s).\n", k3sRegistriesPath)
	}

	fmt.Fprintln(stdout, "\nDone. The in-cluster registry is removed. The PersistentVolume backing it may")
	fmt.Fprintln(stdout, "linger depending on your StorageClass reclaim policy; a build that explicitly targets")
	fmt.Fprintln(stdout, "the in-cluster registry will now fail until you reinstall it or use an external one.")
	return nil
}

// deleteRegistryResources deletes the in-cluster registry's Deployment, Service, config, and PVC,
// ignoring anything already gone so uninstall is idempotent.
func deleteRegistryResources(ctx context.Context, cs kubernetes.Interface, namespace string, stdout io.Writer) error {
	del := func(kind string, delete func() error) error {
		if err := delete(); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting the in-cluster registry %s: %w", kind, err)
		}
		return nil
	}
	if err := del("Deployment", func() error {
		return cs.AppsV1().Deployments(namespace).Delete(ctx, "burrow-registry", metav1.DeleteOptions{})
	}); err != nil {
		return err
	}
	if err := del("Service", func() error {
		return cs.CoreV1().Services(namespace).Delete(ctx, "burrow-registry", metav1.DeleteOptions{})
	}); err != nil {
		return err
	}
	if err := del("ConfigMap", func() error {
		return cs.CoreV1().ConfigMaps(namespace).Delete(ctx, "burrow-registry-config", metav1.DeleteOptions{})
	}); err != nil {
		return err
	}
	if err := del("PersistentVolumeClaim", func() error {
		return cs.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, "burrow-registry", metav1.DeleteOptions{})
	}); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Removed the in-cluster registry Deployment, Service, config, and volume claim.")
	return nil
}

// setBuildRegistryEnv sets the BURROW_BUILD_REGISTRY env on the running burrowd container to endpoint,
// so an in-cluster build with no explicit target defaults here (ADR-0053 §5). It updates the value in
// place when already present, and updating the pod template rolls burrowd to pick it up.
func setBuildRegistryEnv(ctx context.Context, cs kubernetes.Interface, namespace, endpoint string) error {
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
		if c.Env[i].Name == buildRegistryEnv {
			if c.Env[i].Value == endpoint {
				return nil
			}
			c.Env[i].Value = endpoint
			return updateDeployment(ctx, cs, namespace, dep)
		}
	}
	c.Env = append(c.Env, corev1.EnvVar{Name: buildRegistryEnv, Value: endpoint})
	return updateDeployment(ctx, cs, namespace, dep)
}

// unsetBuildRegistryEnv removes the BURROW_BUILD_REGISTRY env from the burrowd container, reversing
// setBuildRegistryEnv. A missing burrowd or a value already absent is a no-op so uninstall is
// idempotent.
func unsetBuildRegistryEnv(ctx context.Context, cs kubernetes.Interface, namespace string) error {
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
		if c.Env[i].Name == buildRegistryEnv {
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
