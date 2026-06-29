// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPromQLQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" || r.Method != http.MethodGet {
			t.Errorf("request = %s %s, want GET /api/v1/query", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
		if got := r.URL.Query().Get("query"); got != `up{job="web"}` {
			t.Errorf("query = %q, want up{job=\"web\"}", got)
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"job":"web","instance":"a"},"value":[1700000000,"1"]},
			{"metric":{"job":"web","instance":"b"},"value":[1700000000.5,"0"]}
		]}}`)
	}))
	defer srv.Close()

	p := NewPromQL(srv.Client())
	endpoint := strings.TrimPrefix(srv.URL, "http://")
	samples, err := p.QueryMetrics(context.Background(), endpoint, `up{job="web"}`, "tok")
	if err != nil {
		t.Fatalf("QueryMetrics: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("got %d samples, want 2: %+v", len(samples), samples)
	}
	if samples[0].Value != "1" || samples[0].Labels["instance"] != "a" || samples[0].Time != "2023-11-14T22:13:20Z" {
		t.Errorf("sample[0] = %+v", samples[0])
	}
	if samples[1].Value != "0" || samples[1].Time != "2023-11-14T22:13:20.5Z" {
		t.Errorf("sample[1] = %+v (want fractional ts)", samples[1])
	}

	// An empty token sends no Authorization header.
	gotAuth := "unset"
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	defer srv2.Close()
	if _, err := NewPromQL(srv2.Client()).QueryMetrics(context.Background(), strings.TrimPrefix(srv2.URL, "http://"), "up", ""); err != nil {
		t.Fatalf("QueryMetrics (no auth): %v", err)
	}
	if gotAuth != "" {
		t.Errorf("empty token sent Authorization %q, want none", gotAuth)
	}

	// A scalar result yields a single label-less sample.
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"scalar","result":[1700000000,"42"]}}`)
	}))
	defer srv3.Close()
	scalar, err := NewPromQL(srv3.Client()).QueryMetrics(context.Background(), strings.TrimPrefix(srv3.URL, "http://"), "scalar(1)", "")
	if err != nil {
		t.Fatalf("QueryMetrics (scalar): %v", err)
	}
	if len(scalar) != 1 || scalar[0].Value != "42" || len(scalar[0].Labels) != 0 {
		t.Errorf("scalar = %+v, want one label-less sample with value 42", scalar)
	}

	// A non-2xx is an error.
	srv4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "bad promql")
	}))
	defer srv4.Close()
	if _, err := NewPromQL(srv4.Client()).QueryMetrics(context.Background(), strings.TrimPrefix(srv4.URL, "http://"), "x", ""); err == nil {
		t.Error("want error on http 400")
	}

	// A success=false status is an error even on a 200.
	srv5 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"error","error":"parse error"}`)
	}))
	defer srv5.Close()
	if _, err := NewPromQL(srv5.Client()).QueryMetrics(context.Background(), strings.TrimPrefix(srv5.URL, "http://"), "x", ""); err == nil {
		t.Error("want error on status != success")
	}
}
