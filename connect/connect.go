// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package connect reaches the in-cluster Burrow control plane from a developer's machine
// using their ambient kubeconfig and the Kubernetes API server's service proxy — no
// port-forward, no ingress (ADR-0014). It reads the API token from the install Secret, so
// a developer with kubectl access configures nothing else.
//
// It is Apache-2.0 (the client surface): it imports client-go to talk to the API server
// but not the controlplane packages — it reaches Burrow over HTTP, like any client.
package connect

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/burrow-cloud/burrow/client"
)

// Defaults for a standard `burrow install`.
const (
	DefaultNamespace   = "burrow"
	DefaultService     = "burrowd"
	DefaultPort        = 8080
	DefaultTokenSecret = "burrowd-api-token"
	DefaultTokenKey    = "token"
	// DefaultAddonNamespace is where `install` provisions Burrow's curated backing services
	// (logs, metrics) and their collectors — separate from both the control-plane namespace
	// (which holds credentials) and the app namespace (user workloads), so add-ons don't
	// clutter apps and stay out of the credential blast radius (ADR-0025).
	DefaultAddonNamespace = "burrow-addons"
	// DefaultAppNamespace is where `install` deploys apps by default: a dedicated namespace
	// rather than the cluster's shared `default`, so burrowd's namespace-scoped Secrets grant
	// (ADR-0029) stays isolated to Burrow's own app workloads. An operator may still choose
	// `--app-namespace default` explicitly.
	DefaultAppNamespace = "burrow-apps"

	// DefaultRegistryService is the in-cluster Service name of the optional lightweight registry
	// `burrow cluster registry install` deploys (Zot, ADR-0053 §5). It is the zero-config default
	// push target for the in-cluster build — a registry that happens to be local; external
	// registries remain fully supported.
	DefaultRegistryService = "burrow-registry"
	// DefaultRegistryPort is the port the in-cluster registry serves on (Zot's default).
	DefaultRegistryPort = 5000
	// DefaultRegistryNodePort is the fixed NodePort the in-cluster registry Service is published on
	// so the node's containerd reaches it at a deterministic localhost address through the k3s
	// registries.yaml mirror (ADR-0053 §5). Pinned so the mirror endpoint the bootstrap writes and
	// the Service agree without having to discover a dynamically assigned port.
	DefaultRegistryNodePort = 30500
)

// RegistryEndpoint is the in-cluster registry reference host:port for the given control-plane
// namespace (ADR-0053 §5): the DNS name a build pushes to and the resulting deploy pulls by. It is a
// fully-qualified cluster-DNS name so a build pod in any namespace resolves it, and it is the exact
// host the k3s registries.yaml mirror keys on so the node's containerd can pull what was pushed.
func RegistryEndpoint(namespace string) string {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", DefaultRegistryService, namespace, DefaultRegistryPort)
}

// Options configures how to find the control plane. The zero value uses the defaults and
// the ambient kubeconfig.
type Options struct {
	Kubeconfig string // explicit kubeconfig path; empty = in-cluster, else ambient
	// Context selects which kubeconfig context (cluster) to target. Empty = the current
	// context (today's behavior). Each context's cluster runs its own burrowd, so this is
	// how a developer points an operation at a specific environment's control plane (ADR-0035).
	Context     string
	Namespace   string
	Service     string
	Port        int
	TokenSecret string
	TokenKey    string
	// ClientVersion is this client's release version, forwarded as X-Burrow-Client-Version so
	// burrowd can make version skew legible (ADR-0039). Empty omits the header.
	ClientVersion string
}

func (o *Options) setDefaults() {
	if o.Namespace == "" {
		o.Namespace = DefaultNamespace
	}
	if o.Service == "" {
		o.Service = DefaultService
	}
	if o.Port == 0 {
		o.Port = DefaultPort
	}
	if o.TokenSecret == "" {
		o.TokenSecret = DefaultTokenSecret
	}
	if o.TokenKey == "" {
		o.TokenKey = DefaultTokenKey
	}
}

// KubeconfigTransport reaches the in-cluster control plane through the Kubernetes API-server
// service proxy, authenticated by the developer's kubeconfig, with the burrowd API token read
// from the install Secret (ADR-0014). Its Connect wraps Client, so requests carry the token on
// the wire in X-Burrow-Token (ADR-0015). It is the default open-source transport (ADR-0045),
// shared by the CLI and the MCP server so both reach burrowd over one seam. It lives with the
// kubeconfig logic it wraps and is importable by both binaries and a private module.
type KubeconfigTransport struct {
	Options Options
}

// Connect resolves the token from the install Secret and returns a proxy-routed client.
func (t KubeconfigTransport) Connect(ctx context.Context) (*client.Client, error) {
	return Client(ctx, t.Options)
}

// Client returns a control-plane API client that reaches burrowd through the API-server
// service proxy, authenticated by the kubeconfig, with the API token read from the install
// Secret.
func Client(ctx context.Context, o Options) (*client.Client, error) {
	o.setDefaults()
	cfg, err := RESTConfig(o.Kubeconfig, o.Context)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: building clientset: %w", err)
	}
	token, err := readToken(ctx, cs, o.Namespace, o.TokenSecret, o.TokenKey)
	if err != nil {
		// Translate the raw Kubernetes error into an actionable, context-named message: a
		// missing token Secret means burrowd is not installed, a dial error means the cluster
		// is unreachable. Every command that connects goes through here, so they all benefit.
		return nil, connectError(o, err)
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: building HTTP client: %w", err)
	}
	// The kubeconfig transport authenticates to the API server; wrap it so every request also
	// carries the burrowd API token in X-Burrow-Token, which the proxy forwards untouched
	// (ADR-0015, ADR-0045). The Client itself stays auth-agnostic.
	hc.Transport = client.NewTokenRoundTripper(token, o.ClientVersion, hc.Transport)
	return client.NewClientWithHTTP(proxyBaseURL(cfg.Host, o.Namespace, o.Service, o.Port), hc), nil
}

// proxyBaseURL is the API-server service-proxy prefix for the service. A client request to
// this base + "/v1/apps/..." is proxied by the API server to the service's "/v1/apps/...".
func proxyBaseURL(apiServer, namespace, service string, port int) string {
	return strings.TrimRight(apiServer, "/") +
		fmt.Sprintf("/api/v1/namespaces/%s/services/%s:%d/proxy", namespace, service, port)
}

func readToken(ctx context.Context, cs kubernetes.Interface, namespace, secret, key string) (string, error) {
	s, err := cs.CoreV1().Secrets(namespace).Get(ctx, secret, metav1.GetOptions{})
	if err != nil {
		// Return the raw error so the caller can classify it (NotFound vs. a dial failure)
		// and render an actionable message; readToken does not wrap it itself.
		return "", err
	}
	v, ok := s.Data[key]
	if !ok {
		return "", fmt.Errorf("token secret %s/%s has no key %q", namespace, secret, key)
	}
	return string(v), nil
}

// RESTConfig prefers in-cluster config (when burrow runs inside Kubernetes) and otherwise
// loads the kubeconfig at path, or the ambient KUBECONFIG / ~/.kube/config when path is
// empty. When kubeContext is non-empty it overrides the current context, selecting that
// context's cluster (ADR-0035); empty keeps the kubeconfig's current context (no regression).
// It is exported so the CLI can build a clientset from the same config logic.
func RESTConfig(path, kubeContext string) (*rest.Config, error) {
	// A selected context implies a kubeconfig, so only fall back to in-cluster config when
	// neither a path nor a context is given.
	if path == "" && kubeContext == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	overrides := &clientcmd.ConfigOverrides{}
	if kubeContext != "" {
		overrides.CurrentContext = kubeContext
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("connect: loading kubeconfig: %w", err)
	}
	return cfg, nil
}
