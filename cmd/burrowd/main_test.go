// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestServerHandlerReadiness(t *testing.T) {
	var ready atomic.Bool
	var apiHandler atomic.Pointer[http.Handler]
	h := serverHandler(&ready, &apiHandler)

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	// Before the control plane has started, health is 503 and API calls are 503 — the pod is
	// up (serving) but not ready, never crash-looping while the database comes up.
	if rec := get("/healthz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("healthz before ready = %d, want 503", rec.Code)
	}
	if rec := get("/v1/apps/web/status"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/v1 before ready = %d, want 503", rec.Code)
	}

	// Once the API is wired and readiness flips, health is 200 and calls reach the API.
	var api http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "from-api")
	})
	apiHandler.Store(&api)
	ready.Store(true)

	if rec := get("/healthz"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("healthz when ready = %d %q", rec.Code, rec.Body.String())
	}
	if rec := get("/v1/apps/web/status"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "from-api") {
		t.Errorf("/v1 when ready = %d %q", rec.Code, rec.Body.String())
	}
}
