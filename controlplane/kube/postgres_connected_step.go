// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"
	"time"
)

// PostgresConnectedStepKind names one of the steps an attachment needs a live SQL connection for.
//
// It exists so a substituted implementation is told WHAT it is being asked to do, not merely handed
// a list of statements. The two steps are performed at different moments, against a different
// disposition of the same objects, and an implementation that runs them somewhere else — a Job, a
// remote executor — usually wants to say so in the object it creates and in the error it raises when
// it fails. A kind is also the thing a `switch` can be exhaustive over, which an opaque string of SQL
// is not.
type PostgresConnectedStepKind string

const (
	// PostgresRevokePublicConnect is the ATTACH step: the `REVOKE CONNECT ... FROM PUBLIC` that keeps
	// one app's role out of another app's database (lockDownDatabase). It runs after every object of
	// the attachment has been applied and before any connection string is handed out, and an attach
	// whose revoke did not run is an attach that failed — a database with PostgreSQL's default ACL
	// intact is reachable by every other login role on the instance.
	PostgresRevokePublicConnect PostgresConnectedStepKind = "revoke-public-connect"

	// PostgresReleaseOwnedObjects is the plain-DETACH step: the `REASSIGN OWNED` + `DROP OWNED` pair
	// that empties the app's login role so it can be dropped while its data survives
	// (releaseWhatTheRoleOwns). It runs after the `Database` object has handed its owner to the data
	// role and before the login role's object is deleted; running it earlier reassigns objects the
	// operator is about to hand back, and skipping it leaves a `DROP ROLE` that fails forever.
	PostgresReleaseOwnedObjects PostgresConnectedStepKind = "release-owned-objects"
)

// PostgresConnectedStep is one unit of work an attachment cannot express as an object: some
// statements, and the credential they are to be run as.
//
// IT IS A DESCRIPTION, NOT A DELEGATION OF THE DECISION. Everything in it — the object names, the
// role names, the statements, the order the steps arrive in — is decided by the provisioner, which
// is the same place that decides what the `Database` and the `DatabaseRole` say. An implementation
// chooses HOW the step is performed and nothing else. That is the whole point of the seam: an
// embedder that reimplemented the declarative half in order to move the connected half would have
// two answers to "what does a provisioned attachment consist of", and they agree only for as long as
// nobody edits one of them (issue #532).
//
// It is passed BY VALUE and must be treated as read-only; the provisioner reuses nothing from it
// after the call returns.
type PostgresConnectedStep struct {
	// Kind is which step this is. An implementation that does not recognise a kind must FAIL rather
	// than skip it: every step here is load-bearing, and a new one will be added the same way these
	// were.
	Kind PostgresConnectedStepKind

	// Target is the instance the step runs against — the pair that names the CloudNativePG `Cluster`,
	// the Service (Target.Host()), and the namespace every object of this attachment lives in.
	Target PostgresTarget
	// Environment is the environment the attachment belongs to. It selects the instance above and is
	// worth carrying into an implementation's own errors: "web's attach failed" is ambiguous across
	// environments in a way "web's attach in staging failed" is not.
	Environment string
	// App is the app the attachment belongs to, and Database is the database the statements are to be
	// run against. They are the same string today (ADR-0031: an app called web has a database called
	// web) and are separate fields anyway, because an implementation needs the database to connect and
	// the app to describe what it is doing, and reading one off the other is how a convention becomes
	// an assumption.
	App      string
	Database string

	// Role is the role the statements must be run AS, and Password is that role's freshly written
	// password. Both are the app's own login role — never the instance's superuser, on any step. The
	// owner of a database holds its privileges with grant option implicitly, and the login role holds
	// the data role's through its membership, so the app's own credential is the least-privileged one
	// that can issue every statement below.
	//
	// PASSWORD IS A SECRET VALUE. It must not be logged, returned in an error, or written into
	// anything a `ps` listing or an object spec would show. An implementation that runs the step
	// elsewhere should prefer PasswordSecret.
	Role     string
	Password string
	// PasswordSecret is the name of the `kubernetes.io/basic-auth` Secret in Target.Namespace that
	// holds the same credential — the one CloudNativePG applies to the role (`username` and
	// PostgresPasswordKey). It is here so an implementation that runs the statements in a pod beside
	// the instance can mount the credential rather than carry it, and so that it does not have to
	// re-derive a name this package already decided.
	PasswordSecret string

	// Statements are the statements to run, IN ORDER, all of them, stopping at the first failure. They
	// are already quoted and are composed only from identifiers this package validated, so they carry
	// no caller input. Every one of them is idempotent: a step re-run after a failure finishes rather
	// than compounding.
	Statements []string

	// CredentialTimeout is how long the step may keep retrying a refused connection before giving up.
	//
	// IT IS NOT A POLITENESS BUDGET. CloudNativePG applies a rewritten password by reloading the
	// labelled Secret, which no `status.applied` covers, so REACHING THE SERVER WITH THIS CREDENTIAL
	// IS THE ONLY PROOF THAT THE PASSWORD LANDED — see lockDownDatabase. An implementation must
	// therefore retry an authentication refusal for at least this long before reporting failure, and
	// must never report success without having run the statements: an attach that skipped the proof
	// hands out a connection string that merely ought to work.
	CredentialTimeout time.Duration

	// Labels are the descriptive labels this attachment's objects carry (the add-on and the
	// environment). An implementation that creates an object of its own to perform the step should
	// stamp them on it, so what it creates is legible in the namespace as part of the same attachment.
	Labels map[string]string
}

// PostgresConnectedStepFunc performs one connected step, or reports why it could not.
//
// A NIL ERROR IS A CLAIM THAT EVERY STATEMENT RAN. Both callers treat this as the last thing between
// provisioning and reporting the operation complete, so an implementation that could not run the
// step — no route, a credential the server never accepted, a Job that failed — must return an error
// rather than nothing. Every failure mode here is one where the honest answer is that the attach or
// the detach did not finish.
type PostgresConnectedStepFunc func(ctx context.Context, step PostgresConnectedStep) error

// WithConnectedStep substitutes how the provisioner performs the steps that need a SQL connection,
// leaving everything else — which objects an attachment consists of, what they say, and in what
// order they are written — exactly where it is.
//
// A SELF-HOSTED INSTALL NEVER CALLS THIS. burrowd and the database share a cluster there, so the
// default runner opens its own connection to the instance's Service and the whole seam is invisible;
// nothing about an existing install is configuration it now has to supply.
//
// IT IS FOR AN EMBEDDER WHOSE CONTROL PLANE IS NOT ON THE DATABASE'S NETWORK. A managed product that
// runs its control plane on one provider and its tenant instances on another, reachable only through
// that fleet's API server, can write every object of an attachment through that API server — and
// cannot dial a ClusterIP. Before this existed the only way to move the connected half was to
// reimplement the declarative half around it, which duplicated the object names and the specs into a
// second place that the teardown path here still had to agree with, with nothing failing at compile
// time when the two drifted (issue #532). Such an implementation typically runs each step as a
// one-shot Job inside the target cluster, using PasswordSecret rather than Password.
func (p *PostgresProvisioner) WithConnectedStep(run PostgresConnectedStepFunc) *PostgresProvisioner {
	p.connected = run
	return p
}

// connectedStep composes the description of one step. It is the single place the credential, the
// object name, and the timeout of a step are decided, so the substituted path and the default path
// are answering the same question rather than two similar ones.
func (p *PostgresProvisioner) connectedStep(kind PostgresConnectedStepKind, target PostgresTarget, app, env, password string, statements ...string) PostgresConnectedStep {
	return PostgresConnectedStep{
		Kind:           kind,
		Target:         target,
		Environment:    env,
		App:            app,
		Database:       app,
		Role:           roleName(app),
		Password:       password,
		PasswordSecret: provisioningObjectName(target.Instance, app),
		Statements:     statements,
		// The same bound the default runner spends on a refused credential, handed on rather than left
		// for an implementation to invent: the thing being waited for is CloudNativePG reloading a
		// Secret, and it takes as long from a Job as it does from here.
		CredentialTimeout: p.credentialTimeout,
		Labels:            attachmentLabels(env),
	}
}

// runConnectedStep performs a step through whatever this provisioner was given, defaulting to the
// in-process connection.
func (p *PostgresProvisioner) runConnectedStep(ctx context.Context, step PostgresConnectedStep) error {
	if p.connected != nil {
		return p.connected(ctx, step)
	}
	return p.runConnectedStepInProcess(ctx, step)
}

// runConnectedStepInProcess is the default implementation, and the only one a self-hosted install
// ever uses: open one connection as the app's own role, run the statements over it, close it.
//
// It dials dialHostPort rather than the step's Target.Host(), because those two differ for exactly
// one caller — an out-of-cluster test reaching the instance through a port-forward — and that
// override must never leak into what an app is handed. A substituted implementation is inside the
// target cluster by construction and uses the Target.
func (p *PostgresProvisioner) runConnectedStepInProcess(ctx context.Context, step PostgresConnectedStep) error {
	hostPort, err := p.dialHostPort(step.Environment, step.Target.Instance)
	if err != nil {
		return err
	}
	db, err := p.connectAsApp(ctx, appDSN(step.Role, step.Password, hostPort, step.Database), step.App, step.Environment)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, stmt := range step.Statements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			// The caller says what the step was for; this says which statement of it stopped.
			return fmt.Errorf("running %s: %w", stmt, err)
		}
	}
	return nil
}
