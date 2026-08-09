// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// The engine tests for per-caller identity (ADR-0084 §2, §3): claiming the first principal, who may
// issue what for whom, turning a presented token back into a caller, and revocation.
//
// Nothing here reaches an HTTP route, because there is not one yet. That is deliberate: the
// authorization lands before anything that could call it, so there is never a merged issuance
// endpoint with no decision behind it.

// newIdentityEngine builds an engine with a token source wired, and returns the fakes a test needs
// to arrange time and inspect what was stored.
func newIdentityEngine(t *testing.T) (*cp.Engine, *fake.Database, *fake.Clock) {
	t.Helper()
	d := fake.NewDatabase()
	c := fake.NewClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: d, Clock: c, IDs: fake.NewIDs(),
		Resolver: fake.NewResolver(), Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
		TokenSource: fake.NewTokens(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, d, c
}

// callerFor is the Caller an authenticated request would carry for p, holding a `user` credential.
func callerFor(p cp.Principal) cp.Caller {
	return cp.Caller{PrincipalID: p.ID, PrincipalName: p.Name, Kind: cp.CredentialKindUser}
}

// TestClaimFirstPrincipalMakesAnAdmin: the first signer becomes an admin and receives a credential,
// and a second claim is refused rather than producing a silent second admin.
func TestClaimFirstPrincipalMakesAnAdmin(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newIdentityEngine(t)

	p, issued, err := e.ClaimFirstPrincipal(ctx, "operator")
	if err != nil {
		t.Fatalf("ClaimFirstPrincipal: %v", err)
	}
	if !p.Admin {
		t.Errorf("first principal Admin = false, want true — the first signer is the admin (ADR-0084 §2)")
	}
	if !p.Active() {
		t.Errorf("first principal is not active: %+v", p)
	}
	if issued.Token == "" {
		t.Fatal("the claim returned no token; the operator finishes install holding their own credential")
	}
	if issued.Credential.Kind != cp.CredentialKindUser {
		t.Errorf("claim credential kind = %q, want %q", issued.Credential.Kind, cp.CredentialKindUser)
	}

	_, _, err = e.ClaimFirstPrincipal(ctx, "somebody-else")
	if !errors.Is(err, cp.ErrAlreadyClaimed) {
		t.Fatalf("second claim = %v, want ErrAlreadyClaimed", err)
	}
}

// TestClaimFirstPrincipalNeedsATokenSource: an engine built without a token source refuses to issue
// rather than inventing randomness the engine is not supposed to read.
func TestClaimFirstPrincipalNeedsATokenSource(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive()) // no TokenSource

	if _, _, err := e.ClaimFirstPrincipal(ctx, "operator"); !errors.Is(err, cp.ErrNotImplemented) {
		t.Fatalf("claim without a token source = %v, want ErrNotImplemented", err)
	}
}

// TestClaimFirstPrincipalRejectsABadName: a principal without a usable name would be a row nobody
// can read in a listing or an audit trail.
func TestClaimFirstPrincipalRejectsABadName(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newIdentityEngine(t)

	for _, name := range []string{"", "   ", "two\nlines", strings.Repeat("n", 129)} {
		if _, _, err := e.ClaimFirstPrincipal(ctx, name); !errors.Is(err, cp.ErrInvalid) {
			t.Errorf("ClaimFirstPrincipal(%q) = %v, want ErrInvalid", name, err)
		}
	}
	// A name that only needed trimming is accepted, and stored trimmed.
	p, _, err := e.ClaimFirstPrincipal(ctx, "  operator  ")
	if err != nil {
		t.Fatalf("ClaimFirstPrincipal: %v", err)
	}
	if p.Name != "operator" {
		t.Errorf("stored name = %q, want %q", p.Name, "operator")
	}
}

// TestOnlyAnAdminRecordsAPrincipal: recording a second person is how somebody with no cluster access
// gets access to Burrow at all, so it is an admin action.
func TestOnlyAnAdminRecordsAPrincipal(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newIdentityEngine(t)

	admin, _, err := e.ClaimFirstPrincipal(ctx, "operator")
	if err != nil {
		t.Fatalf("ClaimFirstPrincipal: %v", err)
	}
	member, err := e.CreatePrincipal(ctx, callerFor(admin), "dana", false)
	if err != nil {
		t.Fatalf("CreatePrincipal as an admin: %v", err)
	}
	if member.Admin {
		t.Errorf("a principal created without the admin bit has it: %+v", member)
	}

	if _, err := e.CreatePrincipal(ctx, callerFor(member), "sam", false); !errors.Is(err, cp.ErrForbidden) {
		t.Fatalf("CreatePrincipal as a non-admin = %v, want ErrForbidden", err)
	}
	// The same for granting the bit itself.
	if _, err := e.CreatePrincipal(ctx, callerFor(member), "sam", true); !errors.Is(err, cp.ErrForbidden) {
		t.Fatalf("granting admin as a non-admin = %v, want ErrForbidden", err)
	}
	// A duplicate name is refused: a handle two principals answer to is an ambiguity in the trail.
	if _, err := e.CreatePrincipal(ctx, callerFor(admin), "dana", false); err == nil {
		t.Error("a second principal named dana was accepted, want a refusal")
	}
}

// TestIssueAuthorization walks the whole matrix of who may issue what for whom (ADR-0084 §2): an
// admin issues for anybody, and a non-admin issues an agent credential for themselves and nothing
// else.
func TestIssueAuthorization(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newIdentityEngine(t)

	admin, _, err := e.ClaimFirstPrincipal(ctx, "operator")
	if err != nil {
		t.Fatalf("ClaimFirstPrincipal: %v", err)
	}
	dana, err := e.CreatePrincipal(ctx, callerFor(admin), "dana", false)
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	sam, err := e.CreatePrincipal(ctx, callerFor(admin), "sam", false)
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}

	cases := []struct {
		name    string
		caller  cp.Principal
		subject cp.Principal
		kind    cp.CredentialKind
		allowed bool
	}{
		{"an admin issues for somebody else", admin, dana, cp.CredentialKindUser, true},
		{"an admin issues for themselves", admin, admin, cp.CredentialKindUser, true},
		{"an admin issues a machine credential", admin, dana, cp.CredentialKindMachine, true},
		{"a person provisions their own agent", dana, dana, cp.CredentialKindAgent, true},
		{"a person issues a second user credential for themselves", dana, dana, cp.CredentialKindUser, false},
		{"a person provisions somebody else's agent", dana, sam, cp.CredentialKindAgent, false},
		{"a person issues for somebody else", dana, sam, cp.CredentialKindUser, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.IssueCredential(ctx, callerFor(tc.caller), cp.IssueCredentialRequest{
				PrincipalID: tc.subject.ID, Kind: tc.kind,
			})
			switch {
			case tc.allowed && err != nil:
				t.Fatalf("IssueCredential = %v, want it allowed", err)
			case !tc.allowed && !errors.Is(err, cp.ErrForbidden):
				t.Fatalf("IssueCredential = %v, want ErrForbidden", err)
			}
		})
	}
}

// TestIssueCredentialRejectsWhatCannotWork: a kind that is not one of the three, a negative
// lifetime, an unknown principal, and a principal whose access has already ended.
func TestIssueCredentialRejectsWhatCannotWork(t *testing.T) {
	ctx := context.Background()
	e, d, _ := newIdentityEngine(t)

	admin, _, err := e.ClaimFirstPrincipal(ctx, "operator")
	if err != nil {
		t.Fatalf("ClaimFirstPrincipal: %v", err)
	}
	caller := callerFor(admin)

	if _, err := e.IssueCredential(ctx, caller, cp.IssueCredentialRequest{PrincipalID: admin.ID, Kind: "root"}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("an unknown kind = %v, want ErrInvalid", err)
	}
	if _, err := e.IssueCredential(ctx, caller, cp.IssueCredentialRequest{
		PrincipalID: admin.ID, Kind: cp.CredentialKindUser, TTL: -time.Hour,
	}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("a negative lifetime = %v, want ErrInvalid", err)
	}
	if _, err := e.IssueCredential(ctx, caller, cp.IssueCredentialRequest{
		PrincipalID: "nobody", Kind: cp.CredentialKindUser,
	}); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("an unknown principal = %v, want ErrNotFound", err)
	}

	dana, err := e.CreatePrincipal(ctx, caller, "dana", false)
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if err := d.RevokePrincipal(ctx, dana.ID, time.Now()); err != nil {
		t.Fatalf("RevokePrincipal: %v", err)
	}
	if _, err := e.IssueCredential(ctx, caller, cp.IssueCredentialRequest{
		PrincipalID: dana.ID, Kind: cp.CredentialKindUser,
	}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("issuing for a revoked principal = %v, want ErrInvalid", err)
	}
}

// TestAuthenticateCredentialReadsTheKindFromTheRow is the property ADR-0084 §3 turns on: the kind a
// caller acts under comes from what was stored at issuance, and there is no way for a request to say
// what kind it holds. A caller-declared kind would make a `deny` that binds the agent cooperative,
// which is the guardrail story collapsing (ADR-0020).
func TestAuthenticateCredentialReadsTheKindFromTheRow(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newIdentityEngine(t)

	admin, adminCred, err := e.ClaimFirstPrincipal(ctx, "operator")
	if err != nil {
		t.Fatalf("ClaimFirstPrincipal: %v", err)
	}
	agentCred, err := e.IssueCredential(ctx, callerFor(admin), cp.IssueCredentialRequest{
		PrincipalID: admin.ID, Kind: cp.CredentialKindAgent,
	})
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}
	if agentCred.Token == adminCred.Token {
		t.Fatal("two issues returned the same token")
	}

	// One principal, two credentials, and the presented one is what says which acted.
	for _, tc := range []struct {
		token string
		kind  cp.CredentialKind
	}{
		{adminCred.Token, cp.CredentialKindUser},
		{agentCred.Token, cp.CredentialKindAgent},
	} {
		got, err := e.AuthenticateCredential(ctx, tc.token)
		if err != nil {
			t.Fatalf("AuthenticateCredential: %v", err)
		}
		if got.PrincipalID != admin.ID || got.PrincipalName != "operator" {
			t.Errorf("caller = %+v, want the operator principal", got)
		}
		if got.Kind != tc.kind {
			t.Errorf("caller kind = %q, want %q — the kind is read from the stored row", got.Kind, tc.kind)
		}
	}

	// Revoking the agent leaves the person signed in, which is the whole reason they are separate
	// credentials (ADR-0038).
	if err := e.RevokeCredential(ctx, callerFor(admin), agentCred.Credential.ID); err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}
	if _, err := e.AuthenticateCredential(ctx, agentCred.Token); !errors.Is(err, cp.ErrForbidden) {
		t.Errorf("the revoked agent credential still authenticates: %v", err)
	}
	if _, err := e.AuthenticateCredential(ctx, adminCred.Token); err != nil {
		t.Errorf("revoking the agent logged the person out: %v", err)
	}
}

// TestAuthenticateCredentialDistinguishesItsFailures: an unknown token and a token that was real and
// no longer works are different answers to a person holding one that does not work, and neither
// message carries the token.
func TestAuthenticateCredentialDistinguishesItsFailures(t *testing.T) {
	ctx := context.Background()
	e, d, clock := newIdentityEngine(t)

	admin, _, err := e.ClaimFirstPrincipal(ctx, "operator")
	if err != nil {
		t.Fatalf("ClaimFirstPrincipal: %v", err)
	}
	if _, err := e.AuthenticateCredential(ctx, ""); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("no credential = %v, want ErrNotFound", err)
	}
	if _, err := e.AuthenticateCredential(ctx, "not-a-token"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("an unknown token = %v, want ErrNotFound", err)
	}

	shortLived, err := e.IssueCredential(ctx, callerFor(admin), cp.IssueCredentialRequest{
		PrincipalID: admin.ID, Kind: cp.CredentialKindMachine, TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}
	if _, err := e.AuthenticateCredential(ctx, shortLived.Token); err != nil {
		t.Fatalf("a live credential did not authenticate: %v", err)
	}
	clock.Advance(2 * time.Hour)
	_, err = e.AuthenticateCredential(ctx, shortLived.Token)
	if !errors.Is(err, cp.ErrForbidden) {
		t.Errorf("an expired credential = %v, want ErrForbidden", err)
	}
	if strings.Contains(err.Error(), shortLived.Token) {
		t.Errorf("the refusal carries the token: %v", err)
	}

	// Revoking the PRINCIPAL stops every credential it holds, without either row being deleted.
	live, err := e.IssueCredential(ctx, callerFor(admin), cp.IssueCredentialRequest{
		PrincipalID: admin.ID, Kind: cp.CredentialKindUser,
	})
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}
	if err := d.RevokePrincipal(ctx, admin.ID, clock.Now()); err != nil {
		t.Fatalf("RevokePrincipal: %v", err)
	}
	if _, err := e.AuthenticateCredential(ctx, live.Token); !errors.Is(err, cp.ErrForbidden) {
		t.Errorf("a revoked principal's credential = %v, want ErrForbidden", err)
	}
}

// TestRevokeCredentialAuthorization: anybody revokes their own, only an admin revokes somebody
// else's, and a repeated revoke is a no-op that keeps the first timestamp.
func TestRevokeCredentialAuthorization(t *testing.T) {
	ctx := context.Background()
	e, d, clock := newIdentityEngine(t)

	admin, _, err := e.ClaimFirstPrincipal(ctx, "operator")
	if err != nil {
		t.Fatalf("ClaimFirstPrincipal: %v", err)
	}
	dana, err := e.CreatePrincipal(ctx, callerFor(admin), "dana", false)
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	danaCred, err := e.IssueCredential(ctx, callerFor(admin), cp.IssueCredentialRequest{
		PrincipalID: dana.ID, Kind: cp.CredentialKindUser,
	})
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}
	adminCred, err := e.IssueCredential(ctx, callerFor(admin), cp.IssueCredentialRequest{
		PrincipalID: admin.ID, Kind: cp.CredentialKindMachine,
	})
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}

	if err := e.RevokeCredential(ctx, callerFor(dana), adminCred.Credential.ID); !errors.Is(err, cp.ErrForbidden) {
		t.Errorf("a non-admin revoking somebody else's credential = %v, want ErrForbidden", err)
	}
	if err := e.RevokeCredential(ctx, callerFor(dana), danaCred.Credential.ID); err != nil {
		t.Fatalf("revoking their own credential: %v", err)
	}
	first, err := d.Credential(ctx, danaCred.Credential.ID)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}

	// A retried revoke must not move the timestamp: that is when the access actually ended.
	clock.Advance(time.Hour)
	if err := e.RevokeCredential(ctx, callerFor(admin), danaCred.Credential.ID); err != nil {
		t.Fatalf("re-revoking: %v", err)
	}
	again, err := d.Credential(ctx, danaCred.Credential.ID)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if !again.RevokedAt.Equal(first.RevokedAt) {
		t.Errorf("re-revoking moved the timestamp from %s to %s", first.RevokedAt, again.RevokedAt)
	}
	if err := e.RevokeCredential(ctx, callerFor(admin), "nope"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("revoking an unknown credential = %v, want ErrNotFound", err)
	}
	if err := e.RevokeCredential(ctx, callerFor(admin), "  "); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("revoking an empty id = %v, want ErrInvalid", err)
	}
}

// TestTheTokenIsStoredOnlyAsAHash: what a credential row holds is the hash, and the value the engine
// handed back is the only copy of the secret.
func TestTheTokenIsStoredOnlyAsAHash(t *testing.T) {
	ctx := context.Background()
	e, d, _ := newIdentityEngine(t)

	_, issued, err := e.ClaimFirstPrincipal(ctx, "operator")
	if err != nil {
		t.Fatalf("ClaimFirstPrincipal: %v", err)
	}
	stored, err := d.Credential(ctx, issued.Credential.ID)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if stored.TokenHash == issued.Token {
		t.Fatal("the credential row holds the token itself")
	}
	if stored.TokenHash != cp.HashToken(issued.Token) {
		t.Errorf("stored hash = %q, want the SHA-256 of the issued token", stored.TokenHash)
	}
	// The value that carries the secret must not print it: a %v in a log line or an error is the
	// likeliest way a credential travels somewhere it was never meant to.
	if strings.Contains(issued.String(), issued.Token) {
		t.Errorf("IssuedCredential.String() = %q, want the token redacted", issued.String())
	}
}

// TestAnAuthorizerReplacesTheWholeAnswer: the seam is what an SSO integration takes over, so an
// engine given one never falls back to the admin column.
func TestAnAuthorizerReplacesTheWholeAnswer(t *testing.T) {
	ctx := context.Background()
	d := fake.NewDatabase()
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: d, Clock: fake.NewClock(time.Now()), IDs: fake.NewIDs(),
		Resolver: fake.NewResolver(), Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
		TokenSource: fake.NewTokens(), CredentialAuthorizer: refusingAuthorizer{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The claim is the bootstrap and has no caller to authorize, so it still succeeds.
	admin, _, err := e.ClaimFirstPrincipal(ctx, "operator")
	if err != nil {
		t.Fatalf("ClaimFirstPrincipal: %v", err)
	}
	if !admin.Admin {
		t.Fatal("the first principal is not an admin")
	}
	// Everything after it goes through the injected decider, which refuses an admin the local
	// implementation would have allowed.
	if _, err := e.IssueCredential(ctx, callerFor(admin), cp.IssueCredentialRequest{
		PrincipalID: admin.ID, Kind: cp.CredentialKindAgent,
	}); !errors.Is(err, cp.ErrForbidden) {
		t.Errorf("IssueCredential = %v, want the injected authorizer's refusal", err)
	}
	if _, err := e.CreatePrincipal(ctx, callerFor(admin), "dana", false); !errors.Is(err, cp.ErrForbidden) {
		t.Errorf("CreatePrincipal = %v, want the injected authorizer's refusal", err)
	}
}

// refusingAuthorizer stands in for an external identity provider that says no to everything.
type refusingAuthorizer struct{}

func (refusingAuthorizer) AuthorizeCreatePrincipal(context.Context, string) error {
	return cp.ErrForbidden
}

func (refusingAuthorizer) AuthorizeIssue(context.Context, string, string, cp.CredentialKind) error {
	return cp.ErrForbidden
}

func (refusingAuthorizer) AuthorizeRevoke(context.Context, string, string) error {
	return cp.ErrForbidden
}
