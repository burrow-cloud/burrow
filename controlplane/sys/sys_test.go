// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package sys

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClockNow(t *testing.T) {
	before := time.Now()
	got := Clock{}.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("Clock.Now() = %v, want between %v and %v", got, before, after)
	}
}

func TestIDsUniqueAndHex(t *testing.T) {
	ids := IDs{}
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := ids.NewID()
		if len(id) != 32 {
			t.Fatalf("NewID() = %q, want 32 hex chars", id)
		}
		if _, err := hex.DecodeString(id); err != nil {
			t.Fatalf("NewID() = %q is not hex: %v", id, err)
		}
		if seen[id] {
			t.Fatalf("NewID() produced a duplicate: %q", id)
		}
		seen[id] = true
	}
}

// TestHTTPProbeReportsTheStatus asserts the pre-flight's probe answers with the status code it got,
// including a 404 — the pre-flight asks whether the request ARRIVED at the cluster, not what the app
// made of the challenge path it knows nothing about.
func TestHTTPProbeReportsTheStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	status, err := HTTPProbe{}.ProbeHTTP(context.Background(), srv.URL+"/.well-known/acme-challenge/x")
	if err != nil {
		t.Fatalf("ProbeHTTP: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// TestHTTPProbeDoesNotFollowRedirects pins the other half: an app that redirects port 80 to HTTPS
// has still answered on port 80, which is what the ACME challenge needs. Following the redirect
// would report on the HTTPS listener instead — the one that does not exist yet at publish time.
func TestHTTPProbeDoesNotFollowRedirects(t *testing.T) {
	var hops int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "https://example.invalid/", http.StatusMovedPermanently)
	}))
	defer srv.Close()

	status, err := HTTPProbe{}.ProbeHTTP(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatalf("ProbeHTTP: %v", err)
	}
	if status != http.StatusMovedPermanently {
		t.Errorf("status = %d, want the redirect itself", status)
	}
	if hops != 1 {
		t.Errorf("server saw %d request(s), want exactly one", hops)
	}
}

// TestHTTPProbeReportsNothingAnswering asserts a host nothing serves is an error rather than a
// status, because that is precisely the case a publish must not open an ACME order on.
func TestHTTPProbeReportsNothingAnswering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening any more

	if _, err := (HTTPProbe{}).ProbeHTTP(context.Background(), url+"/"); err == nil {
		t.Fatal("ProbeHTTP against a closed listener = nil error, want a failure")
	}
}
