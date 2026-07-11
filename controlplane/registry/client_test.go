// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package registry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/controlplane"
)

// hostOf returns the host:port of a test server, which parseImageRef treats as an explicit registry
// host because it contains a colon.
func hostOf(srv *httptest.Server) string {
	return strings.TrimPrefix(srv.URL, "https://")
}

// TestListTagsPagination covers the happy path and Link-header pagination across two pages.
func TestListTagsPagination(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/u/app/tags/list" {
			t.Errorf("path = %q, want /v2/u/app/tags/list", r.URL.Path)
		}
		if r.URL.Query().Get("last") == "" {
			// First page: two tags and a Link to the next page.
			w.Header().Set("Link", `</v2/u/app/tags/list?last=1.2.0&n=2>; rel="next"`)
			_, _ = io.WriteString(w, `{"name":"u/app","tags":["1.1.0","1.2.0"]}`)
			return
		}
		// Second page: no Link, the pagination terminates.
		_, _ = io.WriteString(w, `{"name":"u/app","tags":["1.3.0"]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.Client())
	tags, err := c.ListTags(context.Background(), hostOf(srv)+"/u/app:1.1.0", controlplane.RegistryAuth{})
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if strings.Join(tags, ",") != "1.1.0,1.2.0,1.3.0" {
		t.Errorf("tags = %v, want [1.1.0 1.2.0 1.3.0] across both pages", tags)
	}
}

// TestListTagsTokenFlowAnonymous covers the 401 -> WWW-Authenticate -> token -> retry flow with no
// credentials: the token endpoint is called with the challenge's scope and NO basic-auth header.
func TestListTagsTokenFlowAnonymous(t *testing.T) {
	var tokenHit bool
	var gotAuthHeader, gotScope string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/u/app/tags/list":
			if r.Header.Get("Authorization") != "Bearer good-token" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+baseURL(r)+`/token",service="registry.test",scope="repository:u/app:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"name":"u/app","tags":["1.0.0"]}`)
		case "/token":
			tokenHit = true
			gotAuthHeader = r.Header.Get("Authorization")
			gotScope = r.URL.Query().Get("scope")
			_, _ = io.WriteString(w, `{"token":"good-token"}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.Client())
	tags, err := c.ListTags(context.Background(), hostOf(srv)+"/u/app:1.0.0", controlplane.RegistryAuth{})
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 1 || tags[0] != "1.0.0" {
		t.Errorf("tags = %v, want [1.0.0]", tags)
	}
	if !tokenHit {
		t.Fatal("token endpoint was not called")
	}
	if gotAuthHeader != "" {
		t.Errorf("anonymous token request carried Authorization %q, want none", gotAuthHeader)
	}
	if gotScope != "repository:u/app:pull" {
		t.Errorf("token scope = %q, want repository:u/app:pull", gotScope)
	}
}

// TestListTagsTokenFlowBasicAuth covers the same flow with credentials: the token endpoint receives
// HTTP Basic auth carrying the supplied username and password.
func TestListTagsTokenFlowBasicAuth(t *testing.T) {
	var gotUser, gotPass string
	var gotOK bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/priv/app/tags/list":
			if r.Header.Get("Authorization") != "Bearer good-token" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+baseURL(r)+`/token",service="registry.test",scope="repository:priv/app:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"name":"priv/app","tags":["2.0.0"]}`)
		case "/token":
			gotUser, gotPass, gotOK = r.BasicAuth()
			_, _ = io.WriteString(w, `{"access_token":"good-token"}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.Client())
	tags, err := c.ListTags(context.Background(), hostOf(srv)+"/priv/app:2.0.0", controlplane.RegistryAuth{Username: "robot", Password: "s3cret"})
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 1 || tags[0] != "2.0.0" {
		t.Errorf("tags = %v, want [2.0.0]", tags)
	}
	if !gotOK || gotUser != "robot" || gotPass != "s3cret" {
		t.Errorf("token basic auth = (%q,%q,ok=%v), want (robot,s3cret,true)", gotUser, gotPass, gotOK)
	}
}

// TestListTagsRateLimit surfaces a 429 as the typed *RateLimitError carrying Retry-After.
func TestListTagsRateLimit(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := NewClient(srv.Client()).ListTags(context.Background(), hostOf(srv)+"/u/app:1.0.0", controlplane.RegistryAuth{})
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	if rl.RetryAfter != "30" {
		t.Errorf("RetryAfter = %q, want 30", rl.RetryAfter)
	}
}

// TestListTagsUnavailable covers a registry without a usable tags API: a 404 and a malformed body both
// surface as ErrTagsUnavailable so a caller can distinguish "no usable tags" from a transient error.
func TestListTagsUnavailable(t *testing.T) {
	notFound := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()
	if _, err := NewClient(notFound.Client()).ListTags(context.Background(), hostOf(notFound)+"/u/app:1.0.0", controlplane.RegistryAuth{}); !errors.Is(err, ErrTagsUnavailable) {
		t.Errorf("404 err = %v, want ErrTagsUnavailable", err)
	}

	malformed := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `this is not json`)
	}))
	defer malformed.Close()
	if _, err := NewClient(malformed.Client()).ListTags(context.Background(), hostOf(malformed)+"/u/app:1.0.0", controlplane.RegistryAuth{}); !errors.Is(err, ErrTagsUnavailable) {
		t.Errorf("malformed body err = %v, want ErrTagsUnavailable", err)
	}
}

// TestListTagsServerError covers a plain non-2xx (a 500) as a clear, non-typed error.
func TestListTagsServerError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()
	if _, err := NewClient(srv.Client()).ListTags(context.Background(), hostOf(srv)+"/u/app:1.0.0", controlplane.RegistryAuth{}); err == nil {
		t.Error("want an error on http 500")
	}
}

func TestParseImageRef(t *testing.T) {
	cases := []struct {
		ref      string
		wantHost string
		wantRepo string
	}{
		{"ghcr.io/user/app:1.2.3", "ghcr.io", "user/app"},
		{"ghcr.io/user/app", "ghcr.io", "user/app"},
		{"ghcr.io/user/app@sha256:abc", "ghcr.io", "user/app"},
		{"nginx", "registry-1.docker.io", "library/nginx"},
		{"nginx:1.25", "registry-1.docker.io", "library/nginx"},
		{"user/app:1.0.0", "registry-1.docker.io", "user/app"},
		{"docker.io/user/app:1.0.0", "registry-1.docker.io", "user/app"},
		{"registry.example.com:5000/team/app:1.0.0", "registry.example.com:5000", "team/app"},
		{"localhost:5000/app:dev", "localhost:5000", "app"},
	}
	for _, tc := range cases {
		host, repo, err := parseImageRef(tc.ref)
		if err != nil {
			t.Errorf("parseImageRef(%q): %v", tc.ref, err)
			continue
		}
		if host != tc.wantHost || repo != tc.wantRepo {
			t.Errorf("parseImageRef(%q) = (%q,%q), want (%q,%q)", tc.ref, host, repo, tc.wantHost, tc.wantRepo)
		}
	}
}

// baseURL reconstructs the https base of the test server from a request, so a WWW-Authenticate realm
// can point back at the same server's /token endpoint.
func baseURL(r *http.Request) string {
	return "https://" + r.Host
}
