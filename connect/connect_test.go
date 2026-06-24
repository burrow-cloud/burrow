// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package connect

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	fakekube "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestProxyBaseURL(t *testing.T) {
	got := proxyBaseURL("https://api.example.com:6443", "burrow", "burrowd", 8080)
	want := "https://api.example.com:6443/api/v1/namespaces/burrow/services/burrowd:8080/proxy"
	if got != want {
		t.Errorf("proxyBaseURL = %q, want %q", got, want)
	}
	// A trailing slash on the host is trimmed.
	if got := proxyBaseURL("https://h/", "n", "s", 1); got != "https://h/api/v1/namespaces/n/services/s:1/proxy" {
		t.Errorf("trailing slash not handled: %q", got)
	}
}

func TestReadToken(t *testing.T) {
	ctx := context.Background()
	cs := fakekube.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "burrowd-api-token", Namespace: "burrow"},
		Data:       map[string][]byte{"token": []byte("s3cr3t")},
	})

	tok, err := readToken(ctx, cs, "burrow", "burrowd-api-token", "token")
	if err != nil || tok != "s3cr3t" {
		t.Fatalf("readToken = %q, %v; want s3cr3t", tok, err)
	}
	if _, err := readToken(ctx, cs, "burrow", "missing", "token"); err == nil {
		t.Errorf("missing secret should error")
	}
	if _, err := readToken(ctx, cs, "burrow", "burrowd-api-token", "missing"); err == nil {
		t.Errorf("missing key should error")
	}
}

// TestProxyForwardsCustomHeader is the load-bearing integration check (ADR-0014): it
// confirms, against a real API server, that a request reaches an in-cluster service
// through the API-server service proxy AND that the custom X-Burrow-Token header survives
// the hop. Gated on BURROW_TEST_KUBECONFIG; runs in CI's k3d job.
func TestProxyForwardsCustomHeader(t *testing.T) {
	kubeconfig := os.Getenv("BURROW_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set BURROW_TEST_KUBECONFIG to a disposable cluster to run the proxy integration test")
	}
	ctx := context.Background()

	cfg, err := restConfig(kubeconfig)
	if err != nil {
		t.Fatalf("restConfig: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	ns := "burrow-connect-it"
	_, _ = cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	t.Cleanup(func() { _ = cs.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}) })

	deployEcho(t, ctx, cs, ns)

	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatalf("HTTPClientFor: %v", err)
	}
	base := proxyBaseURL(cfg.Host, ns, "echo", 8080)

	const sentinel = "burrow-token-sentinel-42"
	var headers map[string]string
	waitFor(t, 90*time.Second, func() bool {
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/", nil)
		req.Header.Set("X-Burrow-Token", sentinel)
		resp, err := hc.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		body, _ := io.ReadAll(resp.Body)
		var echo struct {
			Headers map[string]string `json:"headers"`
		}
		if json.Unmarshal(body, &echo) != nil {
			return false
		}
		headers = echo.Headers
		return len(headers) > 0
	})

	if headers["x-burrow-token"] != sentinel {
		t.Fatalf("X-Burrow-Token did not survive the API-server proxy; backend saw headers: %v", headers)
	}
}

func deployEcho(t *testing.T, ctx context.Context, cs kubernetes.Interface, ns string) {
	t.Helper()
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "echo"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "echo"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "echo",
					Image: "mendhak/http-https-echo:31",
					Env:   []corev1.EnvVar{{Name: "HTTP_PORT", Value: "8080"}},
					Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
				}}},
			},
		},
	}
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create echo deployment: %v", err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: ns},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "echo"},
			Ports:    []corev1.ServicePort{{Port: 8080, TargetPort: intstr.FromInt(8080)}},
		},
	}
	if _, err := cs.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create echo service: %v", err)
	}
	waitFor(t, 120*time.Second, func() bool {
		d, err := cs.AppsV1().Deployments(ns).Get(ctx, "echo", metav1.GetOptions{})
		return err == nil && d.Status.ReadyReplicas >= 1
	})
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s", timeout)
		}
		time.Sleep(2 * time.Second)
	}
}
