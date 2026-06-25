// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Credentials = (*Credentials)(nil)

// DefaultCredentialsSecret is the one Secret in the control-plane namespace that holds every
// vendor token, one key per provider (ADR-0023).
const DefaultCredentialsSecret = "burrow-credentials"

// Credentials is the production controlplane.Credentials adapter: it reads vendor tokens from
// the single burrow-credentials Secret in the control-plane namespace (ADR-0023). It reads on
// every call so a rotated token is picked up without a restart, and it only ever reads — the
// developer's CLI writes the Secret (ADR-0017). burrowd's Role grants `get` on exactly this
// one object, so this is its sole access to a Secret's contents.
type Credentials struct {
	client    kubernetes.Interface
	namespace string
	secret    string
}

// NewCredentials returns a Credentials reader over the given clientset, control-plane
// namespace, and Secret name (defaulting to burrow-credentials). Tests inject a fake
// clientset; production injects a real one (see NewCredentialsFromConfig).
func NewCredentials(client kubernetes.Interface, namespace, secret string) *Credentials {
	if secret == "" {
		secret = DefaultCredentialsSecret
	}
	return &Credentials{client: client, namespace: namespace, secret: secret}
}

// NewCredentialsFromConfig builds a Credentials reader from a REST config.
func NewCredentialsFromConfig(cfg *rest.Config, namespace, secret string) (*Credentials, error) {
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: building clientset: %w", err)
	}
	return NewCredentials(client, namespace, secret), nil
}

// Token returns the token stored under key, or ErrNotFound when the Secret or the key is
// absent.
func (c *Credentials) Token(ctx context.Context, key string) (string, error) {
	s, err := c.client.CoreV1().Secrets(c.namespace).Get(ctx, c.secret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("kube: credentials secret %s/%s not found: %w", c.namespace, c.secret, controlplane.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("kube: reading credentials secret %s/%s: %w", c.namespace, c.secret, err)
	}
	v, ok := s.Data[key]
	if !ok {
		return "", fmt.Errorf("kube: credentials secret %s/%s has no key %q: %w", c.namespace, c.secret, key, controlplane.ErrNotFound)
	}
	return string(v), nil
}
