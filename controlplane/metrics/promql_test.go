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
	"time"
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

func TestPromQLQueryRange(t *testing.T) {
	start := time.Unix(1700000000, 0).UTC()
	end := time.Unix(1700000060, 0).UTC()
	step := 30 * time.Second

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" || r.Method != http.MethodGet {
			t.Errorf("request = %s %s, want GET /api/v1/query_range", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
		q := r.URL.Query()
		if q.Get("query") != `up{job="web"}` {
			t.Errorf("query = %q", q.Get("query"))
		}
		if q.Get("start") != "1700000000" || q.Get("end") != "1700000060" || q.Get("step") != "30" {
			t.Errorf("start/end/step = %q/%q/%q, want 1700000000/1700000060/30", q.Get("start"), q.Get("end"), q.Get("step"))
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"job":"web","instance":"a"},"values":[[1700000000,"1"],[1700000030,"2"]]},
			{"metric":{"job":"web","instance":"b"},"values":[[1700000000,"3"]]}
		]}}`)
	}))
	defer srv.Close()

	p := NewPromQL(srv.Client())
	endpoint := strings.TrimPrefix(srv.URL, "http://")
	series, err := p.QueryMetricsRange(context.Background(), endpoint, `up{job="web"}`, "tok", start, end, step)
	if err != nil {
		t.Fatalf("QueryMetricsRange: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("got %d series, want 2: %+v", len(series), series)
	}
	if series[0].Labels["instance"] != "a" || len(series[0].Points) != 2 {
		t.Errorf("series[0] = %+v, want instance a with two points", series[0])
	}
	if series[0].Points[0].Value != "1" || series[0].Points[0].Time != "2023-11-14T22:13:20Z" {
		t.Errorf("series[0].Points[0] = %+v", series[0].Points[0])
	}
	if series[0].Points[1].Value != "2" {
		t.Errorf("series[0].Points[1] = %+v, want value 2", series[0].Points[1])
	}
	if len(series[1].Points) != 1 || series[1].Points[0].Value != "3" {
		t.Errorf("series[1] = %+v, want instance b with one point value 3", series[1])
	}

	// An empty matrix yields an empty (non-nil) slice, not an error.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer srv2.Close()
	empty, err := NewPromQL(srv2.Client()).QueryMetricsRange(context.Background(), strings.TrimPrefix(srv2.URL, "http://"), "up", "", start, end, step)
	if err != nil {
		t.Fatalf("QueryMetricsRange (empty): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty matrix = %+v, want no series", empty)
	}

	// A success=false status is an error.
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"error","error":"parse error"}`)
	}))
	defer srv3.Close()
	if _, err := NewPromQL(srv3.Client()).QueryMetricsRange(context.Background(), strings.TrimPrefix(srv3.URL, "http://"), "x", "", start, end, step); err == nil {
		t.Error("want error on status != success")
	}

	// A non-matrix result type is an error.
	srv4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	defer srv4.Close()
	if _, err := NewPromQL(srv4.Client()).QueryMetricsRange(context.Background(), strings.TrimPrefix(srv4.URL, "http://"), "x", "", start, end, step); err == nil {
		t.Error("want error on non-matrix result type")
	}
}
