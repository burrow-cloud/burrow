// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.ClusterProber = (*Prober)(nil)

// defaultStorageClassAnnotation marks the cluster's default StorageClass — the one a PVC with no
// storageClassName binds to. The GA annotation is authoritative; the older beta annotation is also
// honored so a default set the old way is still detected.
const (
	defaultStorageClassAnnotation     = "storageclass.kubernetes.io/is-default-class"
	defaultStorageClassAnnotationBeta = "storageclass.beta.kubernetes.io/is-default-class"
)

// certManagerAPIGroup is cert-manager's API group; its presence in API-group discovery means
// cert-manager's CRDs are installed. Discovery needs no RBAC (ADR-0034).
const certManagerAPIGroup = "cert-manager.io"

// cloudProviders maps a providerID scheme (the part before "://" in a Node's spec.providerID) to a
// human label for known clouds. A node on one of these is taken to support Service
// type=LoadBalancer with a billable cloud load balancer. A recognized cloud is one — but no longer
// the only — signal behind LoadBalancer support: k3s's built-in servicelb and MetalLB also service
// LoadBalancers on non-cloud clusters and are detected separately (ADR-0034, ADR-0043).
var cloudProviders = map[string]string{
	"aws":          "AWS",
	"azure":        "Azure",
	"digitalocean": "DigitalOcean",
	"gce":          "Google Cloud",
	"hetzner":      "Hetzner Cloud",
	"ibm":          "IBM Cloud",
	"linode":       "Linode",
	"openstack":    "OpenStack",
	"scaleway":     "Scaleway",
	"vsphere":      "vSphere",
}

// Prober is the production controlplane.ClusterProber: it detects a cluster's read-only
// capabilities over a client-go clientset (ADR-0034). It wraps the same clientset burrowd already
// holds, so the live read needs only the narrow read-only ClusterRole the install grants
// (get/list on nodes, storageclasses, ingressclasses, and deployments) plus API-group discovery
// (no RBAC). It never writes.
type Prober struct {
	client kubernetes.Interface
}

// NewProber returns a Prober over the given clientset.
func NewProber(client kubernetes.Interface) *Prober {
	return &Prober{client: client}
}

// NewProberFromConfig builds a Prober from a REST config — burrowd's in-cluster config, so the
// live capability read uses the narrow read-only ClusterRole the install grants (ADR-0034).
func NewProberFromConfig(cfg *rest.Config) (*Prober, error) {
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: building clientset: %w", err)
	}
	return NewProber(client), nil
}

// DetectCapabilities reads the cluster's capabilities read-only.
func (p *Prober) DetectCapabilities(ctx context.Context) (controlplane.ClusterCapabilities, error) {
	return DetectCapabilities(ctx, p.client)
}

// DetectCapabilities reads a cluster's capabilities read-only over the given clientset (ADR-0034):
// the ingress controller (a ready ingress-nginx controller Deployment) and its IngressClasses, the
// default and all StorageClasses, the cloud
// provider (from node providerIDs/labels), cert-manager and metrics-server (via API-group
// discovery), and detects
// LoadBalancer support from whatever actually services LoadBalancers — a recognized cloud provider,
// k3s's built-in servicelb, or MetalLB (ADR-0043). It performs only get/list reads and API-group
// discovery — it never writes. It is a free function so the same detection runs whether driven by
// the kubeconfig client (install) or burrowd's in-cluster client (live). The returned report omits
// the DNS capability, which is a control-plane registry fact filled by the engine.
func DetectCapabilities(ctx context.Context, client kubernetes.Interface) (controlplane.ClusterCapabilities, error) {
	var caps controlplane.ClusterCapabilities

	ingress, err := detectIngress(ctx, client)
	if err != nil {
		return controlplane.ClusterCapabilities{}, err
	}
	caps.Ingress = ingress

	storage, err := detectStorage(ctx, client)
	if err != nil {
		return controlplane.ClusterCapabilities{}, err
	}
	caps.Storage = storage

	provider, err := detectProvider(ctx, client)
	if err != nil {
		return controlplane.ClusterCapabilities{}, err
	}
	caps.Provider = provider

	loadBalancer, err := detectLoadBalancer(ctx, client, provider)
	if err != nil {
		return controlplane.ClusterCapabilities{}, err
	}
	caps.LoadBalancer = loadBalancer

	certManager, err := detectCertManager(client)
	if err != nil {
		return controlplane.ClusterCapabilities{}, err
	}
	caps.CertManager = certManager

	metricsServer, err := detectMetricsServer(client)
	if err != nil {
		return controlplane.ClusterCapabilities{}, err
	}
	caps.MetricsServer = metricsServer

	return caps, nil
}

// ingressNginxControllerSelector matches the ingress-nginx controller Deployment by its standard
// recommended labels. The running controller — not merely an IngressClass — is what routes traffic,
// assigns an external address, and runs the admission webhook, so its readiness is the real "you
// can expose" signal.
const ingressNginxControllerSelector = "app.kubernetes.io/name=ingress-nginx,app.kubernetes.io/component=controller"

// detectIngress reports the cluster's ingress situation. It lists IngressClasses (networking.k8s.io)
// to name the class an Ingress binds to and — separately — checks that an ingress-nginx controller
// Deployment is actually present and ready. An IngressClass is cluster-scoped and can OUTLIVE the
// controller that created it: delete the ingress-nginx release and its namespace and the "nginx"
// IngressClass is left orphaned, routing nothing. So the class alone is not proof a controller is
// available; Present (the "you can expose" signal) is set from controller readiness, while Classes
// is always reported so an Ingress can still name its class.
func detectIngress(ctx context.Context, client kubernetes.Interface) (controlplane.IngressCapability, error) {
	list, err := client.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return controlplane.IngressCapability{}, fmt.Errorf("kube: listing ingress classes: %w", err)
	}
	classes := make([]string, 0, len(list.Items))
	for i := range list.Items {
		classes = append(classes, list.Items[i].Name)
	}
	sort.Strings(classes)

	ready, err := ingressControllerReady(ctx, client)
	if err != nil {
		return controlplane.IngressCapability{}, err
	}
	return controlplane.IngressCapability{Present: ready, Classes: classes}, nil
}

// ingressControllerReady reports whether an ingress-nginx controller Deployment is present with at
// least one ready replica. It lists Deployments across all namespaces filtered by the standard
// ingress-nginx controller labels, so it finds the controller wherever the release was installed
// (the conventional "ingress-nginx" namespace or any other). This is the minimal-RBAC accurate
// signal: it needs only get/list on apps/deployments — the one grant added to the
// burrowd-cluster-capabilities ClusterRole — which is narrower and clearer than probing the
// admission ValidatingWebhookConfiguration.
func ingressControllerReady(ctx context.Context, client kubernetes.Interface) (bool, error) {
	deps, err := client.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: ingressNginxControllerSelector,
	})
	if err != nil {
		return false, fmt.Errorf("kube: listing ingress-nginx controller deployments: %w", err)
	}
	for i := range deps.Items {
		if deps.Items[i].Status.ReadyReplicas > 0 {
			return true, nil
		}
	}
	return false, nil
}

// detectStorage lists StorageClasses (storage.k8s.io) and picks the default — the one annotated
// storageclass.kubernetes.io/is-default-class=true (the older beta annotation is also honored).
func detectStorage(ctx context.Context, client kubernetes.Interface) (controlplane.StorageCapability, error) {
	list, err := client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return controlplane.StorageCapability{}, fmt.Errorf("kube: listing storage classes: %w", err)
	}
	out := controlplane.StorageCapability{}
	classes := make([]string, 0, len(list.Items))
	for i := range list.Items {
		sc := &list.Items[i]
		classes = append(classes, sc.Name)
		if !out.DefaultPresent && isDefaultStorageClass(sc.Annotations) {
			out.DefaultPresent = true
			out.DefaultClass = sc.Name
		}
	}
	sort.Strings(classes)
	out.Classes = classes
	return out, nil
}

// isDefaultStorageClass reports whether the annotations mark the class as the cluster default.
func isDefaultStorageClass(annotations map[string]string) bool {
	return annotations[defaultStorageClassAnnotation] == "true" ||
		annotations[defaultStorageClassAnnotationBeta] == "true"
}

// detectProvider infers the cloud provider from the nodes' spec.providerID (its scheme, e.g.
// "digitalocean://…" → DigitalOcean). A node with no recognized provider scheme leaves the cloud
// unknown — treated as bare-metal for the LoadBalancer inference.
func detectProvider(ctx context.Context, client kubernetes.Interface) (controlplane.ProviderCapability, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return controlplane.ProviderCapability{}, fmt.Errorf("kube: listing nodes: %w", err)
	}
	for i := range nodes.Items {
		scheme := providerIDScheme(nodes.Items[i].Spec.ProviderID)
		if scheme == "" {
			continue
		}
		if name, ok := cloudProviders[scheme]; ok {
			return controlplane.ProviderCapability{Cloud: scheme, Name: name}, nil
		}
		// A providerID we don't map to a known cloud (e.g. "k3s") names the platform but is not a
		// cloud that provisions load balancers — report it without claiming LB support.
		return controlplane.ProviderCapability{Cloud: "", Name: ""}, nil
	}
	return controlplane.ProviderCapability{}, nil
}

// providerIDScheme returns the scheme of a Node's spec.providerID — the part before "://", e.g.
// "digitalocean://123" → "digitalocean". An empty or malformed providerID returns "".
func providerIDScheme(providerID string) string {
	i := strings.Index(providerID, "://")
	if i <= 0 {
		return ""
	}
	return providerID[:i]
}

// serviceLBProvider labels the k3s servicelb mechanism in the LoadBalancer capability.
const serviceLBProvider = "servicelb"

// metalLBProvider labels MetalLB in the LoadBalancer capability.
const metalLBProvider = "metallb"

// metalLBControllerSelector and metalLBLegacyControllerSelector match the MetalLB controller
// Deployment. The Helm chart labels it with the recommended app.kubernetes.io/* keys; the upstream
// static manifest uses the older app/component keys. Both are matched so either install method is
// detected. The controller (not the speaker DaemonSet) is the allocator, so it is the definitive
// "MetalLB is installed" signal.
const (
	metalLBControllerSelector       = "app.kubernetes.io/name=metallb,app.kubernetes.io/component=controller"
	metalLBLegacyControllerSelector = "app=metallb,component=controller"
)

// detectLoadBalancer reports whether the cluster can service a Service type=LoadBalancer, and by what
// (ADR-0043). Support is not cloud-only: it is true when ANY LoadBalancer provider is present — a
// recognized cloud provider (a billable cloud load balancer), k3s's built-in servicelb, or MetalLB.
// It reports which provider so the agent can reason about cost (only a cloud LB is billable) and so
// a bare cluster with none can be guided to install MetalLB rather than told LoadBalancer is
// impossible. Supported stays an inference (Inferred=true): detection recognizes a provider rather
// than provisioning a real LoadBalancer, which is the only direct test.
func detectLoadBalancer(ctx context.Context, client kubernetes.Interface, provider controlplane.ProviderCapability) (controlplane.LoadBalancerCapability, error) {
	if provider.Cloud != "" {
		return controlplane.LoadBalancerCapability{Supported: true, Inferred: true, Provider: provider.Cloud}, nil
	}

	serviceLB, err := clusterRunsServiceLB(ctx, client)
	if err != nil {
		return controlplane.LoadBalancerCapability{}, err
	}
	if serviceLB {
		return controlplane.LoadBalancerCapability{Supported: true, Inferred: true, Provider: serviceLBProvider}, nil
	}

	metalLB, err := metalLBInstalled(ctx, client)
	if err != nil {
		return controlplane.LoadBalancerCapability{}, err
	}
	if metalLB {
		return controlplane.LoadBalancerCapability{Supported: true, Inferred: true, Provider: metalLBProvider}, nil
	}

	return controlplane.LoadBalancerCapability{Supported: false, Inferred: true}, nil
}

// clusterRunsServiceLB reports whether the cluster runs k3s's built-in servicelb (klipper-lb) — the
// load-balancer controller that assigns type=LoadBalancer Services a real external IP on k3s/k3d with
// no cloud provider (proven on k3d by the LoadBalancer e2e). Detecting servicelb directly is awkward:
// it runs in-process in the k3s server, so there is no standalone controller Deployment to look for,
// and the per-Service svclb-* DaemonSets exist only AFTER a LoadBalancer Service is created, so
// neither is a reliable pre-exposure signal. The robust, zero-extra-RBAC signal is the k3s providerID
// scheme itself: the same k3s cloud-controller that stamps each Node's spec.providerID as
// "k3s://<node>" is the one that runs servicelb, so a k3s node IS a servicelb cluster. It reuses the
// nodes get/list grant the capability ClusterRole already holds. The accepted tradeoff is a possible
// over-report only when servicelb was explicitly turned off (k3s --disable=servicelb), a rare
// non-default — acceptable for a read-only capability survey, where provisioning a real LoadBalancer
// is the only exact test.
func clusterRunsServiceLB(ctx context.Context, client kubernetes.Interface) (bool, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("kube: listing nodes for servicelb detection: %w", err)
	}
	for i := range nodes.Items {
		if providerIDScheme(nodes.Items[i].Spec.ProviderID) == "k3s" {
			return true, nil
		}
	}
	return false, nil
}

// metalLBInstalled reports whether MetalLB is installed by finding its controller Deployment via the
// standard MetalLB labels, in any namespace. It reuses the apps/deployments get/list grant the
// capability ClusterRole already holds for ingress-controller detection, so it needs NO new RBAC. It
// matches both the Helm-chart and static-manifest label schemes.
func metalLBInstalled(ctx context.Context, client kubernetes.Interface) (bool, error) {
	for _, selector := range []string{metalLBControllerSelector, metalLBLegacyControllerSelector} {
		deps, err := client.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			return false, fmt.Errorf("kube: listing metallb controller deployments: %w", err)
		}
		if len(deps.Items) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// detectCertManager reports whether cert-manager is installed by looking for its API group in
// API-group discovery. Discovery needs no RBAC (ADR-0034): the presence of the cert-manager.io
// group means its CRDs are installed.
func detectCertManager(client kubernetes.Interface) (controlplane.CertManagerCapability, error) {
	groups, err := client.Discovery().ServerGroups()
	if err != nil {
		return controlplane.CertManagerCapability{}, fmt.Errorf("kube: discovering API groups: %w", err)
	}
	for _, g := range groups.Groups {
		if g.Name == certManagerAPIGroup {
			return controlplane.CertManagerCapability{Present: true}, nil
		}
	}
	return controlplane.CertManagerCapability{}, nil
}

// detectMetricsServer reports whether metrics-server is serving the Kubernetes Metrics API by
// looking for the metrics.k8s.io group in API-group discovery. Discovery needs no RBAC (ADR-0034):
// the presence of the group means a metrics-server (or a vendor's equivalent on k3s, GKE, or AKS)
// is registered. This is also how install detects a vendor-shipped copy so it does not install a
// second one (ADR-0054 §1).
func detectMetricsServer(client kubernetes.Interface) (controlplane.MetricsServerCapability, error) {
	groups, err := client.Discovery().ServerGroups()
	if err != nil {
		return controlplane.MetricsServerCapability{}, fmt.Errorf("kube: discovering API groups: %w", err)
	}
	for _, g := range groups.Groups {
		if g.Name == metricsAPIGroup {
			return controlplane.MetricsServerCapability{Present: true}, nil
		}
	}
	return controlplane.MetricsServerCapability{}, nil
}
