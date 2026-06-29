// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package registry_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/registry"
)

// startRegistry serves an in-memory OCI registry over HTTP (no network, no Docker) and
// returns its host:port.
func startRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(ggcrregistry.New())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestResolveReturnsDigest(t *testing.T) {
	host := startRegistry(t)
	refStr := host + "/team/app:1"

	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	ref, err := name.ParseReference(refStr, name.Insecure)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push: %v", err)
	}
	want, _ := img.Digest()

	info, err := registry.New(registry.WithInsecure()).Resolve(context.Background(), refStr)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.Digest != want.String() {
		t.Errorf("digest = %q, want %q", info.Digest, want.String())
	}
	if info.Reference != refStr {
		t.Errorf("reference = %q, want %q", info.Reference, refStr)
	}
}

func TestResolveNotFound(t *testing.T) {
	host := startRegistry(t)
	_, err := registry.New(registry.WithInsecure()).Resolve(context.Background(), host+"/team/app:missing")
	if !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveInvalidReference(t *testing.T) {
	_, err := registry.New().Resolve(context.Background(), "not a valid reference!!")
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}
