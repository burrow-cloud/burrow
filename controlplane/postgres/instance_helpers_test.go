// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	cp "github.com/burrow-cloud/burrow/controlplane"
)

// defaultInstance is the instance a store test means when it names only an environment: that
// environment's FIRST instance (ADR-0091 §2), which is the instance every attachment recorded before
// migration 00035 was against.
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
