// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/internal/agentsurface"
)

// cannedGuardrails answers /v1/guard with one disposition, so a test can assert what `guard list`
// adds around it.
func cannedGuardrails(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
		{"code": "app.deploy", "disposition": "allow", "description": "deploy a new release of an application"},
	}})
}

// TestGuardListPrintsDispositionsOnly asserts the human listing is guardrails and nothing else
// (issue #445). The absent-capability table used to print under it, larger than the listing itself
// and addressed to somebody who was not being refused anything — policy an operator set and the
// shape of another binary are not two halves of one setting. A single line says the list exists and
// where to read it; the capabilities themselves stay in --json, which is what burrow-agent consumes.
func TestGuardListPrintsDispositionsOnly(t *testing.T) {
	out, _, err := runCLI(t, cannedGuardrails, "guard", "list")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "app.deploy") {
		t.Errorf("guard list dropped the dispositions: %q", out)
	}
	for _, unwanted := range []string{"RUN INSTEAD", "burrow addon remove <name>", "addon remove"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("guard list still prints the absent-capability table (%q):\n%s", unwanted, out)
		}
	}
	if !strings.Contains(out, "absent from burrow-agent") || !strings.Contains(out, "burrow agent capabilities") {
		t.Errorf("guard list output is missing the one-line pointer at `burrow agent capabilities`:\n%s", out)
	}
}

// TestGuardListJSONSeparatesTheTwoGroups pins the machine-readable shape: a denied verb and an
// absent verb are different answers, so they are different keys. It is the same report
// `burrow-agent guard` prints, from the same catalogue.
func TestGuardListJSONSeparatesTheTwoGroups(t *testing.T) {
	out, _, err := runCLI(t, cannedGuardrails, "guard", "list", "--json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var got agentsurface.GuardReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("guard list --json is not a guard report: %v\n%s", err, out)
	}
	if len(got.Guardrails) != 1 || got.Guardrails[0].Code != "app.deploy" {
		t.Errorf("guardrails = %+v, want the one canned disposition", got.Guardrails)
	}
	var found bool
	for _, c := range got.AbsentCapabilities {
		if c.Path == "addon remove" {
			found = true
			if c.Why == "" || c.Who == "" || c.Command == "" {
				t.Errorf("`addon remove` is reported without why/who/command: %+v", c)
			}
		}
	}
	if !found {
		t.Errorf("absent_capabilities does not name `addon remove`: %s", out)
	}
}

// TestGuardSetCarriesTheScope confirms `guard set` sends the environment and the name it was given,
// and says in plain words what it changed. The control plane decides which combinations are legal —
// this only has to carry them faithfully (ADR-0085 §1).
//
// The name is in the PATH, and that is the half of issue #472 a test has to hold: as a query
// parameter it was droppable by a control plane that did not know the tier, which turned "deny this
// for one app" into "deny this for every app in the environment". This is also the matched-pair
// case — a current CLI against a current control plane — so it fails if the name-scoped write ever
// stops being carried at all.
func TestGuardSetCarriesTheScope(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		cannedGuardrails(w, r)
	}, "guard", "set", "app.deploy", "deny", "--env", "prod", "--name", "website")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "PUT" || gotPath != "/v1/guard/name/website/app.deploy" {
		t.Errorf("request = %s %s, want PUT /v1/guard/name/website/app.deploy", gotMethod, gotPath)
	}
	if !strings.Contains(gotQuery, "env=prod") {
		t.Errorf("query = %q, want the environment", gotQuery)
	}
	for _, want := range []string{`"app.deploy"`, `"deny"`, `"website"`, `"prod"`} {
		if !strings.Contains(out, want) {
			t.Errorf("confirmation %q is missing %s", out, want)
		}
	}
}

// TestGuardSetNameRefusedByOlderControlPlane is issue #472 at the surface it was hit on. A CLI that
// knows the name tier is pointed at a control plane that does not. The operator has to be told that
// nothing was written, in a message naming both versions and the upgrade, rather than being told
// that one application was protected while every application in the environment was.
func TestGuardSetNameRefusedByOlderControlPlane(t *testing.T) {
	var wrote bool
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		// A control plane without the name tier: it has PUT /v1/guard/{code} and nothing under it,
		// so the name-scoped route gets the structured unknown-operation refusal (ADR-0039).
		if r.URL.Path == "/v1/guard/app.rollback" {
			wrote = true
			cannedGuardrails(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "this control plane (v0.14.0-rc.2) does not recognize " + r.Method + " " + r.URL.Path +
				"; if your burrow CLI (v0.14.0-rc.5) is newer, ask an operator to run `burrow cluster upgrade` to update the control plane",
			"code":           "unknown_operation",
			"server_version": "v0.14.0-rc.2",
		})
	}, "guard", "set", "app.rollback", "deny", "--env", "prod", "--name", "burrowd-cloud")
	if err == nil {
		t.Fatal("guard set succeeded against a control plane without the name tier")
	}
	if wrote {
		t.Fatal("the environment-wide entry was written after the name-scoped route was refused")
	}
	if out != "" {
		t.Errorf("a refused set printed a result line: %q", out)
	}
	for _, want := range []string{"nothing was written", "v0.14.0-rc.2", "v0.14.0-rc.5", "burrow cluster upgrade", "scope_unsupported"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q:\n%v", want, err)
		}
	}
}

// TestGuardSetNameRefusalPrintsNoJSONResult holds the --json contract on the same path: a refusal is
// an error, not a success shape with a result in it. A tool that reads stdout must find nothing to
// parse when nothing was written.
func TestGuardSetNameRefusalPrintsNoJSONResult(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "this control plane (v0.14.0-rc.2) does not recognize the operation",
			"code":  "unknown_operation",
		})
	}, "guard", "set", "app.rollback", "deny", "--env", "prod", "--name", "burrowd-cloud", "--json")
	if err == nil {
		t.Fatal("guard set --json succeeded against a control plane without the name tier")
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("stdout = %q, want nothing: the write did not happen", out)
	}
}

// TestGuardSetForTheDefaultEnvironmentSaysItWroteGlobally is the second half of issue #472. `prod`
// is the default environment, so `--env prod` writes the GLOBAL policy (ADR-0067 §2) and a
// confirmation naming the environment would describe a scope narrower than the one that landed.
func TestGuardSetForTheDefaultEnvironmentSaysItWroteGlobally(t *testing.T) {
	out, _, err := runCLI(t, cannedGuardrails, "guard", "set", "app.delete", "deny", "--env", "prod")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out, `in environment "prod"`) {
		t.Errorf("the confirmation names an environment scope that was not written: %q", out)
	}
	for _, want := range []string{"globally", "prod is the default environment", "global policy"} {
		if !strings.Contains(out, want) {
			t.Errorf("confirmation %q is missing %q", out, want)
		}
	}
}

// TestGuardSetForAnAddedEnvironmentNamesIt keeps the other branch honest: an environment added after
// install has a policy of its own, and the line says so.
func TestGuardSetForAnAddedEnvironmentNamesIt(t *testing.T) {
	out, _, err := runCLI(t, cannedGuardrails, "guard", "set", "app.delete", "deny", "--env", "staging")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, `set guardrail "app.delete" to "deny" in environment "staging"`) {
		t.Errorf("confirmation = %q, want it to name the environment", out)
	}
}

// TestGuardListForOneAppShowsWhichTierAnswered is ADR-0085 §4 at the surface an operator reads: the
// listing for one app carries a SOURCE column naming that app where the disposition was set for it,
// so "why is this denied here and nowhere else" needs no reconstruction of the fallback chain.
func TestGuardListForOneAppShowsWhichTierAnswered(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
			{"code": "app.deploy", "disposition": "deny", "description": "deploy a new release of an application", "source": "name"},
			{"code": "app.delete", "disposition": "allow", "description": "delete an app entirely", "source": "env"},
			{"code": "app.rollback", "disposition": "allow", "description": "roll an application back", "source": "global"},
		}})
	}, "guard", "list", "--env", "prod", "--name", "website")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"SOURCE", "website", "environment", "inherited (global)"} {
		if !strings.Contains(out, want) {
			t.Errorf("guard list output is missing %q:\n%s", want, out)
		}
	}
}

// TestGuardListSourceColumnFollowsTheAnswer holds the rule the SOURCE column obeys: it is printed
// when the control plane named the tier each disposition came from, and left out when it did not.
//
// The case that drove it is `--env prod`. `prod` is the default environment, resolution skips the
// environment tier for it, and its policy IS the global policy (ADR-0067 §2), so the control plane
// reports no source at all for that listing. A column keyed off the flags instead of the answer
// printed one anyway and filled every row from an empty string, so a listing of the global policy
// claimed each disposition was "inherited (default)" when it was nothing of the sort.
//
// `--name` under the same environment is a different question and must not be collapsed into it:
// an app-tier rule in `prod` has a real source worth naming, and the last cases pin it.
func TestGuardListSourceColumnFollowsTheAnswer(t *testing.T) {
	// The rows a control plane returns for each scope, straight from its own resolution: a source
	// per row when there is a tier to name, and no source member when there is not.
	global := []map[string]any{
		{"code": "app.deploy", "disposition": "allow", "description": "deploy a new release of an application"},
		{"code": "app.delete", "disposition": "deny", "description": "delete an app entirely"},
	}
	scoped := func(sources ...string) []map[string]any {
		rows := []map[string]any{
			{"code": "app.deploy", "disposition": "allow", "description": "deploy a new release of an application"},
			{"code": "app.delete", "disposition": "deny", "description": "delete an app entirely"},
		}
		for i, s := range sources {
			rows[i]["source"] = s
		}
		return rows
	}

	for _, tc := range []struct {
		name        string
		args        []string
		guardrails  []map[string]any
		want        []string
		wantMissing []string
	}{
		{
			name:        "the unscoped listing is the global policy and names no tier",
			args:        []string{"guard", "list"},
			guardrails:  global,
			want:        []string{"GUARDRAIL", "DISPOSITION", "app.deploy", "allow"},
			wantMissing: []string{"SOURCE", "inherited", "default environment"},
		},
		{
			name:       "the default environment is the global policy and says so",
			args:       []string{"guard", "list", "--env", "prod"},
			guardrails: global,
			want: []string{
				"prod is the default environment", "global policy", "app.deploy", "allow",
			},
			wantMissing: []string{"SOURCE", "inherited"},
		},
		{
			name:        "an added environment names the tier that answered",
			args:        []string{"guard", "list", "--env", "staging"},
			guardrails:  scoped("env", "global"),
			want:        []string{"SOURCE", "environment", "inherited (global)"},
			wantMissing: []string{"default environment", "inherited (default)"},
		},
		{
			name:        "an app in the default environment names the app tier",
			args:        []string{"guard", "list", "--env", "prod", "--name", "web"},
			guardrails:  scoped("name", "global"),
			want:        []string{"SOURCE", "web", "inherited (global)"},
			wantMissing: []string{"default environment", "inherited (default)"},
		},
		{
			name:        "the built-in default is named as itself",
			args:        []string{"guard", "list", "--env", "staging"},
			guardrails:  scoped("env", "default"),
			want:        []string{"SOURCE", "environment", "inherited (default)"},
			wantMissing: []string{"default environment"},
		},
		{
			name:        "a row without a source is blank rather than mislabelled",
			args:        []string{"guard", "list", "--env", "staging"},
			guardrails:  scoped("env"),
			want:        []string{"SOURCE", "environment", "app.delete"},
			wantMissing: []string{"inherited"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := runCLI(t, func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": tc.guardrails})
			}, tc.args...)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			table, _, _ := strings.Cut(out, "\nAbsent from burrow-agent")
			for _, want := range tc.want {
				if !strings.Contains(table, want) {
					t.Errorf("guard list output is missing %q:\n%s", want, table)
				}
			}
			for _, unwanted := range tc.wantMissing {
				if strings.Contains(table, unwanted) {
					t.Errorf("guard list output claims %q:\n%s", unwanted, table)
				}
			}
		})
	}
}

// TestGuardSetCarriesTheBinding is the caller tier at the surface (ADR-0094 §2): `--binds` rides the
// path, ahead of the name, and the confirmation says what the write did NOT do — an operator who
// binds a deny and reads back "set to deny" has no way to tell it apart from the blunt write they
// were avoiding.
func TestGuardSetCarriesTheBinding(t *testing.T) {
	var gotPath, gotQuery string
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		cannedGuardrails(w, r)
	}, "guard", "set", "app.deploy", "deny", "--binds", "agent", "--env", "prod", "--name", "burrowd-cloud")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotPath != "/v1/guard/binds/agent/name/burrowd-cloud/app.deploy" || !strings.Contains(gotQuery, "env=prod") {
		t.Errorf("request = %s?%s, want /v1/guard/binds/agent/name/burrowd-cloud/app.deploy?env=prod", gotPath, gotQuery)
	}
	for _, want := range []string{"binds agent credentials only", "every other caller reads the disposition underneath it"} {
		if !strings.Contains(out, want) {
			t.Errorf("confirmation %q is missing %q", out, want)
		}
	}
	// Without --binds the request and the confirmation are exactly what they always were.
	out, _, err = runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		cannedGuardrails(w, r)
	}, "guard", "set", "app.deploy", "deny")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotPath != "/v1/guard/app.deploy" {
		t.Errorf("an unbound set went to %s, want /v1/guard/app.deploy", gotPath)
	}
	if strings.Contains(out, "binds") {
		t.Errorf("an unbound set claims a binding: %q", out)
	}
}

// TestGuardListShowsTheBindingWhenThereIsOne pins the read side (ADR-0094 §6). The BINDS column
// appears only when a row has one, so the everyday listing keeps the width it had, and a human
// reading a listing on an install that DOES bind something sees the binding rather than inferring it
// from a surprise later.
func TestGuardListShowsTheBindingWhenThereIsOne(t *testing.T) {
	bound := func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
			{"code": "app.deploy", "disposition": "allow", "description": "deploy a new release"},
			{"code": "app.delete", "disposition": "deny", "description": "delete an app entirely", "binds": "agent"},
		}})
	}
	out, _, err := runCLI(t, bound, "guard", "list")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "BINDS") {
		t.Errorf("the listing hides a binding that exists:\n%s", out)
	}
	// The bound row names the kind; the unbound one says who it applies to rather than showing a
	// dash, because "every caller" is the fact and not a missing value.
	if !strings.Contains(out, "agent") || !strings.Contains(out, "everyone") {
		t.Errorf("the BINDS column does not distinguish the two rows:\n%s", out)
	}

	// An install that binds nothing gets the listing it always had.
	out, _, err = runCLI(t, cannedGuardrails, "guard", "list")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out, "BINDS") {
		t.Errorf("the listing grew a BINDS column with nothing bound:\n%s", out)
	}
}
