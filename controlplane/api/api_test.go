// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/burrow-cloud/burrow/client"
	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/api"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

const token = "secret-token"

func newAPI(t *testing.T) (http.Handler, *fake.Kubernetes, *fake.Database) {
	return newAPIVersion(t, "")
}

// newAPIVersion is newAPI with an explicit burrowd version for the client-version handshake tests
// (ADR-0039). An empty version keeps the handshake permissive, which is what every other test wants.
func newAPIVersion(t *testing.T, version string) (http.Handler, *fake.Kubernetes, *fake.Database) {
	t.Helper()
	return newAPIConfig(t, version, "")
}

// newAPIInstall is newAPI with an explicit install id for the install-check tests (ADR-0084 §5). An
// empty id models a control plane installed before ids existed, which cannot refuse anything.
func newAPIInstall(t *testing.T, installID string) (http.Handler, *fake.Kubernetes, *fake.Database) {
	t.Helper()
	return newAPIConfig(t, "", installID)
}

// newAPIConfig builds the handler with the two identity-of-the-server fields the gates read. Both
// are empty for every other test, which is the permissive configuration.
func newAPIConfig(t *testing.T, version, installID string) (http.Handler, *fake.Kubernetes, *fake.Database) {
	t.Helper()
	k, d := fake.NewKubernetes(), fake.NewDatabase()
	// A restrictive baseline (empty dispositions → deny) so guardrail tests opt in explicitly,
	// but rollback and deploy have a product default of allow, so seed those to match production
	// (deploy is the core action and is what the setup `do(... /deploy ...)` calls exercise).
	d.SetPolicy(cp.Policy{}.
		With(cp.GuardrailRollback, cp.DispositionAllow).
		With(cp.GuardrailAppDeploy, cp.DispositionAllow))
	// A low replica ceiling so the limit tests can cross it without asking for 51 replicas
	// (ADR-0068): it is operational configuration now, not a field on the policy.
	d.SetLimits(cp.OperationalConfig{}.With(cp.LimitReplicaCeiling, "5"))
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d,
		Clock:       fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)),
		IDs:         fake.NewIDs(),
		Resolver:    fake.NewResolver(),
		Credentials: fake.NewCredentials(),
		DNS:         fake.NewDNSFactory(),
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	h, err := api.New(api.Config{Engine: e, Token: token, Version: version, InstallID: installID})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h, k, d
}

func TestGuardEndpoints(t *testing.T) {
	h, _, _ := newAPI(t)

	if rr := do(h, "GET", "/v1/guard", token, ""); rr.Code != 200 || !strings.Contains(rr.Body.String(), "app.scale_to_zero") {
		t.Fatalf("guard list = %d %s", rr.Code, rr.Body.String())
	}

	rr := do(h, "PUT", "/v1/guard/app.scale_to_zero", token, `{"disposition":"allow"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"disposition":"allow"`) {
		t.Fatalf("guard set = %d %s", rr.Code, rr.Body.String())
	}

	// Invalid disposition and unknown guardrail are rejected (ErrInvalid -> 400).
	if rr := do(h, "PUT", "/v1/guard/app.scale_to_zero", token, `{"disposition":"nope"}`); rr.Code != 400 {
		t.Errorf("invalid disposition code = %d, want 400", rr.Code)
	}
	if rr := do(h, "PUT", "/v1/guard/bogus", token, `{"disposition":"allow"}`); rr.Code != 400 {
		t.Errorf("unknown guardrail code = %d, want 400", rr.Code)
	}
}

// TestGuardEndpointsCarryTheNameInTheRoute confirms the name tier is reachable as a ROUTE, which is
// what lets a control plane that does not have the tier refuse a name-scoped call outright instead
// of ignoring the name and writing the environment-wide entry (issue #472, ADR-0039 §4).
func TestGuardEndpointsCarryTheNameInTheRoute(t *testing.T) {
	h, _, d := newAPI(t)

	rr := do(h, "PUT", "/v1/guard/name/website/app.deploy?env=prod", token, `{"disposition":"deny"}`)
	if rr.Code != 200 {
		t.Fatalf("guard set for one app = %d %s", rr.Code, rr.Body.String())
	}
	if got := storedPolicy(t, d).Dispositions[cp.GuardrailCode("prod.website.app.deploy")]; got != cp.DispositionDeny {
		t.Errorf("stored policy = %+v, want a deny under prod.website.app.deploy", storedPolicy(t, d).Dispositions)
	}
	if got := storedPolicy(t, d).Dispositions[cp.GuardrailCode("app.deploy")]; got == cp.DispositionDeny {
		t.Errorf("the deny landed on the wider entry too: %+v", storedPolicy(t, d).Dispositions)
	}

	rr = do(h, "GET", "/v1/guard/name/website?env=prod", token, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"source":"name"`) {
		t.Errorf("guard list for one app = %d %s", rr.Code, rr.Body.String())
	}

	// The engine's refusals are unchanged by the move: a name with no environment is still 400.
	if rr := do(h, "PUT", "/v1/guard/name/website/app.deploy", token, `{"disposition":"deny"}`); rr.Code != 400 {
		t.Errorf("name without env = %d, want 400", rr.Code)
	}
}

// TestGuardEndpointsCarryTheName confirms the scope travels over HTTP as query parameters and that
// the control plane, not the client, is what refuses an illegal combination (ADR-0085 §1). A rule
// enforced only in the CLI would be a rule a second client does not have.
//
// The query form is the shape the first clients of the name tier sent. It stays served because the
// control plane is the compatibility anchor and does not break a client already in the field
// (ADR-0039 §2–§3); current clients send the route form above.
func TestGuardEndpointsCarryTheName(t *testing.T) {
	h, _, d := newAPI(t)

	rr := do(h, "PUT", "/v1/guard/app.deploy?env=prod&name=website", token, `{"disposition":"deny"}`)
	if rr.Code != 200 {
		t.Fatalf("guard set for one app = %d %s", rr.Code, rr.Body.String())
	}
	if got := storedPolicy(t, d).Dispositions[cp.GuardrailCode("prod.website.app.deploy")]; got != cp.DispositionDeny {
		t.Errorf("stored policy = %+v, want a deny under prod.website.app.deploy", storedPolicy(t, d).Dispositions)
	}

	// The listing for that app reports which tier answered, and the app's neighbours are untouched.
	rr = do(h, "GET", "/v1/guard?env=prod&name=website", token, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"source":"name"`) {
		t.Errorf("guard list for one app = %d %s", rr.Code, rr.Body.String())
	}
	rr = do(h, "GET", "/v1/guard?env=prod&name=other", token, "")
	if rr.Code != 200 || strings.Contains(rr.Body.String(), `"source":"name"`) {
		t.Errorf("another app's listing = %d %s, want nothing set for it", rr.Code, rr.Body.String())
	}

	// A name with no environment is refused at the server, as ErrInvalid -> 400.
	if rr := do(h, "PUT", "/v1/guard/app.deploy?name=website", token, `{"disposition":"deny"}`); rr.Code != 400 {
		t.Errorf("name without env = %d, want 400", rr.Code)
	}
	// So is a name on a guardrail whose effect is wider than one thing.
	if rr := do(h, "PUT", "/v1/guard/dns.write?env=prod&name=website", token, `{"disposition":"allow"}`); rr.Code != 400 {
		t.Errorf("name on dns.write = %d, want 400", rr.Code)
	}
}

// TestGuardEndpointsEnvScoped confirms the guard endpoints carry the optional env query through to the
// engine: a registered env scopes the set, an unknown env is 404, and a cluster-level guardrail
// cannot be env-scoped (400) (ADR-0035 phase 2c).
func TestGuardEndpointsEnvScoped(t *testing.T) {
	h, _, d := newAPI(t)
	if err := d.CreateEnvironment(context.Background(), "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	// Scope app.delete to staging: the response reflects the env-specific disposition with its source.
	rr := do(h, "PUT", "/v1/guard/app.delete?env=staging", token, `{"disposition":"deny"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"source":"env"`) {
		t.Fatalf("env guard set = %d %s", rr.Code, rr.Body.String())
	}
	// The global policy is untouched: a plain list does not carry the env source.
	if rr := do(h, "GET", "/v1/guard", token, ""); rr.Code != 200 || strings.Contains(rr.Body.String(), `"source"`) {
		t.Errorf("global guard list leaked an env source = %d %s", rr.Code, rr.Body.String())
	}
	// An unknown environment is a 404.
	if rr := do(h, "PUT", "/v1/guard/app.delete?env=ghost", token, `{"disposition":"deny"}`); rr.Code != 404 {
		t.Errorf("unknown env code = %d, want 404", rr.Code)
	}
	if rr := do(h, "GET", "/v1/guard?env=ghost", token, ""); rr.Code != 404 {
		t.Errorf("unknown env list code = %d, want 404", rr.Code)
	}
	// A cluster-level guardrail cannot be env-scoped (400).
	if rr := do(h, "PUT", "/v1/guard/addon.install?env=staging", token, `{"disposition":"deny"}`); rr.Code != 400 {
		t.Errorf("cluster-level env scope code = %d, want 400", rr.Code)
	}
}

// TestAutoDeployEndpoints covers the auto-deploy get/set API (ADR-0052 §6): the default reads back
// with no row, a set is reflected, an unknown level is a 400, the optional env query routes through to
// the engine, and an unknown environment is a 404.
func TestAutoDeployEndpoints(t *testing.T) {
	h, _, d := newAPI(t)

	// A brand-new app reads the built-in default (off — auto-deploy is opt-in, ADR-0058), keyed to the
	// default environment.
	rr := do(h, "GET", "/v1/apps/web/auto-deploy", token, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"level":"off"`) || !strings.Contains(rr.Body.String(), `"env":"`+cp.DefaultEnvironment+`"`) {
		t.Fatalf("auto-deploy get = %d %s", rr.Code, rr.Body.String())
	}

	// A valid set is reflected in the response.
	rr = do(h, "PUT", "/v1/apps/web/auto-deploy", token, `{"level":"off"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"level":"off"`) {
		t.Fatalf("auto-deploy set = %d %s", rr.Code, rr.Body.String())
	}
	if rr := do(h, "GET", "/v1/apps/web/auto-deploy", token, ""); !strings.Contains(rr.Body.String(), `"level":"off"`) {
		t.Errorf("auto-deploy get after set = %s", rr.Body.String())
	}

	// An unknown level is a 400 (ParseAutoDeployLevel rejects it at the boundary).
	if rr := do(h, "PUT", "/v1/apps/web/auto-deploy", token, `{"level":"sometimes"}`); rr.Code != 400 {
		t.Errorf("unknown level code = %d, want 400", rr.Code)
	}

	// The optional env query routes through to a registered environment, independent of the default.
	if err := d.CreateEnvironment(context.Background(), "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	rr = do(h, "PUT", "/v1/apps/web/auto-deploy?env=staging", token, `{"level":"patch"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"level":"patch"`) || !strings.Contains(rr.Body.String(), `"env":"staging"`) {
		t.Fatalf("env-scoped auto-deploy set = %d %s", rr.Code, rr.Body.String())
	}
	// The default environment is untouched by the staging set.
	if rr := do(h, "GET", "/v1/apps/web/auto-deploy", token, ""); !strings.Contains(rr.Body.String(), `"level":"off"`) {
		t.Errorf("default env level after staging set = %s", rr.Body.String())
	}
	// An unknown environment is a 404.
	if rr := do(h, "PUT", "/v1/apps/web/auto-deploy?env=ghost", token, `{"level":"patch"}`); rr.Code != 404 {
		t.Errorf("unknown env code = %d, want 404", rr.Code)
	}
	if rr := do(h, "GET", "/v1/apps/web/auto-deploy?env=ghost", token, ""); rr.Code != 404 {
		t.Errorf("unknown env get code = %d, want 404", rr.Code)
	}
}

// newProviderAPI builds an API whose engine exposes the credential store and DNS factory, so
// the provider-endpoint test can seed the token the CLI would have written and control the
// vendor's verdict.
func newProviderAPI(t *testing.T) (http.Handler, *fake.Credentials, *fake.DNSFactory) {
	t.Helper()
	d := fake.NewDatabase()
	// dns.write defaults to confirm. dns.delete defaults to DENY (ADR-0065 §3), which no --confirm
	// can open, so the removal leg of TestDomainEndpoints — a route-wiring test, not a policy one —
	// carries the confirm an operator would have set to make the verb reachable at all.
	d.SetPolicy(cp.DefaultPolicy().With(cp.GuardrailDNSDelete, cp.DispositionConfirm))
	creds := fake.NewCredentials()
	dnsF := fake.NewDNSFactory()
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: d,
		Clock: fake.NewClock(time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: creds, DNS: dnsF,
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	h, err := api.New(api.Config{Engine: e, Token: token})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h, creds, dnsF
}

func TestProviderEndpoints(t *testing.T) {
	h, creds, dnsF := newProviderAPI(t)

	// Add a provider: the token VALUE travels in the BODY (never the path or query), is validated,
	// then written into the credential store. The response carries the Secret key, never the value.
	rr := do(h, "POST", "/v1/providers", token, `{"type":"digitalocean","token":"dop_v1_tok"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"capabilities":["dns"]`) {
		t.Fatalf("add provider = %d %s", rr.Code, rr.Body.String())
	}
	// The response must NOT echo the token value back.
	if strings.Contains(rr.Body.String(), "dop_v1_tok") {
		t.Errorf("provider-add response leaked the token value: %s", rr.Body.String())
	}
	// burrowd wrote the token into the credential store under the provider key.
	if tok, ok := creds.Get("digitalocean"); !ok || tok != "dop_v1_tok" {
		t.Errorf("credential store has %q ok=%v, want dop_v1_tok true", tok, ok)
	}

	// List shows it.
	if rr := do(h, "GET", "/v1/providers", token, ""); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"name":"digitalocean"`) {
		t.Fatalf("list providers = %d %s", rr.Code, rr.Body.String())
	}

	// An unsupported type is a 400 (ErrInvalid).
	if rr := do(h, "POST", "/v1/providers", token, `{"type":"aws","token":"x"}`); rr.Code != 400 {
		t.Errorf("unknown type code = %d, want 400", rr.Code)
	}

	// A token the vendor rejects is a 400, and nothing is recorded.
	dnsF.SetVerifyError(fmt.Errorf("rejected: %w", cp.ErrInvalid))
	if rr := do(h, "POST", "/v1/providers", token, `{"type":"cloudflare","token":"bad"}`); rr.Code != 400 {
		t.Errorf("rejected token code = %d, want 400", rr.Code)
	}
	if _, ok := creds.Get("cloudflare"); ok {
		t.Errorf("a rejected token must not be written to the credential store")
	}

	// The endpoints require the token like every other /v1 route.
	if rr := do(h, "GET", "/v1/providers", "", ""); rr.Code != 401 {
		t.Errorf("unauthenticated list code = %d, want 401", rr.Code)
	}
}

// TestConnectAddonAuthEndpointTakesTokenInBody connects an authenticated backend and asserts the
// bearer token VALUE travels in the BODY (never the path or query), is written into the credential
// store, and is not echoed back in the response (ADR-0030).
func TestConnectAddonAuthEndpointTakesTokenInBody(t *testing.T) {
	h, creds, _ := newProviderAPI(t)

	rr := do(h, "POST", "/v1/addons/connect", token,
		`{"backend":"loki","endpoint":"loki.svc:3100","secret_key":"addon-loki","token":"s3cr3t"}`)
	if rr.Code != 200 {
		t.Fatalf("connect addon = %d %s", rr.Code, rr.Body.String())
	}
	// The response (the recorded AddonInfo) carries the key, never the token value.
	if strings.Contains(rr.Body.String(), "s3cr3t") {
		t.Errorf("connect response leaked the token value: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"secret_key":"addon-loki"`) {
		t.Errorf("connect response should record the secret key: %s", rr.Body.String())
	}
	// burrowd wrote the token into the credential store under the key.
	if tok, ok := creds.Get("addon-loki"); !ok || tok != "s3cr3t" {
		t.Errorf("credential store has %q ok=%v, want s3cr3t true", tok, ok)
	}
}

func TestDomainEndpoints(t *testing.T) {
	h, _, _ := newProviderAPI(t)
	if rr := do(h, "POST", "/v1/providers", token, `{"type":"digitalocean","token":"tok"}`); rr.Code != 200 {
		t.Fatalf("register provider = %d %s", rr.Code, rr.Body.String())
	}

	// Add with confirm succeeds and reports the inferred record type.
	rr := do(h, "POST", "/v1/domains", token, `{"host":"app.example.com","provider":"digitalocean","address":"203.0.113.5","confirm":true}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"type":"A"`) {
		t.Fatalf("add domain = %d %s", rr.Code, rr.Body.String())
	}

	// Without confirm the dns.write guardrail holds it (422, needs confirmation).
	rr = do(h, "POST", "/v1/domains", token, `{"host":"x.example.com","provider":"digitalocean","address":"203.0.113.6"}`)
	if rr.Code != 422 || !strings.Contains(rr.Body.String(), `"needs_confirmation":true`) {
		t.Errorf("unconfirmed add = %d %s, want 422 needs_confirmation", rr.Code, rr.Body.String())
	}

	// Remove via DELETE with provider + confirm in the query.
	if rr := do(h, "DELETE", "/v1/domains/app.example.com?provider=digitalocean&confirm=true", token, ""); rr.Code != 200 {
		t.Errorf("remove domain = %d %s", rr.Code, rr.Body.String())
	}

	// Authenticated like every other /v1 route.
	if rr := do(h, "POST", "/v1/domains", "", `{}`); rr.Code != 401 {
		t.Errorf("unauthenticated add code = %d, want 401", rr.Code)
	}
}

// TestEnvironmentEndpoints exercises the environment registry routes: add, list (default first),
// and remove, plus the refusals (removing the implicit default, and authentication).
func TestEnvironmentEndpoints(t *testing.T) {
	h, _, _ := newAPI(t)

	if rr := do(h, "POST", "/v1/environments", token, `{"name":"staging","namespace":"burrow-apps-staging"}`); rr.Code != 200 {
		t.Fatalf("add environment = %d %s", rr.Code, rr.Body.String())
	}

	// List returns the default environment first, then the one added later.
	rr := do(h, "GET", "/v1/environments", token, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"staging"`) {
		t.Fatalf("list environments = %d %s", rr.Code, rr.Body.String())
	}

	// Remove via DELETE with the name in the path.
	if rr := do(h, "DELETE", "/v1/environments/staging", token, ""); rr.Code != 200 {
		t.Errorf("remove environment = %d %s", rr.Code, rr.Body.String())
	}
	// After removal only the default environment remains.
	rr = do(h, "GET", "/v1/environments", token, "")
	if rr.Code != 200 || strings.Contains(rr.Body.String(), `"staging"`) {
		t.Errorf("list after remove = %d %s, want no staging", rr.Code, rr.Body.String())
	}
	// Removing the environment install created is refused (400 ErrInvalid).
	if rr := do(h, "DELETE", "/v1/environments/"+cp.DefaultEnvironment, token, ""); rr.Code != 400 {
		t.Errorf("remove %s = %d %s, want 400", cp.DefaultEnvironment, rr.Code, rr.Body.String())
	}
	// Removing an unregistered environment is 404.
	if rr := do(h, "DELETE", "/v1/environments/nope", token, ""); rr.Code != 404 {
		t.Errorf("remove unknown = %d %s, want 404", rr.Code, rr.Body.String())
	}
	// Authenticated like every other /v1 route.
	if rr := do(h, "DELETE", "/v1/environments/staging", "", ""); rr.Code != 401 {
		t.Errorf("unauthenticated remove = %d, want 401", rr.Code)
	}
}

// TestAuditEndpoint exercises the read path: a deploy records audit rows, and GET /v1/audit
// returns them newest-first, with the app/operation/outcome filters applied.
func TestAuditEndpoint(t *testing.T) {
	h, _, _ := newAPI(t)
	if rr := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"registry.example.com/web:1","replicas":2}`); rr.Code != 200 {
		t.Fatalf("deploy = %d %s", rr.Code, rr.Body.String())
	}

	rec := do(h, "GET", "/v1/audit?app=web", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Entries []cp.AuditEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Entries) != 2 {
		t.Fatalf("audit entries = %d, want 2 (allowed + executed)", len(out.Entries))
	}
	// Newest first: the executed row precedes the allowed decision.
	if out.Entries[0].Outcome != cp.AuditExecuted || out.Entries[1].Outcome != cp.AuditAllowed {
		t.Errorf("outcomes = %s,%s, want executed,allowed (newest first)", out.Entries[0].Outcome, out.Entries[1].Outcome)
	}

	// Outcome filter narrows to one.
	rec = do(h, "GET", "/v1/audit?app=web&outcome=executed", token, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Entries) != 1 || out.Entries[0].Outcome != cp.AuditExecuted {
		t.Errorf("outcome filter returned %d rows, want 1 executed", len(out.Entries))
	}

	// A bad limit is a 400.
	if rr := do(h, "GET", "/v1/audit?limit=nope", token, ""); rr.Code != http.StatusBadRequest {
		t.Errorf("bad limit = %d, want 400", rr.Code)
	}
}

// TestFailuresEndpoint exercises GET /v1/failures (ADR-0074 §8): the ledger rows come back oldest
// first and UNGROUPED — the grouping is the human listing's presentation, and §5 keeps it off the
// wire so an agent correlates on its own terms — with the observation coverage attached to every
// answer so a gap cannot be read as health.
func TestFailuresEndpoint(t *testing.T) {
	h, _, d := newAPI(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	for i, name := range []string{"api", "web"} {
		if err := d.RecordFailure(ctx, cp.FailureObservation{
			Object: cp.ObjectRef{Kind: cp.FailureApp, Name: name, Environment: "prod"},
			Reason: cp.ReasonUnschedulable, Detail: "no node could run it",
			At: now.Add(-time.Duration(30-i) * time.Minute),
		}); err != nil {
			t.Fatalf("RecordFailure(%s): %v", name, err)
		}
	}

	rec := do(h, "GET", "/v1/failures", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("failures = %d %s", rec.Code, rec.Body.String())
	}
	var out client.FailureReport
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Failures) != 2 {
		t.Fatalf("failures = %d, want both rows — one cause, two addressable rows", len(out.Failures))
	}
	if out.Failures[0].Object.Name != "api" || out.Failures[0].FirstSeen.After(out.Failures[1].FirstSeen) {
		t.Errorf("rows = %+v, want oldest first", out.Failures)
	}
	// Nothing has observed, so the coverage says so rather than leaving the list looking whole.
	if len(out.Coverage.Gaps) == 0 || out.Coverage.Observed() {
		t.Errorf("coverage = %+v, want it to report that nothing was watching", out.Coverage)
	}

	// The kind filter narrows; an unknown kind or reason is a 400, not an empty list.
	rec = do(h, "GET", "/v1/failures?kind=addon", token, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Failures) != 0 {
		t.Errorf("kind=addon returned %d rows, want none", len(out.Failures))
	}
	for _, path := range []string{"/v1/failures?kind=deployment", "/v1/failures?reason=ContainerCreating", "/v1/failures?since=yesterday", "/v1/failures?limit=nope"} {
		if rr := do(h, "GET", path, token, ""); rr.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", path, rr.Code)
		}
	}

	// `since` is a duration resolved against the control plane's own clock, and it widens the
	// answer to failures that have since recovered.
	if err := d.ResolveFailures(ctx, now.Add(-10*time.Minute), nil, nil); err != nil {
		t.Fatalf("ResolveFailures: %v", err)
	}
	rec = do(h, "GET", "/v1/failures", token, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Failures) != 0 {
		t.Fatalf("default listing returned %d resolved rows, want only what is still broken", len(out.Failures))
	}
	rec = do(h, "GET", "/v1/failures?since=2h", token, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Failures) != 2 {
		t.Fatalf("since=2h returned %d rows, want the two resolved episodes", len(out.Failures))
	}
}

// TestHistoryEndpoint exercises GET /v1/apps/{app}/history: two deploys produce a two-entry timeline,
// newest first, and an app with no releases returns an empty list rather than an error. An unknown
// environment is a 404.
func TestHistoryEndpoint(t *testing.T) {
	h, _, _ := newAPI(t)
	for _, img := range []string{"registry.example.com/web:1", "registry.example.com/web:2"} {
		if rr := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"`+img+`","replicas":1}`); rr.Code != 200 {
			t.Fatalf("deploy %s = %d %s", img, rr.Code, rr.Body.String())
		}
	}

	rec := do(h, "GET", "/v1/apps/web/history", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("history = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Releases []cp.Release `json:"releases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Releases) != 2 {
		t.Fatalf("history releases = %d, want 2", len(out.Releases))
	}
	// Newest first: the second deploy (web:2, deployed) precedes the first (web:1, superseded).
	if out.Releases[0].Image != "registry.example.com/web:2" || out.Releases[0].Status != cp.ReleaseDeployed {
		t.Errorf("newest = %+v, want the deployed web:2", out.Releases[0])
	}
	if out.Releases[1].Image != "registry.example.com/web:1" || out.Releases[1].Status != cp.ReleaseSuperseded {
		t.Errorf("oldest = %+v, want the superseded web:1", out.Releases[1])
	}

	// An app with no releases is an empty timeline, not a 404.
	rec = do(h, "GET", "/v1/apps/ghost/history", token, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || len(out.Releases) != 0 {
		t.Errorf("history(ghost) = %d %s, want 200 with an empty list", rec.Code, rec.Body.String())
	}

	// An unknown environment is a 404 (the engine resolves it before reading).
	if rr := do(h, "GET", "/v1/apps/web/history?env=nope", token, ""); rr.Code != http.StatusNotFound {
		t.Errorf("history(unknown env) = %d, want 404", rr.Code)
	}
}

// TestAuditPrincipalCrossStructRoundTrip guards the json-tag-must-match contract (ADR-0038): the
// engine marshals cp.AuditEntry on the wire and the client decodes into its own AuditEntry, so a
// mismatched `principal` tag on either struct would silently drop the field. The engine records
// the shared-agent principal today; this asserts it survives the engine→wire→client hop.
func TestAuditPrincipalCrossStructRoundTrip(t *testing.T) {
	h, _, _ := newAPI(t)
	if rr := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"registry.example.com/web:1","replicas":2}`); rr.Code != 200 {
		t.Fatalf("deploy = %d %s", rr.Code, rr.Body.String())
	}

	rec := do(h, "GET", "/v1/audit?app=web", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit = %d %s", rec.Code, rec.Body.String())
	}
	// Decode the engine-produced JSON through the CLIENT's struct — the deserialization side of
	// the contract. If the tags disagreed, Principal would come back empty here.
	var out struct {
		Entries []client.AuditEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode into client struct: %v", err)
	}
	if len(out.Entries) == 0 {
		t.Fatal("expected audit entries")
	}
	for i, e := range out.Entries {
		if e.Principal != "shared-agent" {
			t.Errorf("client entry[%d] principal = %q, want shared-agent (json tag mismatch drops it)", i, e.Principal)
		}
		if e.Caller != "control-plane" {
			t.Errorf("client entry[%d] caller = %q, want control-plane (distinct from principal)", i, e.Caller)
		}
	}
}

// TestEnvironmentEndpointsRoundTrip exercises POST/GET /v1/environments through the typed client
// against a live httptest server (ADR-0035 phase 2a): registering an environment and listing it,
// with the implicit default first.
func TestEnvironmentEndpointsRoundTrip(t *testing.T) {
	d := fake.NewDatabase()
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: d,
		Clock: fake.NewClock(time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
		AppNamespace: "burrow-apps",
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	h, err := api.New(api.Config{Engine: e, Token: token})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := client.NewClient(srv.URL, token)
	ctx := context.Background()

	if err := c.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	// A duplicate is rejected (ErrInvalid -> 400).
	if err := c.AddEnvironment(ctx, "staging", "other"); err == nil {
		t.Errorf("duplicate AddEnvironment should error")
	}

	envs, err := c.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(envs) != 2 {
		t.Fatalf("environments = %+v, want 2 (the default environment + staging)", envs)
	}
	if envs[0].Name != cp.DefaultEnvironment || !envs[0].Default || envs[0].Namespace != "burrow-apps" {
		t.Errorf("first environment should be the default in the app namespace: %+v", envs[0])
	}
	if envs[1].Name != "staging" || envs[1].Default || envs[1].Namespace != "burrow-apps-staging" {
		t.Errorf("registered environment wrong: %+v", envs[1])
	}
}

func do(h http.Handler, method, path, tok, body string) *httptest.ResponseRecorder {
	var br io.Reader
	if body != "" {
		br = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, br)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type errBody struct {
	Error             string `json:"error"`
	Code              string `json:"code"`
	Requested         *int32 `json:"requested"`
	Limit             *int32 `json:"limit"`
	NeedsConfirmation bool   `json:"needs_confirmation"`
	ServerInstallID   string `json:"server_install_id"`
	ServerVersion     string `json:"server_version"`
}

func TestHealthNoAuth(t *testing.T) {
	h, _, _ := newAPI(t)
	rec := do(h, "GET", "/healthz", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", rec.Code)
	}
}

func TestAuthRequired(t *testing.T) {
	h, _, _ := newAPI(t)
	if rec := do(h, "GET", "/v1/apps/web/status", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rec.Code)
	}
	if rec := do(h, "GET", "/v1/apps/web/status", "wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", rec.Code)
	}
}

func TestAuthViaCustomHeader(t *testing.T) {
	h, _, _ := newAPI(t)
	// X-Burrow-Token (no Authorization) is accepted — the header that survives the
	// API-server proxy (ADR-0014).
	req := httptest.NewRequest("POST", "/v1/apps/web/deploy", strings.NewReader(`{"image":"img:1","replicas":2}`))
	req.Header.Set("X-Burrow-Token", token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("X-Burrow-Token auth: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestDeployHappyPath(t *testing.T) {
	h, k, _ := newAPI(t)

	rec := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"registry.example.com/web:1","replicas":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res cp.DeployResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Release.Status != cp.ReleaseDeployed {
		t.Errorf("release status = %q, want deployed", res.Release.Status)
	}
	if res.Release.App != "web" {
		t.Errorf("release app = %q, want web (from the path)", res.Release.App)
	}
	if res.Release.Digest != "" {
		t.Errorf("digest = %q, want empty (burrowd does not resolve; ADR-0040)", res.Release.Digest)
	}
	if spec, ok := k.Spec("web"); !ok || spec.Image != "registry.example.com/web:1" || spec.Replicas != 2 {
		t.Errorf("cluster spec = %+v ok=%v", spec, ok)
	}
}

func TestDeployBadRequest(t *testing.T) {
	h, _, _ := newAPI(t)
	// Missing image is a malformed request.
	rec := do(h, "POST", "/v1/apps/web/deploy", token, `{"replicas":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	// Invalid JSON is also 400.
	if rec := do(h, "POST", "/v1/apps/web/deploy", token, `{not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON status = %d, want 400", rec.Code)
	}
}

// TestDeployOverReplicaCeiling covers the wire shape of an operational-limit refusal (ADR-0068 §2):
// 422 with the limit's code and the requested/limit pair, and — unlike a guardrail hold —
// needs_confirmation absent, because there is nothing a caller can confirm.
func TestDeployOverReplicaCeiling(t *testing.T) {
	h, _, _ := newAPI(t)
	rec := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":9}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	var e errBody
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Code != string(cp.LimitReplicaCeiling) {
		t.Errorf("code = %q, want %q", e.Code, cp.LimitReplicaCeiling)
	}
	if e.Limit == nil || *e.Limit != 5 {
		t.Errorf("limit = %v, want 5", e.Limit)
	}
	if e.Requested == nil || *e.Requested != 9 {
		t.Errorf("requested = %v, want 9", e.Requested)
	}
	if e.NeedsConfirmation {
		t.Errorf("a limit refusal must not offer a confirmation: %s", rec.Body.String())
	}
	// The refusal names the command that raises the bound, so the agent can relay it.
	if !strings.Contains(rec.Body.String(), "burrow cluster config set") {
		t.Errorf("body = %s, want the operator command that raises the limit", rec.Body.String())
	}
}

// TestLimitEndpoints covers the operational-configuration surface: listing reports every limit with
// its effective value and the tier it came from, a set raises the bound and is visible to the very
// next operation, and a bad value or an unknown code is refused rather than stored (ADR-0068).
func TestLimitEndpoints(t *testing.T) {
	h, _, _ := newAPI(t)

	rec := do(h, "GET", "/v1/config", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("config list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Limits []cp.LimitInfo `json:"limits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Limits) == 0 {
		t.Fatalf("config list returned no limits")
	}
	found := false
	for _, l := range list.Limits {
		if l.Code != cp.LimitReplicaCeiling {
			continue
		}
		found = true
		if l.Value != "5" || l.Scope != cp.LimitScopeCluster {
			t.Errorf("replica ceiling = (%q, %q), want (5, cluster)", l.Value, l.Scope)
		}
		if l.Default != "50" {
			t.Errorf("replica ceiling default = %q, want 50", l.Default)
		}
	}
	if !found {
		t.Errorf("config list omitted %s", cp.LimitReplicaCeiling)
	}

	// Raising the bound is the whole point: the guardrail this replaced could be turned off but
	// never turned up (ADR-0068 §2).
	if rec := do(h, "PUT", "/v1/config/app.replica_ceiling", token, `{"value":"80"}`); rec.Code != http.StatusOK {
		t.Fatalf("config set status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":9}`); rec.Code != http.StatusOK {
		t.Fatalf("deploy after raising the ceiling = %d, body = %s", rec.Code, rec.Body.String())
	}

	// A value that is not a whole number, and an unknown code, are both refused as invalid.
	if rec := do(h, "PUT", "/v1/config/app.replica_ceiling", token, `{"value":"lots"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("non-numeric value status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "PUT", "/v1/config/app.made_up", token, `{"value":"3"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown limit status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

// TestGuardSetRejectsALimitCode pins the upgrade path a human hits: `guard set app.replica_ceiling`
// used to work, and now names the surface that carries the bound instead of failing opaquely
// (ADR-0068 §2).
func TestGuardSetRejectsALimitCode(t *testing.T) {
	h, _, _ := newAPI(t)
	rec := do(h, "PUT", "/v1/guard/app.replica_ceiling", token, `{"disposition":"allow"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "burrow cluster config set") {
		t.Errorf("body = %s, want it to name the command that sets the limit", rec.Body.String())
	}
}

// TestBuildEndpoint confirms POST /v1/apps/{app}/build clones the git ref, builds inside the cluster
// via the Builder seam, and hands the digest-pinned reference into the guarded deploy path (ADR-0053):
// the release is recorded with the pinned image, the workload is applied with it, and the builder saw
// only the git ref and target image — never source bytes.
func TestBuildEndpoint(t *testing.T) {
	k, d := fake.NewKubernetes(), fake.NewDatabase()
	d.SetPolicy(cp.Policy{}.With(cp.GuardrailAppDeploy, cp.DispositionAllow))
	b := fake.NewBuilder()
	b.SetDigest("sha256:abc123")
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d,
		Clock:       fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)),
		IDs:         fake.NewIDs(),
		Resolver:    fake.NewResolver(),
		Credentials: fake.NewCredentials(),
		DNS:         fake.NewDNSFactory(),
		Builder:     b,
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	h, err := api.New(api.Config{Engine: e, Token: token})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	body := `{"source":{"Repo":"https://github.com/acme/web","Ref":"v1.2.3"},"target_image":"ghcr.io/acme/web:1.2.3"}`
	rec := do(h, "POST", "/v1/apps/web/build", token, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("build status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res cp.BuildResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Digest != "sha256:abc123" {
		t.Errorf("digest = %q, want sha256:abc123", res.Digest)
	}
	wantImage := "ghcr.io/acme/web:1.2.3@sha256:abc123"
	if res.Deploy.Release.Image != wantImage {
		t.Errorf("deployed image = %q, want %q", res.Deploy.Release.Image, wantImage)
	}
	if res.Deploy.Release.App != "web" {
		t.Errorf("release app = %q, want web (from the path)", res.Deploy.Release.App)
	}
	if spec, ok := k.Spec("web"); !ok || spec.Image != wantImage {
		t.Errorf("cluster spec = %+v ok=%v", spec, ok)
	}
	if got := b.LastSource(); got.Repo != "https://github.com/acme/web" || got.Ref != "v1.2.3" {
		t.Errorf("builder source = %+v, want the git ref", got)
	}
	if got := b.LastTarget(); got != "ghcr.io/acme/web:1.2.3" {
		t.Errorf("builder target = %q, want ghcr.io/acme/web:1.2.3", got)
	}
}

// TestBuildEndpointNotConfigured confirms an engine with no Builder wired reports the in-cluster build
// path as a 501 not_implemented rather than crashing (ADR-0053 §6): the seam is optional.
func TestBuildEndpointNotConfigured(t *testing.T) {
	h, _, _ := newAPI(t) // newAPI wires no Builder
	body := `{"source":{"Repo":"https://github.com/acme/web","Ref":"v1.2.3"},"target_image":"ghcr.io/acme/web:1"}`
	rec := do(h, "POST", "/v1/apps/web/build", token, body)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body = %s", rec.Code, rec.Body.String())
	}
}

// TestBuildEndpointBadRequest confirms a malformed build request — an incomplete git reference, or
// invalid JSON — is a 400, rejected before any builder or deploy runs (ADR-0053 §3).
func TestBuildEndpointBadRequest(t *testing.T) {
	h, _, _ := newAPI(t)
	// A source with a repo but no ref is malformed.
	rec := do(h, "POST", "/v1/apps/web/build", token, `{"source":{"Repo":"https://github.com/acme/web"},"target_image":"img:1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("incomplete-ref status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "POST", "/v1/apps/web/build", token, `{not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON status = %d, want 400", rec.Code)
	}
}

func TestStatus(t *testing.T) {
	h, _, _ := newAPI(t)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":3}`)

	rec := do(h, "GET", "/v1/apps/web/status", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res cp.StatusResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if !res.HasRelease || !res.Running || res.Workload.DesiredReplicas != 3 {
		t.Errorf("status result = %+v", res)
	}
}

func TestStatusUnknown(t *testing.T) {
	h, _, _ := newAPI(t)
	if rec := do(h, "GET", "/v1/apps/ghost/status", token, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestScaleAndGuardrail(t *testing.T) {
	h, _, _ := newAPI(t)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":2}`)

	rec := do(h, "POST", "/v1/apps/web/scale", token, `{"replicas":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("scale status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res cp.ScaleResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.PreviousReplicas != 2 || res.Replicas != 4 {
		t.Errorf("scale result = %+v, want prev 2 new 4", res)
	}

	// Scale to zero is refused by policy.
	if rec := do(h, "POST", "/v1/apps/web/scale", token, `{"replicas":0}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("scale-to-zero status = %d, want 422", rec.Code)
	}
}

func TestExposeEndpoints(t *testing.T) {
	h, _, d := newAPI(t)
	d.SetPolicy(cp.DefaultPolicy().With(cp.GuardrailExposePublic, cp.DispositionAllow))
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)

	rec := do(h, "POST", "/v1/apps/web/expose", token, `{"host":"web.example.com","port":8080}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "web.example.com") {
		t.Fatalf("expose = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "POST", "/v1/apps/web/unexpose", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("unexpose = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "POST", "/v1/apps/web/unexpose", token, ""); rec.Code != http.StatusNotFound {
		t.Errorf("second unexpose = %d, want 404", rec.Code)
	}
}

func TestReachabilityEndpoint(t *testing.T) {
	h, _, _ := newAPI(t)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	rec := do(h, "GET", "/v1/apps/web/reachability", token, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not exposed") {
		t.Fatalf("reachability = %d %s", rec.Code, rec.Body.String())
	}
}

func TestExposeGuardrailHolds(t *testing.T) {
	h, _, _ := newAPI(t)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	// newAPI leaves app.expose_public unset → deny, so exposure is refused (422 guardrail).
	if rec := do(h, "POST", "/v1/apps/web/expose", token, `{"host":"web.example.com","port":8080}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("expose code = %d, want 422", rec.Code)
	}
}

// TestExposeMissingPrerequisitesEndpoint confirms an expose against a cluster missing its
// prerequisites surfaces as a 422 with the machine-readable "missing_prerequisites" code and the
// actionable checklist in the body (ADR-0006), so the agent gets the guidance over the API.
func TestExposeMissingPrerequisitesEndpoint(t *testing.T) {
	k, d := fake.NewKubernetes(), fake.NewDatabase()
	d.SetPolicy(cp.DefaultPolicy().With(cp.GuardrailAppDeploy, cp.DispositionAllow).With(cp.GuardrailExposePublic, cp.DispositionAllow))
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d,
		Clock:       fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)),
		IDs:         fake.NewIDs(),
		Resolver:    fake.NewResolver(),
		Credentials: fake.NewCredentials(),
		DNS:         fake.NewDNSFactory(),
		// An empty capability report models a cluster with no ingress controller and no cert-manager.
		ClusterProber: fake.NewClusterProber(cp.ClusterCapabilities{}),
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	h, err := api.New(api.Config{Engine: e, Token: token})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)

	rec := do(h, "POST", "/v1/apps/web/expose", token, `{"host":"web.example.com","port":8080,"tls":true,"issuer":"letsencrypt"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expose code = %d, want 422; body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"missing_prerequisites", "cert-manager", "ingress controller", "burrow cluster ingress install"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// TestCapacityEndpoint confirms GET /v1/cluster/capacity returns the scheduling-headroom report the
// CapacityProber feeds the engine: per-node and cluster totals, and the plain-language verdict.
func TestCapacityEndpoint(t *testing.T) {
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: fake.NewDatabase(),
		Clock: fake.NewClock(time.Now()), IDs: fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
		CapacityProber: fake.NewCapacityProber(cp.ClusterResourceState{
			Nodes: []cp.NodeAllocatable{{Name: "node-a", CPUMillis: 2000, MemBytes: 4 << 30}},
			Pods:  []cp.PodRequest{{Namespace: "burrow", Name: "burrowd", Node: "node-a", CPUMillis: 100, MemBytes: 128 << 20}},
		}),
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	h, err := api.New(api.Config{Engine: e, Token: token})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	rec := do(h, "GET", "/v1/cluster/capacity", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("capacity code = %d, body %s", rec.Code, rec.Body.String())
	}
	var report cp.CapacityReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(report.Nodes) != 1 || report.Nodes[0].FreeCPUMillis != 1900 {
		t.Errorf("node free CPU = %+v, want 1900m", report.Nodes)
	}
	if !report.BuildFits || report.Verdict == "" {
		t.Errorf("build should fit and verdict be set: %+v", report)
	}
}

func TestRollback(t *testing.T) {
	h, k, _ := newAPI(t)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:2","replicas":1}`)

	rec := do(h, "POST", "/v1/apps/web/rollback", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res cp.RollbackResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Release.Image != "img:1" {
		t.Errorf("rollback image = %q, want img:1", res.Release.Image)
	}
	if spec, _ := k.Spec("web"); spec.Image != "img:1" {
		t.Errorf("cluster image = %q, want img:1", spec.Image)
	}
}

func TestLogs(t *testing.T) {
	h, k, _ := newAPI(t)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	k.SetLogs("web", []cp.LogLine{{Pod: "web-1", Message: "a"}, {Pod: "web-1", Message: "b"}})

	rec := do(h, "GET", "/v1/apps/web/logs?tail=1", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("logs status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Lines []cp.LogLine `json:"lines"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if len(res.Lines) != 1 || res.Lines[0].Message != "b" {
		t.Errorf("lines = %+v, want last line b", res.Lines)
	}
}

func TestConfigEndpoints(t *testing.T) {
	h, k, d := newAPI(t)
	// A config write is guarded (ADR-0098) and this test is about the endpoints, not the guardrail,
	// so allow it here; the hold and the confirm are TestConfigWriteIsHeldUntilConfirmed's subject.
	d.SetPolicy(cp.Policy{}.
		With(cp.GuardrailRollback, cp.DispositionAllow).
		With(cp.GuardrailAppDeploy, cp.DispositionAllow).
		With(cp.GuardrailAppConfig, cp.DispositionAllow))
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)

	// Set rolls the workload by default: the value reaches the live spec.
	if rec := do(h, "POST", "/v1/apps/web/config", token, `{"key":"LOG_LEVEL","value":"debug"}`); rec.Code != http.StatusOK {
		t.Fatalf("config set = %d %s", rec.Code, rec.Body.String())
	}
	if spec, _ := k.Spec("web"); spec.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("spec env = %+v, want LOG_LEVEL=debug after set", spec.Env)
	}

	// no_restart persists without rolling.
	if rec := do(h, "POST", "/v1/apps/web/config", token, `{"key":"FEATURE","value":"on","no_restart":true}`); rec.Code != http.StatusOK {
		t.Fatalf("config set no_restart = %d %s", rec.Code, rec.Body.String())
	}
	if _, present := func() (string, bool) { s, _ := k.Spec("web"); v, ok := s.Env["FEATURE"]; return v, ok }(); present {
		t.Errorf("FEATURE should not be in the live spec until the next deploy")
	}

	// List round-trips both keys.
	rec := do(h, "GET", "/v1/apps/web/config", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("config list = %d %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Config map[string]string `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if listed.Config["LOG_LEVEL"] != "debug" || listed.Config["FEATURE"] != "on" {
		t.Errorf("listed config = %+v, want LOG_LEVEL=debug and FEATURE=on", listed.Config)
	}

	// Unset removes a key and rolls.
	if rec := do(h, "DELETE", "/v1/apps/web/config/LOG_LEVEL", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("config unset = %d %s", rec.Code, rec.Body.String())
	}
	if spec, _ := k.Spec("web"); spec.Env["LOG_LEVEL"] != "" {
		t.Errorf("spec env = %+v, want LOG_LEVEL removed", spec.Env)
	}

	// An invalid config key is a 400.
	if rec := do(h, "POST", "/v1/apps/web/config", token, `{"key":"1BAD","value":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad key = %d, want 400", rec.Code)
	}
}

// TestConfigWriteIsHeldUntilConfirmed covers the app.config guardrail at the endpoints (ADR-0098).
// A held write is a 422 with needs_confirmation and nothing persisted; the same call with confirm
// proceeds. The unset path carries confirm as a query parameter, since a DELETE has no body, so both
// shapes are exercised.
func TestConfigWriteIsHeldUntilConfirmed(t *testing.T) {
	h, _, d := newAPI(t)
	d.SetPolicy(cp.Policy{}.
		With(cp.GuardrailAppDeploy, cp.DispositionAllow).
		With(cp.GuardrailAppConfig, cp.DispositionConfirm))
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)

	rec := do(h, "POST", "/v1/apps/web/config", token, `{"key":"LOG_LEVEL","value":"debug"}`)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"needs_confirmation":true`) {
		t.Fatalf("unconfirmed set = %d %s, want 422 needs_confirmation", rec.Code, rec.Body.String())
	}
	if rec := do(h, "GET", "/v1/apps/web/config", token, ""); strings.Contains(rec.Body.String(), "LOG_LEVEL") {
		t.Errorf("a held write persisted the value: %s", rec.Body.String())
	}

	if rec := do(h, "POST", "/v1/apps/web/config", token, `{"key":"LOG_LEVEL","value":"debug","confirm":true}`); rec.Code != http.StatusOK {
		t.Fatalf("confirmed set = %d %s", rec.Code, rec.Body.String())
	}

	// Unset: held without the query parameter, executed with it.
	if rec := do(h, "DELETE", "/v1/apps/web/config/LOG_LEVEL", token, ""); rec.Code != 422 {
		t.Fatalf("unconfirmed unset = %d %s, want 422", rec.Code, rec.Body.String())
	}
	if rec := do(h, "DELETE", "/v1/apps/web/config/LOG_LEVEL?confirm=true", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("confirmed unset = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSecretEndpoints(t *testing.T) {
	h, k, _ := newAPI(t)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)

	// `secret set` carries a VALUE over the authenticated API; burrowd writes it to the per-app
	// Secret (ADR-0029). Set via the API and assert the value lands in the fake Secret, the
	// response echoes the app+KEY only (never the value), and the running workload rolls.
	rec := do(h, "POST", "/v1/apps/web/secrets", token, `{"key":"STRIPE_KEY","value":"sk_live_x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("secret set = %d %s", rec.Code, rec.Body.String())
	}
	if b := rec.Body.String(); strings.Contains(b, "sk_live_x") {
		t.Fatalf("secret set response leaked the value: %s", b)
	}
	if v, ok := k.SecretValue("web", "STRIPE_KEY"); !ok || v != "sk_live_x" {
		t.Errorf("STRIPE_KEY in fake Secret = %q, %v; want sk_live_x written via the API", v, ok)
	}
	if _, rolled := k.RestartedAt("web"); !rolled {
		t.Error("default secret set should roll the running workload")
	}
	// A no_restart set writes the value but does not roll. Use a fresh app to reset roll state.
	do(h, "POST", "/v1/apps/noroll/deploy", token, `{"image":"img:1","replicas":1}`)
	if rec := do(h, "POST", "/v1/apps/noroll/secrets", token, `{"key":"K","value":"v","no_restart":true}`); rec.Code != http.StatusOK {
		t.Fatalf("secret set no_restart = %d %s", rec.Code, rec.Body.String())
	}
	if v, ok := k.SecretValue("noroll", "K"); !ok || v != "v" {
		t.Errorf("K in fake Secret = %q, %v; want v", v, ok)
	}
	if _, rolled := k.RestartedAt("noroll"); rolled {
		t.Error("no_restart secret set must not roll the workload")
	}
	// An invalid key is a 400 — the value never makes it to the Secret.
	if rec := do(h, "POST", "/v1/apps/web/secrets", token, `{"key":"1BAD","value":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("set bad key = %d, want 400", rec.Code)
	}

	// Seed a second key directly for the list/unset assertions below.
	k.SetSecret("web", "DATABASE_URL", "postgres://y")

	// List returns KEYS only, sorted — never the values.
	rec = do(h, "GET", "/v1/apps/web/secrets", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("secret list = %d %s", rec.Code, rec.Body.String())
	}
	if b := rec.Body.String(); strings.Contains(b, "sk_live_x") || strings.Contains(b, "postgres://y") {
		t.Fatalf("secret list leaked a value: %s", b)
	}
	var listed struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed.Keys) != 2 || listed.Keys[0] != "DATABASE_URL" || listed.Keys[1] != "STRIPE_KEY" {
		t.Errorf("keys = %v, want [DATABASE_URL STRIPE_KEY]", listed.Keys)
	}

	// Unset removes a key and rolls the running workload by default.
	if rec := do(h, "DELETE", "/v1/apps/web/secrets/STRIPE_KEY", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("secret unset = %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := k.SecretValue("web", "STRIPE_KEY"); ok {
		t.Error("STRIPE_KEY should be removed")
	}
	if _, rolled := k.RestartedAt("web"); !rolled {
		t.Error("default unset should roll the workload")
	}

	// no_restart=true removes without rolling. Reset roll state by re-deploying a fresh app.
	do(h, "POST", "/v1/apps/api/deploy", token, `{"image":"img:1","replicas":1}`)
	k.SetSecret("api", "TOKEN", "t")
	if rec := do(h, "DELETE", "/v1/apps/api/secrets/TOKEN?no_restart=true", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("secret unset no_restart = %d %s", rec.Code, rec.Body.String())
	}
	if _, rolled := k.RestartedAt("api"); rolled {
		t.Error("no_restart unset must not roll the workload")
	}

	// An invalid key on unset is a 400 too.
	if rec := do(h, "DELETE", "/v1/apps/web/secrets/1BAD", token, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad key = %d, want 400", rec.Code)
	}
}

func TestNotImplementedMapsTo501(t *testing.T) {
	h, k, _ := newAPI(t)
	// An adapter that is not wired yet surfaces ErrNotImplemented; the API reports 501.
	k.SetError(fake.OpApply, fmt.Errorf("cluster adapter: %w", cp.ErrNotImplemented))
	rec := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body = %s", rec.Code, rec.Body.String())
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h, _, _ := newAPI(t)
	// GET on a POST-only route — the mux returns 405.
	if rec := do(h, "GET", "/v1/apps/web/deploy", token, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestAutoscaleEndpoint(t *testing.T) {
	h, k, d := newAPI(t)
	d.SetPolicy(cp.Policy{}.With(cp.GuardrailAutoscale, cp.DispositionAllow))

	rec := do(h, "POST", "/v1/apps/web/autoscale", token, `{"min":1,"max":4,"cpu":90}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("autoscale status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res cp.AutoscaleResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.MaxReplicas != 4 || res.CPUPercent != 90 {
		t.Errorf("result = %+v, want max 4 cpu 90", res)
	}
	if _, ok := k.Autoscaler("web"); !ok {
		t.Errorf("HPA not applied by the endpoint")
	}

	// DELETE turns autoscaling off.
	if rec := do(h, "DELETE", "/v1/apps/web/autoscale", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok := k.Autoscaler("web"); ok {
		t.Errorf("HPA should be gone after disable")
	}
}

func TestAutoscaleMaxOverCeilingDenied(t *testing.T) {
	h, _, d := newAPI(t)
	d.SetPolicy(cp.Policy{}.With(cp.GuardrailAutoscale, cp.DispositionAllow))
	// A max above the ceiling is refused as an operational limit (422, a structured refusal that
	// no confirmation opens).
	rec := do(h, "POST", "/v1/apps/web/autoscale", token, `{"min":1,"max":50,"cpu":80}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "app.replica_ceiling") {
		t.Errorf("body = %s, want the replica-ceiling code", rec.Body.String())
	}
}

// TestRemoveAddonEndpointDefaultsToKeepingData asserts the wire contract of the removal: delete_data
// is an explicit opt-in, so a DELETE without it keeps the add-on's data volume and the response says
// which volume survived. The safe behaviour is what a caller that forgets the parameter gets
// (ADR-0025/0031).
func TestRemoveAddonEndpointDefaultsToKeepingData(t *testing.T) {
	h, _, _ := newProviderAPI(t)
	if rr := do(h, "POST", "/v1/addons", token, `{"type":"postgres","confirm":true}`); rr.Code != 200 {
		t.Fatalf("install addon = %d %s", rr.Code, rr.Body.String())
	}

	rr := do(h, "DELETE", "/v1/addons/burrow-postgres?confirm=true", token, "")
	if rr.Code != 200 {
		t.Fatalf("remove addon = %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"retained_data_volume":"burrow-postgres-1"`) {
		t.Errorf("removal response does not report the retained data volume: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"data_deleted":false`) {
		t.Errorf("removal without delete_data reports data_deleted true: %s", rr.Body.String())
	}
}

// TestListAddonsReportsRetainedVolumes asserts the wire shape of ADR-0064 §6: the add-on listing
// carries the volumes an earlier removal left behind in their OWN field, so a reader (and an agent
// parsing the JSON) cannot mistake allocated storage for a running add-on. Each entry names the
// add-on it belonged to and its size — the reason the field exists is cost.
func TestListAddonsReportsRetainedVolumes(t *testing.T) {
	h, _, _ := newProviderAPI(t)
	if rr := do(h, "POST", "/v1/addons", token, `{"type":"postgres","confirm":true}`); rr.Code != 200 {
		t.Fatalf("install addon = %d %s", rr.Code, rr.Body.String())
	}
	// While it is installed there is nothing retained, and the field is absent rather than empty.
	if rr := do(h, "GET", "/v1/addons", token, ""); rr.Code != 200 {
		t.Fatalf("list addons = %d %s", rr.Code, rr.Body.String())
	} else if strings.Contains(rr.Body.String(), "retained_volumes") {
		t.Errorf("nothing was removed, so no retained volumes should be reported: %s", rr.Body.String())
	}

	if rr := do(h, "DELETE", "/v1/addons/burrow-postgres?confirm=true", token, ""); rr.Code != 200 {
		t.Fatalf("remove addon = %d %s", rr.Code, rr.Body.String())
	}

	rr := do(h, "GET", "/v1/addons", token, "")
	if rr.Code != 200 {
		t.Fatalf("list addons = %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Addons          []map[string]any `json:"addons"`
		RetainedVolumes []struct {
			Name            string `json:"name"`
			Namespace       string `json:"namespace"`
			Addon           string `json:"addon"`
			Role            string `json:"role"`
			Size            string `json:"size"`
			ReinstallAdopts bool   `json:"reinstall_adopts"`
		} `json:"retained_volumes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the listing: %v (%s)", err, rr.Body.String())
	}
	if len(body.Addons) != 0 {
		t.Errorf("addons = %+v, want none after the removal", body.Addons)
	}
	if len(body.RetainedVolumes) != 1 {
		t.Fatalf("retained_volumes = %+v, want the claim the removal kept", body.RetainedVolumes)
	}
	v := body.RetainedVolumes[0]
	if v.Name != "burrow-postgres-1" || v.Addon != "postgres" || v.Role != "data" {
		t.Errorf("retained volume = %+v, want the postgres data claim", v)
	}
	if v.Size == "" || v.Namespace == "" {
		t.Errorf("retained volume = %+v, want a size and a namespace to act on", v)
	}
	if !v.ReinstallAdopts {
		t.Errorf("retained volume = %+v, want reinstall_adopts true for a data claim", v)
	}
}

// TestRemoveAddonEndpointHonoursDeleteData asserts the opt-in reaches the engine when it is asked for.
func TestRemoveAddonEndpointHonoursDeleteData(t *testing.T) {
	h, _, _ := newProviderAPI(t)
	if rr := do(h, "POST", "/v1/addons", token, `{"type":"postgres","confirm":true}`); rr.Code != 200 {
		t.Fatalf("install addon = %d %s", rr.Code, rr.Body.String())
	}

	rr := do(h, "DELETE", "/v1/addons/burrow-postgres?confirm=true&delete_data=true", token, "")
	if rr.Code != 200 {
		t.Fatalf("remove addon = %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"data_deleted":true`) {
		t.Errorf("delete_data=true did not delete the data: %s", rr.Body.String())
	}
}

// TestBackupHealthEndpoint asserts the ADR-0063 §7 status surface is served, requires auth like
// every other v1 route, defaults the add-on to postgres, and refuses an add-on that has no backups
// with a 4xx rather than a 500.
func TestBackupHealthEndpoint(t *testing.T) {
	h, _, _ := newAPI(t)

	if rr := do(h, "GET", "/v1/addons/backup-health", "", ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated backup-health = %d, want 401", rr.Code)
	}

	rr := do(h, "GET", "/v1/addons/backup-health", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("backup-health = %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Addon   string `json:"addon"`
		State   string `json:"state"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the report: %v (%s)", err, rr.Body.String())
	}
	if body.Addon != string(cp.AddonPostgres) {
		t.Errorf("addon = %q, want the postgres default", body.Addon)
	}
	if body.State != string(cp.BackupHealthNever) {
		t.Errorf("state = %q, want %q with nothing recorded", body.State, cp.BackupHealthNever)
	}
	if body.Summary == "" {
		t.Error("the report carries no summary line")
	}

	if rr := do(h, "GET", "/v1/addons/backup-health?addon=cache", token, ""); rr.Code != http.StatusBadRequest {
		t.Errorf("backup-health for an add-on without backups = %d, want 400 (%s)", rr.Code, rr.Body.String())
	}
}

// TestHealthEndpoints covers the health get/set/unset API (ADR-0076 §3, §5): the conservative
// default reads back for an app that declared nothing, a declaration is reflected, a path that is
// not a pod-local path is a clean 400 at the boundary, and unsetting returns the app to the default.
func TestHealthEndpoints(t *testing.T) {
	h, _, d := newAPI(t)

	// An app that declared nothing and is not published: no probe, and the §5 guidance so an agent
	// reading this learns what to do about it and what the endpoint must not check.
	rr := do(h, "GET", "/v1/apps/web/health", token, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"probe":"none"`) {
		t.Fatalf("health get = %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "never its database") {
		t.Errorf("health get carries no dependency warning: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"liveness":false`) {
		t.Errorf("health get does not report that no liveness probe is set: %s", rr.Body.String())
	}

	// Publishing the app turns on the conservative TCP default with no configuration at all.
	if err := d.RecordExposure(context.Background(), cp.Exposure{App: "web", Environment: cp.DefaultEnvironment, Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("RecordExposure: %v", err)
	}
	rr = do(h, "GET", "/v1/apps/web/health", token, "")
	if !strings.Contains(rr.Body.String(), `"probe":"tcp"`) || !strings.Contains(rr.Body.String(), `"probe_port":8080`) {
		t.Errorf("health get after publishing = %s, want a tcp probe on 8080", rr.Body.String())
	}

	// Declaring an endpoint switches it to an HTTP check.
	rr = do(h, "PUT", "/v1/apps/web/health", token, `{"path":"/healthz"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"probe":"http"`) || !strings.Contains(rr.Body.String(), `"probe_path":"/healthz"`) {
		t.Fatalf("health set = %d %s", rr.Code, rr.Body.String())
	}

	// A path that names another host is refused before it is stored (ADR-0076 §2).
	if rr := do(h, "PUT", "/v1/apps/web/health", token, `{"path":"http://postgres:5432/"}`); rr.Code != 400 {
		t.Errorf("off-pod path code = %d, want 400", rr.Code)
	}
	if rr := do(h, "PUT", "/v1/apps/web/health", token, `{"path":"/healthz","port":70000}`); rr.Code != 400 {
		t.Errorf("out-of-range port code = %d, want 400", rr.Code)
	}
	// The refused declaration did not overwrite the good one.
	if rr := do(h, "GET", "/v1/apps/web/health", token, ""); !strings.Contains(rr.Body.String(), `"probe_path":"/healthz"`) {
		t.Errorf("health get after a refused set = %s", rr.Body.String())
	}

	// Unsetting returns the app to the default rather than to nothing: it is still published.
	rr = do(h, "DELETE", "/v1/apps/web/health", token, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"probe":"tcp"`) {
		t.Fatalf("health unset = %d %s", rr.Code, rr.Body.String())
	}

	// An unknown environment is a 404 on every verb.
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		body := ""
		if m == "PUT" {
			body = `{"path":"/healthz"}`
		}
		if rr := do(h, m, "/v1/apps/web/health?env=ghost", token, body); rr.Code != 404 {
			t.Errorf("%s unknown env code = %d, want 404", m, rr.Code)
		}
	}
}

// TestChecksEndpoints covers the deploy-time dependency check's API (ADR-0076 §4): the derived list
// reads back, the write turns it off and on, and an unknown environment is a clean 404. It also pins
// that the response carries key NAMES and Burrow-composed addresses and never a credential.
func TestChecksEndpoints(t *testing.T) {
	h, _, d := newAPI(t)

	// An app Burrow provisioned nothing for: nothing to check, said so rather than left blank.
	rr := do(h, "GET", "/v1/apps/web/checks", token, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"enabled":true`) {
		t.Fatalf("checks get = %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "provisioned nothing") {
		t.Errorf("checks get does not explain an empty list: %s", rr.Body.String())
	}

	// Publishing the app derives an exposure dependency with no configuration at all.
	if err := d.RecordExposure(context.Background(), cp.Exposure{App: "web", Environment: cp.DefaultEnvironment, Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("RecordExposure: %v", err)
	}
	rr = do(h, "GET", "/v1/apps/web/checks", token, "")
	if !strings.Contains(rr.Body.String(), `"kind":"exposure"`) {
		t.Errorf("checks get after publishing = %s, want an exposure dependency", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "postgres://") {
		t.Errorf("the checks response carries a connection string: %s", rr.Body.String())
	}

	// The write turns it off, and the read reflects it.
	rr = do(h, "PUT", "/v1/apps/web/checks", token, `{"enabled":false}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"enabled":false`) {
		t.Fatalf("checks set = %d %s", rr.Code, rr.Body.String())
	}
	if rr := do(h, "GET", "/v1/apps/web/checks", token, ""); !strings.Contains(rr.Body.String(), `"enabled":false`) {
		t.Errorf("checks get after disabling = %s", rr.Body.String())
	}
	rr = do(h, "PUT", "/v1/apps/web/checks", token, `{"enabled":true}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"enabled":true`) {
		t.Fatalf("checks re-enable = %d %s", rr.Code, rr.Body.String())
	}

	for _, m := range []string{"GET", "PUT"} {
		body := ""
		if m == "PUT" {
			body = `{"enabled":false}`
		}
		if rr := do(h, m, "/v1/apps/web/checks?env=ghost", token, body); rr.Code != 404 {
			t.Errorf("%s unknown env code = %d, want 404", m, rr.Code)
		}
	}
}

// storedPolicy reads the policy the fake database holds, so a test can assert the KEY a set landed
// under rather than only the answer it produces (ADR-0085 §2: the key is a persistence format).
func storedPolicy(t *testing.T, d *fake.Database) cp.Policy {
	t.Helper()
	p, err := d.Policy(context.Background())
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	return p
}

// TestSecretMountEndpoints is ADR-0089 over the API: a key is projected into a file by NAME, the
// projection reads back with the path it lands at, and no request or response on this path has
// anywhere to put a value.
func TestSecretMountEndpoints(t *testing.T) {
	h, k, _ := newAPI(t)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	const value = "-----BEGIN PRIVATE KEY-----super-secret"
	k.SetSecret("web", "TLS_KEY", value)
	k.SetSecret("web", "STRIPE_KEY", "sk_live_x")

	// Nothing mounted: the default directory and an empty list, not an error.
	rr := do(h, "GET", "/v1/apps/web/secrets/mounts", token, "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"dir":"/run/secrets"`) {
		t.Fatalf("mounts (none) = %d %s", rr.Code, rr.Body.String())
	}

	rr = do(h, "PUT", "/v1/apps/web/secrets/mounts/TLS_KEY", token, `{"filename":"tls.key"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("mount = %d %s", rr.Code, rr.Body.String())
	}
	if b := rr.Body.String(); !strings.Contains(b, `"path":"/run/secrets/tls.key"`) {
		t.Errorf("mount response = %s, want the path the key lands at", b)
	}
	if b := rr.Body.String(); strings.Contains(b, value) {
		t.Fatalf("mount response leaked the value: %s", b)
	}
	// STRIPE_KEY is set on the app and not mounted, so it must not be in the projection.
	if b := rr.Body.String(); strings.Contains(b, "STRIPE_KEY") {
		t.Errorf("mount response names an unmounted key: %s", b)
	}

	// Mounting a key that is not set is refused rather than producing an app that fails later.
	if rr := do(h, "PUT", "/v1/apps/web/secrets/mounts/NOT_SET", token, `{}`); rr.Code != http.StatusBadRequest {
		t.Errorf("mount of an unset key = %d, want 400", rr.Code)
	}
	// A filename that escapes the directory Burrow owns is refused.
	if rr := do(h, "PUT", "/v1/apps/web/secrets/mounts/TLS_KEY", token, `{"filename":"../../etc/passwd"}`); rr.Code != http.StatusBadRequest {
		t.Errorf("mount with a traversal filename = %d, want 400", rr.Code)
	}

	rr = do(h, "DELETE", "/v1/apps/web/secrets/mounts/TLS_KEY", token, "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"mounts":[]`) {
		t.Fatalf("unmount = %d %s", rr.Code, rr.Body.String())
	}
	// An unmount removes a file, never a value.
	if v, ok := k.SecretValue("web", "TLS_KEY"); !ok || v != value {
		t.Errorf("TLS_KEY after an unmount = %q, %v; want the value untouched", v, ok)
	}

	// An unknown environment is a 404 on every verb.
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/v1/apps/web/secrets/mounts?env=nope", ""},
		{"PUT", "/v1/apps/web/secrets/mounts/TLS_KEY?env=nope", `{}`},
		{"DELETE", "/v1/apps/web/secrets/mounts/TLS_KEY?env=nope", ""},
	} {
		if rr := do(h, tc.method, tc.path, token, tc.body); rr.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.path, rr.Code)
		}
	}
}

// TestSecretMountNoEnvOverTheAPI is ADR-0089 §4 over the wire. The interesting field is `no_env`
// being ABSENT rather than false: absent means "leave the key's marking alone", so a mount that only
// renames a file cannot put a credential back into an environment somebody took it out of.
func TestSecretMountNoEnvOverTheAPI(t *testing.T) {
	h, k, _ := newAPI(t)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	k.SetSecret("web", "KUBECONFIG", "apiVersion: v1")
	k.SetSecret("web", "STRIPE_KEY", "sk_live_x")

	rr := do(h, "PUT", "/v1/apps/web/secrets/mounts/KUBECONFIG", token, `{"no_env":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("mount --no-env = %d %s", rr.Code, rr.Body.String())
	}
	if b := rr.Body.String(); !strings.Contains(b, `"no_env":true`) || !strings.Contains(b, `"enumerated":true`) {
		t.Errorf("mount response = %s, want the key marked file-only and the app reported as enumerated", b)
	}

	// A body that says nothing about the environment leaves the marking as it is.
	rr = do(h, "PUT", "/v1/apps/web/secrets/mounts/KUBECONFIG", token, `{"filename":"kubeconfig.yaml"}`)
	if b := rr.Body.String(); !strings.Contains(b, `"no_env":true`) || !strings.Contains(b, `"path":"/run/secrets/kubeconfig.yaml"`) {
		t.Errorf("rename response = %s, want the file renamed and the key still file-only", b)
	}

	// And an explicit false puts the variable back, which returns the app to the fast path.
	rr = do(h, "PUT", "/v1/apps/web/secrets/mounts/KUBECONFIG", token, `{"no_env":false}`)
	if b := rr.Body.String(); !strings.Contains(b, `"no_env":false`) || !strings.Contains(b, `"enumerated":false`) {
		t.Errorf("un-mark response = %s, want the marking removed and the app back on envFrom", b)
	}
}

// TestGuardEndpointsCarryTheBindingInTheRoute confirms the caller tier is reachable as a ROUTE
// (ADR-0094 §2), which is what makes a control plane without it refuse the call rather than store a
// disposition that binds every caller. Dropping a `--binds agent` does not widen a scope, it inverts
// an intent: the operator asked to be left alone and would have been bound.
func TestGuardEndpointsCarryTheBindingInTheRoute(t *testing.T) {
	h, _, d := newAPI(t)
	// The install has to issue per-caller credentials before a binding can bind anything (§4).
	if err := d.CreatePrincipal(context.Background(), cp.Principal{ID: "p-1", Name: "ada", Admin: true}); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}

	rr := do(h, "PUT", "/v1/guard/binds/agent/app.delete", token, `{"disposition":"deny"}`)
	if rr.Code != 200 {
		t.Fatalf("guard set --binds agent = %d %s", rr.Code, rr.Body.String())
	}
	if got := storedPolicy(t, d).Dispositions[cp.GuardrailCode("agent:app.delete")]; got != cp.DispositionDeny {
		t.Errorf("stored policy = %+v, want a deny under agent:app.delete", storedPolicy(t, d).Dispositions)
	}
	if _, ok := storedPolicy(t, d).Dispositions[cp.GuardrailAppDelete]; ok {
		t.Errorf("the deny landed on the unbound key too, binding the operator: %+v", storedPolicy(t, d).Dispositions)
	}

	// The binding composes with the name tier, and the two segments keep their order.
	rr = do(h, "PUT", "/v1/guard/binds/agent/name/burrowd-cloud/app.deploy?env=prod", token, `{"disposition":"deny"}`)
	if rr.Code != 200 {
		t.Fatalf("guard set --binds agent --name = %d %s", rr.Code, rr.Body.String())
	}
	if got := storedPolicy(t, d).Dispositions[cp.GuardrailCode("agent:prod.burrowd-cloud.app.deploy")]; got != cp.DispositionDeny {
		t.Errorf("stored policy = %+v, want a deny under agent:prod.burrowd-cloud.app.deploy", storedPolicy(t, d).Dispositions)
	}

	// A kind outside the closed set is the engine's ErrInvalid -> 400, not a key nothing will match.
	if rr := do(h, "PUT", "/v1/guard/binds/robot/app.delete", token, `{"disposition":"deny"}`); rr.Code != 400 {
		t.Errorf("unknown kind = %d, want 400", rr.Code)
	}
}

// TestGuardListingAnswersForTheCallerAsking is ADR-0094 §6 over HTTP: the listing resolves for the
// kind of credential the REQUEST carries, so an agent reading the policy sees what binds the agent.
// The kind comes from the authenticated caller on the request context and from nowhere else — there
// is no query parameter for it, because a caller-declared kind would make a deny cooperative.
func TestGuardListingAnswersForTheCallerAsking(t *testing.T) {
	h, _, d := newAPI(t)
	d.SetPolicy(cp.Policy{}.
		With(cp.GuardrailAppDelete, cp.DispositionAllow).
		With(cp.GuardrailCode("agent:app.delete"), cp.DispositionDeny))

	// The shared install token carries no kind, so it reads the unbound disposition — the behaviour
	// of every install nobody has signed in to.
	rr := do(h, "GET", "/v1/guard", token, "")
	if rr.Code != 200 {
		t.Fatalf("guard list = %d %s", rr.Code, rr.Body.String())
	}
	if got := guardDisposition(t, rr.Body.Bytes(), "app.delete"); got != "allow" {
		t.Errorf("the shared token reads app.delete = %q, want allow", got)
	}
	if strings.Contains(rr.Body.String(), `"binds"`) {
		t.Errorf("an unbound answer reported a binding: %s", rr.Body.String())
	}
}

// guardDisposition reads one guardrail's disposition out of a guard listing response.
func guardDisposition(t *testing.T, body []byte, code string) string {
	t.Helper()
	var resp struct {
		Guardrails []struct {
			Code        string `json:"code"`
			Disposition string `json:"disposition"`
		} `json:"guardrails"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decoding the guard listing: %v", err)
	}
	for _, g := range resp.Guardrails {
		if g.Code == code {
			return g.Disposition
		}
	}
	t.Fatalf("%s missing from the listing: %s", code, body)
	return ""
}
