// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import "github.com/burrow-cloud/burrow/controlplane"

// The instance a test means when it names only an environment.
//
// Every adapter entry point is now GIVEN the instance the engine resolved out of the registry rather
// than composing one (ADR-0091 §2), so a test that used to pass an environment alone has to say which
// instance it means. These helpers say "the environment's own", which is what those tests were always
// about — and a test about a SECOND instance passes its name outright instead of calling them.

// testInstance is the Postgres instance of environment env: the derived name of that environment's
// first instance. An env the derivation refuses yields "", which is what a caller that named no
// environment passes and what the adapter refuses in its own right.
func testInstance(env string) string {
	name, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, env)
	if err != nil {
		return ""
	}
	return name
}

// testInstanceOf is testInstance for an add-on type other than Postgres.
func testInstanceOf(spec controlplane.AddonSpec, env string) string {
	name, err := controlplane.AddonInstanceName(spec.Type, env)
	if err != nil {
		return ""
	}
	return name
}
