// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package jointoken is the codec for the compact join token that carries admin access to a
// freshly bootstrapped single-VPS k3s cluster from `burrow bootstrap` (run once on the VPS) to
// `burrow join` (run on the laptop) — ADR-0044. The bootstrap has the k3s admin kubeconfig
// rewritten to the node's public IP; it encodes the parts of that kubeconfig into one paste-able
// string, and the laptop decodes it to record admin access locally and land the scoped agent
// credential (ADR-0038).
//
// The token is ADMIN-grade: it carries the cluster's admin credential (a client cert+key, or a
// bearer token), so it must be handled like a kubeconfig — transmitted over a private channel,
// not logged, and not committed. It is deliberately self-contained (no network round-trip to
// decode) and versioned so the format can evolve.
package jointoken

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// version is the current token schema version, carried both in the string prefix (so a decoder
// can reject a foreign or future token before base64-decoding) and inside the payload (so a
// tampered prefix cannot masquerade as a different version).
const version = "v1"

// prefix is the human-recognizable, one-token marker that precedes the base64url payload. A join
// token is therefore one line of the form "burrowjoin.v1.<base64url>".
const prefix = "burrowjoin." + version + "."

// Token is the decoded join token (ADR-0044). It carries everything `burrow join` needs to reach
// the freshly bootstrapped cluster as admin and to record it locally:
//   - Server + CertificateAuthorityData locate and authenticate the API server;
//   - exactly one admin credential form — a client cert+key (k3s's admin) or a bearer token;
//   - Namespace is the burrowd control-plane namespace on the cluster;
//   - ContextName is the kube context/cluster name to record admin access under locally.
//
// It is ADMIN-grade (see the package doc): treat a Token, and its encoded form, like a kubeconfig.
type Token struct {
	// Version is the payload schema version; it must equal the prefix's version ("v1" today).
	Version string `json:"version"`
	// Server is the API server URL, e.g. https://<public-ip>:6443.
	Server string `json:"server"`
	// CertificateAuthorityData is the cluster CA certificate (PEM bytes).
	CertificateAuthorityData []byte `json:"certificateAuthorityData"`
	// ClientCertificateData and ClientKeyData carry the admin client certificate and key (k3s's
	// admin credential). Both are set together, or both empty when BearerToken is used instead.
	ClientCertificateData []byte `json:"clientCertificateData,omitempty"`
	ClientKeyData         []byte `json:"clientKeyData,omitempty"`
	// BearerToken is an admin bearer token, the alternative to the client cert+key. Exactly one of
	// {client cert+key, bearer token} is present in a valid token.
	BearerToken string `json:"bearerToken,omitempty"`
	// Namespace is the burrowd control-plane namespace on the bootstrapped cluster.
	Namespace string `json:"namespace"`
	// ContextName is the kube context (and cluster) name to record admin access under locally, and
	// the name the environment handle points at.
	ContextName string `json:"contextName"`
}

// Encode validates the token, JSON-marshals it, and returns the one-line, paste-able string
// "burrowjoin.v1.<base64url>" (URL-safe base64, no padding). It defaults Version to the current
// schema version. The returned string is ADMIN-grade; see the package doc.
func Encode(t Token) (string, error) {
	if t.Version == "" {
		t.Version = version
	}
	if err := t.validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("jointoken: marshaling token: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(data), nil
}

// Decode parses a one-line join token produced by Encode. It rejects a missing or wrong version
// prefix, malformed base64, malformed JSON, a payload version that disagrees with the prefix, and
// a token that fails validation. Surrounding whitespace is tolerated.
func Decode(s string) (Token, error) {
	s = strings.TrimSpace(s)
	payload, ok := strings.CutPrefix(s, prefix)
	if !ok {
		return Token{}, fmt.Errorf("jointoken: not a %s join token (expected the %q prefix)", version, prefix)
	}
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Token{}, fmt.Errorf("jointoken: decoding base64url payload: %w", err)
	}
	var t Token
	if err := json.Unmarshal(data, &t); err != nil {
		return Token{}, fmt.Errorf("jointoken: parsing token payload: %w", err)
	}
	if t.Version != version {
		return Token{}, fmt.Errorf("jointoken: unsupported token version %q (want %q)", t.Version, version)
	}
	if err := t.validate(); err != nil {
		return Token{}, err
	}
	return t, nil
}

// validate checks a token carries a complete, self-consistent admin credential and cluster
// coordinates. Exactly one credential form must be present: a client cert+key (both halves) or a
// bearer token, never both and never neither.
func (t Token) validate() error {
	if t.Server == "" {
		return fmt.Errorf("jointoken: token has no server URL")
	}
	if len(t.CertificateAuthorityData) == 0 {
		return fmt.Errorf("jointoken: token has no cluster CA certificate")
	}
	if t.Namespace == "" {
		return fmt.Errorf("jointoken: token has no control-plane namespace")
	}
	if t.ContextName == "" {
		return fmt.Errorf("jointoken: token has no context name")
	}
	hasCert := len(t.ClientCertificateData) > 0 || len(t.ClientKeyData) > 0
	hasBearer := t.BearerToken != ""
	switch {
	case hasCert && hasBearer:
		return fmt.Errorf("jointoken: token carries both a client certificate and a bearer token; exactly one admin credential is expected")
	case !hasCert && !hasBearer:
		return fmt.Errorf("jointoken: token carries no admin credential (neither a client certificate+key nor a bearer token)")
	case hasCert && (len(t.ClientCertificateData) == 0 || len(t.ClientKeyData) == 0):
		return fmt.Errorf("jointoken: token's client credential is incomplete (needs both the certificate and the key)")
	}
	return nil
}
