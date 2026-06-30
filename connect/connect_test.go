// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package connect

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	fakekube "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// writeKubeconfig writes cfg to a temp file and returns its path.
func writeKubeconfig(t *testing.T, cfg *api.Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// twoContextConfig builds a kubeconfig with two contexts (ctx-one current, ctx-two not) whose
// clusters point at serverOne and serverTwo.
func twoContextConfig(serverOne, serverTwo string) *api.Config {
	cfg := api.NewConfig()
	cfg.Clusters["cluster-one"] = &api.Cluster{Server: serverOne, InsecureSkipTLSVerify: true}
	cfg.Clusters["cluster-two"] = &api.Cluster{Server: serverTwo, InsecureSkipTLSVerify: true}
	cfg.AuthInfos["user"] = &api.AuthInfo{Token: "t"}
	cfg.Contexts["ctx-one"] = &api.Context{Cluster: "cluster-one", AuthInfo: "user"}
	cfg.Contexts["ctx-two"] = &api.Context{Cluster: "cluster-two", AuthInfo: "user"}
	cfg.CurrentContext = "ctx-one"
	return cfg
}

// TestRESTConfigContextSelectsCluster confirms the Context override picks that context's cluster,
// and that an empty Context keeps the kubeconfig's current context (no regression) — ADR-0035.
func TestRESTConfigContextSelectsCluster(t *testing.T) {
	path := writeKubeconfig(t, twoContextConfig("https://one.example:6443", "https://two.example:6443"))

	current, err := RESTConfig(path, "")
	if err != nil {
		t.Fatalf("RESTConfig (current context): %v", err)
	}
	if current.Host != "https://one.example:6443" {
		t.Errorf("empty context host = %q, want the current context's cluster", current.Host)
	}

	selected, err := RESTConfig(path, "ctx-two")
	if err != nil {
		t.Fatalf("RESTConfig (selected context): %v", err)
	}
	if selected.Host != "https://two.example:6443" {
		t.Errorf("selected context host = %q, want ctx-two's cluster", selected.Host)
	}
}

// TestClientContextSelectsCluster confirms Client reads its token from — and so targets — the
// cluster of the selected context, not the current one.
func TestClientContextSelectsCluster(t *testing.T) {
	var oneHit, twoHit bool
	one := tokenServer(&oneHit)
	two := tokenServer(&twoHit)
	defer one.Close()
	defer two.Close()

	path := writeKubeconfig(t, twoContextConfig(one.URL, two.URL))

	c, err := Client(context.Background(), Options{Kubeconfig: path, Context: "ctx-two", Namespace: "burrow"})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if c == nil {
		t.Fatal("Client returned a nil client")
	}
	if !twoHit {
		t.Errorf("selected context's cluster (ctx-two) was not contacted")
	}
	if oneHit {
		t.Errorf("current context's cluster (ctx-one) was contacted; --context should redirect to ctx-two")
	}
}

// notInstalledServer is a fake API server that answers the token Secret Get with a Kubernetes
// NotFound, standing in for a cluster where burrowd has not been installed.
func notInstalledServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(&metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status:   metav1.StatusFailure,
			Code:     http.StatusNotFound,
			Reason:   metav1.StatusReasonNotFound,
			Message:  `secrets "burrowd-api-token" not found`,
			Details:  &metav1.StatusDetails{Name: "burrowd-api-token", Kind: "secrets"},
		})
	}))
}

// TestClientNotInstalled confirms that when the token Secret is absent (burrowd not installed),
// Client returns an actionable message that names the targeted context and points at
// `burrow install`, with no raw "reading token secret ... not found" Kubernetes error.
func TestClientNotInstalled(t *testing.T) {
	srv := notInstalledServer()
	defer srv.Close()

	// The current context (ctx-one) points at the not-installed cluster.
	path := writeKubeconfig(t, twoContextConfig(srv.URL, "https://unused.invalid:6443"))

	_, err := Client(context.Background(), Options{Kubeconfig: path, Namespace: "burrow"})
	if err == nil {
		t.Fatal("Client should fail when burrowd is not installed")
	}
	msg := err.Error()
	for _, want := range []string{`burrow is not installed in context "ctx-one"`, `namespace "burrow"`, `run "burrow install"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want substring %q", msg, want)
		}
	}
	for _, no := range []string{"reading token secret", "not found", "secrets"} {
		if strings.Contains(msg, no) {
			t.Errorf("error = %q, should not contain the raw Kubernetes error %q", msg, no)
		}
	}
}

// TestClientUnreachable confirms that when the cluster cannot be reached, Client reports the
// control plane unreachable, names the targeted context, and leaks no dialed URL.
func TestClientUnreachable(t *testing.T) {
	cfg := twoContextConfig("https://burrow-connect-unreachable.invalid:6443", "https://unused.invalid:6443")
	// Rename the current context so the message clearly names it.
	cfg.Contexts["do-nyc1-prod"] = cfg.Contexts["ctx-one"]
	delete(cfg.Contexts, "ctx-one")
	cfg.CurrentContext = "do-nyc1-prod"
	path := writeKubeconfig(t, cfg)

	_, err := Client(context.Background(), Options{Kubeconfig: path, Namespace: "burrow"})
	if err == nil {
		t.Fatal("Client should fail when the cluster is unreachable")
	}
	msg := err.Error()
	if !strings.Contains(msg, `control plane unreachable via context "do-nyc1-prod"`) {
		t.Errorf("error = %q, want the unreachable line naming the context", msg)
	}
	if strings.Contains(msg, "https://") || strings.Contains(msg, `Get "`) {
		t.Errorf("error = %q, leaked the dialed URL", msg)
	}
}

// TestFailureReason confirms each common connectivity failure reduces to a concise reason with no
// dialed URL, and that an unrecognized error keeps its message minus the `Get "<url>": ` prefix.
// This is the shared classifier `burrow version` and Client both depend on.
func TestFailureReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"timeout", context.DeadlineExceeded, "timed out after 5s"},
		{"dns", &net.DNSError{Err: "no such host", Name: "abc123.example.com"}, "no such host"},
		{"refused", syscall.ECONNREFUSED, "connection refused"},
		{"other strips the Get URL prefix", errString(`Get "https://abc123.example.com/apis/apps/v1": broken pipe`), "broken pipe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FailureReason(tc.err); got != tc.want {
				t.Errorf("FailureReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
			if strings.Contains(FailureReason(tc.err), "https://") {
				t.Errorf("FailureReason(%v) leaked a URL: %q", tc.err, FailureReason(tc.err))
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// tokenServer is a fake API server that records that it was hit and serves the install token
// Secret for any namespace, so Client.readToken succeeds against it.
func tokenServer(hit *bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hit = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&corev1.Secret{
			TypeMeta:   metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
			ObjectMeta: metav1.ObjectMeta{Name: "burrowd-api-token", Namespace: "burrow"},
			Data:       map[string][]byte{"token": []byte("s3cr3t")},
		})
	}))
}

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

	cfg, err := RESTConfig(kubeconfig, "")
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
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
