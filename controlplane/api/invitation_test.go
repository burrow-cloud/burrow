// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The HTTP tests for giving a second person access (ADR-0084 §2): who may issue an invitation, what
// an invitation can reach, and what the exchange returns.

// invite POSTs an invitation request as a caller holding tok.
func invite(h http.Handler, tok, name string, admin bool) *httptest.ResponseRecorder {
	body := `{"name":` + quote(name) + `,"admin":` + boolJSON(admin) + `}`
	req := httptest.NewRequest("POST", "/v1/auth/invitations", strings.NewReader(body))
	req.Header.Set("X-Burrow-Token", tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// redeem POSTs the exchange as a caller holding tok, with no body at all — which is what the client
// sends, and what the route is specified to need.
func redeem(h http.Handler, tok string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/auth/redeem", nil)
	req.Header.Set("X-Burrow-Token", tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// decodeCredential reads one of the three credential responses off a recorder.
func decodeCredential(t *testing.T, rec *httptest.ResponseRecorder) claimBody {
	t.Helper()
	var got claimBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return got
}

// signedInAdmin claims the install and returns the admin's token, so the tests below start where a
// real install does: one person, holding their own credential.
func signedInAdmin(t *testing.T, h http.Handler) claimBody {
	t.Helper()
	rec := claim(h, token, "operator")
	if rec.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
	}
	return decodeCredential(t, rec)
}

// TestAnInvitationIsExchangedForACredentialMadeOnThisRequest walks the whole second-person path over
// HTTP: an admin invites, the recipient exchanges, and what they end up with is not what was sent.
func TestAnInvitationIsExchangedForACredentialMadeOnThisRequest(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")
	admin := signedInAdmin(t, h)

	rec := invite(h, admin.Token, "ada", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body.String())
	}
	invitation := decodeCredential(t, rec)
	if invitation.Token == "" {
		t.Fatal("the invitation carried no token to hand over")
	}
	if invitation.ExpiresAt == "" {
		t.Error("the invitation does not expire; the short life is what makes handing it over safe")
	}
	if invitation.Principal != "ada" {
		t.Errorf("invitation principal = %q, want ada", invitation.Principal)
	}

	rec = redeem(h, invitation.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("redeem: %d %s", rec.Code, rec.Body.String())
	}
	credential := decodeCredential(t, rec)
	if credential.Token == invitation.Token {
		t.Fatal("the exchange returned the invitation itself; the credential is supposed to be made on this request")
	}
	if credential.ExpiresAt != "" {
		t.Errorf("the exchanged credential expires at %q; a person's own credential does not", credential.ExpiresAt)
	}
	if credential.PrincipalID != invitation.PrincipalID {
		t.Errorf("exchanged credential is for %q, want the invited principal %q", credential.PrincipalID, invitation.PrincipalID)
	}
	// The recipient has no other authenticated exchange before this one, so this is where they learn
	// which install to file the credential under (ADR-0084 §5).
	if credential.InstallID != "install-abc" {
		t.Errorf("exchange install_id = %q, want install-abc; without it there is nowhere to file the credential", credential.InstallID)
	}

	// It works, and the invitation does not.
	if rec := getAs(h, "/v1/apps", credential.Token); rec.Code != http.StatusOK {
		t.Errorf("the exchanged credential does not authenticate: %d %s", rec.Code, rec.Body.String())
	}
	if rec := redeem(h, invitation.Token); rec.Code == http.StatusOK {
		t.Error("the invitation was exchanged twice; it is spent on first use")
	}
}

// TestAnInvitationReachesNothingButTheExchange is the property the enrollment column exists for: an
// invitation authenticates, so burrowd knows whose it is, and it can still do nothing at all.
func TestAnInvitationReachesNothingButTheExchange(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")
	admin := signedInAdmin(t, h)

	invitation := decodeCredential(t, invite(h, admin.Token, "ada", false))

	rec := getAs(h, "/v1/apps", invitation.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reading apps with an invitation = %d %s, want 403", rec.Code, rec.Body.String())
	}
	var errBody struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	if errBody.Code != "invitation_not_redeemed" {
		t.Errorf("code = %q, want invitation_not_redeemed", errBody.Code)
	}
	if !strings.Contains(errBody.Error, "ada") {
		t.Errorf("refusal = %q, want it to name whose invitation it is", errBody.Error)
	}

	// Including the route that would hand out more of them.
	if rec := invite(h, invitation.Token, "bob", false); rec.Code != http.StatusForbidden {
		t.Errorf("inviting with an invitation = %d %s, want 403", rec.Code, rec.Body.String())
	}
}

// TestOnlyAnIdentityInvites: the install's shared token names nobody, and giving somebody access is
// a decision that has to be recorded against whoever took it. The refusal says how to be somebody.
func TestOnlyAnIdentityInvites(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")

	rec := invite(h, token, "ada", false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("inviting with the shared install token = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "auth login") {
		t.Errorf("refusal = %q, want it to say how to sign in first", rec.Body.String())
	}
}

// TestTheInvitationRequestHasNowhereToPutAKind pins the boundary at the wire, like the claim's: a
// caller-declared kind would make a `deny` that binds the agent cooperative (ADR-0084 §3), and the
// only durable defence is a decoder that refuses the field outright.
func TestTheInvitationRequestHasNowhereToPutAKind(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")
	admin := signedInAdmin(t, h)

	req := httptest.NewRequest("POST", "/v1/auth/invitations",
		strings.NewReader(`{"name":"ada","kind":"agent"}`))
	req.Header.Set("X-Burrow-Token", admin.Token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an invitation naming a kind = %d %s, want 400", rec.Code, rec.Body.String())
	}
}

// TestAnAdminInvitationCarriesTheBit: the flag is how a second admin is made, which is the only way
// an install survives its first person leaving.
func TestAnAdminInvitationCarriesTheBit(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")
	admin := signedInAdmin(t, h)

	invitation := decodeCredential(t, invite(h, admin.Token, "ada", true))
	if !invitation.Admin {
		t.Fatalf("invitation admin = false, want true: %s", invitation.Principal)
	}
	credential := decodeCredential(t, redeem(h, invitation.Token))
	if !credential.Admin {
		t.Error("the exchanged credential does not report the admin bit the invitation carried")
	}
	// And they can invite in turn, which is what the bit is for.
	if rec := invite(h, credential.Token, "bob", false); rec.Code != http.StatusOK {
		t.Errorf("the new admin cannot invite: %d %s", rec.Code, rec.Body.String())
	}
}
