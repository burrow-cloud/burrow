// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/burrow-cloud/burrow/client"
)

func TestClientDeploy(t *testing.T) {
	var gotAuth, gotToken, gotPath, gotBody, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotToken, gotPath, gotMethod = r.Header.Get("Authorization"), r.Header.Get("X-Burrow-Token"), r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"release":               map[string]any{"id": "r1", "app": "web", "image": "img:1", "status": "deployed", "replicas": 2, "digest": "sha256:abc"},
			"superseded_release_id": "r0",
		})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	res, err := c.Deploy(context.Background(), "web", client.DeployRequest{Image: "img:1", Replicas: 2})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res.Release.ID != "r1" || res.Release.Status != "deployed" || res.Release.Digest != "sha256:abc" {
		t.Errorf("result = %+v", res.Release)
	}
	if res.SupersededReleaseID != "r0" {
		t.Errorf("superseded = %q, want r0", res.SupersededReleaseID)
	}
	if gotToken != "tok" {
		t.Errorf("X-Burrow-Token = %q, want tok", gotToken)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty (the token must ride X-Burrow-Token only, ADR-0015)", gotAuth)
	}
	if gotMethod != "POST" || gotPath != "/v1/apps/web/deploy" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"img:1"`) {
		t.Errorf("body = %s, want it to carry the image", gotBody)
	}
}

func TestClientBuild(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"digest": "sha256:abc",
			"deploy": map[string]any{
				"release":               map[string]any{"id": "r1", "app": "web", "image": "img:1@sha256:abc", "status": "deployed", "replicas": 2},
				"superseded_release_id": "r0",
			},
		})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	res, err := c.Build(context.Background(), "web", client.BuildRequest{
		Source:      client.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.2.3"},
		TargetImage: "img:1",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Digest != "sha256:abc" || res.Deploy.Release.ID != "r1" || res.Deploy.SupersededReleaseID != "r0" {
		t.Errorf("result = %+v", res)
	}
	if gotMethod != "POST" || gotPath != "/v1/apps/web/build" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	// The git source (repo + ref) and the target image cross the channel; source bytes never do.
	for _, want := range []string{`"https://github.com/acme/web"`, `"v1.2.3"`, `"target_image":"img:1"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("body = %s, missing %s", gotBody, want)
		}
	}
}

func TestClientAutoDeploy(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "env": "prod", "level": "patch"})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")

	// Get carries no body and routes to the app's auto-deploy path.
	res, err := c.AutoDeploy(context.Background(), "web", "")
	if err != nil {
		t.Fatalf("AutoDeploy: %v", err)
	}
	if res.App != "web" || res.Level != "patch" {
		t.Errorf("result = %+v", res)
	}
	if gotMethod != "GET" || gotPath != "/v1/apps/web/auto-deploy" {
		t.Errorf("get request = %s %s", gotMethod, gotPath)
	}

	// Set carries the level in the body and the environment in the query.
	res, err = c.SetAutoDeploy(context.Background(), "web", "prod", "patch")
	if err != nil {
		t.Fatalf("SetAutoDeploy: %v", err)
	}
	if res.Env != "prod" {
		t.Errorf("result env = %q, want prod", res.Env)
	}
	if gotMethod != "PUT" || gotPath != "/v1/apps/web/auto-deploy" || gotQuery != "env=prod" {
		t.Errorf("set request = %s %s?%s", gotMethod, gotPath, gotQuery)
	}
	if !strings.Contains(gotBody, `"level":"patch"`) {
		t.Errorf("set body = %s, want it to carry the level", gotBody)
	}
}

func TestClientErrorMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "too many replicas", "code": "app.replica_ceiling"})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	_, err := c.Deploy(context.Background(), "web", client.DeployRequest{Image: "img:1", Replicas: 99})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnprocessableEntity || apiErr.Code != "app.replica_ceiling" {
		t.Errorf("apiErr = %+v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "too many replicas") {
		t.Errorf("error text = %q", apiErr.Error())
	}
}

// TestClientLimits covers the operational-configuration calls (ADR-0068): the list reads
// /v1/config, and a set PUTs the value under the limit's code with the environment as a query
// parameter — the same shape the guardrail calls use, because the two resolve the same way.
func TestClientLimits(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{"limits": []map[string]any{
			{"code": "app.replica_ceiling", "value": "200", "kind": "count", "scope": "environment", "env_scoped": true, "default": "50"},
		}})
	}))
	defer srv.Close()
	c := client.NewClient(srv.URL, "tok")

	limits, err := c.Limits(context.Background(), "")
	if err != nil {
		t.Fatalf("Limits: %v", err)
	}
	if gotMethod != "GET" || gotPath != "/v1/config" || gotQuery != "" {
		t.Errorf("list request = %s %s?%s, want GET /v1/config", gotMethod, gotPath, gotQuery)
	}
	if len(limits) != 1 || limits[0].Code != "app.replica_ceiling" || limits[0].Value != "200" {
		t.Errorf("limits = %+v", limits)
	}
	if limits[0].Scope != "environment" || !limits[0].EnvScoped || limits[0].Default != "50" {
		t.Errorf("limit tier fields = %+v, want scope environment, env-scoped, default 50", limits[0])
	}

	if _, err := c.SetLimit(context.Background(), "staging", "app.replica_ceiling", "200"); err != nil {
		t.Fatalf("SetLimit: %v", err)
	}
	if gotMethod != "PUT" || gotPath != "/v1/config/app.replica_ceiling" || gotQuery != "env=staging" {
		t.Errorf("set request = %s %s?%s", gotMethod, gotPath, gotQuery)
	}
	if !strings.Contains(gotBody, `"value":"200"`) {
		t.Errorf("set body = %s, want it to carry the value", gotBody)
	}
}

func TestClientLogsTail(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"lines": []map[string]any{{"pod": "web-1", "message": "hello"}}})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	lines, err := c.Logs(context.Background(), "web", "", 5)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(lines) != 1 || lines[0].Message != "hello" {
		t.Errorf("lines = %+v", lines)
	}
	if gotQuery != "tail=5" {
		t.Errorf("query = %q, want tail=5", gotQuery)
	}
}

func TestClientNeedsConfirmation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":              "scaling to zero replicas requires confirmation to proceed",
			"code":               "app.scale_to_zero",
			"needs_confirmation": true,
		})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	_, err := c.Scale(context.Background(), "web", "", 0, false)
	var ae *client.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v, want *client.APIError", err)
	}
	if !ae.NeedsConfirmation {
		t.Errorf("APIError.NeedsConfirmation = false, want true")
	}
	if !strings.Contains(ae.Error(), "--confirm") {
		t.Errorf("error should hint at --confirm, got %q", ae.Error())
	}
}

func TestClientScaleBody(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "previous_replicas": 2, "replicas": 4})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	res, err := c.Scale(context.Background(), "web", "", 4, false)
	if err != nil {
		t.Fatalf("Scale: %v", err)
	}
	if res.PreviousReplicas != 2 || res.Replicas != 4 {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(gotBody, `"replicas":4`) {
		t.Errorf("body = %s, want replicas 4", gotBody)
	}
}

// TestClientPublish confirms the publish call posts the one publish operation and decodes the
// verdict — including the fields that say the app is NOT live, which is the half a caller must not
// lose (ADR-0041 §3).
func TestClientPublish(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "host": "web.example.com", "reachable": false,
			"blocked_on": "tls certificate", "next": "wait for cert-manager",
			"summary": "web is published at web.example.com but not live yet.",
		})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	res, err := c.Publish(context.Background(), "web", client.PublishRequest{Host: "web.example.com", Port: 8080})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if gotPath != "/v1/apps/web/publish" {
		t.Errorf("path = %q, want /v1/apps/web/publish", gotPath)
	}
	if strings.Contains(gotBody, "no_tls") || strings.Contains(gotBody, "skip_dns") {
		t.Errorf("body = %s, want the zero request to ask for the complete publish", gotBody)
	}
	if res.Reachable || res.BlockedOn != "tls certificate" || res.Next == "" {
		t.Errorf("result = %+v, want the not-live verdict carried through", res)
	}
}

func TestClientStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "has_release": true, "running": true,
			"workload": map[string]any{"app": "web", "desired_replicas": 3, "ready_replicas": 3, "available": true},
		})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	res, err := c.Status(context.Background(), "web", "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !res.HasRelease || !res.Running || res.Workload.DesiredReplicas != 3 || !res.Workload.Available {
		t.Errorf("status = %+v", res)
	}
}

// immediateAfter is a WaitReachable poll clock that fires at once, so the wait loop runs to
// convergence or timeout without any real sleeping.
func immediateAfter(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

func TestWaitReachableConverges(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		// Flip to live on the third poll, modelling a chain that converges after a few checks.
		if polls >= 3 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"app": "web", "reachable": true, "url": "https://web.example.com",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "reachable": false, "blocked_on": "tls certificate",
		})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	res, err := c.WaitReachable(context.Background(), "web", "", time.Minute, immediateAfter)
	if err != nil {
		t.Fatalf("WaitReachable: %v", err)
	}
	if !res.Reachable || res.URL != "https://web.example.com" {
		t.Errorf("verdict = {reachable:%v url:%q}", res.Reachable, res.URL)
	}
	if polls != 3 {
		t.Errorf("polls = %d, want 3 (stops as soon as it converges)", polls)
	}
}

func TestWaitReachableTimesOut(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "reachable": false, "blocked_on": "tls certificate",
		})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	// 9s timeout at a 3s poll interval is one immediate check plus three interval polls.
	res, err := c.WaitReachable(context.Background(), "web", "", 9*time.Second, immediateAfter)
	if err != nil {
		t.Fatalf("WaitReachable: %v", err)
	}
	if res.Reachable || res.BlockedOn != "tls certificate" {
		t.Errorf("verdict = {reachable:%v blocked:%q}, want blocked on tls certificate", res.Reachable, res.BlockedOn)
	}
	if polls != 4 {
		t.Errorf("polls = %d, want 4 (bounded by the timeout, no infinite loop)", polls)
	}
}

func TestClientAutoscaleBody(t *testing.T) {
	var gotBody, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotMethod, gotPath = string(b), r.Method, r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "min_replicas": 1, "max_replicas": 8, "cpu_percent": 90,
			"metrics_available": false, "warning": "autoscaling needs metrics-server, which was not detected.",
		})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	res, err := c.Autoscale(context.Background(), "web", client.AutoscaleRequest{Min: 1, Max: 8, CPU: 90})
	if err != nil {
		t.Fatalf("Autoscale: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/v1/apps/web/autoscale" {
		t.Errorf("request = %s %s, want POST /v1/apps/web/autoscale", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"max":8`) || !strings.Contains(gotBody, `"cpu":90`) {
		t.Errorf("body = %s, want max 8 cpu 90", gotBody)
	}
	if res.MaxReplicas != 8 || res.CPUPercent != 90 || res.MetricsAvailable {
		t.Errorf("result = %+v", res)
	}
	if res.Warning == "" {
		t.Errorf("expected a metrics-absent warning")
	}
}

func TestClientDisableAutoscale(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	if err := c.DisableAutoscale(context.Background(), "web", "prod", true); err != nil {
		t.Fatalf("DisableAutoscale: %v", err)
	}
	if gotMethod != "DELETE" || gotPath != "/v1/apps/web/autoscale" {
		t.Errorf("request = %s %s, want DELETE /v1/apps/web/autoscale", gotMethod, gotPath)
	}
	if !strings.Contains(gotQuery, "confirm=true") || !strings.Contains(gotQuery, "env=prod") {
		t.Errorf("query = %q, want confirm and env", gotQuery)
	}
}
