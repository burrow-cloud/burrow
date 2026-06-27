// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package logs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLokiQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" || r.Method != http.MethodGet {
			t.Errorf("request = %s %s, want GET /loki/api/v1/query_range", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("query") != `{app="web"}` || q.Get("limit") != "10" || q.Get("direction") != "backward" {
			t.Errorf("params = query=%q limit=%q direction=%q", q.Get("query"), q.Get("limit"), q.Get("direction"))
		}
		_, _ = io.WriteString(w, `{"data":{"result":[
			{"stream":{"pod":"web-1"},"values":[["1700000000000000000","boom"],["1700000001000000000","again"]]},
			{"stream":{"pod_name":"web-2"},"values":[["1700000002000000000","third"]]}
		]}}`)
	}))
	defer srv.Close()

	l := NewLoki(srv.Client())
	endpoint := strings.TrimPrefix(srv.URL, "http://")
	entries, err := l.QueryLogs(context.Background(), endpoint, `{app="web"}`, 10)
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
	}
	if entries[0].Message != "boom" || entries[0].Pod != "web-1" || entries[0].Time != "2023-11-14T22:13:20Z" {
		t.Errorf("entry[0] = %+v", entries[0])
	}
	if entries[2].Message != "third" || entries[2].Pod != "web-2" {
		t.Errorf("entry[2] = %+v (want pod from pod_name)", entries[2])
	}

	// An empty query defaults to the match-everything selector.
	got := ""
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("query")
		_, _ = io.WriteString(w, `{"data":{"result":[]}}`)
	}))
	defer srv2.Close()
	_, _ = NewLoki(srv2.Client()).QueryLogs(context.Background(), strings.TrimPrefix(srv2.URL, "http://"), "", 5)
	if got != `{job=~".+"}` {
		t.Errorf("empty query sent %q, want {job=~\".+\"}", got)
	}

	// The limit caps the flattened result.
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"result":[{"stream":{},"values":[["1","a"],["2","b"],["3","c"]]}]}}`)
	}))
	defer srv3.Close()
	capped, err := NewLoki(srv3.Client()).QueryLogs(context.Background(), strings.TrimPrefix(srv3.URL, "http://"), "x", 2)
	if err != nil {
		t.Fatalf("QueryLogs (cap): %v", err)
	}
	if len(capped) != 2 {
		t.Errorf("got %d entries, want 2 (capped at limit)", len(capped))
	}

	// A non-2xx is an error.
	srv4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "bad logql")
	}))
	defer srv4.Close()
	if _, err := NewLoki(srv4.Client()).QueryLogs(context.Background(), strings.TrimPrefix(srv4.URL, "http://"), "x", 5); err == nil {
		t.Error("want error on http 400")
	}
}
