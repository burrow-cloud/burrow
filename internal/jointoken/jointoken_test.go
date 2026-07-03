// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package jointoken

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

// certToken is a valid cert+key-credential token.
func certToken() Token {
	return Token{
		Server:                   "https://203.0.113.10:6443",
		CertificateAuthorityData: []byte("ca-pem"),
		ClientCertificateData:    []byte("client-cert-pem"),
		ClientKeyData:            []byte("client-key-pem"),
		Namespace:                "burrow",
		ContextName:              "burrow-vps",
	}
}

// bearerToken is a valid bearer-credential token.
func bearerToken() Token {
	return Token{
		Server:                   "https://203.0.113.10:6443",
		CertificateAuthorityData: []byte("ca-pem"),
		BearerToken:              "admin-bearer-token",
		Namespace:                "burrow",
		ContextName:              "burrow-vps",
	}
}

// TestEncodeDecodeRoundTripCert round-trips the client cert+key variant and confirms the encoded
// form is a single "burrowjoin.v1." line.
func TestEncodeDecodeRoundTripCert(t *testing.T) {
	in := certToken()
	s, err := Encode(in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.HasPrefix(s, "burrowjoin.v1.") {
		t.Errorf("encoded token missing the burrowjoin.v1. prefix: %q", s)
	}
	if strings.ContainsAny(s, " \n\t") {
		t.Errorf("encoded token must be one line with no whitespace: %q", s)
	}
	out, err := Decode(s)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	// Encode defaults Version; set it on the input so the round-trip compares equal.
	in.Version = version
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip mismatch:\n in = %+v\nout = %+v", in, out)
	}
}

// TestEncodeDecodeRoundTripBearer round-trips the bearer-token variant.
func TestEncodeDecodeRoundTripBearer(t *testing.T) {
	in := bearerToken()
	s, err := Encode(in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := Decode(s)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	in.Version = version
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip mismatch:\n in = %+v\nout = %+v", in, out)
	}
}

// TestDecodeSurroundingWhitespace tolerates a pasted token with leading/trailing whitespace.
func TestDecodeSurroundingWhitespace(t *testing.T) {
	s, err := Encode(certToken())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := Decode("  \n" + s + "\n "); err != nil {
		t.Errorf("Decode should tolerate surrounding whitespace: %v", err)
	}
}

// TestDecodeRejectsMalformed asserts Decode rejects bad prefixes, bad base64, bad JSON, and a
// version mismatch, each without panicking.
func TestDecodeRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":           "",
		"no prefix":       "not-a-token",
		"wrong brand":     "hamsterjoin.v1.YWJj",
		"wrong version":   "burrowjoin.v2.YWJj",
		"bad base64":      "burrowjoin.v1.!!!not-base64!!!",
		"bad json":        "burrowjoin.v1." + rawBase64("{not json"),
		"empty payload":   "burrowjoin.v1.",
		"version in body": "burrowjoin.v1." + rawBase64(`{"version":"v9","server":"https://x:6443","certificateAuthorityData":"Y2E=","bearerToken":"t","namespace":"burrow","contextName":"c"}`),
	}
	for name, in := range cases {
		if _, err := Decode(in); err == nil {
			t.Errorf("%s: Decode(%q) should have failed", name, in)
		}
	}
}

// TestEncodeRejectsInvalid asserts Encode validates: missing coordinates, and both/neither
// credential forms.
func TestEncodeRejectsInvalid(t *testing.T) {
	valid := certToken()

	noServer := valid
	noServer.Server = ""

	noCA := valid
	noCA.CertificateAuthorityData = nil

	noCred := valid
	noCred.ClientCertificateData = nil
	noCred.ClientKeyData = nil

	bothCreds := valid
	bothCreds.BearerToken = "also-a-bearer"

	halfCert := valid
	halfCert.ClientKeyData = nil

	for name, tok := range map[string]Token{
		"no server":        noServer,
		"no CA":            noCA,
		"no credential":    noCred,
		"both credentials": bothCreds,
		"incomplete cert":  halfCert,
	} {
		if _, err := Encode(tok); err == nil {
			t.Errorf("%s: Encode should have failed validation", name)
		}
	}
}

// rawBase64 is base64url-no-padding of s, for constructing malformed-payload fixtures.
func rawBase64(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
