// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package localconfig

import (
	"testing"

	"github.com/burrow-cloud/burrow/internal/ambienttest"
)

// TestMain runs this package's tests against an empty home directory, so they read only the
// state they set up themselves. This package IS the ambient resolution: Path falls back to
// ~/.burrow/config, and the kube-context lookups to $KUBECONFIG else ~/.kube/config.
//
// See [ambienttest] for what the guard covers and what it does not (issue #486).
func TestMain(m *testing.M) { ambienttest.Isolate(m) }
