// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

func TestRegistryHost(t *testing.T) {
	cases := []struct {
		image string
		want  string
	}{
		{"ghcr.io/burrow-cloud/website:0.1.1", "ghcr.io"},
		{"ghcr.io/org/app@sha256:abc", "ghcr.io"},
		{"library/nginx", "docker.io"},
		{"library/nginx:1.27", "docker.io"},
		{"nginx", "docker.io"},
		{"nginx:1.27", "docker.io"},
		{"registry.example.com:5000/team/app:1.2.3", "registry.example.com:5000"},
		{"localhost:5000/app:dev", "localhost:5000"},
		{"localhost/app", "localhost"},
	}
	for _, c := range cases {
		if got := cp.RegistryHost(c.image); got != c.want {
			t.Errorf("RegistryHost(%q) = %q, want %q", c.image, got, c.want)
		}
	}
}

func TestIsImagePullReason(t *testing.T) {
	for _, r := range []string{cp.ReasonImagePullBackOff, cp.ReasonErrImagePull} {
		if !cp.IsImagePullReason(r) {
			t.Errorf("IsImagePullReason(%q) = false, want true", r)
		}
	}
	for _, r := range []string{"", "ContainerCreating", "PodInitializing", "CrashLoopBackOff"} {
		if cp.IsImagePullReason(r) {
			t.Errorf("IsImagePullReason(%q) = true, want false", r)
		}
	}
}

func TestImagePullIssue(t *testing.T) {
	msg := cp.ImagePullIssue("ghcr.io/burrow-cloud/website:0.1.1", cp.ReasonImagePullBackOff)
	for _, want := range []string{
		`ghcr.io/burrow-cloud/website:0.1.1`,
		`registry "ghcr.io"`,
		"burrow config registry login ghcr.io",
		cp.ReasonImagePullBackOff,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("ImagePullIssue = %q, want it to contain %q", msg, want)
		}
	}
}
