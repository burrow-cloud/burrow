// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package objectstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

const (
	testKeyID  = "AKIDEXAMPLE"
	testSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
)

var testTime = time.Date(2026, 7, 25, 12, 30, 0, 0, time.UTC)

// newTestStore points a real Store at an httptest server, so the adapter is exercised over actual
// HTTP with actual signing rather than through a stub of itself.
func newTestStore(t *testing.T, h http.HandlerFunc) (*Store, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	f := &Factory{http: srv.Client(), now: func() time.Time { return testTime }}
	s, err := f.ObjectStore(srv.URL, "us-west-002", controlplane.ObjectStoreCredential{
		AccessKeyID: testKeyID, SecretAccessKey: testSecret,
	})
	if err != nil {
		t.Fatalf("ObjectStore: %v", err)
	}
	return s.(*Store), srv
}

// TestRequestsAreSignedAndCarryNoSecret asserts the two properties every request must have: it is
// authenticated with SigV4 over the headers the scheme requires, and the SECRET half of the
// credential never appears on the wire in the clear — it derives the signature and nothing else.
func TestRequestsAreSignedAndCarryNoSecret(t *testing.T) {
	var got *http.Request
	store, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	if err := store.PutObject(context.Background(), "bucket", ".burrow/probe-1", []byte("probe")); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	auth := got.Header.Get("Authorization")
	for _, want := range []string{
		"AWS4-HMAC-SHA256",
		"Credential=" + testKeyID + "/20260725/us-west-002/s3/aws4_request",
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date",
		"Signature=",
	} {
		if !strings.Contains(auth, want) {
			t.Errorf("Authorization header missing %q:\n%s", want, auth)
		}
	}
	if got.Header.Get("X-Amz-Date") != "20260725T123000Z" {
		t.Errorf("x-amz-date = %q, want the injected time", got.Header.Get("X-Amz-Date"))
	}
	if got.Header.Get("X-Amz-Content-Sha256") == "" {
		t.Error("x-amz-content-sha256 is unset, so the payload is unsigned")
	}
	for name, values := range got.Header {
		for _, v := range values {
			if strings.Contains(v, testSecret) {
				t.Fatalf("header %s carries the secret access key", name)
			}
		}
	}
	if strings.Contains(got.URL.String(), testSecret) {
		t.Fatal("the URL carries the secret access key")
	}
	// Path-style addressing: the bucket is the first path segment, and the key is escaped.
	if got.URL.EscapedPath() != "/bucket/.burrow/probe-1" {
		t.Errorf("path = %q, want path-style /bucket/<key>", got.URL.EscapedPath())
	}
}

// TestCanonicalRequestFollowsTheSpecification pins the construction against a canonical request
// written from AWS's specification rather than from this implementation, so a change to the signer
// that still self-consistently signs something is caught.
func TestCanonicalRequestFollowsTheSpecification(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://s3.example.com/my-bucket?lifecycle=", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Amz-Date", "20260725T123000Z")
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadHash)

	signed, canonical := canonicalRequest(req, emptyPayloadHash)
	want := strings.Join([]string{
		"GET",
		"/my-bucket",
		"lifecycle=",
		"host:s3.example.com",
		"x-amz-content-sha256:" + emptyPayloadHash,
		"x-amz-date:20260725T123000Z",
		"",
		"host;x-amz-content-sha256;x-amz-date",
		emptyPayloadHash,
	}, "\n")
	if canonical != want {
		t.Errorf("canonical request:\n%q\nwant:\n%q", canonical, want)
	}
	if signed != "host;x-amz-content-sha256;x-amz-date" {
		t.Errorf("signed headers = %q", signed)
	}

	toSign := stringToSign("20260725T123000Z", credentialScope(testTime, "us-east-1"), canonical)
	lines := strings.Split(toSign, "\n")
	if len(lines) != 4 || lines[0] != "AWS4-HMAC-SHA256" || lines[1] != "20260725T123000Z" ||
		lines[2] != "20260725/us-east-1/s3/aws4_request" || len(lines[3]) != 64 {
		t.Errorf("string to sign is not the four specified lines:\n%q", toSign)
	}
}

func TestURIEncodeIsAWSFlavoured(t *testing.T) {
	cases := map[string]string{
		"abc-._~":   "abc-._~",
		"a b":       "a%20b",
		"a/b":       "a%2Fb",
		"café":      "caf%C3%A9",
		"2026-07=1": "2026-07%3D1",
	}
	for in, want := range cases {
		if got := uriEncode(in, true); got != want {
			t.Errorf("uriEncode(%q) = %q, want %q", in, got, want)
		}
	}
	if got := uriEncode("a/b", false); got != "a/b" {
		t.Errorf("uriEncode(%q, false) = %q, want the slash preserved", "a/b", got)
	}
}

// TestLifecycleRulesParsesBothSchemas: rules carry a prefix directly (the original schema) or inside
// a Filter (the current one), and S3-compatible vendors still serve both.
func TestLifecycleRulesParsesBothSchemas(t *testing.T) {
	const body = `<?xml version="1.0" encoding="UTF-8"?>
<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Rule><ID>expire-30d</ID><Prefix>backups/</Prefix><Status>Enabled</Status>
    <Expiration><Days>30</Days></Expiration></Rule>
  <Rule><ID>filtered</ID><Filter><Prefix>logs/</Prefix></Filter><Status>Disabled</Status>
    <Expiration><Days>7</Days></Expiration></Rule>
  <Rule><ID>abort-mpu</ID><Filter></Filter><Status>Enabled</Status></Rule>
</LifecycleConfiguration>`
	store, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "lifecycle=" {
			t.Errorf("lifecycle read used query %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(body))
	})

	rules, err := store.LifecycleRules(context.Background(), "bucket")
	if err != nil {
		t.Fatalf("LifecycleRules: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3: %+v", len(rules), rules)
	}
	if rules[0] != (controlplane.LifecycleRule{ID: "expire-30d", Prefix: "backups/", Enabled: true, ExpireAfterDays: 30}) {
		t.Errorf("rule 0 = %+v", rules[0])
	}
	if rules[1].Prefix != "logs/" || rules[1].Enabled {
		t.Errorf("rule 1 = %+v, want the Filter prefix and disabled", rules[1])
	}
	if rules[2].ExpireAfterDays != 0 || !rules[2].Enabled {
		t.Errorf("rule 2 = %+v, want an enabled rule that expires nothing by age", rules[2])
	}
}

// TestLifecycleUnreadableIsDistinctFromEmpty is the adapter's half of ADR-0063 §3's honesty clause.
// "No rules" and "I am not allowed to look" must not arrive as the same answer, because the engine
// reports the first as verified and the second as UNKNOWN.
func TestLifecycleUnreadableIsDistinctFromEmpty(t *testing.T) {
	t.Run("no configuration is an empty answer", func(t *testing.T) {
		store, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchLifecycleConfiguration</Code></Error>`))
		})
		rules, err := store.LifecycleRules(context.Background(), "bucket")
		if err != nil || len(rules) != 0 {
			t.Fatalf("rules = %v, err = %v; want an empty, definitive answer", rules, err)
		}
	})

	for name, status := range map[string]int{
		"forbidden":       http.StatusForbidden,
		"not implemented": http.StatusNotImplemented,
		"not allowed":     http.StatusMethodNotAllowed,
	} {
		t.Run(name+" is unknown", func(t *testing.T) {
			store, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			})
			_, err := store.LifecycleRules(context.Background(), "bucket")
			if !errors.Is(err, controlplane.ErrLifecycleUnknown) {
				t.Fatalf("err = %v, want ErrLifecycleUnknown so the engine reports the invariant as "+
					"unknown rather than verified", err)
			}
		})
	}
}

// TestBucketExistsTreatsForbiddenAsAbsent: a bucket that answers 403 may exist and belong to
// somebody else. Burrow can do nothing with it either way, and treating it as present is the more
// dangerous answer.
func TestBucketExistsTreatsForbiddenAsAbsent(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusOK:        true,
		http.StatusNotFound:  false,
		http.StatusForbidden: false,
	} {
		store, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodHead {
				t.Errorf("BucketExists used %s, want HEAD", r.Method)
			}
			w.WriteHeader(status)
		})
		got, err := store.BucketExists(context.Background(), "bucket")
		if err != nil {
			t.Fatalf("BucketExists (http %d): %v", status, err)
		}
		if got != want {
			t.Errorf("BucketExists (http %d) = %v, want %v", status, got, want)
		}
	}
}

// TestCreateBucketUsesTheGivenNameAndRegion: the engine owns the name (ADR-0063 §4), so the adapter
// creates exactly what it is handed and never derives one.
func TestCreateBucketUsesTheGivenNameAndRegion(t *testing.T) {
	var path, body string
	store, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		body = string(b)
		w.WriteHeader(http.StatusOK)
	})
	if err := store.CreateBucket(context.Background(), "burrow-backups-9f2c1ab3"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if path != "/burrow-backups-9f2c1ab3" {
		t.Errorf("path = %q, want the given bucket name", path)
	}
	if !strings.Contains(body, "<LocationConstraint>us-west-002</LocationConstraint>") {
		t.Errorf("create body = %q, want the region as a location constraint", body)
	}
}

// TestDeleteObjectIsIdempotent: the probe's cleanup must not fail because the object is already
// gone.
func TestDeleteObjectIsIdempotent(t *testing.T) {
	store, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if err := store.DeleteObject(context.Background(), "bucket", "gone"); err != nil {
		t.Fatalf("DeleteObject on an absent object: %v", err)
	}
}

// TestVendorErrorsCarryTheStatusAndNoCredential: a failure has to be diagnosable, and a diagnosable
// failure must still not leak the credential.
func TestVendorErrorsCarryTheStatusAndNoCredential(t *testing.T) {
	store, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>SignatureDoesNotMatch</Code></Error>`))
	})
	err := store.PutObject(context.Background(), "bucket", "key", []byte("x"))
	if err == nil {
		t.Fatal("a 403 write reported success")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("error is not diagnosable: %v", err)
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Error("the error leaked the secret access key")
	}
}

func TestFactoryRejectsMalformedConfiguration(t *testing.T) {
	f := NewFactory()
	cred := controlplane.ObjectStoreCredential{AccessKeyID: "k", SecretAccessKey: "s"}
	for name, call := range map[string]func() error{
		"no endpoint": func() error { _, err := f.ObjectStore("", "r", cred); return err },
		"not a URL":   func() error { _, err := f.ObjectStore("s3.example.com", "r", cred); return err },
		"half a pair": func() error {
			_, err := f.ObjectStore("https://s3.example.com", "r", controlplane.ObjectStoreCredential{AccessKeyID: "k"})
			return err
		},
		"no credential": func() error {
			_, err := f.ObjectStore("https://s3.example.com", "r", controlplane.ObjectStoreCredential{})
			return err
		},
		"other half only": func() error {
			_, err := f.ObjectStore("https://s3.example.com", "r", controlplane.ObjectStoreCredential{SecretAccessKey: "s"})
			return err
		},
	} {
		if err := call(); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}
