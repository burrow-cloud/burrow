// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderAddWithoutTypeListsSupportedTypes(t *testing.T) {
	var out, errb bytes.Buffer
	// Missing <type>: the error and usage must name the available types so the user isn't left
	// guessing what to pass.
	_ = run(context.Background(), []string{"config", "provider", "add"}, &out, &errb)
	s := errb.String()
	for _, want := range []string{"needs <type>", "cloudflare", "digitalocean"} {
		if !strings.Contains(s, want) {
			t.Errorf("provider add (no type) output missing %q:\n%s", want, s)
		}
	}
}

// TestProviderAddSendsTokenInBody asserts `provider add` issues the control-plane API call with the
// token VALUE in the POST body — not a kubeconfig-direct Secret write, and not in the path or query
// (ADR-0030). The token is piped in (a script path), so the test drives the real RunE.
func TestProviderAddSendsTokenInBody(t *testing.T) {
	var gotPath, gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		// Respond with a recorded provider (no token echoed).
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "digitalocean", "type": "digitalocean",
			"capabilities": []string{"dns"}, "secret_key": "digitalocean",
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	cmd := newRootCmd()
	cmd.SetIn(strings.NewReader("dop_v1_secret\n")) // piped token (non-terminal)
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{
		"config", "provider", "add", "digitalocean",
		"--control-plane", srv.URL, "--token", "api-tok",
	})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("provider add: %v (stderr: %s)", err, errb.String())
	}

	if gotPath != "/v1/providers" {
		t.Errorf("path = %q, want /v1/providers", gotPath)
	}
	// The token must never appear in the path or query — only the body.
	if strings.Contains(gotPath, "dop_v1_secret") || strings.Contains(gotQuery, "dop_v1_secret") {
		t.Errorf("token leaked into the request path/query: path=%q query=%q", gotPath, gotQuery)
	}
	if !strings.Contains(gotBody, `"token":"dop_v1_secret"`) {
		t.Errorf("request body missing the token: %s", gotBody)
	}
	// The human output names the key, never the token value.
	if strings.Contains(out.String(), "dop_v1_secret") {
		t.Errorf("CLI output leaked the token value:\n%s", out.String())
	}
}

func TestReadTokenFromPipe(t *testing.T) {
	// A non-terminal reader (a pipe/redirect, as in a script) is read directly and trimmed.
	got, err := readToken(strings.NewReader("  dop_v1_abc\n"), io.Discard, "token: ")
	if err != nil {
		t.Fatalf("readToken: %v", err)
	}
	if got != "dop_v1_abc" {
		t.Errorf("readToken = %q, want the trimmed token", got)
	}
}

func TestProviderTypesCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"config", "provider", "types"}, &out, &errb); err != nil {
		t.Fatalf("provider types: %v", err)
	}
	s := out.String()
	for _, want := range []string{"TYPE", "SUPPORTS", "cloudflare", "digitalocean", "dns"} {
		if !strings.Contains(s, want) {
			t.Errorf("provider types output missing %q:\n%s", want, s)
		}
	}
}

// TestProviderAddS3SendsPairAndReportsVerification covers the operator side of ADR-0063: the
// credential is a PAIR, its secret half is read from stdin (never a flag, so it stays out of shell
// history and the process table), and what the control plane observed is reported back — including,
// and especially, a lifecycle check it could not perform.
func TestProviderAddS3SendsPairAndReportsVerification(t *testing.T) {
	var gotBody, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "s3", "type": "s3", "capabilities": []string{"object-storage"},
			"secret_key": "s3.access-key-id",
			"object_store": map[string]any{
				"endpoint":              "https://s3.example.com",
				"region":                "us-west-002",
				"bucket":                "burrow-backups-9f2c1ab3",
				"created":               true,
				"access_key_id_key":     "s3.access-key-id",
				"secret_access_key_key": "s3.secret-access-key",
				"retention_days":        30,
			},
			"verification": map[string]any{
				"bucket": "burrow-backups-9f2c1ab3", "bucket_created": true, "probe_object": true,
				"lifecycle": map[string]any{
					"status": "unknown",
					"detail": "Burrow could not read the lifecycle configuration of bucket burrow-backups-9f2c1ab3",
				},
			},
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	cmd := newRootCmd()
	cmd.SetIn(strings.NewReader("s3-secret-access-key\n")) // piped secret half
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{
		"config", "provider", "add", "s3",
		"--endpoint", "https://s3.example.com", "--region", "us-west-002",
		"--access-key-id", "AKIAEXAMPLE", "--create-bucket", "--retention-days", "30", "--confirm",
		"--control-plane", srv.URL, "--token", "api-tok",
	})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("provider add s3: %v (stderr: %s)", err, errb.String())
	}

	if gotPath != "/v1/providers" {
		t.Errorf("path = %q, want /v1/providers", gotPath)
	}
	if strings.Contains(gotPath+gotQuery, "s3-secret-access-key") {
		t.Error("the secret access key leaked into the request path or query")
	}
	for _, want := range []string{
		`"access_key_id":"AKIAEXAMPLE"`,
		`"secret_access_key":"s3-secret-access-key"`,
		`"endpoint":"https://s3.example.com"`,
		`"create_bucket":true`,
		`"retention_days":30`,
		`"confirm":true`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body missing %s:\n%s", want, gotBody)
		}
	}

	human := out.String()
	if strings.Contains(human, "s3-secret-access-key") {
		t.Errorf("the CLI output leaked the secret access key:\n%s", human)
	}
	for _, want := range []string{
		"burrow-backups-9f2c1ab3",  // the recorded bucket — the only one Burrow writes to
		"s3.access-key-id",         // the key NAMES, never the values
		"s3.secret-access-key",     //
		"probe object was written", // the destination was proven, not assumed
		"UNKNOWN",                  // an unverifiable invariant is never reported as verified
		"most consequential key",   // the credential-scoping advice ADR-0063 asks for
	} {
		if !strings.Contains(human, want) {
			t.Errorf("provider add output missing %q:\n%s", want, human)
		}
	}
}

// TestProviderAddS3RequiresAccessKeyID: the pair's identifier half is a flag and the secret half is
// stdin, so a missing identifier must fail before anything is read or sent.
func TestProviderAddS3RequiresAccessKeyID(t *testing.T) {
	var out, errb bytes.Buffer
	cmd := newRootCmd()
	cmd.SetIn(strings.NewReader("secret\n"))
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{"config", "provider", "add", "s3", "--endpoint", "https://s3.example.com"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("provider add s3 succeeded without --access-key-id")
	}
	if !strings.Contains(err.Error(), "access-key-id") {
		t.Errorf("error does not name the missing flag: %v", err)
	}
}

// TestProviderTypesListsObjectStorage: the type is discoverable before it is configured.
func TestProviderTypesListsObjectStorage(t *testing.T) {
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"config", "provider", "types"}, &out, &errb); err != nil {
		t.Fatalf("provider types: %v", err)
	}
	if !strings.Contains(out.String(), "s3") || !strings.Contains(out.String(), "object-storage") {
		t.Errorf("provider types does not list the object-storage type:\n%s", out.String())
	}
}
