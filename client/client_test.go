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

	"github.com/burrow-cloud/burrow/client"
)

func TestClientDeploy(t *testing.T) {
	var gotAuth, gotPath, gotBody, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotMethod = r.Header.Get("Authorization"), r.URL.Path, r.Method
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
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q, want Bearer tok", gotAuth)
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
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "too many replicas", "code": "replica_ceiling"})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	_, err := c.Deploy(context.Background(), "web", client.DeployRequest{Image: "img:1", Replicas: 99})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnprocessableEntity || apiErr.Code != "replica_ceiling" {
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

func TestClientScaleBody(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "previous_replicas": 2, "replicas": 4})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	res, err := c.Scale(context.Background(), "web", 4)
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
