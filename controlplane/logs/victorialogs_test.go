// SPDX-License-Identifier: Apache-2.0
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

func TestVictoriaLogsQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/select/logsql/query" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s, want POST /select/logsql/query", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
		_ = r.ParseForm()
		if r.FormValue("query") != "error" || r.FormValue("limit") != "10" {
			t.Errorf("form = query=%q limit=%q", r.FormValue("query"), r.FormValue("limit"))
		}
		_, _ = io.WriteString(w, `{"_time":"2026-06-27T00:00:00Z","_msg":"boom","kubernetes_pod_name":"web-1"}`+"\n")
		_, _ = io.WriteString(w, "\n") // blank line ignored
		_, _ = io.WriteString(w, `{"_time":"2026-06-27T00:00:01Z","_msg":"again"}`+"\n")
	}))
	defer srv.Close()

	v := NewVictoriaLogs(srv.Client())
	endpoint := strings.TrimPrefix(srv.URL, "http://")
	entries, err := v.QueryLogs(context.Background(), endpoint, "error", 10, "tok")
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	if entries[0].Message != "boom" || entries[0].Pod != "web-1" || entries[0].Time == "" {
		t.Errorf("entry[0] = %+v", entries[0])
	}
	if entries[1].Message != "again" || entries[1].Pod != "" {
		t.Errorf("entry[1] = %+v", entries[1])
	}

	// An empty query defaults to "*", and an empty token sends no Authorization header.
	got := ""
	gotAuth := "unset"
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.FormValue("query")
		gotAuth = r.Header.Get("Authorization")
	}))
	defer srv2.Close()
	_, _ = NewVictoriaLogs(srv2.Client()).QueryLogs(context.Background(), strings.TrimPrefix(srv2.URL, "http://"), "", 5, "")
	if got != "*" {
		t.Errorf("empty query sent %q, want *", got)
	}
	if gotAuth != "" {
		t.Errorf("empty token sent Authorization %q, want none", gotAuth)
	}

	// A non-2xx is an error.
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "bad logsql")
	}))
	defer srv3.Close()
	if _, err := NewVictoriaLogs(srv3.Client()).QueryLogs(context.Background(), strings.TrimPrefix(srv3.URL, "http://"), "x", 5, ""); err == nil {
		t.Error("want error on http 400")
	}
}
