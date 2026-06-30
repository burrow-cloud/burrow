// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCliVersionDevDefault(t *testing.T) {
	// A test binary has no module release version, so the CLI reports the dev default.
	if got := cliVersion(); got != "dev" {
		t.Errorf("cliVersion() = %q, want dev for an unversioned build", got)
	}
}

func TestBurrowdImage(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "burrowd", Namespace: "burrow"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{
					{Name: "burrowd", Image: "ghcr.io/burrow-cloud/burrowd:v0.2.1"},
				}},
			},
		},
	})
	img, err := burrowdImage(ctx, cs, "burrow")
	if err != nil {
		t.Fatalf("burrowdImage: %v", err)
	}
	if img != "ghcr.io/burrow-cloud/burrowd:v0.2.1" {
		t.Errorf("image = %q", img)
	}

	// No control plane installed → IsNotFound, which the command renders as "not installed".
	if _, err := burrowdImage(ctx, fake.NewSimpleClientset(), "burrow"); !apierrors.IsNotFound(err) {
		t.Errorf("absent burrowd err = %v, want IsNotFound", err)
	}
}

func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/burrow-cloud/burrowd:v0.2.1": "v0.2.1",
		"burrowd:e2e":                         "e2e",
		"registry:5000/burrowd:v1":            "v1",                // a registry-host port colon is not the tag
		"ghcr.io/x/burrowd@sha256:abcd":       "ghcr.io/x/burrowd", // digest, no tag
		"burrowd":                             "burrowd",           // untagged
	}
	for in, want := range cases {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVersionCommandPrintsCLILine(t *testing.T) {
	var out, errb bytes.Buffer
	// No reachable cluster in the test env, so the control-plane line is best-effort; the CLI
	// line must always print and the command must succeed.
	if err := run(context.Background(), []string{"version", "--kubeconfig", "/nonexistent"}, &out, &errb); err != nil {
		t.Fatalf("version: %v", err)
	}
	if s := out.String(); !strings.Contains(s, "burrow (CLI):  dev") {
		t.Errorf("version output = %q, want the CLI version line", s)
	}
}

// TestControlPlaneLine covers the rendered control-plane line for each probe outcome: it must name
// the targeted context on success, when nothing is installed, and when the cluster is unreachable.
func TestControlPlaneLine(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "deployments"}, "burrowd")
	cases := []struct {
		name      string
		img       string
		err       error
		wantHas   []string
		wantNotIn []string
	}{
		{
			name:    "success names the context and namespace",
			img:     "ghcr.io/burrow-cloud/burrowd:v0.6.0",
			wantHas: []string{`control plane: v0.6.0 (context "nonprod", namespace "burrow")`},
		},
		{
			name:    "not installed names the context and namespace",
			err:     notFound,
			wantHas: []string{`control plane: not installed (context "nonprod", namespace "burrow")`},
		},
		{
			name:      "unreachable names the context and omits the URL",
			err:       &net.DNSError{Err: "no such host", Name: "abc123.k8s.example.com", IsNotFound: true},
			wantHas:   []string{`control plane: unreachable via context "nonprod" (no such host)`},
			wantNotIn: []string{"http", "abc123"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := controlPlaneLine(tc.img, tc.err, "nonprod", "burrow")
			for _, want := range tc.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("line = %q, want substring %q", got, want)
				}
			}
			for _, no := range tc.wantNotIn {
				if strings.Contains(got, no) {
					t.Errorf("line = %q, should not contain %q", got, no)
				}
			}
		})
	}
}

// fakeBurrowdDeployment is a fake API server for one cluster that serves the burrowd Deployment
// with the given image, recording that it was hit, so a version probe through it reports that
// image. It stands in for a cluster's API server the way fakeBurrowdCluster does for app status.
func fakeBurrowdDeployment(hit *bool, image string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*hit = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&appsv1.Deployment{
			TypeMeta:   metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
			ObjectMeta: metav1.ObjectMeta{Name: "burrowd", Namespace: "burrow"},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "burrowd", Image: image}},
			}}},
		})
	}))
}

// TestVersionContextFlagSelectsCluster confirms --context targets the named context's cluster: the
// probe reports prod's burrowd image and names prod, not the current context (staging).
func TestVersionContextFlagSelectsCluster(t *testing.T) {
	var stagingHit, prodHit bool
	staging := fakeBurrowdDeployment(&stagingHit, "ghcr.io/burrow-cloud/burrowd:v0.5.0")
	prod := fakeBurrowdDeployment(&prodHit, "ghcr.io/burrow-cloud/burrowd:v0.6.0")
	defer staging.Close()
	defer prod.Close()

	path := writeKubeconfig(t, twoContextConfig(staging.URL, prod.URL))

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"version", "--context", "prod", "--kubeconfig", path}, &out, &errb); err != nil {
		t.Fatalf("version: %v\n%s", err, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, `control plane: v0.6.0 (context "prod", namespace "burrow")`) {
		t.Errorf("version output = %q, want prod's image and context", s)
	}
	if !prodHit {
		t.Errorf("--context prod did not reach the prod cluster")
	}
	if stagingHit {
		t.Errorf("staging (current context) was contacted; --context should redirect to prod")
	}
}

// TestVersionUnreachableCluster confirms a dead cluster yields the concise "unreachable via context"
// line that names the current context and carries no full URL, with the command still succeeding.
func TestVersionUnreachableCluster(t *testing.T) {
	// A current context "do-nyc1-prod" pointing at a non-resolvable host: the probe fails fast on
	// DNS without needing a real cluster.
	cfg := twoContextConfig("https://burrow-version-unreachable.invalid:6443", "https://unused.invalid:6443")
	cfg.Contexts["do-nyc1-prod"] = cfg.Contexts["staging"]
	delete(cfg.Contexts, "staging")
	cfg.CurrentContext = "do-nyc1-prod"
	path := writeKubeconfig(t, cfg)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"version", "--kubeconfig", path}, &out, &errb); err != nil {
		t.Fatalf("version: %v\n%s", err, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, `unreachable via context "do-nyc1-prod"`) {
		t.Errorf("version output = %q, want the unreachable line naming the context", s)
	}
	if strings.Contains(s, "https://") || strings.Contains(s, `Get "`) {
		t.Errorf("version output = %q, leaked the dialed URL", s)
	}
}
