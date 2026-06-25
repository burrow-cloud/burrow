// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package dns

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

func TestFactoryMapsTypesAndRejectsUnknown(t *testing.T) {
	f := NewFactory()
	for _, typ := range []cp.ProviderType{cp.ProviderDigitalOcean, cp.ProviderCloudflare} {
		p, err := f.DNS(typ, "tok")
		if err != nil || p == nil {
			t.Errorf("DNS(%q) = %v, %v; want a provider", typ, p, err)
		}
	}
	if _, err := f.DNS("aws", "tok"); !errors.Is(err, cp.ErrNotImplemented) {
		t.Errorf("DNS(aws) err = %v, want ErrNotImplemented", err)
	}
}

func TestDigitalOceanVerifyAccess(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		switch r.Header.Get("X-Case") {
		case "unauth":
			w.WriteHeader(http.StatusUnauthorized)
		case "boom":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"domains":[]}`))
		}
	}))
	defer srv.Close()

	do := &digitalOcean{token: "dop_tok", baseURL: srv.URL, http: srv.Client()}

	if err := do.VerifyAccess(context.Background()); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if gotAuth != "Bearer dop_tok" {
		t.Errorf("Authorization = %q, want Bearer dop_tok", gotAuth)
	}
	if gotPath != "/v2/domains" {
		t.Errorf("path = %q, want /v2/domains", gotPath)
	}

	// A 401 is a rejected token (ErrInvalid); a 500 is a vendor error, not ErrInvalid.
	if err := withCase(do, "unauth").VerifyAccess(context.Background()); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("401 err = %v, want ErrInvalid", err)
	}
	if err := withCase(do, "boom").VerifyAccess(context.Background()); err == nil || errors.Is(err, cp.ErrInvalid) {
		t.Errorf("500 err = %v, want a non-ErrInvalid error", err)
	}
}

func TestCloudflareVerifyAccess(t *testing.T) {
	var body, code string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/tokens/verify" {
			t.Errorf("path = %q, want /user/tokens/verify", r.URL.Path)
		}
		switch code {
		case "401":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusOK)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	cf := &cloudflare{token: "cf_tok", baseURL: srv.URL, http: srv.Client()}
	ctx := context.Background()

	// Active token.
	body, code = `{"success":true,"result":{"status":"active"}}`, "200"
	if err := cf.VerifyAccess(ctx); err != nil {
		t.Fatalf("active token: %v", err)
	}

	// 200 but not active → invalid.
	body, code = `{"success":true,"result":{"status":"disabled"}}`, "200"
	if err := cf.VerifyAccess(ctx); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("inactive err = %v, want ErrInvalid", err)
	}

	// success:false → invalid.
	body, code = `{"success":false,"result":{"status":""}}`, "200"
	if err := cf.VerifyAccess(ctx); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("success=false err = %v, want ErrInvalid", err)
	}

	// 401 → invalid.
	body, code = "", "401"
	if err := cf.VerifyAccess(ctx); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("401 err = %v, want ErrInvalid", err)
	}

	// 200 with unparseable body → a non-ErrInvalid error.
	body, code = "not json", "200"
	if err := cf.VerifyAccess(ctx); err == nil || errors.Is(err, cp.ErrInvalid) {
		t.Errorf("bad json err = %v, want a non-ErrInvalid error", err)
	}
}

// withCase returns a copy of the DigitalOcean adapter whose requests carry an X-Case header
// the test server branches on (the real client sets no such header).
func withCase(d *digitalOcean, c string) *digitalOcean {
	return &digitalOcean{token: d.token, baseURL: d.baseURL, http: &http.Client{Transport: caseRT{c, d.http.Transport}}}
}

type caseRT struct {
	c    string
	next http.RoundTripper
}

func (rt caseRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("X-Case", rt.c)
	next := rt.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(r)
}

func TestSnippetTruncates(t *testing.T) {
	if got := snippet([]byte("  line one\nline two  ")); got != "line one line two" {
		t.Errorf("snippet = %q", got)
	}
	if got := snippet([]byte(strings.Repeat("x", 300))); len(got) <= 200 || !strings.HasSuffix(got, "…") {
		t.Errorf("snippet did not truncate: len=%d", len(got))
	}
}
