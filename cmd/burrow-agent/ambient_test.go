// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"testing"

	"github.com/burrow-cloud/burrow/internal/ambienttest"
)

// TestMain runs this package's tests against an empty home directory, so they read only the
// state they set up themselves. Resolving a pinned environment handle checks its kube context
// against the ambient kubeconfig, which is how four of these tests came to fail on a machine
// that had one and pass on CI, which has none.
//
// See [ambienttest] for what the guard covers and what it does not (issue #486).
func TestMain(m *testing.M) { ambienttest.Isolate(m) }
