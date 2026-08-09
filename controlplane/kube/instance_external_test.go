// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import "github.com/burrow-cloud/burrow/controlplane"

// The instance a test means when it names only an environment — the external-test twin of the
// helpers in instance_test_helpers_test.go, and there for the same reason: every adapter entry point
// is now GIVEN the instance the engine resolved out of the registry rather than composing one
// (ADR-0091 §2).

func testInstance(env string) string {
	name, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, env)
	if err != nil {
		return ""
	}
	return name
}

func testInstanceOf(spec controlplane.AddonSpec, env string) string {
	name, err := controlplane.AddonInstanceName(spec.Type, env)
	if err != nil {
		return ""
	}
	return name
}
