// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// defaultInstance is the instance a test means when it names only an environment: that
// environment's FIRST instance, whose name in the cluster is derived and whose label is that same
// name (ADR-0091 §2). An attachment recorded against it is what every attachment was before an
// environment could hold more than one.
//
// An environment the derivation refuses yields "", which is what a caller that named none passes.
func defaultInstance(env string) string {
	if env == "" {
		env = cp.DefaultEnvironment
	}
	name, err := cp.AddonInstanceName(cp.AddonPostgres, env)
	if err != nil {
		return ""
	}
	return name
}

// mustInstance is the derived name of environment env's first instance of type t, for a test that
// needs the name a claim or a Job is composed from.
func mustInstance(t *testing.T, typ cp.AddonType, env string) string {
	t.Helper()
	name, err := cp.AddonInstanceName(typ, env)
	if err != nil {
		t.Fatalf("AddonInstanceName(%s, %s): %v", typ, env, err)
	}
	return name
}
