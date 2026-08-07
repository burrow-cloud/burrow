// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package cloudcred

import (
	"testing"

	"github.com/burrow-cloud/burrow/internal/ambienttest"
)

// TestMain runs this package's tests against an empty home directory, so they read only the
// state they set up themselves. The credential directories are siblings of whatever
// localconfig.Path resolves to, which is the real ~/.burrow/config when $BURROW_CONFIG is unset
// — so an unisolated test here writes credentials into a developer's own Burrow config.
//
// See [ambienttest] for what the guard covers and what it does not (issue #486).
func TestMain(m *testing.M) { ambienttest.Isolate(m) }
