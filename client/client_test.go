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

func TestClientLogsTail(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"lines": []map[string]any{{"pod": "web-1", "message": "hello"}}})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	lines, err := c.Logs(context.Background(), "web", 5)
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
	_, err := c.Scale(context.Background(), "web", 0, false)
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
	res, err := c.Scale(context.Background(), "web", 4, false)
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

func TestClientStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "has_release": true, "running": true,
			"workload": map[string]any{"app": "web", "desired_replicas": 3, "ready_replicas": 3, "available": true},
		})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	res, err := c.Status(context.Background(), "web")
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
	res, err := c.WaitReachable(context.Background(), "web", time.Minute, immediateAfter)
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
	res, err := c.WaitReachable(context.Background(), "web", 9*time.Second, immediateAfter)
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
