// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// TestLoadBalancerGetsExternalIPE2E is the ground-truth check behind moving public exposure to be
// LoadBalancer-centric: it proves that a self-hosted / bare-metal-style cluster hands a
// type=LoadBalancer Service a real external address for free, with no cloud load balancer. The CI
// k3d job runs k3s, which ships the built-in servicelb (klipper-lb) load-balancer controller, so a
// LoadBalancer Service must get an external IP without any cloud provider. The test creates a
// minimal LoadBalancer Service in a throwaway namespace (a dummy selector and port — servicelb
// assigns an address regardless of whether endpoints exist) and polls until
// .status.loadBalancer.ingress carries a non-empty IP or hostname. It is gated on
// BURROW_TEST_KUBECONFIG like the other e2e tests and adds no container image.
//
// It also asserts that Burrow's own capability detection agrees with the ground truth on this
// cluster. Detection now recognizes k3s's built-in servicelb (controlplane/kube/capabilities.go), so
// on k3s it must report LoadBalancer.Supported=true — closing the gap this test originally surfaced
// (#193), where detection keyed only off a recognized cloud providerID and wrongly reported
// Supported=false while a LoadBalancer in fact got an address. The ground-truth address is still the
// load-bearing proof; the capability assertion locks in that detection matches it (ADR-0043).
func TestLoadBalancerGetsExternalIPE2E(t *testing.T) {
	kubeconfig := os.Getenv("BURROW_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set BURROW_TEST_KUBECONFIG to a disposable cluster to run the end-to-end test")
	}
	ctx := context.Background()

	cfg, err := kube.ConfigFromKubeconfig(kubeconfig)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	nsName := fmt.Sprintf("burrow-lb-e2e-%d", time.Now().UnixNano())
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() { _ = client.CoreV1().Namespaces().Delete(context.Background(), nsName, metav1.DeleteOptions{}) })

	// Diagnostic: k3s ships traefik as its own type=LoadBalancer Service, already backed by
	// servicelb. If traefik already carries an external address, that is direct proof servicelb
	// assigns LoadBalancer IPs in this environment — independent of our probe below. The name and
	// namespace can differ across k3s versions, so this is logged (and asserted only when found),
	// never hard-required.
	if traefik, err := client.CoreV1().Services("kube-system").Get(ctx, "traefik", metav1.GetOptions{}); err == nil {
		addr := loadBalancerAddress(&traefik.Status.LoadBalancer)
		t.Logf("kube-system/traefik LoadBalancer address = %q (servicelb-backed; direct proof servicelb assigns IPs here)", addr)
		if addr == "" {
			t.Logf("kube-system/traefik has no LoadBalancer address yet; the probe below is the ground-truth check")
		}
	} else {
		t.Logf("kube-system/traefik Service not found (%v); k3s version may differ — relying on the probe below", err)
	}

	const svcName = "lb-probe"
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: nsName},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app": "lb-probe"},
			// Port 8080, not 80/443: servicelb implements a LoadBalancer by scheduling a svclb
			// DaemonSet whose pods bind the Service's ports as hostPorts on the nodes. k3s already
			// ships traefik as its own type=LoadBalancer Service on :80 and :443, so a probe on :80
			// would leave the svclb pods unable to bind the host port (Pending) and no external IP
			// would ever be assigned — a host-port collision, not a servicelb failure. :8080 is free.
			Ports: []corev1.ServicePort{{
				Port:       8080,
				TargetPort: intstr.FromInt(8080),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
	if _, err := client.CoreV1().Services(nsName).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create LoadBalancer service: %v", err)
	}

	// Poll for servicelb to assign an external address. On k3s this is normally seconds; allow a
	// generous window for a cold CI cluster.
	addr := waitForLoadBalancerAddress(t, ctx, client, nsName, svcName, 90*time.Second)
	if addr == "" {
		t.Fatalf("service %s/%s got no LoadBalancer address — servicelb should assign one on k3s", nsName, svcName)
	}
	t.Logf("LoadBalancer service %s/%s got external address %q from servicelb (no cloud provider)", nsName, svcName, addr)

	// Assert Burrow's capability detection agrees with the ground truth on this same cluster. A
	// LoadBalancer Service just got a real external address from servicelb, so detection must report
	// LoadBalancer.Supported=true — this is the #193 gap, now closed: detection recognizes k3s's
	// built-in servicelb (via the "k3s" providerID scheme), not only a recognized cloud provider.
	caps, err := kube.DetectCapabilities(ctx, client)
	if err != nil {
		t.Fatalf("DetectCapabilities: %v", err)
	}
	t.Logf("Burrow capability detection on this servicelb cluster: LoadBalancer.Supported=%t (lb provider=%q, cloud=%q); a LoadBalancer received %q",
		caps.LoadBalancer.Supported, caps.LoadBalancer.Provider, caps.Provider.Cloud, addr)
	if !caps.LoadBalancer.Supported {
		t.Errorf("capability detection reports LoadBalancer.Supported=false, but a LoadBalancer Service got external address %q on this servicelb cluster — servicelb detection should report support (ADR-0043, #193)", addr)
	}
}

// waitForLoadBalancerAddress polls a Service until .status.loadBalancer.ingress carries a non-empty
// IP or hostname, or the timeout elapses. It returns the address, or "" if none was assigned in time.
func waitForLoadBalancerAddress(t *testing.T, ctx context.Context, client kubernetes.Interface, ns, name string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		svc, err := client.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if addr := loadBalancerAddress(&svc.Status.LoadBalancer); addr != "" {
				return addr
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(2 * time.Second)
	}
}

// loadBalancerAddress returns the first non-empty ingress IP or hostname from a Service's
// LoadBalancer status, or "" if none is assigned yet.
func loadBalancerAddress(status *corev1.LoadBalancerStatus) string {
	for _, ing := range status.Ingress {
		if ing.IP != "" {
			return ing.IP
		}
		if ing.Hostname != "" {
			return ing.Hostname
		}
	}
	return ""
}
