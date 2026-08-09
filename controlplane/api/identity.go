// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// Authenticating a caller, and the one route that mints a credential (ADR-0084 §1, §2).
//
// Until now there was one string. It sat in a Secret on the cluster, everybody who could read it
// presented it, and burrowd could not tell an operator from an agent from a CI job. That string
// still works and is still checked first, because an install that never signs anybody in must behave
// exactly as it did — and because it is ADR-0084 §8's break-glass, the way in when burrowd is too
// broken to check anything in its database.
//
// What is new is the second answer: a token burrowd issued, whose hash is a row in a table it owns.
// A caller presenting one is resolved to a Caller and the request carries that identity down to the
// engine.
//
// THE ROUTE AND THE IDENTITY ARE STILL SEPARATE, AND BOTH ARE STILL REQUIRED. A cluster target
// reaches burrowd through the API-server proxy, which needs the kubeconfig; the token in
// X-Burrow-Token says who is asking. Neither alone is enough, which is why no NetworkPolicy is
// load-bearing here (ADR-0084 §2).

// claimPath is the one route that hands back a token. It is a POST because it writes, and it is
// under /v1 like everything else, so it inherits authentication, the install check and the version
// handshake rather than being a second front door with its own rules.
const claimPath = "POST /v1/auth/claim"

// invitePath is how an admin gives a second person access to this Burrow (ADR-0084 §2). What it
// returns is an invitation rather than a working credential, which is what keeps a usable token out
// of whatever the two of them talk over.
const invitePath = "POST /v1/auth/invitations"

// redeemPath is where an invitation is exchanged for the credential its holder will carry. It is
// called from the RECIPIENT's machine — that is the whole reason it exists as a route rather than
// as a second thing the admin does — and it is the only route an invitation may reach.
const redeemPath = "POST /v1/auth/redeem"

// redeemRoute is redeemPath without its method, for the one comparison that has to be made against a
// live request rather than against the mux's pattern table. It is derived from the pattern by hand
// and pinned by a test, so the two cannot drift into a restriction that guards a path nothing
// serves.
const redeemRoute = "/v1/auth/redeem"

// authClaimRequest is the body of a first-principal claim: the name to record for the person
// claiming. It carries NO KIND. What kind of credential this is, is burrowd's to decide and record —
// a caller-declared kind would make a `deny` that binds the agent cooperative, and ADR-0020 requires
// a deny to hold "even against an over-eager or misbehaving agent" (ADR-0084 §3).
type authClaimRequest struct {
	Name string `json:"name"`
}

// authInviteRequest is the body of an invitation: who it is for, and whether they get the admin bit.
//
// It carries NO KIND, for the reason the claim carries none. It also carries no lifetime: an
// invitation always expires, on a schedule burrowd chooses (controlplane.DefaultInvitationTTL),
// because the short life is what makes handing one over safe and a field for it is how that gets
// set to zero.
type authInviteRequest struct {
	// Name is the principal to invite. An existing name issues that principal a further invitation
	// rather than failing, which is what somebody who lost the first one needs.
	Name string `json:"name"`
	// Admin grants the admin bit, and only when the principal is being recorded now. Inviting
	// somebody already recorded never changes it.
	Admin bool `json:"admin,omitempty"`
}

// authCredentialResponse is what a claim, an invitation and an exchange each return, and they are
// the ONLY responses in this API that carry a credential in the clear. The token exists nowhere
// else — burrowd stored a hash of it and will not produce it again — so a caller that drops it has
// to be issued another.
//
// It carries the INSTALL ID alongside the token, and that is not incidental. A target learns which
// install it is pointed at from something it did against that install (ADR-0084 §5), and for a
// second person the only authenticated thing they do before they hold a credential is obtain one.
// Sending the id here is what lets the credential and the target that spends it be recorded
// together, in one round trip, rather than leaving the target unchecked until some later command.
type authCredentialResponse struct {
	// PrincipalID is the opaque, stable identity credentials and audit rows reference.
	PrincipalID string `json:"principal_id"`
	// Principal is the human-readable handle, as recorded.
	Principal string `json:"principal"`
	// Admin is whether this principal may issue for somebody else. The first claimant is one.
	Admin bool `json:"admin"`
	// CredentialID is which credential this is, so a revocation can name it without guessing.
	CredentialID string `json:"credential_id"`
	// Kind is what the credential is held by, as STORED. It is echoed so the holder can see what
	// they are holding; it is not something the request chose.
	Kind string `json:"kind"`
	// ExpiresAt is when the credential stops authenticating, RFC 3339, or absent when it does not
	// expire.
	ExpiresAt string `json:"expires_at,omitempty"`
	// InstallID is this install's own id, so the caller can record which Burrow issued this.
	InstallID string `json:"install_id,omitempty"`
	// Token is the secret, for this one return.
	Token string `json:"token"`
}

// claimFirstPrincipal records the install's first principal as an admin and issues them a `user`
// credential (ADR-0084 §1, §2).
//
// The proof it rests on is deployment-shaped rather than credential-shaped, and it is worth being
// exact about: reaching this route at all means presenting a token that authenticates, which on the
// self-hosted path means having read the install Secret through the API-server proxy, which means
// holding cluster RBAC. Whoever can make this call already holds the access being bootstrapped from,
// so the claim grants nothing that was not already reachable.
//
// A second claim is refused rather than merged. Whoever holds the first principal holds the admin
// bit, and a second claimant would be a silent second admin — so the refusal names the way in that
// does exist: ask an admin.
func (s *server) claimFirstPrincipal(w http.ResponseWriter, r *http.Request) {
	var req authClaimRequest
	if !s.decode(w, r, &req) {
		return
	}
	p, issued, err := s.engine.ClaimFirstPrincipal(r.Context(), req.Name)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.credentialResponse(p, issued))
}

// invitePrincipal gives a second person access to this Burrow, and hands back an invitation rather
// than a credential (ADR-0084 §2).
//
// This is the route that makes "additional people, without cluster admin" true. The recipient needs
// no `get` on `burrow-credentials` and no claim of their own: they need a route to burrowd, which
// their kubeconfig already is, and an identity, which this is how they get.
//
// WHAT COMES BACK CANNOT OPERATE ANYTHING. That is the difference between this and an admin issuing
// a working token and pasting it somewhere — the invitation expires, is spent on first use, and is
// refused at every route but the exchange, so the interval during which a copy of it is worth
// something is short and the thing it is worth is small.
func (s *server) invitePrincipal(w http.ResponseWriter, r *http.Request) {
	var req authInviteRequest
	if !s.decode(w, r, &req) {
		return
	}
	caller, ok := controlplane.CallerFromContext(r.Context())
	if !ok {
		// The shared install token names nobody, and inviting somebody is a decision an identity has
		// to be behind (ADR-0084 §2). The engine's authorizer refuses this too; saying it here says
		// what to do about it, because the caller holding the shared token is the operator and they
		// are one command away from being able to.
		writeError(w, http.StatusForbidden,
			"this request presented the install's shared token, which names nobody, and giving somebody access is a decision that records who took it. "+
				"Sign in with `burrow auth login --context <cluster>` and run it again",
			"forbidden")
		return
	}
	p, issued, err := s.engine.InvitePrincipal(r.Context(), caller, req.Name, req.Admin)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.credentialResponse(p, issued))
}

// redeemInvitation exchanges the invitation the caller presented for the credential they will carry
// (ADR-0084 §2). It takes no body: everything it needs is the credential on the request, and a field
// here would be a field describing a credential whose holder does not get to describe it.
//
// It answers on the RECIPIENT's machine, which is the point. The token in the response was generated
// for this request and has been nowhere else, so what the admin sent them is not what they end up
// holding.
func (s *server) redeemInvitation(w http.ResponseWriter, r *http.Request) {
	caller, ok := controlplane.CallerFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusForbidden,
			"this request presented the install's shared token rather than an invitation, and there is nothing to exchange. "+
				"An invitation comes from an admin of this Burrow, who issues one with `burrow auth invite <name>`",
			"forbidden")
		return
	}
	issued, err := s.engine.RedeemInvitation(r.Context(), caller)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	// The principal is the one the invitation named, unchanged: an exchange gives somebody the
	// credential they were invited to hold, and never a different identity.
	p := controlplane.Principal{ID: caller.PrincipalID, Name: caller.PrincipalName}
	writeJSON(w, http.StatusOK, s.credentialResponse(p, issued))
}

// credentialResponse renders an issued credential for the wire. It is shared by the three routes
// that return one so the install id cannot be attached to some of them and forgotten on the rest —
// a recipient who is handed a token and not the id of the install that issued it has nowhere to
// file it (ADR-0084 §5).
//
// Admin is read from the principal rather than from anything the request said, like the kind.
func (s *server) credentialResponse(p controlplane.Principal, issued controlplane.IssuedCredential) authCredentialResponse {
	resp := authCredentialResponse{
		PrincipalID:  p.ID,
		Principal:    p.Name,
		Admin:        p.Admin,
		CredentialID: issued.Credential.ID,
		Kind:         string(issued.Credential.Kind),
		InstallID:    s.installID,
		Token:        issued.Token,
	}
	if !issued.Credential.ExpiresAt.IsZero() {
		resp.ExpiresAt = issued.Credential.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return resp
}

// authenticate is the /v1 gate: it resolves the presented token to whoever is behind it and refuses
// a request that has nobody behind it.
//
// THE SHARED INSTALL TOKEN IS CHECKED FIRST, IN CONSTANT TIME, AND SHORT-CIRCUITS. Two things follow
// from that ordering and both are deliberate. An install where nobody has ever signed in makes no
// database call to authenticate, so its behaviour and its cost are exactly what they were. And when
// the database is unreachable, the shared token still gets in while a minted credential cannot —
// which is ADR-0084 §8's break-glass working as described rather than by accident.
//
// A minted credential is resolved by hash against a table burrowd owns. There is no ServiceAccount,
// no TokenReview, and no round trip to the API server, so authenticating to Burrow does not depend
// on the Kubernetes API being reachable (ADR-0084 §2).
func authenticate(sharedToken string, engine *controlplane.Engine, next http.Handler) http.Handler {
	want := []byte(sharedToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := presentedToken(r)
		if got == "" {
			writeError(w, http.StatusUnauthorized, "missing or invalid token", "unauthorized")
			return
		}
		if subtle.ConstantTimeCompare([]byte(got), want) == 1 {
			// The shared install token names nobody, so nothing is put on the context and the
			// engine records the honest constants (ADR-0084 §9).
			next.ServeHTTP(w, r)
			return
		}
		caller, err := engine.AuthenticateCredential(r.Context(), got)
		if err != nil {
			writeCredentialError(w, err)
			return
		}
		// AN INVITATION REACHES ONE ROUTE. It authenticates, so the request has an identity behind it
		// and the refusal below can say whose; what it does not have is permission to do anything but
		// become a credential. Enforcing that here rather than per handler is what makes it true of
		// every route, including the ones written after this one.
		if caller.Enrollment && !isRedeem(r) {
			writeError(w, http.StatusForbidden,
				"the credential presented is an invitation for "+caller.PrincipalName+", and an invitation can only be exchanged for a credential of your own. "+
					"Run `burrow auth login --context <cluster> --invite <invitation>` to exchange it",
				"invitation_not_redeemed")
			return
		}
		next.ServeHTTP(w, r.WithContext(controlplane.ContextWithCaller(r.Context(), caller)))
	})
}

// isRedeem reports whether this request is the exchange, and it is deliberately an exact match on
// the method and the path rather than a prefix: a rule that admitted anything UNDER the path would
// admit a route somebody adds there later without noticing what it inherited.
func isRedeem(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == redeemRoute
}

// writeCredentialError renders a failed authentication. It distinguishes the three cases because
// they have three different remedies, and because a person who signed in yesterday and is refused
// today deserves better than a bare 401:
//
//   - The token is not one this Burrow issued: 401, and the likeliest cause is a credential for a
//     different install or a stale one on disk.
//   - The token was real and no longer authenticates — revoked, or expired: 401, and the engine's
//     message says which and when, and to sign in again.
//   - The lookup itself failed: 503, NOT 401. The caller may well hold a perfectly good credential;
//     saying "invalid token" to somebody whose token was never checked sends them to re-authenticate
//     against a control plane that cannot authenticate anybody.
//
// The underlying error is never echoed on the 503 path. It comes from the database layer, and an
// error message is the likeliest place for something to end up somewhere it was never meant to be.
func writeCredentialError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, controlplane.ErrNotFound):
		writeError(w, http.StatusUnauthorized, err.Error(), "unauthorized")
	case errors.Is(err, controlplane.ErrForbidden):
		writeError(w, http.StatusUnauthorized, err.Error(), "credential_not_live")
	default:
		writeError(w, http.StatusServiceUnavailable,
			"this control plane could not check the credential presented, so it neither accepted nor refused it; "+
				"its database may be unreachable. Retry, and see `burrow cluster status` if it persists",
			"credential_check_failed")
	}
}
