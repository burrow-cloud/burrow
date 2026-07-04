// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLogRequests(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	h := logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))

	// A real request is logged with method, path, and status.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/providers", nil))
	if got := buf.String(); !strings.Contains(got, "POST /v1/providers 400") {
		t.Errorf("access log = %q, want it to record the request and status", got)
	}

	// The readiness probe is not logged (it would be every-few-seconds noise).
	buf.Reset()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	if buf.Len() != 0 {
		t.Errorf("healthz should not be logged, got %q", buf.String())
	}
}

// fastWait is a dbWait tuned for tests: tiny per-attempt timeout and backoff so the loop runs in
// milliseconds, a generous budget so a hung-then-succeeding pinger has room, and a log interval
// large enough to exercise throttling.
func fastWait() dbWait {
	return dbWait{
		attempt:     10 * time.Millisecond,
		backoff:     time.Millisecond,
		budget:      2 * time.Second,
		logInterval: time.Hour,
	}
}

// TestDBWaitRetriesPastHungAttempt asserts the per-attempt timeout rescues the loop from a hung
// dial: the first two attempts block until their context is cancelled (modeling a stuck TCP
// dial that honors the context), yet the loop cancels each after w.attempt and retries, reaching
// the succeeding attempt quickly instead of blocking for the whole budget.
func TestDBWaitRetriesPastHungAttempt(t *testing.T) {
	var calls int32
	start := time.Now()
	err := fastWait().run(context.Background(), func(ctx context.Context) error {
		if atomic.AddInt32(&calls, 1) <= 2 {
			<-ctx.Done() // hang until the per-attempt timeout cancels this attempt
			return ctx.Err()
		}
		return nil // Postgres is accepting connections now
	})
	if err != nil {
		t.Fatalf("run returned %v, want success after retrying past the hung attempts", err)
	}
	if got := atomic.LoadInt32(&calls); got < 3 {
		t.Errorf("attempts = %d, want at least 3 (two hung, then a success)", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("run took %v, want it bounded well under the budget (the hung attempts were cancelled)", elapsed)
	}
}

// TestDBWaitSucceedsAfterTransientFailures asserts that a pinger which fails a few times then
// succeeds returns promptly, after a bounded number of attempts, not after the whole budget.
func TestDBWaitSucceedsAfterTransientFailures(t *testing.T) {
	var calls int32
	err := fastWait().run(context.Background(), func(context.Context) error {
		if atomic.AddInt32(&calls, 1) < 3 {
			return errors.New("dial error: connection refused")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("run returned %v, want success after the transient failures cleared", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("attempts = %d, want exactly 3 (two failures then a success)", got)
	}
}

// TestDBWaitBudgetBoundsTotal asserts the overall budget still bounds a never-succeeding wait: it
// returns an error wrapping the last failure at roughly the budget, not indefinitely.
func TestDBWaitBudgetBoundsTotal(t *testing.T) {
	w := dbWait{attempt: 5 * time.Millisecond, backoff: 5 * time.Millisecond, budget: 40 * time.Millisecond, logInterval: time.Hour}
	start := time.Now()
	sentinel := errors.New("dial error: connection refused")
	err := w.run(context.Background(), func(context.Context) error { return sentinel })
	if err == nil {
		t.Fatal("run returned nil, want an error once the budget is exhausted")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap the last attempt's failure", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("run took %v, want it bounded near the budget", elapsed)
	}
}

// TestDBWaitCanceledContext asserts an outer cancellation ends the wait promptly rather than
// blocking through the backoff and budget.
func TestDBWaitCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := dbWait{attempt: time.Second, backoff: time.Second, budget: time.Minute, logInterval: time.Hour}
	err := w.run(ctx, func(context.Context) error { return errors.New("refused") })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

// TestDBWaitThrottlesLog asserts the wait log is throttled: with a long log interval only the
// first failure is logged even though several attempts fail, so a fast retry loop does not spam.
func TestDBWaitThrottlesLog(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	var calls int32
	err := fastWait().run(context.Background(), func(context.Context) error {
		if atomic.AddInt32(&calls, 1) < 5 {
			return errors.New("dial error: connection refused")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("run returned %v, want success", err)
	}
	if got := strings.Count(buf.String(), "waiting for the database"); got != 1 {
		t.Errorf("wait log lines = %d, want 1 (the first failure; the rest throttled): %q", got, buf.String())
	}
}

// TestWithConnectTimeout covers both DSN forms plus the no-op cases.
func TestWithConnectTimeout(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{"url form", "postgres://u:p@host:5432/db?sslmode=disable", "connect_timeout=5"},
		{"keyword form", "host=db user=u password=p dbname=burrow", "host=db user=u password=p dbname=burrow connect_timeout=5"},
		{"already set url", "postgres://u:p@host/db?connect_timeout=9", "postgres://u:p@host/db?connect_timeout=9"},
		{"already set keyword", "host=db connect_timeout=9", "host=db connect_timeout=9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withConnectTimeout(tt.dsn, 5*time.Second)
			if !strings.Contains(got, tt.want) {
				t.Errorf("withConnectTimeout(%q) = %q, want it to contain %q", tt.dsn, got, tt.want)
			}
		})
	}
}

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
