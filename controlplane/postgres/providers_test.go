// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

func TestStoreProvidersRoundTripAndUpsert(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	name := t.Name() + "-do"

	p := cp.Provider{
		Name:         name,
		Type:         cp.ProviderDigitalOcean,
		Capabilities: []cp.Capability{cp.CapabilityDNS},
		SecretKey:    "do_token",
		CreatedAt:    time.Date(2026, 6, 25, 1, 2, 3, 0, time.UTC),
	}
	if err := s.SaveProvider(ctx, p); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}

	got, err := s.Provider(ctx, name)
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}
	if got.Type != cp.ProviderDigitalOcean || got.SecretKey != "do_token" {
		t.Errorf("round trip: type=%q key=%q", got.Type, got.SecretKey)
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0] != cp.CapabilityDNS {
		t.Errorf("capabilities = %v, want [dns]", got.Capabilities)
	}
	if !got.CreatedAt.Equal(p.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, p.CreatedAt)
	}

	// Upsert by name: rotating the key (a new token under the same provider) overwrites.
	p.SecretKey = "do_rotated"
	if err := s.SaveProvider(ctx, p); err != nil {
		t.Fatalf("SaveProvider upsert: %v", err)
	}
	if got, _ := s.Provider(ctx, name); got.SecretKey != "do_rotated" {
		t.Errorf("upsert secret key = %q, want do_rotated", got.SecretKey)
	}

	// Providers lists it (among any others in the shared database).
	all, err := s.Providers(ctx)
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	found := false
	for _, q := range all {
		if q.Name == name {
			found = true
		}
	}
	if !found {
		t.Errorf("Providers did not include %q", name)
	}

	// An unknown provider is ErrNotFound.
	if _, err := s.Provider(ctx, t.Name()+"-missing"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("missing provider err = %v, want ErrNotFound", err)
	}
}

// TestStoreObjectStorageProviderRoundTrip pins the non-secret half of an ADR-0063 registration in
// the row: the destination, whether Burrow created the bucket, the retention window the lifecycle
// rules were reconciled against, and the NAMES of the two burrow-credentials keys holding the
// credential pair. No credential value is stored — the row is what can be inspected without reading
// a Secret at all.
func TestStoreObjectStorageProviderRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	name := "objstore-rt"

	p := cp.Provider{
		Name:         name,
		Type:         cp.ProviderS3,
		Capabilities: []cp.Capability{cp.CapabilityObjectStorage},
		SecretKey:    name + ".access-key-id",
		CreatedAt:    time.Date(2026, 7, 25, 1, 2, 3, 0, time.UTC),
		ObjectStore: &cp.ObjectStoreConfig{
			Endpoint:           "https://s3.us-west-002.backblazeb2.com",
			Region:             "us-west-002",
			Bucket:             "burrow-backups-9f2c1ab34de56789",
			Created:            true,
			AccessKeyIDKey:     name + ".access-key-id",
			SecretAccessKeyKey: name + ".secret-access-key",
			RetentionDays:      30,
		},
	}
	if err := s.SaveProvider(ctx, p); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}

	got, err := s.Provider(ctx, name)
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}
	if got.ObjectStore == nil {
		t.Fatal("the object-storage configuration did not survive the round trip")
	}
	if *got.ObjectStore != *p.ObjectStore {
		t.Errorf("object store = %+v, want %+v", *got.ObjectStore, *p.ObjectStore)
	}
	if !got.Serves(cp.CapabilityObjectStorage) {
		t.Errorf("capabilities = %v, want object-storage", got.Capabilities)
	}

	// The recorded bucket is the only bucket Burrow ever writes to (ADR-0063 §4), so an upsert
	// carries the new one rather than merging with the old.
	p.ObjectStore.Bucket = "burrow-backups-0000111122223333"
	p.ObjectStore.Created = false
	if err := s.SaveProvider(ctx, p); err != nil {
		t.Fatalf("SaveProvider upsert: %v", err)
	}
	got, err = s.Provider(ctx, name)
	if err != nil {
		t.Fatalf("Provider after upsert: %v", err)
	}
	if got.ObjectStore.Bucket != "burrow-backups-0000111122223333" || got.ObjectStore.Created {
		t.Errorf("upsert left %+v", *got.ObjectStore)
	}

	// A provider with no destination reads back with no object-storage configuration at all, rather
	// than an empty struct that would read as "configured with nothing".
	dns := cp.Provider{
		Name: "objstore-rt-dns", Type: cp.ProviderCloudflare,
		Capabilities: []cp.Capability{cp.CapabilityDNS},
		SecretKey:    "cf", CreatedAt: p.CreatedAt,
	}
	if err := s.SaveProvider(ctx, dns); err != nil {
		t.Fatalf("SaveProvider (dns): %v", err)
	}
	if got, err := s.Provider(ctx, dns.Name); err != nil || got.ObjectStore != nil {
		t.Errorf("dns provider carries an object store %+v (err %v)", got.ObjectStore, err)
	}
}
