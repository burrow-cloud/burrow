// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package objectstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4, the authentication scheme every S3-compatible vendor speaks. It is
// implemented here, in about a hundred lines, rather than by taking an SDK: Burrow keeps its
// dependency graph small and justifies each entry (CLAUDE.md), and the alternative for FIVE API
// calls is a vendor SDK and its transitive module set. The DNS adapters make the same trade.
//
// The construction is fully specified by AWS and is deterministic, which is what makes it testable
// without a vendor: canonicalRequest and stringToSign are pure functions of the request, and the
// tests assert them against strings written from the specification rather than from this code.

const (
	algorithm    = "AWS4-HMAC-SHA256"
	service      = "s3"
	terminator   = "aws4_request"
	amzDateFmt   = "20060102T150405Z"
	amzShortDate = "20060102"
	// emptyPayloadHash is the SHA-256 of the empty string, the payload hash of a request with no
	// body (GET, HEAD, DELETE).
	emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// credentials is one signing credential pair. The secret is used to derive a signing key and is
// never logged, echoed, or placed in an error — the same rule the engine holds it to.
type credentials struct {
	accessKeyID     string
	secretAccessKey string
}

// sign adds the x-amz-date, x-amz-content-sha256 and Authorization headers that authenticate req
// to an S3-compatible endpoint. payloadHash is the hex SHA-256 of the request body.
func sign(req *http.Request, cred credentials, region string, payloadHash string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format(amzDateFmt)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	signed, canonical := canonicalRequest(req, payloadHash)
	scope := credentialScope(now, region)
	toSign := stringToSign(amzDate, scope, canonical)
	signature := hex.EncodeToString(hmacSHA256(signingKey(cred.secretAccessKey, now, region), toSign))

	req.Header.Set("Authorization", algorithm+
		" Credential="+cred.accessKeyID+"/"+scope+
		", SignedHeaders="+signed+
		", Signature="+signature)
}

// canonicalRequest builds the canonical request and the SignedHeaders list. Only the headers that
// must be signed are: host, and the x-amz-* headers, which is the minimum every S3 implementation
// accepts.
func canonicalRequest(req *http.Request, payloadHash string) (signedHeaders, canonical string) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") {
			headers[lower] = strings.TrimSpace(strings.Join(values, ","))
		}
	}
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	sort.Strings(names)

	var canonicalHeaders strings.Builder
	for _, n := range names {
		canonicalHeaders.WriteString(n)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(headers[n])
		canonicalHeaders.WriteString("\n")
	}
	signedHeaders = strings.Join(names, ";")

	return signedHeaders, strings.Join([]string{
		req.Method,
		canonicalPath(req.URL.EscapedPath()),
		canonicalQuery(req.URL.RawQuery),
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
}

// canonicalPath returns the request path for the canonical request. S3 (unlike every other AWS
// service) signs the path SINGLY encoded, so an already-escaped path is used as it is.
func canonicalPath(escaped string) string {
	if escaped == "" {
		return "/"
	}
	return escaped
}

// canonicalQuery sorts and re-encodes the query string as the canonical request requires: sorted by
// key, every key and value URI-encoded, and a valueless key rendered as "key=".
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	pairs := strings.Split(raw, "&")
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if p == "" {
			continue
		}
		key, value, _ := strings.Cut(p, "=")
		out = append(out, uriEncode(key, true)+"="+uriEncode(value, true))
	}
	sort.Strings(out)
	return strings.Join(out, "&")
}

// uriEncode is AWS's URI encoding: unreserved characters pass through, everything else becomes
// uppercase percent-encoded bytes. A slash is unreserved in a PATH and reserved everywhere else,
// which is what encodeSlash selects.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

func credentialScope(now time.Time, region string) string {
	return strings.Join([]string{now.UTC().Format(amzShortDate), region, service, terminator}, "/")
}

func stringToSign(amzDate, scope, canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return strings.Join([]string{algorithm, amzDate, scope, hex.EncodeToString(sum[:])}, "\n")
}

// signingKey derives the date/region/service-scoped key, so the long-lived secret never signs a
// request directly.
func signingKey(secret string, now time.Time, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), now.UTC().Format(amzShortDate))
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, terminator)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// hashPayload returns the hex SHA-256 of a request body.
func hashPayload(body []byte) string {
	if len(body) == 0 {
		return emptyPayloadHash
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
