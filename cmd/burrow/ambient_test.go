// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"testing"

	"github.com/burrow-cloud/burrow/internal/ambienttest"
)

// TestMain runs this package's tests against an empty home directory, so they read only the
// state they set up themselves. The admin CLI resolves a kube context and a local config handle
// from the ambient environment whenever no flag names one, and these tests drive it, so without
// the guard they answer differently on a configured machine than on CI's bare runners — and on a
// maintainer's machine the kubeconfig they would pick up names live clusters.
//
// See [ambienttest] for what the guard covers and what it does not (issue #486).
func TestMain(m *testing.M) { ambienttest.Isolate(m) }
