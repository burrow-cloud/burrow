// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package objectstore is the production controlplane.ObjectStoreFactory: a small S3-compatible
// client for the ONE thing Burrow does with object storage, which is own the seam between a bucket
// and the backups written to it (ADR-0063).
//
// Its surface is the whole of what ADR-0063 §2 permits and no more — create the bucket Burrow will
// own, write and delete a probe object, read the lifecycle configuration that must be reconciled
// against backup retention, and check a bucket is reachable. There is no cp, no sync, no listing of
// arbitrary prefixes, no presigned URL, and no policy/IAM/replication surface. That is not an
// omission to be filled in later: every vendor ships a capable CLI for those, and a capability
// enters here only when a Burrow feature requires it.
//
// The client is a net/http client with an in-tree SigV4 signer (sigv4.go) rather than a vendor SDK,
// for the same reason the DNS adapters are: five API calls do not justify an SDK and its transitive
// module set (CLAUDE.md). Requests are PATH-STYLE (https://endpoint/bucket/key), which is what the
// S3-compatible vendors accept in common.
//
// It lives under controlplane/ (not controlplane/internal) so cmd/burrowd and the managed module
// can wire it; it is licensed Apache-2.0.
package objectstore

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

var (
	_ controlplane.ObjectStoreFactory = (*Factory)(nil)
	_ controlplane.ObjectStore        = (*Store)(nil)
)

// Factory builds a Store for an endpoint and credential pair (ADR-0063 §1). The vendor is whoever
// answers the endpoint: there is no vendor list here, deliberately.
type Factory struct {
	http *http.Client
	now  func() time.Time
}

// NewFactory returns a Factory with a sensible HTTP timeout.
func NewFactory() *Factory {
	return &Factory{http: &http.Client{Timeout: 30 * time.Second}, now: time.Now}
}

// ObjectStore returns a client for the bucket-holding endpoint, signing with cred.
func (f *Factory) ObjectStore(endpoint, region string, cred controlplane.ObjectStoreCredential) (controlplane.ObjectStore, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	switch {
	case endpoint == "":
		return nil, fmt.Errorf("objectstore: an endpoint is required: %w", controlplane.ErrInvalid)
	case !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://"):
		return nil, fmt.Errorf("objectstore: endpoint %q must be a URL: %w", endpoint, controlplane.ErrInvalid)
	case cred.AccessKeyID == "" || cred.SecretAccessKey == "":
		return nil, fmt.Errorf("objectstore: a credential pair is required: %w", controlplane.ErrInvalid)
	}
	if region == "" {
		// Vendors with no meaningful region still require one in the signature; us-east-1 is the
		// value they accept in common.
		region = "us-east-1"
	}
	return &Store{
		endpoint: endpoint,
		region:   region,
		cred:     credentials{accessKeyID: cred.AccessKeyID, secretAccessKey: cred.SecretAccessKey},
		http:     f.http,
		now:      f.now,
	}, nil
}

// Store is one endpoint's S3-compatible client, holding one credential pair.
type Store struct {
	endpoint string
	region   string
	cred     credentials
	http     *http.Client
	now      func() time.Time
}

// BucketExists reports whether the bucket is present AND reachable with this credential. A bucket
// that answers 403 is reported as absent: it may exist and belong to someone else, and either way
// Burrow can do nothing with it, so treating it as present would be the more dangerous answer.
func (s *Store) BucketExists(ctx context.Context, bucket string) (bool, error) {
	status, body, err := s.do(ctx, http.MethodHead, "/"+bucket, "", nil)
	if err != nil {
		return false, err
	}
	switch {
	case status >= 200 && status < 300:
		return true, nil
	case status == http.StatusNotFound || status == http.StatusForbidden:
		return false, nil
	default:
		return false, fmt.Errorf("objectstore: HEAD bucket %s: unexpected response (http %d): %s", bucket, status, snippet(body))
	}
}

// CreateBucket creates the bucket under exactly the name it is given — the engine owns the name
// (ADR-0063 §4), and this adapter never derives, adopts, or falls back to another one.
func (s *Store) CreateBucket(ctx context.Context, bucket string) error {
	var body []byte
	if s.region != "us-east-1" {
		// us-east-1 must send NO location constraint; every other region must send one.
		body = []byte(`<CreateBucketConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
			`<LocationConstraint>` + xmlEscape(s.region) + `</LocationConstraint></CreateBucketConfiguration>`)
	}
	status, respBody, err := s.do(ctx, http.MethodPut, "/"+bucket, "", body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("objectstore: creating bucket %s (http %d): %s", bucket, status, snippet(respBody))
	}
	return nil
}

// PutObject writes body at key.
func (s *Store) PutObject(ctx context.Context, bucket, key string, body []byte) error {
	status, respBody, err := s.do(ctx, http.MethodPut, objectPath(bucket, key), "", body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("objectstore: writing %s to bucket %s (http %d): %s", key, bucket, status, snippet(respBody))
	}
	return nil
}

// DeleteObject removes key. S3 answers 204 whether or not the object was there, so deleting an
// absent object is a no-op rather than an error.
func (s *Store) DeleteObject(ctx context.Context, bucket, key string) error {
	status, respBody, err := s.do(ctx, http.MethodDelete, objectPath(bucket, key), "", nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("objectstore: deleting %s from bucket %s (http %d): %s", key, bucket, status, snippet(respBody))
	}
	return nil
}

// lifecycleConfiguration is the subset of the GetBucketLifecycleConfiguration response Burrow
// reconciles against retention. Rules carry a prefix either directly (the original schema) or
// inside a Filter (the current one), and both are still served by S3-compatible vendors.
type lifecycleConfiguration struct {
	Rules []struct {
		ID     string `xml:"ID"`
		Prefix string `xml:"Prefix"`
		Status string `xml:"Status"`
		Filter struct {
			Prefix string `xml:"Prefix"`
			And    struct {
				Prefix string `xml:"Prefix"`
			} `xml:"And"`
		} `xml:"Filter"`
		Expiration struct {
			Days int `xml:"Days"`
		} `xml:"Expiration"`
	} `xml:"Rule"`
}

// LifecycleRules returns the bucket's lifecycle rules, or an error wrapping ErrLifecycleUnknown
// where the answer cannot be had — the vendor does not serve the lifecycle API (501/405), or this
// credential is not permitted to read it (403). That distinction is load-bearing: ADR-0063 §3 wants
// an unreadable configuration reported as UNKNOWN, never as verified, and an adapter that quietly
// returned "no rules" for a 403 would report the second as the first.
func (s *Store) LifecycleRules(ctx context.Context, bucket string) ([]controlplane.LifecycleRule, error) {
	status, body, err := s.do(ctx, http.MethodGet, "/"+bucket, "lifecycle=", nil)
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusNotFound:
		// NoSuchLifecycleConfiguration: a definitive answer that there are no rules.
		if strings.Contains(string(body), "NoSuchLifecycleConfiguration") || len(body) == 0 {
			return []controlplane.LifecycleRule{}, nil
		}
		return nil, fmt.Errorf("bucket %s: %s: %w", bucket, snippet(body), controlplane.ErrLifecycleUnknown)
	case status == http.StatusForbidden:
		return nil, fmt.Errorf("this credential is not permitted to read bucket %s's lifecycle configuration (http 403): %w",
			bucket, controlplane.ErrLifecycleUnknown)
	case status == http.StatusNotImplemented || status == http.StatusMethodNotAllowed:
		return nil, fmt.Errorf("this endpoint does not serve the bucket lifecycle API (http %d): %w",
			status, controlplane.ErrLifecycleUnknown)
	case status < 200 || status >= 300:
		return nil, fmt.Errorf("objectstore: reading bucket %s lifecycle configuration (http %d): %s", bucket, status, snippet(body))
	}

	var cfg lifecycleConfiguration
	if err := xml.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("bucket %s: the lifecycle configuration did not parse (%v): %w", bucket, err, controlplane.ErrLifecycleUnknown)
	}
	out := make([]controlplane.LifecycleRule, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		prefix := r.Prefix
		if prefix == "" {
			prefix = r.Filter.Prefix
		}
		if prefix == "" {
			prefix = r.Filter.And.Prefix
		}
		out = append(out, controlplane.LifecycleRule{
			ID:              r.ID,
			Prefix:          prefix,
			Enabled:         strings.EqualFold(r.Status, "Enabled"),
			ExpireAfterDays: r.Expiration.Days,
		})
	}
	return out, nil
}

// do signs and issues one request, returning the status and the body. The credential is used to
// sign and never appears in a returned error.
func (s *Store) do(ctx context.Context, method, path, rawQuery string, body []byte) (int, []byte, error) {
	url := s.endpoint + path
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("objectstore: building the request: %w", err)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	sign(req, s.cred, s.region, hashPayload(body), s.now())

	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("objectstore: %s %s: %w", method, redactedURL(url), err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("objectstore: reading the response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}

// objectPath builds the path-style path for an object, encoding each key segment.
func objectPath(bucket, key string) string {
	segments := strings.Split(strings.TrimPrefix(key, "/"), "/")
	for i, seg := range segments {
		segments[i] = uriEncode(seg, true)
	}
	return "/" + bucket + "/" + strings.Join(segments, "/")
}

// redactedURL is the URL as it may appear in an error. Burrow never puts a credential in a query
// string (it signs with headers), so the URL is safe as it is; the helper exists so that stays a
// deliberate decision rather than an assumption.
func redactedURL(url string) string {
	if i := strings.Index(url, "?"); i >= 0 {
		return url[:i]
	}
	return url
}

// snippet trims a vendor's error body to something loggable. It never contains a credential:
// Burrow signs with headers, and a vendor's error body echoes the request's identifiers, not its
// secret.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func xmlEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return ""
	}
	return b.String()
}
