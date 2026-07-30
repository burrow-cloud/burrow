// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var (
	_ controlplane.ObjectStoreFactory = (*ObjectStoreFactory)(nil)
	_ controlplane.ObjectStore        = (*ObjectStore)(nil)
)

// ObjectStoreFactory is an in-memory controlplane.ObjectStoreFactory (ADR-0063). It hands out one
// shared ObjectStore so a test can seed buckets and lifecycle rules before the engine asks for a
// client, and inspect afterwards what the engine wrote, deleted, and left behind.
type ObjectStoreFactory struct {
	mu sync.Mutex
	// Store is the single object store every call is served from.
	Store *ObjectStore
	// Endpoint, Region and Cred record the arguments of the last ObjectStore call, so a test can
	// assert the engine passed the configured destination and the credential PAIR through.
	Endpoint string
	Region   string
	Cred     controlplane.ObjectStoreCredential
	// Err, when set, is returned instead of a store.
	Err error
}

// NewObjectStoreFactory returns a factory over a fresh empty object store.
func NewObjectStoreFactory() *ObjectStoreFactory {
	return &ObjectStoreFactory{Store: NewObjectStore()}
}

// ObjectStore records the arguments and returns the shared store.
func (f *ObjectStoreFactory) ObjectStore(endpoint, region string, cred controlplane.ObjectStoreCredential) (controlplane.ObjectStore, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Endpoint, f.Region, f.Cred = endpoint, region, cred
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Store, nil
}

// ObjectStore is an in-memory controlplane.ObjectStore: a set of buckets, the objects currently in
// them, the lifecycle rules a test seeded, and a log of every write and delete. Failure is injected
// per operation, so a test can make exactly the probe fail — the case ADR-0063 §2 exists for.
type ObjectStore struct {
	mu      sync.Mutex
	buckets map[string]bool
	objects map[string][]byte // "bucket/key" -> body
	rules   map[string][]controlplane.LifecycleRule
	// Created, Wrote and Deleted are the operation logs, in call order.
	Created []string
	Wrote   []string
	Deleted []string
	// ExistsErr, CreateErr, PutErr, DeleteErr and LifecycleErr are returned by the corresponding
	// operation when set. LifecycleErr wrapping controlplane.ErrLifecycleUnknown models a vendor
	// that does not serve the lifecycle API, or a credential not permitted to read it.
	ExistsErr    error
	CreateErr    error
	PutErr       error
	DeleteErr    error
	LifecycleErr error

	// StreamErrs are returned by successive PutObjectStream calls, one per call, so a test can model
	// the case ADR-0063 §7 is built around: a destination that fails and then works. A nil entry is a
	// success; calls past the end of the slice succeed. StreamRefused pairs with it, marking which of
	// those failures the endpoint REFUSED (not retryable) rather than failed to complete.
	StreamErrs    []error
	StreamRefused []bool
	// StatErr, when set, is returned by StatObject — the store that accepted the write and then
	// cannot serve it back.
	StatErr error
	// StatSize, when non-zero, is the length StatObject reports instead of the object's real one, so
	// a test can model a store that serves the object back at the wrong length.
	StatSize int64
	// Streamed is the log of keys PutObjectStream was called with, in order, INCLUDING the calls
	// that failed — how a test asserts a retry actually happened.
	Streamed []string
}

// NewObjectStore returns an empty store: no buckets, no objects, no rules.
func NewObjectStore() *ObjectStore {
	return &ObjectStore{
		buckets: map[string]bool{},
		objects: map[string][]byte{},
		rules:   map[string][]controlplane.LifecycleRule{},
	}
}

// AddBucket seeds an existing bucket, as when the operator points Burrow at one they already have.
func (s *ObjectStore) AddBucket(bucket string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets[bucket] = true
}

// SetLifecycle seeds a bucket's lifecycle rules.
func (s *ObjectStore) SetLifecycle(bucket string, rules ...controlplane.LifecycleRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[bucket] = rules
}

// Objects returns the keys currently present in bucket, sorted — how a test asserts the probe
// object was cleaned up and nothing of Burrow's was left behind.
func (s *ObjectStore) Objects(bucket string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		if b, key, ok := splitObjectKey(k); ok && b == bucket {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func (s *ObjectStore) BucketExists(ctx context.Context, bucket string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ExistsErr != nil {
		return false, s.ExistsErr
	}
	return s.buckets[bucket], nil
}

func (s *ObjectStore) CreateBucket(ctx context.Context, bucket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.CreateErr != nil {
		return s.CreateErr
	}
	if s.buckets[bucket] {
		return fmt.Errorf("fake: bucket %s already exists", bucket)
	}
	s.buckets[bucket] = true
	s.Created = append(s.Created, bucket)
	return nil
}

func (s *ObjectStore) PutObject(ctx context.Context, bucket, key string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.PutErr != nil {
		return s.PutErr
	}
	if !s.buckets[bucket] {
		return fmt.Errorf("fake: bucket %s does not exist: %w", bucket, controlplane.ErrNotFound)
	}
	s.objects[bucket+"/"+key] = body
	s.Wrote = append(s.Wrote, key)
	return nil
}

// PutObjectStream reads the body and stores it, honouring the per-call StreamErrs script so a test
// can make the first attempt fail and the second succeed. A refused attempt is reported as refused,
// which the caller must NOT retry.
func (s *ObjectStore) PutObjectStream(ctx context.Context, bucket, key string, body io.Reader, size int64, sha256Hex string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.Streamed)
	s.Streamed = append(s.Streamed, key)
	if n < len(s.StreamErrs) && s.StreamErrs[n] != nil {
		refused := n < len(s.StreamRefused) && s.StreamRefused[n]
		return refused, s.StreamErrs[n]
	}
	if sha256Hex == "" {
		return true, fmt.Errorf("fake: a payload sha256 is required: %w", controlplane.ErrInvalid)
	}
	if !s.buckets[bucket] {
		// A bucket that is not there is an answer, not a network failure: refused, not retryable.
		return true, fmt.Errorf("fake: bucket %s does not exist: %w", bucket, controlplane.ErrNotFound)
	}
	buf, err := io.ReadAll(body)
	if err != nil {
		return false, err
	}
	s.objects[bucket+"/"+key] = buf
	s.Wrote = append(s.Wrote, key)
	return false, nil
}

// StatObject reports the stored object's length, or the seeded StatErr/StatSize — the store that
// took the write and then cannot serve it back, or serves it back at the wrong length.
func (s *ObjectStore) StatObject(ctx context.Context, bucket, key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.StatErr != nil {
		return 0, s.StatErr
	}
	if s.StatSize != 0 {
		return s.StatSize, nil
	}
	body, ok := s.objects[bucket+"/"+key]
	if !ok {
		return 0, fmt.Errorf("fake: %s is not in bucket %s: %w", key, bucket, controlplane.ErrNotFound)
	}
	return int64(len(body)), nil
}

func (s *ObjectStore) DeleteObject(ctx context.Context, bucket, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.DeleteErr != nil {
		return s.DeleteErr
	}
	delete(s.objects, bucket+"/"+key)
	s.Deleted = append(s.Deleted, key)
	return nil
}

func (s *ObjectStore) LifecycleRules(ctx context.Context, bucket string) ([]controlplane.LifecycleRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.LifecycleErr != nil {
		return nil, s.LifecycleErr
	}
	rules := s.rules[bucket]
	out := make([]controlplane.LifecycleRule, len(rules))
	copy(out, rules)
	return out, nil
}

func splitObjectKey(k string) (bucket, key string, ok bool) {
	for i := 0; i < len(k); i++ {
		if k[i] == '/' {
			return k[:i], k[i+1:], true
		}
	}
	return "", "", false
}
