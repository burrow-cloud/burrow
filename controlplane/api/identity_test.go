// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/api"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// The HTTP tests for signing in (ADR-0084 §1, §2): the one route that mints a credential, and what
// burrowd does with the token afterwards.

// newIdentityAPI builds the handler with a token source wired, so the claim route can actually
// issue. It returns the database so a test can inspect what was stored, and the install id it was
// configured with rides back in the claim response.
func newIdentityAPI(t *testing.T, installID string) (http.Handler, *fake.Database) {
	t.Helper()
	d := fake.NewDatabase()
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: d,
		Clock:       fake.NewClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)),
		IDs:         fake.NewIDs(),
		Resolver:    fake.NewResolver(),
		Credentials: fake.NewCredentials(),
		DNS:         fake.NewDNSFactory(),
		TokenSource: fake.NewTokens(),
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	h, err := api.New(api.Config{Engine: e, Token: token, InstallID: installID})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h, d
}

// claimBody is the decoded claim response. It mirrors the wire shape rather than importing it,
// because the wire shape is the contract every client depends on and a test that shared the struct
// would not notice a field being renamed.
type claimBody struct {
	PrincipalID  string `json:"principal_id"`
	Principal    string `json:"principal"`
	Admin        bool   `json:"admin"`
	CredentialID string `json:"credential_id"`
	Kind         string `json:"kind"`
	ExpiresAt    string `json:"expires_at"`
	InstallID    string `json:"install_id"`
	Token        string `json:"token"`
}

// claim POSTs a first-principal claim as a caller holding tok.
func claim(h http.Handler, tok, name string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/auth/claim", strings.NewReader(`{"name":`+quote(name)+`}`))
	req.Header.Set("X-Burrow-Token", tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// quote JSON-encodes a string, so a name with a quote in it does not build a broken body.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// getAs issues a read as a caller holding tok, for the tests about what a token gets you.
func getAs(h http.Handler, path, tok string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if tok != "" {
		req.Header.Set("X-Burrow-Token", tok)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestClaimIssuesACredentialAndNamesTheInstall is the sign-in path end to end over HTTP: the first
// claimant becomes an admin, gets a `user` credential back exactly once, and learns which install
// issued it — the fact their target has to record, and the only authenticated exchange they make
// before they hold a credential (ADR-0084 §5).
func TestClaimIssuesACredentialAndNamesTheInstall(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")

	rec := claim(h, token, "ada")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got claimBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Principal != "ada" || !got.Admin {
		t.Errorf("principal = %q admin = %v, want ada / true (the first signer is the admin)", got.Principal, got.Admin)
	}
	if got.Kind != string(cp.CredentialKindUser) {
		t.Errorf("kind = %q, want user", got.Kind)
	}
	if got.Token == "" || got.CredentialID == "" || got.PrincipalID == "" {
		t.Errorf("claim returned %+v, want a token and both ids", got)
	}
	if got.InstallID != "install-abc" {
		t.Errorf("install_id = %q, want install-abc — the target has to learn which Burrow issued this", got.InstallID)
	}
	if got.ExpiresAt != "" {
		t.Errorf("expires_at = %q, want absent: a person's own terminal credential does not expire by default", got.ExpiresAt)
	}
}

// TestTheIssuedCredentialAuthenticates: the token a claim hands back is one burrowd accepts on an
// ordinary request afterwards. Without this the claim is a write nobody can spend.
func TestTheIssuedCredentialAuthenticates(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")

	var issued claimBody
	if err := json.Unmarshal(claim(h, token, "ada").Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec := getAs(h, "/v1/apps", issued.Token); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// TestTheSharedInstallTokenStillWorks is ADR-0084's "existing installs keep working" as a test. The
// shared token is checked first and short-circuits, so an install where nobody has ever signed in
// behaves exactly as it did — and it stays the break-glass route when burrowd cannot check anything
// in its database (§8).
func TestTheSharedInstallTokenStillWorks(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")

	if rec := getAs(h, "/v1/apps", token); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// TestAnUnknownTokenIsRefused: a credential this Burrow did not issue gets nowhere, and the refusal
// says so rather than returning a bare 401 that reads like the cluster being down.
func TestAnUnknownTokenIsRefused(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")

	rec := getAs(h, "/v1/apps", "not-a-token-this-burrow-issued")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	var e errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Code != "unauthorized" {
		t.Errorf("code = %q, want unauthorized", e.Code)
	}
}

// TestNoTokenIsRefused: an anonymous request never reaches the install check or the version
// handshake, so it learns neither this install's id nor its version.
func TestNoTokenIsRefused(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")

	rec := getAs(h, "/v1/apps", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "install-abc") {
		t.Errorf("an anonymous refusal must not name this install: %s", rec.Body.String())
	}
}

// TestASecondClaimIsRefusedAndPointsAtAnAdmin: whoever holds the first principal holds the admin
// bit, so a second claimant would be a silent second admin. The refusal is a conflict rather than a
// fault, carries a code a client can branch on, and names the way in that does exist.
func TestASecondClaimIsRefusedAndPointsAtAnAdmin(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")

	if rec := claim(h, token, "ada"); rec.Code != http.StatusOK {
		t.Fatalf("first claim: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec := claim(h, token, "grace")
	if rec.Code != http.StatusConflict {
		t.Fatalf("second claim: status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	var e errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Code != "already_claimed" {
		t.Errorf("code = %q, want already_claimed", e.Code)
	}
	if !strings.Contains(e.Error, "admin") {
		t.Errorf("error %q should point at asking an admin, which is the way in that exists", e.Error)
	}
}

// TestTheClaimWillNotTakeAKindFromTheRequest is the property ADR-0084 §3 turns on, asserted at the
// boundary where it would erode: the wire format has nowhere to put a kind, so a caller cannot ask
// to be issued an `agent` credential and thereby choose which guardrails bind it. A `deny` that
// binds the agent has to hold against an agent that would rather it did not (ADR-0020).
//
// It relies on the decoder refusing unknown fields, which is what makes "the field does not exist"
// a refusal rather than a silent ignore.
func TestTheClaimWillNotTakeAKindFromTheRequest(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")

	req := httptest.NewRequest("POST", "/v1/auth/claim", strings.NewReader(`{"name":"ada","kind":"agent"}`))
	req.Header.Set("X-Burrow-Token", token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: a caller-declared kind must not be accepted; body = %s", rec.Code, rec.Body.String())
	}
}

// TestAClaimNeedsAName: a principal with no name is indistinguishable from every other principal in
// the audit trail and the listing, which is the surface this whole record exists to fix.
func TestAClaimNeedsAName(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")

	rec := claim(h, token, "   ")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

// TestAnAuditedOperationRecordsTheAuthenticatedPrincipal: the token is not only accepted, the
// identity behind it reaches the engine and lands in the column ADR-0027 reserved for it. Before
// this the column held a literal on every row (ADR-0084 §9).
func TestAnAuditedOperationRecordsTheAuthenticatedPrincipal(t *testing.T) {
	h, d := newIdentityAPI(t, "install-abc")

	var issued claimBody
	if err := json.Unmarshal(claim(h, token, "ada").Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/apps/web/deploy", strings.NewReader(`{"image":"img:1","replicas":1,"confirm":true}`))
	req.Header.Set("X-Burrow-Token", issued.Token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rows := d.AuditRows()
	if len(rows) == 0 {
		t.Fatal("no audit rows recorded for the deploy")
	}
	for i, r := range rows {
		if r.Principal != "ada" {
			t.Errorf("row[%d] principal = %q, want ada", i, r.Principal)
		}
		if r.Caller != string(cp.CredentialKindUser) {
			t.Errorf("row[%d] caller = %q, want user — the kind comes from the stored row", i, r.Caller)
		}
	}
}
