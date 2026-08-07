// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package agentsurface is the single source of truth for which Burrow capabilities the
// `burrow-agent` binary carries and which ones it deliberately does not.
//
// It exists to serve two readers from ONE table:
//
//   - The surface-guard test (cmd/burrow-agent/agent_surface_guard_test.go) reads the
//     agent-surface entries as its closed allow-list, so a command that appears in the binary
//     without an entry here fails the build.
//   - `guard` (on both CLIs) reads the entries that are NOT on the agent surface and reports them
//     to the agent, with what each capability is and who can perform it
//     ([ADR-0065](../../docs/adr/0065-what-belongs-on-the-agent-surface.md) §7).
//
// # Why the absent capabilities are reported at all
//
// ADR-0065 §5 is the reasoning, and it is a security argument rather than a usability one. Burrow
// has two ways to keep a capability away from an agent: leave the verb out of the binary (tier 1)
// or ship it with a guardrail that denies it (tier 2). They fail differently. A denied verb
// produces `denied: app.delete` — a legible refusal the agent can relay to a human, and one it can
// see coming by reading `guard` first. An ABSENT verb produces `unknown command`, which is a dead
// end; and a dead end is exactly what pushes an agent to route around the control channel
// entirely, reaching for `kubectl` or a shell. That is the failure
// [ADR-0021](../../docs/adr/0021-guardrails-require-control-plane-only-agent-access.md) says
// Burrow cannot close from the inside: once the agent is off the channel, the guardrails are not
// in the path at all.
//
// So a verb that is absent AND legible is a refusal the agent can relay — "removing an add-on is
// not something I can do, and here is who can" — which is what makes tier 1 tolerable rather than
// merely safe (ADR-0065 §7).
//
// Reporting this enumerates the surface to anything that can read it. ADR-0065 §7 accepts that
// outright: the CLI is open source and `--help` already reveals it, so the read carries no access
// control and no capability is withheld from the answer.
//
// # Why one table rather than two lists
//
// The absent list must not be a second, hand-maintained copy of the surface-guard test's
// allow-list, because two lists drift and the drift is silent. Instead every capability appears
// once, tagged with the surface that carries it, so moving a verb across the line is a single
// edit and both readers follow it. AbsentFrom goes further for the agent binary itself: it
// subtracts the paths the running command tree actually registers, so a verb REMOVED from the
// binary becomes legible in `guard` immediately, with no second edit anywhere.
package agentsurface

import "github.com/burrow-cloud/burrow/client"

// Surface names which of Burrow's two CLIs carries a capability
// ([ADR-0049](../../docs/adr/0049-burrow-agent-scoped-cli-control-channel.md)).
type Surface string

const (
	// Agent marks a capability compiled into `burrow-agent`: app lifecycle, scoped to the app the
	// agent was asked about, and gated server-side by the control-plane guardrails.
	Agent Surface = "agent"
	// Operator marks a capability the agent binary deliberately does not carry. It lives on the
	// human `burrow` CLI only, and `guard` reports it so the agent can say what it is and who can
	// run it instead of failing with `unknown command` (ADR-0065 §7).
	Operator Surface = "operator"
)

// WhoOperator is the answer to "who can do this instead" for every capability held back from the
// agent binary. It is one sentence on purpose: the agent relays it verbatim to a person, so it
// names the tool, the human, and the credential that tool needs.
const WhoOperator = "the burrow operator CLI, run by a human with the cluster's admin kubeconfig"

// Capability is one Burrow capability and the surface that carries it.
//
// The JSON field names are the shape `guard` reports for an absent capability, so an agent that
// hits a wall has, in one object, what the capability is (What), why it is not on its surface
// (Why), who can perform it (Who), and the exact command that person would run (Command).
type Capability struct {
	// Path is the command path the capability has, or WOULD have, on the agent surface —
	// space-separated, binary name omitted (e.g. "addon remove"). For an agent capability it is
	// the registered path and matches the surface-guard allow-list key. For an operator
	// capability it is the path the verb would occupy if it were compiled in, which is what makes
	// "is it registered?" a meaningful question to ask of the running command tree.
	Path string `json:"capability"`
	// Surface is which CLI carries it.
	Surface Surface `json:"-"`
	// What describes the capability in one line. For an agent capability it doubles as the reason
	// it qualifies for the surface under ADR-0065 §1 — the claim a reviewer checks.
	What string `json:"what"`
	// Why is the reason an operator capability is held back from the agent binary. Empty for an
	// agent capability.
	Why string `json:"why,omitempty"`
	// Who names who can perform it instead. Defaults to WhoOperator for an operator capability.
	Who string `json:"who,omitempty"`
	// Command is the operator CLI invocation, so the agent can hand a person something to run.
	Command string `json:"operator_command,omitempty"`
}

// GuardReport is what `guard` answers on both CLIs: the guardrail dispositions the control plane
// enforces, plus the capabilities absent from the agent binary (ADR-0065 §7).
//
// The two groups are separate fields on purpose. A denied verb and an absent verb are different
// answers — the first is a limit an operator can relax with `guard set`, the second is not on the
// binary at all — and an agent must be able to tell them apart without parsing prose.
type GuardReport struct {
	// Scope echoes what the answer is ABOUT when it is about something narrower than the whole
	// cluster, and is omitted otherwise. It is what makes each entry's `source` legible: `deny` with
	// `"source":"name"` means nothing on its own, and means "denied for the app website in prod, by a
	// disposition set for that app" beside `{"env":"prod","name":"website"}`. Without it a reader has
	// to remember the arguments the call was made with to interpret the answer, which an agent
	// relaying the answer to a human generally cannot (ADR-0085 §4, ADR-0065 §7).
	Scope      *GuardReportScope  `json:"scope,omitempty"`
	Guardrails []client.Guardrail `json:"guardrails"`
	// AbsentCapabilities is never omitted, even when empty: an agent reading this answer is asking
	// what it cannot do, and a missing key reads as "unknown" where an empty list reads as "none".
	AbsentCapabilities []Capability `json:"absent_capabilities"`
}

// AbsentCapabilitiesReport is what `burrow agent capabilities --json` answers: the capabilities the
// agent binary does not carry, on their own rather than beside a policy they have nothing to do with
// (issue #445).
//
// It carries the same key under the same name as GuardReport.AbsentCapabilities so the two answers
// cannot drift into two spellings of one list, and it is never omitted when empty for the same
// reason that field is not: a missing key reads as "unknown" where an empty list reads as "none".
type AbsentCapabilitiesReport struct {
	AbsentCapabilities []Capability `json:"absent_capabilities"`
}

// NewAbsentCapabilitiesReport wraps an absent-capability list for the standalone report,
// normalizing a nil list to an empty one so the JSON shape is stable.
func NewAbsentCapabilitiesReport(absent []Capability) AbsentCapabilitiesReport {
	if absent == nil {
		absent = []Capability{}
	}
	return AbsentCapabilitiesReport{AbsentCapabilities: absent}
}

// GuardReportScope names the environment and the one app or add-on instance a scoped report answers
// for (ADR-0085 §1). Both members are omitted when empty, so the global listing carries no scope at
// all rather than an object of blanks.
type GuardReportScope struct {
	Env  string `json:"env,omitempty"`
	Name string `json:"name,omitempty"`
}

// NewGuardReport pairs guardrail dispositions with absent capabilities for the scope they were read
// for, normalizing a nil absent list to an empty one so the JSON shape is stable.
//
// A scope that names nothing produces no `scope` member: the report is then the global policy, every
// entry's Source is empty because there is no tier to distinguish, and an object saying so would be
// noise on the answer every agent reads first.
func NewGuardReport(scope client.GuardScope, guardrails []client.Guardrail, absent []Capability) GuardReport {
	if absent == nil {
		absent = []Capability{}
	}
	if guardrails == nil {
		guardrails = []client.Guardrail{}
	}
	r := GuardReport{Guardrails: guardrails, AbsentCapabilities: absent}
	if scope.Env != "" || scope.Name != "" {
		r.Scope = &GuardReportScope{Env: scope.Env, Name: scope.Name}
	}
	return r
}

// catalogue is every Burrow capability that either sits on the agent surface or is deliberately
// held back from it, in the order `guard` reports them.
//
// This is THE membership list. The surface-guard test derives its closed allow-list from the
// Agent entries and `guard` derives its absent report from the Operator ones, so moving a
// capability across the line is one edit to one line here.
//
// ADR-0065 §1 supplies the membership rule. A capability qualifies for the agent surface unless it
// fails scope (its effect reaches beyond the app the agent was asked about — disqualifying
// outright, tier 1, Operator here) or reversibility alone (a human cannot restore the prior state
// — the operator decides, so it stays on the surface as a guardrail denied by default, tier 2, and
// remains an Agent entry). ADR-0065 §5 says prefer tier 2: absence is the blunter instrument and
// should be the exception.
var catalogue = []Capability{
	// ── The agent surface ───────────────────────────────────────────────────────────────────
	// Compute: the release lifecycle of an app that already has a namespace.
	{Surface: Agent, Path: "deploy", What: "deploys an existing app by image reference into its own namespace"},
	{Surface: Agent, Path: "build", What: "builds an image from a git ref as a Job in the operator-provisioned build namespace"},
	{Surface: Agent, Path: "rollback", What: "returns an app to a previous release"},
	{Surface: Agent, Path: "scale", What: "sets an app's replica count"},
	{Surface: Agent, Path: "autoscale", What: "configures an app's HorizontalPodAutoscaler"},
	{Surface: Agent, Path: "run", What: "runs a one-off command in the app's own image, guarded by app.run"},
	{Surface: Agent, Path: "delete", What: "removes an app the control plane already manages, guarded"},
	{Surface: Agent, Path: "apps", What: "read-only: the apps the control plane manages"},
	{Surface: Agent, Path: "status", What: "read-only: one app's release and workload state"},
	{Surface: Agent, Path: "history", What: "read-only: one app's release history"},
	{Surface: Agent, Path: "next-tag", What: "read-only: the next semver tag to build"},

	// Config and secrets: app-scoped values. Setting a secret VALUE is deliberately absent
	// (ADR-0029) and appears below as an Operator capability.
	{Surface: Agent, Path: "config", What: "read-only: one app's non-secret config vars"},
	{Surface: Agent, Path: "config set", What: "writes a non-secret config var on one app"},
	{Surface: Agent, Path: "config unset", What: "removes a non-secret config var from one app"},
	{Surface: Agent, Path: "secret", What: "read-only: one app's secret KEY names, never values"},
	{Surface: Agent, Path: "secret unset", What: "removes one app secret by key; carries no value"},

	// Health. The agent is on this surface because it is the only party that CAN close the gap
	// ADR-0076 describes: Burrow cannot write a health endpoint into the user's application, and the
	// agent has the source. The verb's effect is one app's readiness probe, it is reversible with
	// `health unset`, and it destroys nothing — ADR-0065 §1 straight down the middle.
	{Surface: Agent, Path: "health", What: "read-only: the readiness probe Burrow sets on one app, and why"},
	{Surface: Agent, Path: "health set", What: "declares the health endpoint one app serves, so Burrow probes it instead of just its port"},
	{Surface: Agent, Path: "health unset", What: "returns one app to the default readiness probe"},

	// The deploy-time dependency check (ADR-0076 §4), READ ONLY on this surface. The read moves no
	// value — a dependency carries an environment variable's key NAME and an in-cluster address
	// Burrow composed — and an agent needs it to reason about a failed dependency on a deploy result.
	// Turning the check OFF is deliberately absent: it is standing authority to stop verifying what
	// Burrow handed an app, the same class as a lifecycle hook or an auto-deploy level, and an agent
	// that could silence a check it was failing could make its own work look correct.
	{Surface: Agent, Path: "checks", What: "read-only: what Burrow checks after a deploy of one app, derived from what it provisioned"},

	// Add-ons: in-cluster building blocks the control plane deploys into its OWN add-on
	// namespace. burrowd holds only namespaced Roles and is forbidden from creating namespaces or
	// RBAC, so an add-on install cannot widen anything — where an add-on needs a ServiceAccount
	// (the metrics scraper), burrowd only VERIFIES the operator staged it and otherwise fails
	// cleanly (controlplane/kube/addons.go). The destructive add-on verbs are Operator entries
	// below.
	{Surface: Agent, Path: "addon", What: "group: the add-on operations"},
	{Surface: Agent, Path: "addon install", What: "deploys a building block for one environment into the existing add-on namespace; creates no namespace and no RBAC"},
	{Surface: Agent, Path: "addon attach", What: "gives one app a database on its environment's add-on instance; the URL is generated server-side"},
	{Surface: Agent, Path: "addon backup", What: "takes a backup of an add-on's data"},
	{Surface: Agent, Path: "addon backup-health", What: "reads how old the last backup is, how old the last one to leave the cluster is, the last failure, and whether the destination answers; changes nothing"},
	{Surface: Agent, Path: "addons", What: "read-only: the installed add-ons and their capabilities"},
	{Surface: Agent, Path: "backups", What: "read-only: the backups taken of an add-on"},

	// Routing: Services, Ingresses, and DNS records for an app.
	{Surface: Agent, Path: "expose", What: "creates a Service and Ingress for one app, guarded"},
	{Surface: Agent, Path: "unexpose", What: "removes an app's Service and Ingress"},
	{Surface: Agent, Path: "domain", What: "group: the DNS operations"},
	{Surface: Agent, Path: "domain add", What: "writes a DNS record at an already-configured provider, guarded"},
	{Surface: Agent, Path: "domain remove", What: "removes a DNS record at an already-configured provider"},
	{Surface: Agent, Path: "reachability", What: "read-only: whether an app is reachable, link by link"},

	// Diagnosis: reads only.
	{Surface: Agent, Path: "logs", What: "read-only: one app's pod logs"},
	{Surface: Agent, Path: "logs-query", What: "read-only: queries the installed logs add-on"},
	{Surface: Agent, Path: "metrics-query", What: "read-only: queries the installed metrics add-on"},
	{Surface: Agent, Path: "cluster", What: "read-only: the cluster's capabilities"},
	{Surface: Agent, Path: "cluster capacity", What: "read-only: scheduling headroom, from the Kubernetes API"},
	{Surface: Agent, Path: "audit", What: "read-only: the audit record of guarded operations"},
	// The failure ledger's read surface (ADR-0074 §8). It qualifies on ADR-0065 §1: a read that
	// changes nothing passes the reversibility test outright, and the scope test asks about the
	// blast radius of an EFFECT, which a read does not have — the same ground `cluster` and `audit`
	// are already here on. ADR-0074 §5 goes further and names the agent as the surface's most
	// important consumer: Burrow reports correlation and refuses to claim a cause, and turning
	// twenty rows into "the node pool was tainted at 02:14; remove the taint" is the agent's half
	// of that division of labour. Withholding it would leave the agent with a control plane that
	// knows what broke and no way to ask — the dead end ADR-0021 says pushes it to kubectl.
	{Surface: Agent, Path: "failures", What: "read-only: what is broken across everything Burrow manages, with the observation coverage behind the answer"},
	{Surface: Agent, Path: "guard", What: "read-only: the guardrail dispositions and the capabilities absent from this binary; `guard set` is operator-only (ADR-0020)"},

	// Targeting: selects among environments the OPERATOR registered.
	{Surface: Agent, Path: "providers", What: "read-only: the provider names already configured by the operator"},
	{Surface: Agent, Path: "environments", What: "read-only: lists the local environment handles; selects, never creates"},

	// ── Held back from the agent binary (reported by `guard`) ───────────────────────────────
	// Add-on teardown. ADR-0065 §2 names `addon remove` as THE tier-1 case: add-ons are one
	// instance per type per environment (ADR-0067 §1) and ADR-0031 puts every app in an
	// environment on that one Postgres, so this is not "remove an add-on" but "remove THE add-on"
	// — every attached app in the environment goes down at once. There is no configuration in
	// which its blast radius is small, so configurability would be a liability rather than a
	// feature.
	{
		Surface: Operator,
		Path:    "addon remove",
		What:    "removes an installed add-on from an environment",
		Why: "tier 1 (ADR-0065 §2): one add-on instance per type per environment, with every app in " +
			"that environment's database on it, so removing it takes them all down at once. Its blast " +
			"radius reaches far beyond the app the agent was asked about, in every configuration",
		Command: "burrow addon remove <name>",
	},
	{
		Surface: Operator,
		Path:    "addon remove --delete-data",
		What:    "removes an add-on AND destroys its retained data volumes",
		Why: "tier 1 (ADR-0065 §2, ADR-0064 §2): removal keeps the data by default precisely so it can " +
			"be recovered; deleting the volumes as well is unrecoverable and is a decision for a human",
		Command: "burrow addon remove <name> --delete-data",
	},
	{
		Surface: Operator,
		Path:    "addon detach",
		What:    "detaches one app from an add-on, dropping its database and role",
		Why: "destroys the app's data with no way back (ADR-0031); the agent can attach, which " +
			"provisions and destroys nothing, but the destructive counterpart is a human's call",
		Command: "burrow addon detach <addon> <app>",
	},
	{
		Surface: Operator,
		Path:    "addon restore",
		What:    "restores an app's database from a backup, overwriting its live contents",
		Why: "overwrites live data with a snapshot, so the state at the moment of the restore is gone " +
			"(ADR-0032); the agent can take backups, which destroy nothing",
		Command: "burrow addon restore <addon> <app> --backup <id>",
	},
	// The instance-wide restore is a SEPARATE entry from the per-app one, and keeping them apart is
	// the point ADR-0066 §4 makes about naming: the two operations differ by blast radius, not by
	// mechanism, and an agent told "restore is not something I can do" learns less than one told which
	// restore and why. `addon restore` fails reversibility for one app; this fails SCOPE — it rewinds
	// the instance every app in the environment shares — which is the disqualifying test, so it is
	// tier 1 in its own right rather than by association.
	{
		Surface: Operator,
		Path:    "addon restore-instance",
		What:    "rewinds a whole Postgres instance to a point in its object-storage repository",
		Why: "tier 1 (ADR-0065 §2, ADR-0066 §4): a physical recovery cannot be scoped to one app — every " +
			"database on the instance goes back to the same point together, so its blast radius is every " +
			"app in the environment no matter which one the agent was asked about. It also destroys the " +
			"instance's current state, which is a decision for a human at a terminal",
		Command: "burrow addon restore-instance <addon> --backup <id>",
	},

	// Object storage. ADR-0063 §5 splits the two bucket operations across two tiers, and the split
	// is the point of the record. CREATION is tier 3: additive, reversible, part of a legitimate
	// workflow, so it ships as the `bucket.create` guardrail at `confirm` and a human approves the
	// billable resource. DELETION is tier 1 and is not compiled into either binary — it is not an
	// operation Burrow performs at all, which is why the entry below names the vendor rather than
	// an operator command.
	{
		Surface: Operator,
		Path:    "bucket delete",
		What:    "deletes a bucket at an object-storage provider, and every backup in it",
		Why: "tier 1 (ADR-0065 §2, ADR-0063 §5): it fails the scope test twice over. Its blast radius " +
			"is every backup the platform holds, far beyond the app the agent was asked about; and a " +
			"bucket is addressed by a name in a GLOBAL namespace, so a mistaken argument does not " +
			"merely delete the wrong bucket of yours — it can reach outside the cluster entirely, " +
			"beyond anything Burrow's guardrails cover. Burrow therefore has no such command on " +
			"either CLI",
		Who: "a human, at the object-storage vendor's own console or CLI",
		// Burrow ships no bucket-deletion command, so there is no Burrow invocation to hand over.
		// Naming the vendor's own tool IS the answer here, and it is a better one than a Burrow
		// command would be: the vendor's CLI is what should perform it.
		Command: "the vendor's own CLI (e.g. `aws s3 rb`, `b2 bucket delete`), run by a human",
	},

	// Governance. `guard set` is load-bearing twice over: it is what an operator uses to relax or
	// tighten a guardrail, and its absence here is what makes every tier-2 `deny` trustworthy
	// rather than advisory (ADR-0065 "Consequences"). If the agent could set its own guardrails,
	// the middle tier would collapse in a single step.
	{
		Surface: Operator,
		Path:    "guard set",
		What:    "sets a guardrail's disposition (allow/confirm/deny), globally or per environment",
		Why: "an agent that could rewrite its own guardrails would have none (ADR-0020, ADR-0065); the " +
			"dispositions this command reports are exactly the limits the agent cannot move",
		Command: "burrow guard set [--env <name>] <code> <allow|confirm|deny>",
	},
	// Operational limits. ADR-0068 §4 puts SETTING a limit beside `guard set` in ADR-0065's tier 1,
	// and for the identical reason: a bound the agent can raise is not a bound. The replica ceiling
	// used to be a guardrail whose disposition the operator set, which meant it could be turned off
	// but not turned up; it is now a number a human sets, and the number lives behind this command.
	//
	// Only the WRITE is held back. Reading a limit is harmless and useful — an agent that could see
	// the ceiling could say it was about to exceed one — but ADR-0068 leaves the shape of that read
	// undecided, so nothing here is on the agent surface yet.
	{
		Surface: Operator,
		Path:    "cluster config set",
		What:    "sets an operational limit (e.g. the replica ceiling), for the cluster or for one environment",
		Why: "a bound the agent can raise is not a bound (ADR-0068 §4); a limit is a number a human " +
			"chooses, and exceeding it is refused rather than held for a confirmation the agent could " +
			"satisfy itself",
		Command: "burrow cluster config set [--env <name>] <limit> <value>",
	},
	{
		Surface: Operator,
		Path:    "secret set",
		What:    "sets a secret VALUE on an app",
		Why: "a secret value never crosses the agent control channel (ADR-0029); the agent can list " +
			"secret KEY names and unset a key, and never reads or writes a value",
		Command: "burrow app secret set <app> KEY=VALUE",
	},
	{
		Surface: Operator,
		Path:    "auto-deploy",
		What:    "arms burrowd to deploy new registry tags for an app automatically",
		Why: "standing authority to deploy without a further call is a policy decision the operator " +
			"makes once, deliberately, and opts into (ADR-0058)",
		Command: "burrow app auto-deploy <app> <patch|minor|major|off>",
	},
	// The rollback itself IS on the agent surface — it is the recovery action, allowed by default —
	// and only the override that steps around its `pre-rollback` hook is held back (ADR-0080 §3). The
	// entry is a flag variant for the same reason `addon remove --delete-data` is: the verb and the
	// dangerous way of spelling it are different capabilities, and reporting only the verb would leave
	// an agent that met a hook abort with no account of what closes it.
	//
	// It is reported rather than merely absent BECAUSE it is the incident path. An agent that hits a
	// blocked rollback and can say "a human runs `burrow app rollback web --skip-hooks`" has turned a
	// dead end into a handover; one that can only report a failure is the case ADR-0021 says ends with
	// the agent reaching for kubectl.
	{
		Surface: Operator,
		Path:    "rollback --skip-hooks",
		What:    "rolls an app back WITHOUT running its pre-rollback hook",
		Why: "tier 1 (ADR-0065 §2, ADR-0080 §3): deciding that a safety step does not apply is a " +
			"judgement about the situation — whether the schema is actually fine and the hook's failure " +
			"unrelated — and the blast radius of being wrong is an application serving against a schema " +
			"nobody verified. The rollback itself is on this surface; only skipping the hook is not",
		Command: "burrow app rollback <app> --skip-hooks",
	},
	{
		Surface: Operator,
		Path:    "hook set",
		What:    "stores the command an app runs at a lifecycle phase (pre-deploy, pre-rollback)",
		Why: "a pre-deploy hook runs on EVERY deploy of the app, including unattended ones, so setting " +
			"one is standing authority for a command the agent would not be present to see fail " +
			"(ADR-0072 §1); the agent can already sequence a command itself with `run`",
		Command: "burrow app hook set <app> --on <phase> -- <command>",
	},

	// Setup. Every entry below creates namespaces, writes RBAC, replaces the running control
	// plane, admits a node, or stores a credential. An agent reads untrusted input by its nature,
	// so prompt injection is a live path into whatever it can express; a verb here would let an
	// agent WIDEN ITS OWN CONTROL PLANE'S ACCESS on the user's cluster (ADR-0049 §2a).
	{
		Surface: Operator,
		Path:    "cluster install",
		What:    "installs the Burrow control plane into a Kubernetes context",
		Why:     "creates namespaces, RBAC, and credentials: the boundary the agent runs inside (ADR-0049 §2a, ADR-0054)",
		Command: "burrow cluster install <kube-context>",
	},
	{
		Surface: Operator,
		Path:    "cluster upgrade",
		What:    "rolls the installed control plane forward in place",
		Why:     "replaces the component that holds the cluster credentials and enforces the guardrails (ADR-0049 §2a)",
		Command: "burrow cluster upgrade",
	},
	{
		Surface: Operator,
		Path:    "cluster bootstrap",
		What:    "turns a bare VPS into a single-node cluster with the control plane on it",
		Why:     "stands up the cluster itself, over the human's own SSH session (ADR-0044)",
		Command: "burrow cluster bootstrap",
	},
	{
		Surface: Operator,
		Path:    "cluster ingress install",
		What:    "provisions the shared ingress/TLS infrastructure (ingress-nginx, cert-manager, an issuer)",
		Why:     "cluster-wide infrastructure with its own RBAC, installed once by a human (ADR-0060)",
		Command: "burrow cluster ingress install",
	},
	{
		Surface: Operator,
		Path:    "cluster registry install",
		What:    "provisions the optional in-cluster image registry",
		Why:     "cluster-wide infrastructure and a credential, installed once by a human (ADR-0060)",
		Command: "burrow cluster registry install",
	},
	{
		Surface: Operator,
		Path:    "join",
		What:    "joins this machine to an installed Burrow control plane using a join token",
		Why:     "admits a principal to the cluster and writes local credentials (ADR-0038)",
		Command: "burrow join <token>",
	},
	{
		Surface: Operator,
		Path:    "env add",
		What:    "registers an environment",
		Why: "registering an environment creates a namespace, which is the operator's boundary to " +
			"draw; the agent can list environments and select one, never create one",
		Command: "burrow env add <name>",
	},
	{
		Surface: Operator,
		Path:    "provider add",
		What:    "stores a cloud/DNS provider credential for Burrow to use",
		Why:     "a provider credential is a human/CLI step and never reaches the agent (ADR-0030)",
		Command: "burrow config provider add <type>",
	},
	{
		Surface: Operator,
		Path:    "registry login",
		What:    "stores a container-registry credential for the cluster to pull with",
		Why:     "a registry credential is a human/CLI step and never reaches the agent (ADR-0030)",
		Command: "burrow config registry login <host>",
	},
	{
		Surface: Operator,
		Path:    "addon connect",
		What:    "points Burrow at an add-on backend already running elsewhere",
		Why:     "wires cluster-wide backing services and their endpoints, which is operator setup",
		Command: "burrow addon connect <backend>",
	},
	{
		Surface: Operator,
		Path:    "agent install",
		What:    "wires an AI coding tool up to burrow-agent (permission rules and orientation)",
		Why: "it configures the agent's own permissions on the human's workstation; an agent that " +
			"could run it could widen what it is allowed to run",
		Command: "burrow agent <tool> install",
	},
}

// AgentSurface returns the capabilities compiled into `burrow-agent`, keyed by command path with
// the one-line reason each qualifies. It is the closed allow-list the surface-guard test enforces:
// a command registered without an entry here fails that test, and an entry here with no registered
// command fails it too.
func AgentSurface() map[string]string {
	out := make(map[string]string, len(catalogue))
	for _, c := range catalogue {
		if c.Surface == Agent {
			out[c.Path] = c.What
		}
	}
	return out
}

// AbsentFrom returns the capabilities in the catalogue that `registered` does not contain, each
// with what it is, why it is not on the agent surface, and who can perform it (ADR-0065 §7).
//
// The subtraction is what keeps this answer honest. Passing the paths a running command tree
// actually registers means a verb removed from the binary becomes legible here immediately, with
// no second edit: it stops matching a registered path and starts being reported as absent.
// Capability paths that are not commands at all — `addon remove --delete-data`, a flag variant —
// never match anything and so are always reported.
func AbsentFrom(registered []string) []Capability {
	have := make(map[string]bool, len(registered))
	for _, p := range registered {
		have[p] = true
	}
	var out []Capability
	for _, c := range catalogue {
		if have[c.Path] {
			continue
		}
		if c.Who == "" {
			c.Who = WhoOperator
		}
		out = append(out, c)
	}
	return out
}

// AbsentFromAgentSurface returns the capabilities absent from `burrow-agent` as DECLARED by the
// catalogue, for callers that cannot walk the agent binary's command tree — the operator CLI,
// which is a different binary. The two answers agree whenever the surface-guard test passes, since
// that test is what pins the declaration to the real command tree.
func AbsentFromAgentSurface() []Capability {
	var registered []string
	for _, c := range catalogue {
		if c.Surface == Agent {
			registered = append(registered, c.Path)
		}
	}
	return AbsentFrom(registered)
}
