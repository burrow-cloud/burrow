// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import "fmt"

// A hint names the way out of the situation the command just described, and the operate verbs are
// full of them: `app logs` says where durable history comes from, a deploy that ran out of settle
// time says which bound to raise, a truncated `addon sql` says which cap cut it short. Every one of
// those was written for a self-hosted install, where the reader operates the cluster and every
// command named is theirs to run.
//
// Against the managed product the reader is a tenant, and several of those commands are not theirs.
// `burrow addon install`, `burrow cluster config set` and `burrow addon config postgres <setting>`
// are refused there, because an add-on instance and the limits around it belong to whoever operates
// them (clusteronly.go). So `burrow app logs` on a managed target told the reader to install the
// logs add-on, which the product answers as not available — while `burrow addon logs`, the thing
// the sentence was steering them towards, already worked without it (issue #580).
//
// This is the same leak the managed control plane's refusals are worded to avoid, arriving from the
// other end. The refusals are careful to name no command the tenant cannot run; the client was
// volunteering one before any request was made.
//
// THE HINT IS A PROPERTY OF THE TARGET, not a sentence to be suppressed. Dropping it would fix the
// wrong advice and leave the reader with none, which on the `app logs` note is the worst of both:
// the paragraph exists because live pod logs are easy to mistake for a durable history, and a
// managed tenant makes that mistake exactly as readily as a self-hosted one. What differs is the way
// out, so what differs is the sentence — the tenant queries a service the platform already runs, and
// where a limit is genuinely not theirs to change, the honest hint says so and points at the read
// that shows the value in force. `burrow cluster config list` and `burrow addon logs` both answer on
// either kind of target (main.go, readClient), which is what makes a managed remedy available to
// state rather than merely a refusal to report.
//
// Each renderer takes the fact rather than looking it up, so the sentence is decided by the target
// the command actually connected to. commonOpts.managed records that at resolution time
// (resolveConnect and eitherTargetClient in main.go), which is why every caller here reads it only
// after its client is in hand.
//
// The cluster wording is unchanged in every case. A self-hosted operator's hint was correct and
// stays exactly as it was, byte for byte.

// onManagedTarget reports whether this invocation connected to the managed product. It reads what
// resolution recorded and asks nothing further: no config, no kubeconfig, no network.
//
// It is false before a client is built, and false for `--control-plane`, which names a control plane
// outright and consults no target (main.go, cloudTargetIfSelected). A control plane named by URL may
// be either kind and the CLI cannot tell, so it keeps the operator wording — the answer that has
// always been printed there, and the one a self-hosted install reached that way still needs.
func (o *commonOpts) onManagedTarget() bool {
	return o.managed
}

// logsSourceNote is the paragraph `burrow app logs` prints ahead of the log lines: what the reader
// is looking at, and where a durable history comes from instead.
//
// The warning half is identical on both, because the hazard is: these are live pod logs, they are
// the current pods only, and they are gone on a restart or a reschedule. Only the remedy differs.
// On a cluster, the logs add-on is something an operator installs, so the sentence names the install
// and the query. On the managed product the platform runs it, so the install is not merely
// unnecessary — `burrow addon install` is refused there — and the sentence names only the query,
// which is the one command that was ever going to help.
func logsSourceNote(managed bool) string {
	const source = "Source: live Kubernetes pod logs — current pods only, not retained across restarts. "
	if managed {
		return source + "For durable, queryable history across replicas, query the logs add-on the platform runs for your tenant with `burrow addon logs`."
	}
	return source + "For durable, queryable history across replicas, install the logs add-on (`burrow addon install logs`), then query with `burrow addon logs`."
}

// settleTimeoutHint is the line a deploy or a rollback prints when its rollout was still going when
// the settle timeout ran out. It is shared by both reports because it is the same fact about the
// same bound (deployreport.go, rollbackreport.go).
//
// `deploy.settle_timeout` is a limit an operator raises with `burrow cluster config set`, which a
// managed tenant cannot run. What a tenant CAN do is see the bound their deploy waited for: limits
// are listed by `burrow cluster config list`, which answers on either target (clusterconfig.go). So
// the managed form states who owns the bound and where to read it, rather than asking for a change
// that would be refused.
func settleTimeoutHint(managed bool) string {
	const observed = "Kubernetes is still rolling it out. "
	if managed {
		return observed + "The settle timeout is the platform's to set; `burrow cluster config list` shows the bound this rollout was given.\n"
	}
	return observed + "Raise the bound with `burrow cluster config set deploy.settle_timeout <duration>` if this app legitimately takes longer.\n"
}

// sqlRowCapHint is what `burrow addon sql` says under a result the row cap cut short. Narrowing the
// statement is advice that holds on any target and leads; raising the cap is `burrow cluster config
// set`, so it is the half that changes, and it changes into where the cap can be read.
func sqlRowCapHint(managed bool, env string) string {
	if managed {
		return "Narrow the statement. The row cap is the platform's to set; `burrow cluster config list` shows it."
	}
	return fmt.Sprintf("Narrow the statement, or raise the cap with `burrow cluster config set --env %s addon.sql_rows <n>`.", env)
}

// addonConfigListTail closes the bare `burrow addon config postgres` listing. The listing itself
// answers on either target — what an instance is set to is a fact about the tenant's own database —
// but changing a setting goes through `burrow addon config postgres <setting> <value>`, which
// reaches a cluster and is refused on the managed product (addonconfig.go, runAddonConfigSet).
//
// So the managed form says what the listing IS rather than what to do next. That is not a smaller
// answer than the cluster one: on the managed product the shape of the instance is the platform's
// to choose, and knowing what it is running is the whole of what the command was ever going to give
// a tenant.
func addonConfigListTail(managed bool) string {
	if managed {
		return "The platform operates this instance, so its shape is set there; this listing is what it is running."
	}
	return "Change one with `burrow addon config postgres <setting> <value>`."
}
