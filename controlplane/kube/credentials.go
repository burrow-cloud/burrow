// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Credentials = (*Credentials)(nil)

// DefaultCredentialsSecret is the one Secret in the control-plane namespace that holds every
// vendor token, one key per provider (ADR-0023).
const DefaultCredentialsSecret = "burrow-credentials"

// Credentials is the production controlplane.Credentials adapter: it reads and writes vendor
// tokens in the single burrow-credentials Secret in the control-plane namespace (ADR-0023,
// ADR-0030). It reads on every call so a rotated token is picked up without a restart, and it
// writes a token value the engine received over burrowd's authenticated control-plane API.
// burrowd's Role grants `get` and `update` on exactly this one object, so this is its sole
// access to a Secret's contents.
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

// SetToken upserts key=value into burrow-credentials, creating the Secret (Opaque) if absent so
// the command works against an install that has not yet created it (ADR-0030). The value arrives
// over burrowd's authenticated control-plane API and is written straight to the Secret; it never
// reaches a log, Postgres, or an API response — the returned error names the key only, never the
// value. It retries on conflict since a concurrent set can race the resourceVersion.
func (c *Credentials) SetToken(ctx context.Context, key, value string) error {
	secrets := c.client.CoreV1().Secrets(c.namespace)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := secrets.Get(ctx, c.secret, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = secrets.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: c.secret, Namespace: c.namespace},
				Type:       corev1.SecretTypeOpaque,
				Data:       map[string][]byte{key: []byte(value)},
			}, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data[key] = []byte(value)
		_, err = secrets.Update(ctx, existing, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		// The error names the credentials Secret and the key only — never the value.
		return fmt.Errorf("kube: writing credentials secret %s/%s key %q: %w", c.namespace, c.secret, key, err)
	}
	return nil
}
