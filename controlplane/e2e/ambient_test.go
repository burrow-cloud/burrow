// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package e2e_test

import (
	"testing"

	"github.com/burrow-cloud/burrow/internal/ambienttest"
)

// TestMain runs this package's tests against an empty home directory, so they read only the
// state they set up themselves. This suite talks to a real cluster, which makes the guard matter
// most here: the only cluster it may reach is the disposable one $BURROW_TEST_KUBECONFIG names,
// never whichever cluster the person running it happens to be pointed at.
//
// See [ambienttest] for what the guard covers and what it does not (issue #486).
func TestMain(m *testing.M) { ambienttest.Isolate(m) }
