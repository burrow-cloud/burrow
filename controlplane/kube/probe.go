// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/burrow-cloud/burrow/controlplane"
)

// Running Burrow's own check inside an image that contains nothing (ADR-0076 §4).
//
// The deploy-time dependency check has to run FROM THE APP'S CONTAINER — a check run anywhere else
// proves the cluster can reach the database and not that the app can, and the app's DNS search path,
// network policy, service account and injected Secret are exactly where misconfiguration lives. But
// the app's image is allowed to be, and is encouraged to be, a scratch or distroless image with no
// shell, no psql and no curl. ADR-0076's consequences name this as real work and it is.
//
// THE MECHANISM: an init container running BURROW'S OWN IMAGE copies its own executable into an
// emptyDir; the check container runs THE APP'S IMAGE with that emptyDir mounted and executes the
// copy. Nothing is required of the app's image at all — not a shell to run a copy with, not a
// dynamic loader, not a package. Both halves are one static binary, so there is no second image to
// publish, no second artifact to keep in step with a release, and no second implementation of the
// connection logic to drift from the control plane's own. It is the shape ADR-0063 §7's backup
// shipper already established, pointed at a container Burrow does not own.
//
// The init container copies ITSELF rather than being handed a `cp`, because a distroless base has no
// `cp` either — the same problem one layer up. See `burrowd install-probe`.

const (
	// probeInstallContainer is the init container that puts the binary in place. It is named for what
	// it does, so a `kubectl describe pod` on a check that failed to start says which half failed.
	probeInstallContainer = "burrow-probe-install"
)

// probeVolume is the emptyDir the binary is copied through. It is memory-backed neither way: the
// binary is a few tens of megabytes and a tmpfs would charge that to the pod's memory limit, where a
// disk-backed emptyDir on the node costs nothing the pod is accounted for.
func probeVolume() corev1.Volume {
	return corev1.Volume{
		Name:         controlplane.ProbeVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}

// probeMount is how both containers see it. It is READ-ONLY in the check container — the app's
// container has no business writing to the directory it executes Burrow's binary out of — and
// writable in the init container, which is the only thing that puts something there.
func probeMount(readOnly bool) corev1.VolumeMount {
	return corev1.VolumeMount{Name: controlplane.ProbeVolumeName, MountPath: controlplane.ProbeMountPath, ReadOnly: readOnly}
}

// probeInitContainer is Burrow's image copying its own executable into the shared directory.
//
// It carries NONE of the app's environment and none of the app's Secret. That is deliberate: this
// container runs Burrow's binary with Burrow's image, and the credential belongs to the container
// that is meant to use it. Only the check container — the app's own image — is given the app's
// environment.
func (a *Adapter) probeInitContainer() corev1.Container {
	return corev1.Container{
		Name:         probeInstallContainer,
		Image:        a.shipImage(),
		Command:      []string{"/burrowd", controlplane.ProbeInstallCommand, controlplane.ProbeMountPath},
		VolumeMounts: []corev1.VolumeMount{probeMount(false)},
	}
}

// withProbe attaches the probe machinery to a one-off Job's pod: the emptyDir, the init container
// that fills it, the read-only mount on the check container, and the probe's own environment.
//
// The probe's env is appended AFTER the app's config, which is what makes it win: Kubernetes applies
// a container's env in order, so an app that happens to define BURROW_CHECK_PLAN cannot shadow the
// plan Burrow is asking for. The app's Secret arrives through envFrom, which `env` already takes
// precedence over.
func withProbe(pod *corev1.PodSpec, spec *controlplane.ProbeSpec, initContainer corev1.Container) {
	if spec == nil {
		return
	}
	pod.Volumes = append(pod.Volumes, probeVolume())
	pod.InitContainers = append(pod.InitContainers, initContainer)
	for i := range pod.Containers {
		pod.Containers[i].VolumeMounts = append(pod.Containers[i].VolumeMounts, probeMount(true))
		for _, k := range sortedKeys(spec.Env) { // deterministic order
			pod.Containers[i].Env = append(pod.Containers[i].Env, corev1.EnvVar{Name: k, Value: spec.Env[k]})
		}
	}
}
