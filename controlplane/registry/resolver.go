// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package registry is the production controlplane.Registry adapter. It resolves an image
// reference to its content digest and confirms the image is pullable, using
// go-containerregistry. The control plane only ever reads metadata here — the image
// bytes ride the registry to the cluster's nodes, never through Burrow (ADR-0004). The
// digest it returns is what makes a deploy (and its rollback) deterministic (ADR-0007).
//
// It lives under controlplane/ (not controlplane/internal) so cmd/burrowd and the
// managed module can wire it; it is source-available under FSL-1.1-ALv2.
package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Registry = (*Resolver)(nil)

// Resolver resolves image references against container registries.
type Resolver struct {
	keychain authn.Keychain
	nameOpts []name.Option
}

// Option configures a Resolver.
type Option func(*Resolver)

// WithInsecure allows resolving against registries served over plain HTTP — for a
// cluster-internal registry or local development. Production registries are HTTPS by
// default.
func WithInsecure() Option {
	return func(r *Resolver) { r.nameOpts = append(r.nameOpts, name.Insecure) }
}

// New returns a Resolver using the ambient registry credentials (the Docker config
// keychain), so it authenticates to private registries the host is already logged in to.
func New(opts ...Option) *Resolver {
	r := &Resolver{keychain: authn.DefaultKeychain}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Resolve returns the registry's view of reference: its content digest. A malformed
// reference is ErrInvalid; an image the registry does not have is ErrNotFound.
func (r *Resolver) Resolve(ctx context.Context, reference string) (controlplane.ImageInfo, error) {
	ref, err := name.ParseReference(reference, r.nameOpts...)
	if err != nil {
		return controlplane.ImageInfo{}, fmt.Errorf("registry: invalid reference %q: %w: %w", reference, err, controlplane.ErrInvalid)
	}
	desc, err := remote.Head(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(r.keychain))
	if err != nil {
		var terr *transport.Error
		if errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound {
			return controlplane.ImageInfo{}, fmt.Errorf("registry: image %q not found: %w", reference, controlplane.ErrNotFound)
		}
		return controlplane.ImageInfo{}, fmt.Errorf("registry: resolving %q: %w", reference, err)
	}
	return controlplane.ImageInfo{Reference: reference, Digest: desc.Digest.String()}, nil
}
