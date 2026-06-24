// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package connect reaches the in-cluster Burrow control plane from a developer's machine
// using their ambient kubeconfig and the Kubernetes API server's service proxy — no
// port-forward, no ingress (ADR-0014). It reads the API token from the install Secret, so
// a developer with kubectl access configures nothing else.
//
// It is Apache-2.0 (the client surface): it imports client-go to talk to the API server
// but not the FSL controlplane packages — it reaches Burrow over HTTP, like any client.
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
)

// Options configures how to find the control plane. The zero value uses the defaults and
// the ambient kubeconfig.
type Options struct {
	Kubeconfig  string // explicit kubeconfig path; empty = in-cluster, else ambient
	Namespace   string
	Service     string
	Port        int
	TokenSecret string
	TokenKey    string
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

// Client returns a control-plane API client that reaches burrowd through the API-server
// service proxy, authenticated by the kubeconfig, with the API token read from the install
// Secret.
func Client(ctx context.Context, o Options) (*client.Client, error) {
	o.setDefaults()
	cfg, err := RESTConfig(o.Kubeconfig)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: building clientset: %w", err)
	}
	token, err := readToken(ctx, cs, o.Namespace, o.TokenSecret, o.TokenKey)
	if err != nil {
		return nil, err
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: building HTTP client: %w", err)
	}
	return client.NewClientWithHTTP(proxyBaseURL(cfg.Host, o.Namespace, o.Service, o.Port), token, hc), nil
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
		return "", fmt.Errorf("connect: reading token secret %s/%s: %w", namespace, secret, err)
	}
	v, ok := s.Data[key]
	if !ok {
		return "", fmt.Errorf("connect: token secret %s/%s has no key %q", namespace, secret, key)
	}
	return string(v), nil
}

// RESTConfig prefers in-cluster config (when burrow runs inside Kubernetes) and otherwise
// loads the kubeconfig at path, or the ambient KUBECONFIG / ~/.kube/config when path is
// empty. It is exported so the CLI can build a clientset from the same config logic.
func RESTConfig(path string) (*rest.Config, error) {
	if path == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("connect: loading kubeconfig: %w", err)
	}
	return cfg, nil
}
