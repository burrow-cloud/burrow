// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Every command that CHANGES something names the target it acted on (ADR-0078 §4). This file is
// where that is ENFORCED, and it is enforced from the COMMAND TREE rather than from a list of
// commands somebody remembers to update — the same shape as
// cmd/burrow-agent/agent_surface_guard_test.go, and for the same reason. A hand-kept list of
// mutating commands drifts silently in exactly the direction that matters: a command lands, nobody
// adds it, and the one control the design has against acting on the wrong target quietly does not
// apply to it.
//
// So the classification below is CLOSED over the tree in both directions. Every registered command
// must appear, and every entry must still be registered. A new command fails this file until its
// author says which of four things it is, and that is the point: the decision is forced at the
// moment the command is written, when the author still knows the answer.
//
// Why the classes, and why read-only commands are exempt: the failure ADR-0078 introduces is acting
// on the WRONG target, which cannot be prevented by design because both targets are legitimate
// things to act on. The only control left is making it visible in the same breath as the change,
// and that control is worth exactly nothing if it is also printed on every listing — noise is what
// gets skimmed past. This is about irreversible acts.

type changeClass int

const (
	// classChanges: acts on an installed control plane through the selected target. MUST name it.
	classChanges changeClass = iota
	// classReads: reads and changes nothing, so naming the target would be noise (ADR-0078 §4).
	classReads
	// classLocal: changes only client-side state — ~/.burrow/config, a coding agent's own config
	// file. Nothing on any target moves, so there is no target act to name.
	classLocal
	// classClusterAdmin: a cluster-admin act performed with the operator's own kubeconfig, which
	// ADR-0078 §3 keeps on a kubeconfig context by design rather than routing through the target.
	// Install takes the context as an explicit argument; bootstrap and join run where no control
	// plane exists yet. Naming a selected target on these would assert something untrue.
	classClusterAdmin
)

// commandClass is one command's classification plus, for a changing one, an invocation the guard
// below can actually run. The invocation is part of the entry on purpose: a new changing command
// cannot be classified without also saying how to exercise it, which is what keeps this a test that
// runs the command rather than a list that asserts about itself.
type commandClass struct {
	class changeClass
	why   string
	args  []string
}

// commandClasses classifies every command `burrow` registers. Keep the reason next to the entry:
// an entry here is a claim about what the command does to a target, and it needs to stay true.
var commandClasses = map[string]commandClass{
	// --- Choosing and inspecting the target itself ---
	"auth":        {class: classReads, why: "a group with no action of its own"},
	"auth login":  {class: classLocal, why: "records which target is active; it installs nothing and contacts no cluster (ADR-0078 §3)"},
	"auth status": {class: classReads, why: "lists the configured targets"},
	"auth switch": {class: classLocal, why: "changes which recorded target is active; nothing on any target moves"},

	// --- Cluster lifecycle: the operator's own kubeconfig, by design (ADR-0078 §3) ---
	"join":                       {class: classClusterAdmin, why: "joins a machine to a cluster from a join token; no control plane is being addressed"},
	"install":                    {class: classClusterAdmin, why: "deprecated alias for `cluster install`"},
	"upgrade":                    {class: classClusterAdmin, why: "deprecated alias for `cluster upgrade`"},
	"mcp":                        {class: classLocal, why: "a removed-command stub that only points at `burrow agent <tool> install`"},
	"agent":                      {class: classLocal, why: "writes a coding agent's own configuration file on this machine"},
	"agent capabilities":         {class: classReads, why: "lists the capabilities absent from the burrow-agent binary, from a compiled-in catalogue; it reaches no target at all"},
	"cluster install":            {class: classClusterAdmin, why: "puts a control plane into a cluster named as an explicit argument (ADR-0078 §3)"},
	"cluster upgrade":            {class: classClusterAdmin, why: "replaces the running control plane in a named kubeconfig context"},
	"cluster ingress install":    {class: classClusterAdmin, why: "installs cluster components with the operator's kubeconfig"},
	"cluster registry install":   {class: classClusterAdmin, why: "installs the in-cluster registry with the operator's kubeconfig"},
	"cluster registry uninstall": {class: classClusterAdmin, why: "removes the in-cluster registry with the operator's kubeconfig"},
	"cluster postgres install":   {class: classClusterAdmin, why: "installs the CloudNativePG operator with the operator's kubeconfig"},
	"cluster metrics install":    {class: classClusterAdmin, why: "installs the metrics-server baseline with the operator's kubeconfig; it registers a cluster-scoped APIService"},
	"cluster bootstrap":          {class: classClusterAdmin, why: "turns a bare VPS into a cluster; there is no control plane to target yet"},
	"config registry login":      {class: classClusterAdmin, why: "writes an image-pull Secret straight through the kubeconfig; it has no --context of its own and never reaches a control plane"},
	"config registry logout":     {class: classClusterAdmin, why: "removes that Secret through the same kubeconfig path"},
	"env add":                    {class: classClusterAdmin, why: "creates a namespace and grants RBAC in it before registering the environment — a cluster-admin act (ADR-0049 §2)"},

	// --- Local selector state ---
	"env":        {class: classReads, why: "lists the environment handles"},
	"env list":   {class: classReads, why: "lists the environment handles"},
	"env use":    {class: classLocal, why: "pins a handle in ~/.burrow/config"},
	"env follow": {class: classLocal, why: "returns to following the kube context, in ~/.burrow/config"},
	"env rename": {class: classLocal, why: "renames a handle in ~/.burrow/config"},
	"env remove": {class: classLocal, why: "removes a handle from ~/.burrow/config; the environment itself is untouched"},

	// --- Reads ---
	"cluster":               {class: classReads, why: "reports the cluster's capabilities"},
	"cluster capacity":      {class: classReads, why: "reads scheduling headroom"},
	"cluster config list":   {class: classReads, why: "lists the operational limits"},
	"config":                {class: classReads, why: "a group with no action of its own"},
	"config provider":       {class: classReads, why: "a group with no action of its own"},
	"config provider types": {class: classReads, why: "lists the supported provider types"},
	"config provider list":  {class: classReads, why: "lists configured providers"},
	"config registry":       {class: classReads, why: "a group with no action of its own"},
	"config registry list":  {class: classReads, why: "lists configured registries"},
	"app":                   {class: classReads, why: "a group with no action of its own"},
	"app list":              {class: classReads, why: "lists apps"},
	"app status":            {class: classReads, why: "reads an app's status"},
	"app history":           {class: classReads, why: "reads an app's deploy timeline"},
	"app logs":              {class: classReads, why: "reads logs"},
	"app hook":              {class: classReads, why: "a group with no action of its own"},
	"app hook list":         {class: classReads, why: "lists an app's hooks"},
	"app config":            {class: classReads, why: "a group with no action of its own"},
	"app config list":       {class: classReads, why: "lists an app's config vars"},
	"app health":            {class: classReads, why: "a group with no action of its own"},
	"app health show":       {class: classReads, why: "reads the probe Burrow sets"},
	"app checks":            {class: classReads, why: "a group with no action of its own"},
	"app checks show":       {class: classReads, why: "reads what Burrow checks"},
	"app secret":            {class: classReads, why: "a group with no action of its own"},
	"app secret list":       {class: classReads, why: "lists secret KEYS, never values"},
	"app reachability":      {class: classReads, why: "reads the reachability chain"},
	"app domain":            {class: classReads, why: "a group with no action of its own"},
	"addon":                 {class: classReads, why: "a group with no action of its own"},
	"addon backups":         {class: classReads, why: "lists recorded backups"},
	"addon backup-health":   {class: classReads, why: "reports backup coverage"},
	"addon list":            {class: classReads, why: "lists installed add-ons"},
	"addon logs":            {class: classReads, why: "queries the logs add-on"},
	"addon metrics":         {class: classReads, why: "queries the metrics add-on"},
	"failures":              {class: classReads, why: "lists what is broken"},
	"guard":                 {class: classReads, why: "a group with no action of its own"},
	"guard list":            {class: classReads, why: "lists the guardrail dispositions"},
	"audit":                 {class: classReads, why: "reads the audit log"},
	"version":               {class: classReads, why: "prints versions"},
	"cluster config":        {class: classReads, why: "a group with no action of its own"},
	"cluster ingress":       {class: classReads, why: "a group with no action of its own"},
	"cluster registry":      {class: classReads, why: "a group with no action of its own"},
	"cluster postgres":      {class: classReads, why: "a group with no action of its own"},
	"cluster metrics":       {class: classReads, why: "a group with no action of its own"},

	// --- Changes: these name the target they acted on ---
	"app deploy":            {class: classChanges, why: "rolls out a release", args: []string{"app", "deploy", "web", "--image", "img:1"}},
	"app build":             {class: classChanges, why: "builds in the cluster and deploys the result", args: []string{"app", "build", "web", "--source", "https://github.com/acme/web", "--ref", "v1.2.3", "--image", "img:1"}},
	"app delete":            {class: classChanges, why: "deletes an app's workload, routing, and history", args: []string{"app", "delete", "web", "--confirm"}},
	"app rollback":          {class: classChanges, why: "puts an earlier release back", args: []string{"app", "rollback", "web"}},
	"app scale":             {class: classChanges, why: "changes the replica count", args: []string{"app", "scale", "web", "2"}},
	"app run":               {class: classChanges, why: "runs a command in the app's own runtime", args: []string{"app", "run", "web", "--", "echo", "hi"}},
	"app autoscale":         {class: classChanges, why: "writes a HorizontalPodAutoscaler", args: []string{"app", "autoscale", "web", "--min", "1", "--max", "3"}},
	"app auto-deploy":       {class: classChanges, why: "sets the auto-deploy level (it also shows it, and only the write names the target)", args: []string{"app", "auto-deploy", "web", "patch"}},
	"app hook set":          {class: classChanges, why: "sets a command that runs around a deploy", args: []string{"app", "hook", "set", "web", "--on", "pre-deploy", "--", "./migrate"}},
	"app hook unset":        {class: classChanges, why: "removes a hook", args: []string{"app", "hook", "unset", "web", "--on", "pre-deploy"}},
	"app config set":        {class: classChanges, why: "writes a config var and rolls the app", args: []string{"app", "config", "set", "web", "K=V"}},
	"app config unset":      {class: classChanges, why: "removes a config var and rolls the app", args: []string{"app", "config", "unset", "web", "K"}},
	"app health set":        {class: classChanges, why: "declares the health endpoint and re-applies the workload", args: []string{"app", "health", "set", "web", "--path", "/healthz"}},
	"app health unset":      {class: classChanges, why: "returns the app to the default probe and re-applies it", args: []string{"app", "health", "unset", "web"}},
	"app checks enable":     {class: classChanges, why: "turns the deploy-time check back on for an app", args: []string{"app", "checks", "enable", "web"}},
	"app checks disable":    {class: classChanges, why: "turns the deploy-time check off for an app", args: []string{"app", "checks", "disable", "web"}},
	"app secret set":        {class: classChanges, why: "writes a secret value into the app's Secret", args: []string{"app", "secret", "set", "web", "K", "--stdin"}},
	"app secret unset":      {class: classChanges, why: "removes a secret from the app", args: []string{"app", "secret", "unset", "web", "K"}},
	"app publish":           {class: classChanges, why: "creates the Service and Ingress that expose an app", args: []string{"app", "publish", "web", "--host", "app.example.com", "--port", "8080"}},
	"app unpublish":         {class: classChanges, why: "removes them again", args: []string{"app", "unpublish", "web"}},
	"app domain add":        {class: classChanges, why: "writes a DNS record at a provider", args: []string{"app", "domain", "add", "app.example.com", "--address", "203.0.113.10"}},
	"app domain remove":     {class: classChanges, why: "removes that DNS record", args: []string{"app", "domain", "remove", "app.example.com"}},
	"addon install":         {class: classChanges, why: "deploys a backing service into the add-on namespace", args: []string{"addon", "install", "logs"}},
	"addon connect":         {class: classChanges, why: "registers an existing backend as a capability", args: []string{"addon", "connect", "loki", "--endpoint", "loki:3100"}},
	"addon attach":          {class: classChanges, why: "provisions an app's database and writes its connection string", args: []string{"addon", "attach", "postgres", "web"}},
	"addon detach":          {class: classChanges, why: "destroys an app's database", args: []string{"addon", "detach", "postgres", "web", "--confirm"}},
	"addon backup":          {class: classChanges, why: "runs a backup Job and records the backup", args: []string{"addon", "backup", "postgres", "web"}},
	"addon sql":             {class: classChanges, why: "runs a caller-supplied statement against an app's database, which Burrow deliberately does not classify as a read or a write (ADR-0087 §6), so a verb that may have written names the target it ran against", args: []string{"addon", "sql", "postgres", "web", "-c", "select 1"}},
	"addon backup-instance": {class: classChanges, why: "asks CloudNativePG for a physical base backup of a whole instance and records it (ADR-0066 §2)", args: []string{"addon", "backup-instance", "postgres"}},
	"addon restore":         {class: classChanges, why: "overwrites a live database from a backup", args: []string{"addon", "restore", "postgres", "web", "--backup", "b-1", "--confirm"}},
	// The acknowledgement flag is part of the invocation because the command refuses off a terminal
	// without it (ADR-0064 §2), and the guard runs it with a buffer for stdin. That is the gate
	// working, not a workaround for it.
	"addon restore-instance": {class: classChanges, why: "rewinds a whole Postgres instance, taking every app's database on it back together (ADR-0066 §4)", args: []string{"addon", "restore-instance", "postgres", "--latest", "--acknowledge-data-loss", "--confirm"}},
	"addon remove":           {class: classChanges, why: "removes an installed add-on", args: []string{"addon", "remove", "logs", "--confirm"}},
	"guard set":              {class: classChanges, why: "rewrites the guardrail policy the agent runs under", args: []string{"guard", "set", "app.deploy", "allow"}},
	"cluster config set":     {class: classChanges, why: "writes an operational limit", args: []string{"cluster", "config", "set", "app.replicas.max", "5"}},
	"config provider add":    {class: classChanges, why: "registers a provider credential through the control plane", args: []string{"config", "provider", "add", "cloudflare"}},
}

// changeGuardRationale is appended to every failure here so the message teaches rather than merely
// fails.
const changeGuardRationale = `
Why this test exists:

  ` + "`burrow`" + ` can point at more than one place (ADR-0078). The failure that creates is acting on
  the WRONG one: deploying to a cluster while believing you were deploying to the managed product,
  or the reverse. It cannot be prevented by design, because both are legitimate things to do and
  the CLI has no way to know which was meant.

  So the only control available is making it immediately visible. The recoverable form of that
  mistake is noticing in the same breath; the unrecoverable form is hearing about it later from
  somebody else. That is why a changing command names its target in its OWN output, next to what
  it did, and why a read-only one deliberately does not: printed everywhere it would be noise, and
  noise is what gets skimmed past.

What to do:

  - If the command CHANGES something on a target, emit its result with o.emitChange instead of
    emit, and classify it here as classChanges with an invocation the guard can run.
  - If it only READS, classify it classReads.
  - If it changes only client-side state (~/.burrow/config, an agent's own config file), classify
    it classLocal.
  - If it is a cluster-admin act on a kubeconfig context rather than a target — install, upgrade,
    bootstrap, join, ` + "`env add`" + ` — classify it classClusterAdmin and say why in the entry.`

// TestEveryCommandIsClassified holds the classification closed over the real command tree, in both
// directions, so a command added later cannot quietly omit the target: it fails here first.
func TestEveryCommandIsClassified(t *testing.T) {
	registered := registeredCommandPaths(t)

	var unclassified []string
	have := map[string]bool{}
	for _, path := range registered {
		have[path] = true
		if _, ok := commandClasses[path]; !ok {
			unclassified = append(unclassified, path)
		}
	}
	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Errorf("burrow registers %d command(s) with no target classification: %s\n%s",
			len(unclassified), strings.Join(unclassified, ", "), changeGuardRationale)
	}

	var stale []string
	for path := range commandClasses {
		if !have[path] {
			stale = append(stale, path)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("the classification names %d command(s) that are no longer registered: %s\n"+
			"Remove the entry, or fix the path if the command was renamed.", len(stale), strings.Join(stale, ", "))
	}

	for path, c := range commandClasses {
		if strings.TrimSpace(c.why) == "" {
			t.Errorf("%q is classified with no reason; the reason is what keeps the claim checkable", path)
		}
		if c.class == classChanges && len(c.args) == 0 {
			t.Errorf("%q changes something but supplies no invocation, so the guard below cannot run it.\n%s",
				path, changeGuardRationale)
		}
		if c.class != classChanges && len(c.args) > 0 {
			t.Errorf("%q supplies an invocation but is not classified as changing anything", path)
		}
	}
}

// TestChangingCommandsNameTheirTarget runs every command classified as changing something and
// requires the target to appear in what it printed — once in the human output and, with --json, in
// the result an agent parses. It runs the REAL command tree against a stand-in control plane, so a
// command that stops naming its target fails here rather than in somebody's terminal.
//
// ONCE is half the requirement, and the half that is easy to lose. Two paths can answer "where did
// that go": the per-app commands print a targeting line before they work, and every changing command
// can append a clause to its result. A command that did both said it twice, in two vocabularies, and
// a reader comparing the two had to work out whether they disagreed (issues #465, #473). So the
// guard counts: exactly one mention, wherever the command chose to put it.
func TestChangingCommandsNameTheirTarget(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	// Never read the developer's real ~/.burrow/config, and never touch a real cluster: every
	// invocation below is addressed to the httptest server by --control-plane.
	tempConfig(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	for path, c := range commandClasses {
		if c.class != classChanges {
			continue
		}
		t.Run(path, func(t *testing.T) {
			stdout, stderr, err := runChanging(t, srv.URL, c.args, false)
			if err != nil {
				t.Fatalf("%s: %v\n%s\n%s", path, err, stdout, stderr)
			}
			// Both streams, because the two ways of saying it land on different ones: the targeting
			// line goes to stderr (it must never pollute a --json result) and the clause rides the
			// result on stdout.
			switch said := strings.Count(stdout, srv.URL) + strings.Count(stderr, srv.URL); {
			case said == 0:
				t.Errorf("`burrow %s` printed no target.\nstdout: %q\nstderr: %q\n%s", path, stdout, stderr, changeGuardRationale)
			case said > 1:
				t.Errorf("`burrow %s` named its target %d times; one clear statement, not two.\nstdout: %q\nstderr: %q\n%s",
					path, said, stdout, stderr, changeGuardRationale)
			}

			stdout, stderr, err = runChanging(t, srv.URL, c.args, true)
			if err != nil {
				t.Fatalf("%s --json: %v\n%s\n%s", path, err, stdout, stderr)
			}
			// Several commands print one result per key (config/secret set), so read the first
			// document off the stream rather than the whole buffer.
			var doc struct {
				Target *struct {
					Kind     string `json:"kind"`
					Endpoint string `json:"endpoint"`
				} `json:"target"`
			}
			if err := json.NewDecoder(strings.NewReader(stdout)).Decode(&doc); err != nil {
				t.Fatalf("%s --json: decoding %q: %v", path, stdout, err)
			}
			if doc.Target == nil || doc.Target.Endpoint != srv.URL {
				t.Errorf("`burrow %s --json` carried no target, so an agent composing the result cannot "+
					"say where it happened.\ngot: %s\n%s", path, stdout, changeGuardRationale)
			}
		})
	}
}

// runChanging invokes one command against the stand-in control plane. It supplies a stdin the
// credential prompts can read, so a command that asks for a token does not block on the test
// runner's terminal; the value is a placeholder and never leaves the fake server.
func runChanging(t *testing.T, controlPlane string, args []string, asJSON bool) (stdout, stderr string, err error) {
	t.Helper()
	flags := []string{"--control-plane", controlPlane, "--token", "x"}
	if asJSON {
		flags = append(flags, "--json")
	}
	// Anything after a `--` belongs to the app's own command line (`app run web -- echo hi`), so the
	// connection flags go in front of it or they would be handed to the app instead.
	split := len(args)
	for i, a := range args {
		if a == "--" {
			split = i
			break
		}
	}
	var full []string
	full = append(full, args[:split]...)
	full = append(full, flags...)
	full = append(full, args[split:]...)
	var out, errb bytes.Buffer
	root := newRootCmd()
	root.SetArgs(full)
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetIn(strings.NewReader("placeholder\n"))
	err = root.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

// registeredCommandPaths returns every command path the real `burrow` tree registers, so the
// classification is checked against the shipped surface rather than a copy of the registration
// code. Cobra's own generated helpers (help, completion) are skipped: they are not Burrow commands
// and have nothing to say about a target.
func registeredCommandPaths(t *testing.T) []string {
	t.Helper()
	var paths []string
	var walk func(c *cobra.Command, prefix string)
	walk = func(c *cobra.Command, prefix string) {
		for _, sub := range c.Commands() {
			if sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			path := strings.TrimSpace(prefix + " " + sub.Name())
			paths = append(paths, path)
			walk(sub, path)
		}
	}
	walk(newRootCmd(), "")
	sort.Strings(paths)
	return paths
}
