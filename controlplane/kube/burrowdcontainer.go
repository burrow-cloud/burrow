// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	corev1 "k8s.io/api/core/v1"
)

// Running Burrow's own image inside a pod Burrow authors (issue #478).
//
// Two pods run burrowd itself rather than the user's image: the backup shipper (ADR-0063 §7) and the
// dependency check's probe installer (ADR-0076 §4). Both did it by setting the container's `command`
// to an absolute path, `/burrowd`, and that path has never existed in a published image. burrowd is
// built with ko (.ko.yaml, and the release workflow's `ko build ./cmd/burrowd`); ko lays the binary
// down at /ko-app/<name> and sets the image's entrypoint to it. So both pods died before their first
// instruction with `exec: "/burrowd": stat /burrowd: no such file or directory`, on every release.
//
// THE RULE THIS FILE EXISTS TO HOLD: a container that runs Burrow's own image sets ARGS ONLY and
// never `command`. Kubernetes' `command` REPLACES the image's entrypoint, which means writing down
// where the builder put the binary; `args` is appended to the entrypoint the image already carries,
// which means writing down only which subcommand to run. The subcommand is Burrow's own contract and
// cannot drift. The path is the build tool's, and did.
//
// Swapping ko for a Dockerfile, renaming the binary, or moving it therefore changes nothing here —
// which is the point, because the alternative fix (hard-code /ko-app/burrowd) would be correct today
// and wrong again the first time anything about the build changed, with the same silent failure.
//
// burrowdContainer is the one place that shape is expressed, so a third caller inherits it rather
// than reinventing it. TestBurrowdContainersRunTheImageEntrypoint pins it.
func (a *Adapter) burrowdContainer(name string, args ...string) corev1.Container {
	return corev1.Container{
		Name:  name,
		Image: a.shipImage(),
		Args:  args,
	}
}
