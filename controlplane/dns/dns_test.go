// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package dns

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	var code int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// VerifyAccess lists zones (works for account-scoped tokens too), not /user/tokens/verify.
		if r.URL.Path != "/zones" {
			t.Errorf("path = %q, want /zones", r.URL.Path)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"success":true,"result":[]}`))
	}))
	defer srv.Close()

	cf := &cloudflare{token: "cf_tok", baseURL: srv.URL, http: srv.Client()}
	ctx := context.Background()

	// A token that can list zones is accepted.
	code = http.StatusOK
	if err := cf.VerifyAccess(ctx); err != nil {
		t.Fatalf("valid token: %v", err)
	}

	// A rejected or under-permissioned token (401/403) is ErrInvalid.
	for _, c := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		code = c
		if err := cf.VerifyAccess(ctx); !errors.Is(err, cp.ErrInvalid) {
			t.Errorf("http %d err = %v, want ErrInvalid", c, err)
		}
	}

	// A vendor error (500) is not ErrInvalid.
	code = http.StatusInternalServerError
	if err := cf.VerifyAccess(ctx); err == nil || errors.Is(err, cp.ErrInvalid) {
		t.Errorf("500 err = %v, want a non-ErrInvalid error", err)
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

// --- record CRUD against stateful vendor mocks -------------------------------------------

func TestDigitalOceanEnsureAndDelete(t *testing.T) {
	type rec struct {
		ID   int    `json:"id"`
		Type string `json:"type"`
		Name string `json:"name"`
		Data string `json:"data"`
		TTL  int    `json:"ttl"`
	}
	store := map[int]*rec{}
	next := 100
	posts := 0

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/domains", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"domains":[{"name":"example.com"}]}`))
	})
	mux.HandleFunc("/v2/domains/example.com/records", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			name, typ := r.URL.Query().Get("name"), r.URL.Query().Get("type")
			out := []rec{}
			for _, rc := range store {
				fqdn := rc.Name + ".example.com"
				if rc.Name == "@" {
					fqdn = "example.com"
				}
				if name != "" && !strings.EqualFold(fqdn, name) {
					continue
				}
				if typ != "" && !strings.EqualFold(rc.Type, typ) {
					continue
				}
				out = append(out, *rc)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"domain_records": out})
		case http.MethodPost:
			posts++
			var in rec
			_ = json.NewDecoder(r.Body).Decode(&in)
			next++
			in.ID = next
			store[in.ID] = &in
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"domain_record": in})
		}
	})
	mux.HandleFunc("/v2/domains/example.com/records/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		switch r.Method {
		case http.MethodPut:
			var in rec
			_ = json.NewDecoder(r.Body).Decode(&in)
			if rc := store[id]; rc != nil {
				rc.Data = in.Data
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"domain_record": store[id]})
		case http.MethodDelete:
			delete(store, id)
			w.WriteHeader(http.StatusNoContent)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := &digitalOcean{token: "t", baseURL: srv.URL, http: srv.Client()}
	ctx := context.Background()
	want := cp.DNSRecord{Type: cp.RecordA, Name: "app.example.com", Value: "203.0.113.5"}

	// Create.
	if err := d.EnsureRecord(ctx, want); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(store) != 1 {
		t.Fatalf("after create store has %d records, want 1", len(store))
	}
	// Idempotent: same value makes no new record and no extra POST.
	if err := d.EnsureRecord(ctx, want); err != nil {
		t.Fatalf("idempotent: %v", err)
	}
	if posts != 1 || len(store) != 1 {
		t.Errorf("idempotent re-apply: posts=%d store=%d, want 1/1", posts, len(store))
	}
	// Update the value in place.
	if err := d.EnsureRecord(ctx, cp.DNSRecord{Type: cp.RecordA, Name: "app.example.com", Value: "203.0.113.9"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if posts != 1 || len(store) != 1 {
		t.Errorf("update should PUT not POST: posts=%d store=%d", posts, len(store))
	}

	// Delete, then delete again → ErrNotFound.
	if err := d.DeleteRecord(ctx, "app.example.com"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(store) != 0 {
		t.Errorf("store not empty after delete: %d", len(store))
	}
	if err := d.DeleteRecord(ctx, "app.example.com"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("delete missing err = %v, want ErrNotFound", err)
	}

	// A host no managed domain covers → ErrNotFound.
	if err := d.EnsureRecord(ctx, cp.DNSRecord{Type: cp.RecordA, Name: "app.elsewhere.com", Value: "203.0.113.5"}); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("unmanaged zone err = %v, want ErrNotFound", err)
	}
}

func TestCloudflareEnsureAndDelete(t *testing.T) {
	type rec struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Name    string `json:"name"`
		Content string `json:"content"`
		TTL     int    `json:"ttl"`
	}
	store := map[string]*rec{}
	next := 0
	posts := 0

	mux := http.NewServeMux()
	mux.HandleFunc("GET /zones", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"zone1","name":"example.com"}]}`))
	})
	mux.HandleFunc("/zones/zone1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			name, typ := r.URL.Query().Get("name"), r.URL.Query().Get("type")
			out := []rec{}
			for _, rc := range store {
				if name != "" && !strings.EqualFold(rc.Name, name) {
					continue
				}
				if typ != "" && !strings.EqualFold(rc.Type, typ) {
					continue
				}
				out = append(out, *rc)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": out})
		case http.MethodPost:
			posts++
			var in rec
			_ = json.NewDecoder(r.Body).Decode(&in)
			next++
			in.ID = "rec" + strconv.Itoa(next)
			store[in.ID] = &in
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": in})
		}
	})
	mux.HandleFunc("/zones/zone1/dns_records/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		switch r.Method {
		case http.MethodPut:
			var in rec
			_ = json.NewDecoder(r.Body).Decode(&in)
			if rc := store[id]; rc != nil {
				rc.Content = in.Content
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": store[id]})
		case http.MethodDelete:
			delete(store, id)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]string{"id": id}})
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &cloudflare{token: "t", baseURL: srv.URL, http: srv.Client()}
	ctx := context.Background()
	want := cp.DNSRecord{Type: cp.RecordA, Name: "app.example.com", Value: "203.0.113.5"}

	if err := c.EnsureRecord(ctx, want); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(store) != 1 {
		t.Fatalf("after create store has %d, want 1", len(store))
	}
	if err := c.EnsureRecord(ctx, want); err != nil {
		t.Fatalf("idempotent: %v", err)
	}
	if posts != 1 || len(store) != 1 {
		t.Errorf("idempotent: posts=%d store=%d, want 1/1", posts, len(store))
	}
	if err := c.EnsureRecord(ctx, cp.DNSRecord{Type: cp.RecordA, Name: "app.example.com", Value: "203.0.113.9"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if posts != 1 || len(store) != 1 {
		t.Errorf("update should PUT: posts=%d store=%d", posts, len(store))
	}

	if err := c.DeleteRecord(ctx, "app.example.com"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(store) != 0 {
		t.Errorf("store not empty after delete: %d", len(store))
	}
	if err := c.DeleteRecord(ctx, "app.example.com"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("delete missing err = %v, want ErrNotFound", err)
	}
	if err := c.EnsureRecord(ctx, cp.DNSRecord{Type: cp.RecordA, Name: "app.elsewhere.com", Value: "203.0.113.5"}); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("unmanaged zone err = %v, want ErrNotFound", err)
	}
}
