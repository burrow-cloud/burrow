// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// A mutating verb names WHERE it acted, in the outcome envelope the agent parses (ADR-0078 §4). The
// agent's whole job with a result is to relay it to a human, so "deployed" without "to which
// cluster" is the half of the sentence that lets the wrong-target mistake go unnoticed until
// somebody else finds it.
//
// Like cmd/burrow-agent/agent_surface_guard_test.go, this is driven from the COMMAND TREE rather
// than from a list of verbs somebody remembers to extend: the classification below is closed over
// the registered surface in both directions, so a verb added later fails here until its author says
// whether it changes anything, and a mutating one is then actually RUN and its envelope checked.
//
// Read-only verbs deliberately carry no target. This is about irreversible acts; a target stamped on
// every listing is noise, and noise is what gets skimmed past.

// agentCommand is one registered path's classification. mutates commands supply an invocation,
// because a classification that is never exercised is a claim rather than a check.
type agentCommand struct {
	mutates bool
	why     string
	args    []string
}

var agentCommandClasses = map[string]agentCommand{
	// Groups and reads.
	"addon":               {why: "a group with no action of its own"},
	"addon backup-health": {why: "reports backup coverage"},
	"addons":              {why: "lists installed add-ons"},
	"apps":                {why: "lists apps"},
	"audit":               {why: "reads the audit log"},
	"backups":             {why: "lists recorded backups"},
	"checks":              {why: "reads the deploy-time checks"},
	"cluster":             {why: "reports cluster capabilities"},
	"cluster capacity":    {why: "reads scheduling headroom"},
	"config":              {why: "lists an app's config vars"},
	"domain":              {why: "a group with no action of its own"},
	"environments":        {why: "lists and selects environments"},
	"failures":            {why: "lists what is broken"},
	"guard":               {why: "reports guardrail dispositions and absent capabilities"},
	"health":              {why: "reads the probe Burrow sets"},
	"history":             {why: "reads an app's deploy timeline"},
	"logs":                {why: "reads pod logs"},
	"logs-query":          {why: "queries the logs add-on"},
	"metrics-query":       {why: "queries the metrics add-on"},
	"next-tag":            {why: "suggests a tag; nothing is written"},
	"providers":           {why: "lists configured providers"},
	"reachability":        {why: "reads the reachability chain"},
	"secret":              {why: "lists secret KEYS, never values"},
	"secret mounts":       {why: "lists which secret KEYS are read as files, and where"},
	"status":              {why: "reads an app's status"},

	// Mutating verbs: each names the target in its outcome envelope.
	"deploy":         {mutates: true, why: "rolls out a release", args: []string{"deploy", "web", "--image", "img:1"}},
	"build":          {mutates: true, why: "builds in the cluster and deploys the result", args: []string{"build", "web", "--source", "https://github.com/acme/web", "--ref", "v1.2.3", "--image", "img:1"}},
	"rollback":       {mutates: true, why: "puts an earlier release back", args: []string{"rollback", "web"}},
	"scale":          {mutates: true, why: "changes the replica count", args: []string{"scale", "web", "2"}},
	"autoscale":      {mutates: true, why: "writes a HorizontalPodAutoscaler", args: []string{"autoscale", "web", "--min", "1", "--max", "3"}},
	"run":            {mutates: true, why: "runs a command in the app's own runtime", args: []string{"run", "web", "--", "echo", "hi"}},
	"delete":         {mutates: true, why: "deletes an app entirely", args: []string{"delete", "web", "--confirm"}},
	"publish":        {mutates: true, why: "routes a hostname to an app, writes its DNS record, and requests its certificate", args: []string{"publish", "web", "--host", "app.example.com", "--port", "8080"}},
	"unpublish":      {mutates: true, why: "removes the Service and Ingress again", args: []string{"unpublish", "web"}},
	"domain add":     {mutates: true, why: "writes a DNS record at a provider", args: []string{"domain", "add", "app.example.com", "--address", "203.0.113.10"}},
	"domain remove":  {mutates: true, why: "removes that DNS record", args: []string{"domain", "remove", "app.example.com"}},
	"config set":     {mutates: true, why: "writes a config var and rolls the app", args: []string{"config", "set", "web", "K=V"}},
	"config unset":   {mutates: true, why: "removes a config var and rolls the app", args: []string{"config", "unset", "web", "K"}},
	"secret unset":   {mutates: true, why: "removes a secret from the app", args: []string{"secret", "unset", "web", "K"}},
	"secret mount":   {mutates: true, why: "projects a secret key into a file and re-applies the workload", args: []string{"secret", "mount", "web", "K"}},
	"secret unmount": {mutates: true, why: "stops projecting that key as a file and re-applies the workload", args: []string{"secret", "unmount", "web", "K"}},
	"health set":     {mutates: true, why: "declares the health endpoint and re-applies the workload", args: []string{"health", "set", "web", "--path", "/healthz"}},
	"health unset":   {mutates: true, why: "returns the app to the default probe", args: []string{"health", "unset", "web"}},
	"addon install":  {mutates: true, why: "deploys a backing service into the add-on namespace", args: []string{"addon", "install", "logs"}},
	"addon attach":   {mutates: true, why: "provisions an app's database and writes its connection string", args: []string{"addon", "attach", "postgres", "web"}},
	"addon backup":   {mutates: true, why: "runs a backup Job and records the backup", args: []string{"addon", "backup", "postgres", "web"}},
	"addon sql":      {mutates: true, why: "runs a caller-supplied statement against an app's database, which Burrow deliberately does not classify as a read or a write (ADR-0087 §6), so it goes through the outcome envelope that names the target it ran against", args: []string{"addon", "sql", "postgres", "web", "-c", "select 1"}},
}

const agentChangeRationale = `
Why this test exists:

  A mutating verb's outcome envelope is what the agent relays to the human. ADR-0078 lets the CLI
  point at more than one place, so "executed" without "on which target" leaves the wrong-target
  mistake to be discovered later by somebody else — the unrecoverable form of it.

What to do:

  - If the verb changes something, route it through connOpts.mutate, which attaches the target to
    every envelope it prints, and classify it here as mutates with an invocation.
  - If it only reads, classify it with mutates left false and say why.`

// TestEveryAgentCommandIsClassified keeps the classification closed over the registered surface, so
// a verb added later cannot quietly skip the target.
func TestEveryAgentCommandIsClassified(t *testing.T) {
	registered := commandPaths(newRootCmd())

	var unclassified []string
	have := map[string]bool{}
	for _, path := range registered {
		have[path] = true
		if _, ok := agentCommandClasses[path]; !ok {
			unclassified = append(unclassified, path)
		}
	}
	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Errorf("burrow-agent registers %d command(s) with no target classification: %s\n%s",
			len(unclassified), strings.Join(unclassified, ", "), agentChangeRationale)
	}

	var stale []string
	for path := range agentCommandClasses {
		if !have[path] {
			stale = append(stale, path)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("the classification names %d command(s) that are no longer registered: %s",
			len(stale), strings.Join(stale, ", "))
	}

	for path, c := range agentCommandClasses {
		if strings.TrimSpace(c.why) == "" {
			t.Errorf("%q is classified with no reason", path)
		}
		if c.mutates && len(c.args) == 0 {
			t.Errorf("%q mutates but supplies no invocation, so the guard below cannot run it.\n%s", path, agentChangeRationale)
		}
		if !c.mutates && len(c.args) > 0 {
			t.Errorf("%q supplies an invocation but is not classified as mutating", path)
		}
	}
}

// TestMutatingVerbsNameTheirTarget runs every mutating verb against a stand-in control plane and
// requires the outcome envelope to name where it happened.
func TestMutatingVerbsNameTheirTarget(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")

	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}

	for path, c := range agentCommandClasses {
		if !c.mutates {
			continue
		}
		t.Run(path, func(t *testing.T) {
			out, code := runMutate(t, f, c.args...)
			if code != exitCodeExecuted {
				t.Fatalf("%s exited %d: %s", path, code, out)
			}
			oc := decodeOutcome(t, out)
			if oc.Target == nil || oc.Target.Endpoint != f.srv.URL {
				t.Errorf("`burrow-agent %s` printed an envelope with no target, so the agent cannot say "+
					"where it happened.\ngot: %s\n%s", path, out, agentChangeRationale)
			}
		})
	}
}

// TestHeldOutcomeNamesTheTarget: "held on which target" is the question a person asks the moment an
// agent relays a hold, so the envelope carries it on the non-executed outcomes too.
func TestHeldOutcomeNamesTheTarget(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")

	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		held(w, "delete", "app.delete", "deleting an app is irreversible")
	}
	out, code := runMutate(t, f, "delete", "web")
	if code != exitCodeHeld {
		t.Fatalf("exit = %d, want held; %s", code, out)
	}
	var oc outcome
	if err := json.Unmarshal([]byte(out), &oc); err != nil {
		t.Fatalf("decoding %q: %v", out, err)
	}
	if oc.Target == nil || oc.Target.Endpoint != f.srv.URL {
		t.Errorf("a held envelope should still say which target held it: %s", out)
	}
}
