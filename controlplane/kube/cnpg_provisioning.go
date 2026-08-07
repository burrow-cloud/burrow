// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The two resources an app's database is provisioned THROUGH (ADR-0066 §2). They are addressed with
// the dynamic client for cnpgClusterGVR's reason: Burrow does not import CloudNativePG's Go module.
var (
	cnpgDatabaseGVR     = schema.GroupVersionResource{Group: CNPGAPIGroup, Version: "v1", Resource: "databases"}
	cnpgDatabaseRoleGVR = schema.GroupVersionResource{Group: CNPGAPIGroup, Version: "v1", Resource: "databaseroles"}
)

const (
	cnpgDatabaseKind     = "Database"
	cnpgDatabaseRoleKind = "DatabaseRole"
	// cnpgAPIVersionV1 is the object's identity in the write itself, for both kinds above.
	cnpgAPIVersionV1 = CNPGAPIGroup + "/v1"

	// The reclaim policies, which are what decides whether deleting one of these objects destroys the
	// database behind it. BOTH VALUES ARE WRITTEN DELIBERATELY, at different moments, and the pair is
	// the whole safety argument:
	//
	//   - An attach writes `retain`. CloudNativePG's default agrees, and it is spelled anyway — so
	//     that no accident that removes an object (a namespace teardown, a garbage collector, a
	//     `kubectl delete` by somebody tidying up) can take an app's data with it.
	//   - A detach writes `delete`, immediately before deleting the object, because `addon detach` IS
	//     destructive (ADR-0031) and the user was told so at the confirmation prompt.
	//
	// So destruction is never a side effect of an object going away; it happens only where something
	// deliberately said so first, which is exactly the distinction ADR-0064 draws for `addon remove`
	// and this pair draws for one app.
	cnpgReclaimRetain = "retain"
	cnpgReclaimDelete = "delete"

	// cnpgEnsurePresent is the `Database.spec.ensure` value that means "make sure it exists". The
	// other value, `absent`, also drops the database — the teardown path uses the reclaim policy
	// instead, because `DatabaseRole` has no `ensure` field at all and one mechanism spelled the same
	// way on both objects is easier to be right about than two.
	cnpgEnsurePresent = "present"

	// cnpgReloadLabel is the label CloudNativePG watches a referenced Secret under. Without it a
	// password written into the Secret after the role was created is never applied to the role, so a
	// re-attach would hand an app a connection string the server does not honour. EnsureAppDatabase
	// does not take that on trust — it proves the credential works before returning it.
	cnpgReloadLabel = "cnpg.io/reload"
)

// provisioningObjectName is the name of the `Database`, the `DatabaseRole` and the role's password
// Secret that provision app on instance. All three share it: they are one attachment, and three
// naming rules for one thing is three ways to get it wrong.
//
// THE SEPARATOR IS A DOT, AND THAT IS THE WHOLE OF THE CARE HERE. Every environment's instance lives
// in the SAME add-on namespace (ADR-0067 §1), so this name has to be injective over (instance, app)
// or two environments collide on one object — issue #339 again, one layer down. A hyphen is not:
// `burrow-postgres` + `staging-web` and `burrow-postgres-staging` + `web` are different attachments
// with the same hyphen-joined name, and the second would adopt the first's object and point it at
// another environment's server. An app name cannot contain a dot (appIdentifier admits none) and an
// instance name cannot either, so the dot splits the two back apart unambiguously — and a dot is
// legal in an object name, which is a DNS-1123 subdomain rather than a label.
func provisioningObjectName(instance, app string) string { return instance + "." + app }

// cnpgObjects is the pair of resource interfaces the provisioner writes an attachment through, in
// the target instance's namespace, or an error when no dynamic client is wired.
//
// It FAILS CLOSED rather than falling back to admin SQL. There is no second mechanism: a provisioner
// that cannot address custom resources cannot provision, and saying so is better than opening a
// superuser connection nobody asked for.
func (p *PostgresProvisioner) cnpgObjects(target PostgresTarget) (databases, roles dynamic.ResourceInterface, err error) {
	if p.dynamic == nil {
		return nil, nil, fmt.Errorf("kube: the postgres provisioner was given no dynamic client, so it cannot write the %s and %s objects an attach provisions through: %w",
			cnpgDatabaseKind, cnpgDatabaseRoleKind, controlplane.ErrInvalid)
	}
	return p.dynamic.Resource(cnpgDatabaseGVR).Namespace(target.Namespace),
		p.dynamic.Resource(cnpgDatabaseRoleGVR).Namespace(target.Namespace), nil
}

// databaseRoleSpec is the `DatabaseRole` that gives app its login role on the target instance.
//
// IT SPELLS EVERY ATTRIBUTE IT WANTS, because adoption is total: creating a `DatabaseRole` for a role
// that already exists alters that role until it matches the manifest, so an attribute left out is an
// attribute reset to its default. That is exactly what is wanted here — the roles an earlier release
// created with `CREATE ROLE <role> WITH LOGIN PASSWORD ...` have LOGIN and nothing else, which is
// what this asks for — but it is a property worth stating, because the same mechanism would quietly
// strip a privilege an operator had granted the role by hand.
//
// There is deliberately no `ensure` key: the field exists on the inline `managed.roles` stanza, not
// on this CRD, and a key the CRD does not carry is PRUNED by the API server in silence.
func databaseRoleSpec(target PostgresTarget, app string) map[string]any {
	return map[string]any{
		"cluster": map[string]any{"name": target.Instance},
		"name":    roleName(app),
		// The role logs in and does nothing else: no superuser, no createdb, no createrole, no role
		// memberships. An app's role owns its own database and reaches no other (ADR-0031).
		"login":                     true,
		"passwordSecret":            map[string]any{"name": provisioningObjectName(target.Instance, app)},
		"databaseRoleReclaimPolicy": cnpgReclaimRetain,
	}
}

// databaseRoleTeardownSpec is the same role, described one last time by a detach: keep it until this
// object is deleted, then DROP it.
//
// It names NO password Secret, and that is not an oversight. The credential is irrelevant to a role
// about to be dropped, and a reference to a Secret that a half-finished detach already removed —
// or that an attachment predating declarative provisioning never had — would leave CloudNativePG
// unable to apply the object at all, which is to say unable to drop the role.
func databaseRoleTeardownSpec(target PostgresTarget, app string) map[string]any {
	return map[string]any{
		"cluster":                   map[string]any{"name": target.Instance},
		"name":                      roleName(app),
		"login":                     true,
		"databaseRoleReclaimPolicy": cnpgReclaimDelete,
	}
}

// databaseSpec is the `Database` that gives app its database on the target instance, owned by the
// login role above. The database is named after the app, which is ADR-0031's contract and is what
// every DATABASE_URL already says.
//
// `allowConnections` is stated rather than left to the default, because the teardown spec below
// turns it OFF and a re-attach has to turn it back on. A field only ever written in one direction is
// a field that eventually gets stuck there.
//
// Note what is NOT expressible here: the `REVOKE CONNECT ... FROM PUBLIC` that keeps one app's role
// out of another app's database. The CRD has no grant field of any kind, so that statement survives
// on its own path — see lockDownDatabase, which runs it as the database's OWNER.
func databaseSpec(target PostgresTarget, app string) map[string]any {
	return map[string]any{
		"cluster":               map[string]any{"name": target.Instance},
		"name":                  app,
		"owner":                 roleName(app),
		"ensure":                cnpgEnsurePresent,
		"allowConnections":      true,
		"databaseReclaimPolicy": cnpgReclaimRetain,
	}
}

// databaseTeardownSpec is the same database, described one last time by a detach: stop accepting
// connections, and drop when this object is deleted.
//
// `allowConnections: false` is the load-bearing half, and it exists because of a gap between what
// Burrow used to do and what CloudNativePG does. The admin SQL this replaced dropped the database
// `WITH (FORCE)`, which terminates whatever is still connected; the operator issues a plain
// `DROP DATABASE IF EXISTS`, which a single live session refuses. Turning connections off first is
// how the drop stops depending on nothing having reconnected in the meantime — the engine has
// already taken the credential out of the app's environment and rolled it, and this closes the door
// behind it.
func databaseTeardownSpec(target PostgresTarget, app string) map[string]any {
	return map[string]any{
		"cluster":               map[string]any{"name": target.Instance},
		"name":                  app,
		"owner":                 roleName(app),
		"ensure":                cnpgEnsurePresent,
		"allowConnections":      false,
		"databaseReclaimPolicy": cnpgReclaimDelete,
	}
}

// applyCNPGObject writes the object named name with the given spec, creating it if it is absent and
// updating its spec if it is not.
//
// AN OBJECT THAT IS ALREADY THERE IS ADOPTED, NOT RECREATED, and that is the upgrade path: an
// install that has been attaching apps since before this existed has the databases and the roles
// already, and the first attach after the upgrade writes objects that describe what is there rather
// than asking for it a second time. CloudNativePG reconciles an existing database with `ALTER
// DATABASE` and an existing role by altering it to match, so nothing is dropped and nothing is
// re-created.
//
// Only `spec` is written. The status is the operator's, and the labels are set on create so a
// re-attach cannot fight a label somebody added by hand.
func applyCNPGObject(ctx context.Context, ri dynamic.ResourceInterface, kind, name, namespace string, spec map[string]any, labels map[string]string) error {
	existing, err := ri.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": cnpgAPIVersionV1,
			"kind":       kind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
				"labels":    toStringMap(labels),
			},
			"spec": spec,
		}}
		if _, err := ri.Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("kube: creating the CloudNativePG %s %q: %w", kind, name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("kube: reading the CloudNativePG %s %q: %w", kind, name, err)
	}
	updated := existing.DeepCopy()
	if err := unstructured.SetNestedMap(updated.Object, spec, "spec"); err != nil {
		return fmt.Errorf("kube: composing the CloudNativePG %s %q: %w", kind, name, err)
	}
	if _, err := ri.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("kube: updating the CloudNativePG %s %q: %w", kind, name, err)
	}
	return nil
}

// awaitCNPGApplied blocks until the operator reports it has applied the object, the deadline
// expires, or ctx is cancelled.
//
// PROVISIONING IS ASYNCHRONOUS NOW, and this is where that lands. A `CREATE DATABASE` that burrowd
// issued itself either returned or did not; an object is written, accepted, and then reconciled by
// somebody else, so the only honest answer to "does this app have a database" comes from
// `status.applied` — which is also what makes a burrowd restarted mid-attach harmless: the object is
// already there and the operator finishes regardless of who is watching.
//
// `status.message` is carried into the timeout rather than failed on. The operator writes it while
// it is still retrying — an instance that is still starting says so through this field — and a
// provisioner that gave up on the first message would fail an attach issued a second too early. What
// the message is for is the timeout: "the operator did not apply this in two minutes" is a shrug,
// and "…and it has been saying `database "postgres" is reserved` the whole time" is a diagnosis.
func awaitCNPGApplied(ctx context.Context, ri dynamic.ResourceInterface, kind, name string, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		u, err := ri.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("kube: reading the CloudNativePG %s %q: %w", kind, name, err)
		}
		if msg, found, _ := unstructured.NestedString(u.Object, "status", "message"); found && msg != "" {
			last = msg
		}
		// `observedGeneration` is what tells a status about THIS spec from a status left over from the
		// previous one. It is absent until the operator has written a status at all, and an absent one
		// is not read as stale: a fresh object with no status simply has not been reconciled yet, and
		// the applied check below is what waits for it.
		applied, hasApplied, _ := unstructured.NestedBool(u.Object, "status", "applied")
		observed, hasObserved, _ := unstructured.NestedInt64(u.Object, "status", "observedGeneration")
		current := !hasObserved || observed >= u.GetGeneration()
		if hasApplied && applied && current {
			return nil
		}
		if time.Now().After(deadline) {
			if last != "" {
				return fmt.Errorf("kube: CloudNativePG has not applied the %s %q after %s; it reports: %s", kind, name, timeout, last)
			}
			return fmt.Errorf("kube: CloudNativePG has not applied the %s %q after %s, and reported nothing about why — is the operator running?", kind, name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// ensureRolePasswordSecret writes the password CloudNativePG gives app's login role, in the shape
// the `passwordSecret` reference requires: type `kubernetes.io/basic-auth`, with `username` and
// `password`, carrying the reload label so a rewritten password reaches the running server.
//
// THE PASSWORD NEVER REACHES SQL ANY MORE. It is written here and read by the operator; burrowd
// composes a connection string from it and hands that to exactly one caller. Nothing in this package
// interpolates it into a statement, which is why quoteLiteral is gone.
//
// A Secret in the wrong SHAPE is refused rather than worked around, for ensureCNPGSuperuserSecret's
// reason: a Secret's type is immutable, so an Opaque one cannot be converted, and CloudNativePG
// would reject the reference and leave the role without the password this attach is about to hand
// out. The error names the Secret; it never names a value.
func (p *PostgresProvisioner) ensureRolePasswordSecret(ctx context.Context, target PostgresTarget, app, password string, labels map[string]string) error {
	name := provisioningObjectName(target.Instance, app)
	secrets := p.client.CoreV1().Secrets(target.Namespace)
	data := map[string][]byte{
		cnpgSecretUsernameKey: []byte(roleName(app)),
		PostgresPasswordKey:   []byte(password),
	}
	existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: target.Namespace, Labels: labels},
			Type:       corev1.SecretTypeBasicAuth,
			Data:       data,
		}
		if _, err := secrets.Create(ctx, sec, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("kube: creating the role password secret %s/%s: %w", target.Namespace, name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("kube: reading the role password secret %s/%s: %w", target.Namespace, name, err)
	}
	if existing.Type != corev1.SecretTypeBasicAuth {
		return fmt.Errorf("kube: the role password secret %s/%s is of type %s, and CloudNativePG's role management requires %s. A Secret's type cannot be changed, so delete this one and re-run the attach: %w",
			target.Namespace, name, existing.Type, corev1.SecretTypeBasicAuth, controlplane.ErrInvalid)
	}
	updated := existing.DeepCopy()
	updated.Data = data
	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}
	// The reload label is added to a Secret that predates it rather than replacing the label set: an
	// attachment made before the label existed would otherwise rotate a password the operator never
	// applies.
	updated.Labels[cnpgReloadLabel] = "true"
	if _, err := secrets.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("kube: updating the role password secret %s/%s: %w", target.Namespace, name, err)
	}
	return nil
}

// deleteCNPGObject removes one provisioning object and blocks until it is actually gone. An object
// that is already absent is a no-op, not an error — a detach of something half-attached has to be
// able to finish.
//
// WAITING IS THE POINT, not politeness. The object carries a finalizer, and CloudNativePG only
// releases it once it has done what the reclaim policy asks — so on the teardown path, where that
// policy is `delete`, the object disappearing is the only evidence the database was actually
// dropped. Returning at the delete call would report a destroyed database on the strength of having
// asked, which for the one verb a user runs to get rid of data is the wrong thing to be optimistic
// about.
//
// A drop the operator cannot perform yet — a session still attached to the database — is retried by
// the operator and shows up here as the object not going away. That is why the deadline is generous
// and why the error says what it says.
func deleteCNPGObject(ctx context.Context, ri dynamic.ResourceInterface, kind, name string, timeout, poll time.Duration) error {
	if err := ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: deleting the CloudNativePG %s %q: %w", kind, name, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		_, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("kube: reading the CloudNativePG %s %q: %w", kind, name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("kube: CloudNativePG has not finished removing the %s %q after %s, so the database it describes may still be there; nothing has been left in the app's environment pointing at it, and re-running this detach resumes the removal", kind, name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
