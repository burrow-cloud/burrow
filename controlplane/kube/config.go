// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package kube

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewFromConfig builds an Adapter from a REST config and namespace.
func NewFromConfig(cfg *rest.Config, namespace string) (*Adapter, error) {
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: building clientset: %w", err)
	}
	return New(client, namespace), nil
}

// LoadConfig resolves the cluster connection the way a control plane should: the
// in-cluster service account when running inside Kubernetes, otherwise the ambient
// kubeconfig (KUBECONFIG or ~/.kube/config). burrowd uses this.
func LoadConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	loader := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kube: loading kubeconfig: %w", err)
	}
	return cfg, nil
}

// ConfigFromKubeconfig builds a REST config from an explicit kubeconfig file path. It is
// used by the integration tests, which point at a disposable cluster rather than the
// ambient one.
func ConfigFromKubeconfig(path string) (*rest.Config, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("kube: loading kubeconfig %q: %w", path, err)
	}
	return cfg, nil
}
