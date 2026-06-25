// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

const ns = "default"

func i32p(v int32) *int32 { return &v }

func TestExposeCreatesServiceAndIngress(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	if err := a.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("Expose: %v", err)
	}

	svc, err := client.CoreV1().Services(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get service: %v", err)
	}
	if svc.Spec.Selector["app.kubernetes.io/name"] != "web" {
		t.Errorf("service selector = %v", svc.Spec.Selector)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntValue() != 8080 {
		t.Errorf("service ports = %+v, want 80->8080", svc.Spec.Ports)
	}

	ing, err := client.NetworkingV1().Ingresses(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ingress: %v", err)
	}
	rule := ing.Spec.Rules[0]
	if rule.Host != "web.example.com" {
		t.Errorf("ingress host = %q, want web.example.com", rule.Host)
	}
	if b := rule.HTTP.Paths[0].Backend.Service; b.Name != "web" || b.Port.Number != 80 {
		t.Errorf("ingress backend = %+v, want web:80", b)
	}

	// Expose is idempotent (update path).
	if err := a.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web2.example.com", Port: 8080}); err != nil {
		t.Fatalf("re-Expose: %v", err)
	}
	ing, _ = client.NetworkingV1().Ingresses(ns).Get(ctx, "web", metav1.GetOptions{})
	if ing.Spec.Rules[0].Host != "web2.example.com" {
		t.Errorf("host after update = %q, want web2.example.com", ing.Spec.Rules[0].Host)
	}
}

func TestUnexpose(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	// Unexposing nothing is ErrNotFound.
	if err := a.Unexpose(ctx, "web"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("Unexpose missing = %v, want ErrNotFound", err)
	}

	if err := a.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	if err := a.Unexpose(ctx, "web"); err != nil {
		t.Fatalf("Unexpose: %v", err)
	}
	if _, err := client.CoreV1().Services(ns).Get(ctx, "web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("service should be deleted, got %v", err)
	}
	if _, err := client.NetworkingV1().Ingresses(ns).Get(ctx, "web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("ingress should be deleted, got %v", err)
	}
}

func TestExposeTLS(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	if err := a.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web.example.com", Port: 8080, TLS: true, Issuer: "letsencrypt"}); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	ing, err := client.NetworkingV1().Ingresses(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ingress: %v", err)
	}
	if ing.Annotations["cert-manager.io/cluster-issuer"] != "letsencrypt" {
		t.Errorf("issuer annotation = %q, want letsencrypt", ing.Annotations["cert-manager.io/cluster-issuer"])
	}
	if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].SecretName != "web-tls" ||
		len(ing.Spec.TLS[0].Hosts) != 1 || ing.Spec.TLS[0].Hosts[0] != "web.example.com" {
		t.Errorf("ingress TLS = %+v, want host web.example.com secret web-tls", ing.Spec.TLS)
	}
}

func TestExposureStatus(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	// Not exposed → zero status, no error.
	if st, err := a.ExposureStatus(ctx, "web"); err != nil || st.Exposed {
		t.Fatalf("unexposed status = %+v err=%v", st, err)
	}

	if err := a.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("expose: %v", err)
	}
	// Before the controller assigns an address, the host is known but the address is empty.
	st, err := a.ExposureStatus(ctx, "web")
	if err != nil || !st.Exposed || st.Host != "web.example.com" || st.Address != "" {
		t.Fatalf("pre-address status = %+v err=%v", st, err)
	}

	// Simulate the ingress controller writing the external address into the Ingress status.
	ing, _ := client.NetworkingV1().Ingresses(ns).Get(ctx, "web", metav1.GetOptions{})
	ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "1.2.3.4"}}
	if _, err := client.NetworkingV1().Ingresses(ns).UpdateStatus(ctx, ing, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update status: %v", err)
	}
	st, err = a.ExposureStatus(ctx, "web")
	if err != nil || st.Address != "1.2.3.4" {
		t.Errorf("status with address = %+v err=%v", st, err)
	}
}

func TestApplyCreatesDeployment(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	spec := cp.WorkloadSpec{
		App: "web", Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 2,
		Env:     map[string]string{"B": "2", "A": "1"},
		Command: []string{"server", "--port", "8080"},
	}
	if err := a.ApplyWorkload(ctx, spec); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}

	dep, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want 2", *dep.Spec.Replicas)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != "img:1" {
		t.Errorf("image = %q, want img:1", c.Image)
	}
	if len(c.Command) != 3 || c.Command[0] != "server" {
		t.Errorf("command = %v", c.Command)
	}
	// Env is sorted for determinism.
	if len(c.Env) != 2 || c.Env[0].Name != "A" || c.Env[1].Name != "B" {
		t.Errorf("env = %v, want [A B] sorted", c.Env)
	}
	if dep.Spec.Selector.MatchLabels["app.kubernetes.io/name"] != "web" {
		t.Errorf("selector = %v", dep.Spec.Selector.MatchLabels)
	}
}

func TestApplyUpdatesDeployment(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	_ = a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1})
	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:2", Replicas: 3}); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	list, _ := client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if len(list.Items) != 1 {
		t.Fatalf("got %d deployments, want 1 (update, not duplicate)", len(list.Items))
	}
	dep := list.Items[0]
	if dep.Spec.Template.Spec.Containers[0].Image != "img:2" || *dep.Spec.Replicas != 3 {
		t.Errorf("after update: image=%q replicas=%d, want img:2/3", dep.Spec.Template.Spec.Containers[0].Image, *dep.Spec.Replicas)
	}
}

// TestApplyRetriesOnConflict reproduces the resourceVersion race the e2e exposed: the
// first Update returns a 409 Conflict (as it does when the controller has modified the
// live object), and ApplyWorkload must re-read and retry rather than fail.
func TestApplyRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)
	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("initial apply: %v", err)
	}

	var updates int
	client.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web", errors.New("the object has been modified"))
		}
		return false, nil, nil // fall through to the default tracker
	})

	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:2", Replicas: 2}); err != nil {
		t.Fatalf("apply should retry past the conflict: %v", err)
	}
	if updates < 2 {
		t.Errorf("expected a retry (>= 2 update attempts), got %d", updates)
	}
	dep, _ := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if dep.Spec.Template.Spec.Containers[0].Image != "img:2" {
		t.Errorf("image = %q, want img:2 after retried update", dep.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestApplyRejectsUnsupportedKind(t *testing.T) {
	a := kube.New(fake.NewSimpleClientset(), ns)
	err := a.ApplyWorkload(context.Background(), cp.WorkloadSpec{App: "db", Kind: cp.WorkloadStatefulSet, Image: "pg:1", Replicas: 1})
	if !errors.Is(err, cp.ErrNotImplemented) {
		t.Fatalf("StatefulSet apply err = %v, want ErrNotImplemented", err)
	}
}

func TestWorkloadStatusMapping(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: i32p(3),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "img:1"}}}},
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas:   3,
			UpdatedReplicas: 3,
			Conditions:      []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}},
		},
	}
	a := kube.New(fake.NewSimpleClientset(dep), ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.DesiredReplicas != 3 || st.ReadyReplicas != 3 || st.UpdatedReplicas != 3 || !st.Available {
		t.Errorf("status = %+v, want desired=ready=updated=3 available", st)
	}
	if st.Image != "img:1" || st.Kind != cp.WorkloadDeployment {
		t.Errorf("status image/kind = %q/%q", st.Image, st.Kind)
	}
}

func TestWorkloadStatusNotFound(t *testing.T) {
	a := kube.New(fake.NewSimpleClientset(), ns)
	if _, err := a.WorkloadStatus(context.Background(), "ghost"); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestScale(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)
	_ = a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1})

	if err := a.ScaleWorkload(ctx, "web", 4); err != nil {
		t.Fatalf("ScaleWorkload: %v", err)
	}
	dep, _ := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if *dep.Spec.Replicas != 4 {
		t.Errorf("replicas = %d, want 4", *dep.Spec.Replicas)
	}

	if err := a.ScaleWorkload(ctx, "ghost", 2); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("scale missing err = %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)
	_ = a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1})

	if err := a.DeleteWorkload(ctx, "web"); err != nil {
		t.Fatalf("DeleteWorkload: %v", err)
	}
	if _, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{}); err == nil {
		t.Errorf("deployment should be gone")
	}
	if err := a.DeleteWorkload(ctx, "web"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("delete missing err = %v, want ErrNotFound", err)
	}
}

func TestLogs(t *testing.T) {
	ctx := context.Background()
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "web-abc", Namespace: ns,
		Labels: map[string]string{"app.kubernetes.io/name": "web"},
	}}
	a := kube.New(fake.NewSimpleClientset(dep, pod), ns)

	lines, err := a.Logs(ctx, "web", cp.LogOptions{})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(lines) == 0 || lines[0].Pod != "web-abc" {
		t.Fatalf("lines = %+v, want at least one line attributed to web-abc", lines)
	}

	if _, err := a.Logs(ctx, "ghost", cp.LogOptions{}); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("logs for missing app err = %v, want ErrNotFound", err)
	}
}
