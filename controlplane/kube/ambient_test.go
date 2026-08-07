// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"testing"

	"github.com/burrow-cloud/burrow/internal/ambienttest"
)

// TestMain runs this package's tests against an empty home directory, so they read only the
// state they set up themselves. kube.Config loads the ambient KUBECONFIG / ~/.kube/config when
// handed no path. The integration tests take a disposable cluster from $BURROW_TEST_KUBECONFIG,
// which the guard deliberately leaves alone: the cluster a test may touch is named explicitly,
// never inherited.
//
// See [ambienttest] for what the guard covers and what it does not (issue #486).
func TestMain(m *testing.M) { ambienttest.Isolate(m) }
