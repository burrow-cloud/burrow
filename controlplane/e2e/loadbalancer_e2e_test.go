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

	const svcName = "lb-probe"
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: nsName},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app": "lb-probe"},
			Ports: []corev1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt(80),
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
			for _, ing := range svc.Status.LoadBalancer.Ingress {
				if ing.IP != "" {
					return ing.IP
				}
				if ing.Hostname != "" {
					return ing.Hostname
				}
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(2 * time.Second)
	}
}
