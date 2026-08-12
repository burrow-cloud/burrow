// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The bug these tests guard against cannot be observed against a fake client, and it is worth being
// exact about why. The failure was a RACE between two real components — a kubelet marking a pod
// ready and a process inside it binding its socket — and a fake Clientset runs neither. Nothing here
// starts ValKey, nothing evaluates a probe, and the Deployment's status is whatever the fake was
// told to hold.
//
// So these assert the one thing that is decidable without a cluster and is also the whole of the
// fix: the probe is ON the pod spec Burrow authors, on the port the Service publishes, in the form
// the backing service can answer. Kubernetes' behaviour given that probe — do not add the pod to
// the Service's endpoints until it passes — is not Burrow's to test.
//
// What proves the endpoint actually answers is the k3d capstone, which installs the cache add-on
// and connects to the endpoint the install printed.

// addonContainer returns the single container of the add-on instance's authored Deployment.
func addonContainer(t *testing.T, client *fake.Clientset, ns, name string) corev1.Container {
	t.Helper()
	dep, err := client.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get addon deployment %q: %v", name, err)
	}
	if n := len(dep.Spec.Template.Spec.Containers); n != 1 {
		t.Fatalf("containers = %d, want 1", n)
	}
	return dep.Spec.Template.Spec.Containers[0]
}

// deployCatalogAddon installs the catalog entry for t's type against a fake cluster and returns the
// client and the instance name, so a test asserts over the entry a real install would use rather
// than over a hand-written spec that could differ from it.
func deployCatalogAddon(t *testing.T, typ controlplane.AddonType, addonNS string) (*fake.Clientset, string) {
	t.Helper()
	spec, ok := controlplane.LookupAddon(typ)
	if !ok {
		t.Fatalf("LookupAddon(%q) = false, want the catalog entry", typ)
	}
	// The metrics add-on's vmagent scraper needs its pre-provisioned ServiceAccount to exist, or
	// DeployAddon refuses before it authors anything.
	client := fake.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: vmagentServiceAccount, Namespace: addonNS},
	})
	a := New(client, "apps").WithAddonNamespace(addonNS)
	name := testInstanceOf(spec, controlplane.DefaultEnvironment)
	if _, err := a.DeployAddon(context.Background(), spec, controlplane.DefaultEnvironment, name, nil); err != nil {
		t.Fatalf("DeployAddon(%s): %v", typ, err)
	}
	return client, name
}

// TestAddonReadinessProbeIsAuthored covers every Deployment-backed catalog entry: each authors a
// readiness probe, in the form its backing service answers, on the port its Service publishes.
//
// The port assertion is the load-bearing one. The endpoint the install hands the agent is
// `<name>.<namespace>.svc:<Port>`, so a probe on any other port would prove readiness of something
// other than the thing the endpoint names.
func TestAddonReadinessProbeIsAuthored(t *testing.T) {
	const addonNS = "burrow-addons"
	for _, tc := range []struct {
		typ  controlplane.AddonType
		port int32
		path string // empty means the check must be a TCP connect
	}{
		{controlplane.AddonLogs, 9428, "/health"},
		{controlplane.AddonMetrics, 8428, "/health"},
		// ValKey speaks RESP, so a socket is the strongest check Burrow can author without
		// running a command inside the pod. This is the add-on the reported failure was on.
		{controlplane.AddonCache, 6379, ""},
	} {
		t.Run(string(tc.typ), func(t *testing.T) {
			client, name := deployCatalogAddon(t, tc.typ, addonNS)
			p := addonContainer(t, client, addonNS, name).ReadinessProbe
			if p == nil {
				t.Fatal("ReadinessProbe is nil: the instance would join its Service before it answers")
			}
			switch {
			case tc.path != "":
				if p.HTTPGet == nil {
					t.Fatalf("ReadinessProbe = %+v, want an HTTP GET", p)
				}
				if p.HTTPGet.Path != tc.path {
					t.Errorf("probe path = %q, want %q", p.HTTPGet.Path, tc.path)
				}
				if got := p.HTTPGet.Port.IntVal; got != tc.port {
					t.Errorf("probe port = %d, want %d (the port the Service publishes)", got, tc.port)
				}
				// Host is what would make a probe leave the pod (ADR-0076 §2).
				if p.HTTPGet.Host != "" {
					t.Errorf("probe host = %q, want empty so the probe addresses the pod's own IP", p.HTTPGet.Host)
				}
			default:
				if p.TCPSocket == nil {
					t.Fatalf("ReadinessProbe = %+v, want a TCP connect", p)
				}
				if got := p.TCPSocket.Port.IntVal; got != tc.port {
					t.Errorf("probe port = %d, want %d (the port the Service publishes)", got, tc.port)
				}
			}
		})
	}
}

// TestAddonProbePortMatchesItsService reads the two objects an install creates and checks they agree
// — the probe checks the port the Service routes to. It is the same fact the case above asserts
// against a literal, read from the cluster instead, so a catalog entry whose port moved cannot leave
// a probe behind on the old one.
func TestAddonProbePortMatchesItsService(t *testing.T) {
	const addonNS = "burrow-addons"
	ctx := context.Background()
	for _, typ := range []controlplane.AddonType{controlplane.AddonLogs, controlplane.AddonMetrics, controlplane.AddonCache} {
		t.Run(string(typ), func(t *testing.T) {
			client, name := deployCatalogAddon(t, typ, addonNS)
			svc, err := client.CoreV1().Services(addonNS).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get addon service %q: %v", name, err)
			}
			if n := len(svc.Spec.Ports); n != 1 {
				t.Fatalf("service ports = %d, want 1", n)
			}
			p := addonContainer(t, client, addonNS, name).ReadinessProbe
			if p == nil {
				t.Fatal("ReadinessProbe is nil")
			}
			var probePort int32
			switch {
			case p.HTTPGet != nil:
				probePort = p.HTTPGet.Port.IntVal
			case p.TCPSocket != nil:
				probePort = p.TCPSocket.Port.IntVal
			}
			if probePort != svc.Spec.Ports[0].TargetPort.IntVal {
				t.Errorf("probe port = %d, service target port = %d: the probe must answer for the port the endpoint names",
					probePort, svc.Spec.Ports[0].TargetPort.IntVal)
			}
		})
	}
}

// TestAddonProbeTimings pins the rendered timings, which are shared with the app path on purpose:
// an add-on and an app are answering the same question, so they wait the same way. The thresholds
// are the conservatism (ADR-0076 §6) — a serving store must fail for about a minute before it is
// pulled out of its Service, and the per-attempt timeout is generous so a loaded one is not removed
// for being briefly slow.
func TestAddonProbeTimings(t *testing.T) {
	client, name := deployCatalogAddon(t, controlplane.AddonCache, "burrow-addons")
	p := addonContainer(t, client, "burrow-addons", name).ReadinessProbe
	if p == nil {
		t.Fatal("ReadinessProbe is nil")
	}
	for _, tc := range []struct {
		field string
		got   int32
		want  int32
	}{
		{"initialDelaySeconds", p.InitialDelaySeconds, controlplane.ReadinessInitialDelaySeconds},
		{"periodSeconds", p.PeriodSeconds, controlplane.ReadinessPeriodSeconds},
		{"timeoutSeconds", p.TimeoutSeconds, controlplane.ReadinessTimeoutSeconds},
		{"failureThreshold", p.FailureThreshold, controlplane.ReadinessFailureThreshold},
		{"successThreshold", p.SuccessThreshold, controlplane.ReadinessSuccessThreshold},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.field, tc.got, tc.want)
		}
	}
}

// TestAddonAuthorsNoLivenessProbe is ADR-0076 §1 on the add-on path. It is asserted separately from
// the app path's version because the two build their pod specs in different functions, and the rule
// is about what Burrow authors anywhere rather than about one builder.
func TestAddonAuthorsNoLivenessProbe(t *testing.T) {
	const addonNS = "burrow-addons"
	for _, typ := range []controlplane.AddonType{controlplane.AddonLogs, controlplane.AddonMetrics, controlplane.AddonCache} {
		t.Run(string(typ), func(t *testing.T) {
			client, name := deployCatalogAddon(t, typ, addonNS)
			c := addonContainer(t, client, addonNS, name)
			if c.LivenessProbe != nil {
				t.Errorf("LivenessProbe = %+v, want nil — a wrong one restarts a working store in a loop (ADR-0076 §1)", c.LivenessProbe)
			}
			if c.StartupProbe != nil {
				t.Errorf("StartupProbe = %+v, want nil", c.StartupProbe)
			}
		})
	}
}
