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
// type=LoadBalancer; anything else (bare-metal, k3s/k3d, kind, an empty providerID) is treated as
// NodePort-only. The map is also the inference behind LoadBalancer support (ADR-0034).
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
// (get/list on nodes, storageclasses, ingressclasses) plus API-group discovery (no RBAC). It never
// writes.
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
// the ingress controller and its IngressClasses, the default and all StorageClasses, the cloud
// provider (from node providerIDs/labels), and cert-manager (via API-group discovery), and infers
// LoadBalancer support from the provider. It performs only get/list reads and API-group discovery
// — it never writes. It is a free function so the same detection runs whether driven by the
// kubeconfig client (install) or burrowd's in-cluster client (live). The returned report omits the
// DNS capability, which is a control-plane registry fact filled by the engine.
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
	caps.LoadBalancer = controlplane.LoadBalancerCapability{
		Supported: provider.Cloud != "",
		Inferred:  true,
	}

	certManager, err := detectCertManager(client)
	if err != nil {
		return controlplane.ClusterCapabilities{}, err
	}
	caps.CertManager = certManager

	return caps, nil
}

// detectIngress lists IngressClasses (networking.k8s.io). An ingress controller installs an
// IngressClass, so their presence is the signal a controller is available and names the class to
// bind an Ingress to.
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
	return controlplane.IngressCapability{Present: len(classes) > 0, Classes: classes}, nil
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
