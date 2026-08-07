// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/burrow-cloud/burrow/client"
)

func TestProviderAddWithoutTypeListsSupportedTypes(t *testing.T) {
	var out, errb bytes.Buffer
	// Missing <type>: the error and usage must name the available types so the user isn't left
	// guessing what to pass.
	_ = run(context.Background(), []string{"config", "provider", "add"}, &out, &errb)
	s := errb.String()
	for _, want := range []string{"needs <type>", "cloudflare", "digitalocean"} {
		if !strings.Contains(s, want) {
			t.Errorf("provider add (no type) output missing %q:\n%s", want, s)
		}
	}
}

// TestProviderAddSendsTokenInBody asserts `provider add` issues the control-plane API call with the
// token VALUE in the POST body — not a kubeconfig-direct Secret write, and not in the path or query
// (ADR-0030). The token is piped in (a script path), so the test drives the real RunE.
func TestProviderAddSendsTokenInBody(t *testing.T) {
	var gotPath, gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		// Respond with a recorded provider (no token echoed).
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "digitalocean", "type": "digitalocean",
			"capabilities": []string{"dns"}, "secret_key": "digitalocean",
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	cmd := newRootCmd()
	cmd.SetIn(strings.NewReader("dop_v1_secret\n")) // piped token (non-terminal)
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{
		"config", "provider", "add", "digitalocean",
		"--control-plane", srv.URL, "--token", "api-tok",
	})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("provider add: %v (stderr: %s)", err, errb.String())
	}

	if gotPath != "/v1/providers" {
		t.Errorf("path = %q, want /v1/providers", gotPath)
	}
	// The token must never appear in the path or query — only the body.
	if strings.Contains(gotPath, "dop_v1_secret") || strings.Contains(gotQuery, "dop_v1_secret") {
		t.Errorf("token leaked into the request path/query: path=%q query=%q", gotPath, gotQuery)
	}
	if !strings.Contains(gotBody, `"token":"dop_v1_secret"`) {
		t.Errorf("request body missing the token: %s", gotBody)
	}
	// The human output names the key, never the token value.
	if strings.Contains(out.String(), "dop_v1_secret") {
		t.Errorf("CLI output leaked the token value:\n%s", out.String())
	}
}

func TestReadTokenFromPipe(t *testing.T) {
	// A non-terminal reader (a pipe/redirect, as in a script) is read directly and trimmed.
	got, err := readToken(strings.NewReader("  dop_v1_abc\n"), io.Discard, "token: ")
	if err != nil {
		t.Fatalf("readToken: %v", err)
	}
	if got != "dop_v1_abc" {
		t.Errorf("readToken = %q, want the trimmed token", got)
	}
}

func TestProviderTypesCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"config", "provider", "types"}, &out, &errb); err != nil {
		t.Fatalf("provider types: %v", err)
	}
	s := out.String()
	for _, want := range []string{"TYPE", "SUPPORTS", "cloudflare", "digitalocean", "dns"} {
		if !strings.Contains(s, want) {
			t.Errorf("provider types output missing %q:\n%s", want, s)
		}
	}
}

// TestProviderAddS3SendsPairAndReportsVerification covers the operator side of ADR-0063: the
// credential is a PAIR, its secret half is read from stdin (never a flag, so it stays out of shell
// history and the process table), and what the control plane observed is reported back — including,
// and especially, a lifecycle check it could not perform.
func TestProviderAddS3SendsPairAndReportsVerification(t *testing.T) {
	var gotBody, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "s3", "type": "s3", "capabilities": []string{"object-storage"},
			"secret_key": "s3.access-key-id",
			"object_store": map[string]any{
				"endpoint":              "https://s3.example.com",
				"region":                "us-west-002",
				"bucket":                "burrow-backups-9f2c1ab3",
				"created":               true,
				"access_key_id_key":     "s3.access-key-id",
				"secret_access_key_key": "s3.secret-access-key",
				"retention_days":        30,
			},
			"verification": map[string]any{
				"bucket": "burrow-backups-9f2c1ab3", "bucket_created": true, "probe_object": true,
				"lifecycle": map[string]any{
					"status": "unknown",
					"detail": "Burrow could not read the lifecycle configuration of bucket burrow-backups-9f2c1ab3",
				},
			},
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	cmd := newRootCmd()
	cmd.SetIn(strings.NewReader("s3-secret-access-key\n")) // piped secret half
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{
		"config", "provider", "add", "s3",
		"--endpoint", "https://s3.example.com", "--region", "us-west-002",
		"--access-key-id", "AKIAEXAMPLE", "--create-bucket", "--retention-days", "30", "--confirm",
		"--control-plane", srv.URL, "--token", "api-tok",
	})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("provider add s3: %v (stderr: %s)", err, errb.String())
	}

	if gotPath != "/v1/providers" {
		t.Errorf("path = %q, want /v1/providers", gotPath)
	}
	if strings.Contains(gotPath+gotQuery, "s3-secret-access-key") {
		t.Error("the secret access key leaked into the request path or query")
	}
	for _, want := range []string{
		`"access_key_id":"AKIAEXAMPLE"`,
		`"secret_access_key":"s3-secret-access-key"`,
		`"endpoint":"https://s3.example.com"`,
		`"create_bucket":true`,
		`"retention_days":30`,
		`"confirm":true`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body missing %s:\n%s", want, gotBody)
		}
	}

	human := out.String()
	if strings.Contains(human, "s3-secret-access-key") {
		t.Errorf("the CLI output leaked the secret access key:\n%s", human)
	}
	for _, want := range []string{
		"burrow-backups-9f2c1ab3",           // the recorded bucket — the only one Burrow writes to
		"s3.access-key-id",                  // the key NAMES, never the values
		"s3.secret-access-key",              //
		"an object was written and deleted", // the destination was proven, not assumed
		"UNKNOWN",                           // an unverifiable invariant is never reported as verified
		"most consequential key",            // the credential-scoping advice ADR-0063 asks for
	} {
		if !strings.Contains(human, want) {
			t.Errorf("provider add output missing %q:\n%s", want, human)
		}
	}
}

// TestObjectStorageSummaryTicksVerifiedResultsOnly is issue #465: the registration summary is one
// status per line, and a line is marked verified only when Burrow verified it. `lifecycle: unknown`
// is the case that matters — ADR-0063 §3 forbids reporting an unverifiable invariant as verified,
// so it carries the advisory label rather than the tick — and so does the closing advice, which is
// standing guidance rather than a result.
func TestObjectStorageSummaryTicksVerifiedResultsOnly(t *testing.T) {
	p := client.Provider{
		Name: "s3", Type: "s3", Capabilities: []string{"object-storage"},
		ObjectStore: &client.ObjectStoreConfig{
			Bucket: "burrow-cloud", Endpoint: "https://s3.example.com",
			AccessKeyIDKey: "s3.access-key-id", SecretAccessKeyKey: "s3.secret-access-key",
		},
		Verification: &client.ProviderVerification{
			Bucket: "burrow-cloud", BucketCreated: true, ProbeObject: true,
			Lifecycle: client.LifecycleCheck{Status: "ok", Detail: "the bucket has no lifecycle rules"},
		},
	}
	var w bytes.Buffer
	lines := strings.Split(objectStorageSummary(&w, p), "\n")
	if len(lines) != 7 {
		t.Fatalf("summary is %d lines, want one status per line:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, l := range lines[:6] {
		if !strings.HasPrefix(l, okGlyph+" ") {
			t.Errorf("line %q reports a verified result and is not marked as one", l)
		}
	}
	if !strings.HasPrefix(lines[6], "Warning: ") {
		t.Errorf("the credential-scoping advice = %q, want it marked as advice rather than a result", lines[6])
	}

	p.Verification.Lifecycle = client.LifecycleCheck{Status: "unknown", Detail: "Burrow could not read the lifecycle configuration"}
	w.Reset()
	for _, l := range strings.Split(objectStorageSummary(&w, p), "\n") {
		if strings.Contains(l, "UNKNOWN") && strings.HasPrefix(l, okGlyph) {
			t.Errorf("an unverified lifecycle is reported as verified: %q", l)
		}
	}
}

// fakeProviderCluster stands in for one cluster's API server on the privileged path: it serves the
// install-token Secret the CLI reads to authenticate, then answers the proxied `provider add` with a
// recorded provider. It exists so a test can drive the real kubeconfig resolution — which is what
// decides the banner — rather than bypassing it with --control-plane.
func fakeProviderCluster(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/secrets/") {
			_ = json.NewEncoder(w).Encode(&corev1.Secret{
				TypeMeta:   metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{Name: "burrowd-api-token", Namespace: "burrow"},
				Data:       map[string][]byte{"token": []byte("s3cr3t")},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "cloudflare", "type": "cloudflare",
			"capabilities": []string{"dns"}, "secret_key": "cloudflare",
		})
	}))
	t.Cleanup(srv.Close)
	return writeKubeconfig(t, twoContextConfig(srv.URL, srv.URL))
}

// addCloudflareProvider runs `config provider add cloudflare` against a kubeconfig, with the token
// piped in. It returns stdout, which is where the registration summary and its target clause land.
func addCloudflareProvider(t *testing.T, kubeconfig string) string {
	t.Helper()
	var out, errb bytes.Buffer
	root := newRootCmd()
	root.SetArgs([]string{"config", "provider", "add", "cloudflare", "--kubeconfig", kubeconfig})
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetIn(strings.NewReader("cf-token\n"))
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("provider add: %v\nstderr: %s", err, errb.String())
	}
	return out.String()
}

// TestProviderAddNamesTheTargetNotTheKubeContext is the banner half of issue #465. `provider add`
// goes through the privileged connection path, which prints no targeting line, so the clause on the
// result is the only thing that says where the registration landed — and it used to say it in
// Kubernetes vocabulary: `on kube context "do-nyc1-burrow-cloud" (no target selected)`.
//
// The kubeconfig is only the ROUTE here. The CLI never writes the Secret; burrowd does, which is
// what its burrowd-credentials Role exists for. So the context is plumbing, and naming it answered
// with the mechanism instead of the thing (#460 settled that for the app path).
func TestProviderAddNamesTheTargetNotTheKubeContext(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")

	// The selected target and the registered handle are the two names Burrow has for a cluster, and
	// the clause has to reach for either before it falls back to a kube context. The handle case is
	// the one the issue was filed from: somebody who installed Burrow but never ran `burrow auth
	// login`, whose cluster HAS a name.
	t.Run("selected target", func(t *testing.T) {
		tempConfig(t)
		forbidCloud(t)
		kubeconfig := fakeProviderCluster(t)
		selectTarget(t, "prod")

		out := addCloudflareProvider(t, kubeconfig)
		if !strings.Contains(out, "on prod") {
			t.Errorf("provider add did not name the selected target.\ngot: %q", out)
		}
		for _, unwanted := range []string{"kube context", "no target selected"} {
			if strings.Contains(out, unwanted) {
				t.Errorf("provider add said %q.\ngot: %q", unwanted, out)
			}
		}
	})

	t.Run("registered handle", func(t *testing.T) {
		tempConfig(t)
		forbidCloud(t)
		kubeconfig := fakeProviderCluster(t)
		// The handle's name differs from the kube context it names, so "on production" can only have
		// come from Burrow's own name for the cluster.
		saveHandle(t, "production", "staging", false)

		out := addCloudflareProvider(t, kubeconfig)
		if !strings.Contains(out, "on production") {
			t.Errorf("provider add did not name the cluster the way the rest of the CLI does.\ngot: %q", out)
		}
		if strings.Contains(out, "kube context") {
			t.Errorf("provider add named the route rather than the target.\ngot: %q", out)
		}
	})
}

// TestProviderAddTicksTheRegistrationOfATokenProvider covers the OTHER half of the same output. The
// s3 path got the tick pattern; a cloudflare/digitalocean/github registration — the common case —
// was still two bare sentences, so a reader scanning for "did it work" had to read them (issue
// #465). Both lines are results the control plane confirmed, so both tick, and the target clause
// rides the first line rather than trailing the block.
func TestProviderAddTicksTheRegistrationOfATokenProvider(t *testing.T) {
	p := client.Provider{Name: "cloudflare", Type: "cloudflare", Capabilities: []string{"dns"}, SecretKey: "cloudflare"}
	var w bytes.Buffer
	lines := strings.Split(tokenProviderSummary(&w, p), "\n")
	if len(lines) != 2 {
		t.Fatalf("summary is %d lines, want one status per line:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, okGlyph+" ") {
			t.Errorf("line %q reports a confirmed result and is not marked as one", l)
		}
	}
	// The key NAME is reported; there is nowhere in this summary a token value could appear.
	if !strings.Contains(lines[1], `"cloudflare"`) {
		t.Errorf("the summary does not say where the credential was stored: %q", lines[1])
	}
}

// TestProviderAddS3RequiresAccessKeyID: the pair's identifier half is a flag and the secret half is
// stdin, so a missing identifier must fail before anything is read or sent.
func TestProviderAddS3RequiresAccessKeyID(t *testing.T) {
	var out, errb bytes.Buffer
	cmd := newRootCmd()
	cmd.SetIn(strings.NewReader("secret\n"))
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{"config", "provider", "add", "s3", "--endpoint", "https://s3.example.com"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("provider add s3 succeeded without --access-key-id")
	}
	if !strings.Contains(err.Error(), "access-key-id") {
		t.Errorf("error does not name the missing flag: %v", err)
	}
}

// TestProviderTypesListsObjectStorage: the type is discoverable before it is configured.
func TestProviderTypesListsObjectStorage(t *testing.T) {
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"config", "provider", "types"}, &out, &errb); err != nil {
		t.Fatalf("provider types: %v", err)
	}
	if !strings.Contains(out.String(), "s3") || !strings.Contains(out.String(), "object-storage") {
		t.Errorf("provider types does not list the object-storage type:\n%s", out.String())
	}
}
