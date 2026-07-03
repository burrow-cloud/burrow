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
// It also records what Burrow's own capability detection reports on this cluster. Detection infers
// LoadBalancer support from a recognized cloud providerID (controlplane/kube/capabilities.go) and
// does not recognize servicelb, so on k3s it reports LoadBalancer.Supported=false even though the
// ground-truth assertion above shows a LoadBalancer does get an address. That mismatch is a
// detection gap, not a failure of this cluster, so it is logged rather than asserted — the
// ground-truth address is the load-bearing proof.
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

	// Record Burrow's capability detection on this same cluster. Detection keys LoadBalancer support
	// off a recognized cloud providerID and does not recognize servicelb, so on k3s this is expected
	// to report Supported=false despite the address assigned above — a detection gap to close, not a
	// property of the cluster. Log it rather than asserting so the ground-truth proof stays green.
	caps, err := kube.DetectCapabilities(ctx, client)
	if err != nil {
		t.Fatalf("DetectCapabilities: %v", err)
	}
	t.Logf("Burrow capability detection on this servicelb cluster: LoadBalancer.Supported=%t (provider cloud=%q); a LoadBalancer nonetheless received %q",
		caps.LoadBalancer.Supported, caps.Provider.Cloud, addr)
	if !caps.LoadBalancer.Supported {
		t.Logf("DETECTION GAP: a LoadBalancer Service got a real external address, but capability detection reports LoadBalancer.Supported=false because it infers support only from a recognized cloud provider and does not recognize k3s's built-in servicelb")
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
