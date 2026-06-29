// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// newPostgresEngine builds an engine wired with a fake database provisioner, returning the seams a
// Postgres attach/detach test needs to arrange and inspect.
func newPostgresEngine(t *testing.T) (*cp.Engine, *fake.Kubernetes, *fake.Database, *fake.Provisioner) {
	t.Helper()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	prov := fake.NewProvisioner()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Registry: fake.NewRegistry(), Database: d,
		Clock: fake.NewClock(time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
		DatabaseProvisioner: prov,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, k, d, prov
}

// TestPostgresCatalogEntry asserts the catalog carries a well-formed AddonPostgres entry: the
// official image, port 5432, persistent storage, the "database" capability, and a summary.
func TestPostgresCatalogEntry(t *testing.T) {
	spec, ok := cp.LookupAddon(cp.AddonPostgres)
	if !ok {
		t.Fatal("AddonPostgres is not in the catalog")
	}
	if spec.Image != "postgres:17.4" {
		t.Errorf("image = %q, want postgres:17.4", spec.Image)
	}
	if spec.Port != 5432 {
		t.Errorf("port = %d, want 5432", spec.Port)
	}
	if spec.StorageGi != 10 {
		t.Errorf("storage = %dGi, want 10", spec.StorageGi)
	}
	if len(spec.Capabilities) != 1 || spec.Capabilities[0] != "database" {
		t.Errorf("capabilities = %v, want [database]", spec.Capabilities)
	}
	if spec.Summary == "" {
		t.Error("summary is empty")
	}
	// It appears in the stable catalog listing.
	var found bool
	for _, s := range cp.AddonCatalog() {
		if s.Type == cp.AddonPostgres {
			found = true
		}
	}
	if !found {
		t.Error("AddonPostgres is missing from AddonCatalog()")
	}
}

// TestAttachPostgres asserts attach provisions the database, writes the generated URL into the
// app's Secret under DATABASE_URL, rolls the workload, and returns only the key name. The URL value
// is the one the provisioner generated and is never returned.
func TestAttachPostgres(t *testing.T) {
	ctx := context.Background()
	e, k, _, prov := newPostgresEngine(t)

	// An app with a running workload so the restart path is exercised.
	if err := k.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Kind: cp.WorkloadDeployment, Image: "busybox", Replicas: 1}); err != nil {
		t.Fatalf("seed workload: %v", err)
	}

	res, err := e.AttachAddon(ctx, cp.AddonPostgres, "web")
	if err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	if res.SecretKey != "DATABASE_URL" || res.App != "web" || res.Addon != cp.AddonPostgres {
		t.Errorf("result = %+v, want web/postgres/DATABASE_URL", res)
	}
	// The provisioner was asked to provision web.
	if got := prov.Ensured(); len(got) != 1 || got[0] != "web" {
		t.Errorf("EnsureAppDatabase called with %v, want [web]", got)
	}
	// The generated URL was written into the app's Secret under DATABASE_URL.
	val, ok := k.SecretValue("web", "DATABASE_URL")
	if !ok {
		t.Fatal("DATABASE_URL was not written into the app's Secret")
	}
	if val != fake.URLFor("web") {
		t.Errorf("stored DATABASE_URL = %q, want the provisioner-generated URL", val)
	}
	// The workload was rolled.
	if _, rolled := k.RestartedAt("web"); !rolled {
		t.Error("attach did not roll the workload")
	}
}

// TestAttachPostgresNoWorkload attaches an app that is not running yet: it still provisions and
// writes the Secret, and a missing workload is not an error.
func TestAttachPostgresNoWorkload(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newPostgresEngine(t)
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web"); err != nil {
		t.Fatalf("AttachAddon with no workload: %v", err)
	}
	if _, ok := k.SecretValue("web", "DATABASE_URL"); !ok {
		t.Error("DATABASE_URL should be written even with no running workload")
	}
}

// TestAttachUnknownAddon and a bad app name are rejected as ErrInvalid before any provisioning.
func TestAttachRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	e, _, _, prov := newPostgresEngine(t)
	if _, err := e.AttachAddon(ctx, cp.AddonCache, "web"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("attach non-postgres err = %v, want ErrInvalid", err)
	}
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "Bad_Name"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("attach bad app name err = %v, want ErrInvalid", err)
	}
	if got := prov.Ensured(); len(got) != 0 {
		t.Errorf("provisioner should not be called on invalid input, got %v", got)
	}
}

// TestAttachWithoutProvisioner errors cleanly when the provisioner is not wired.
func TestAttachWithoutProvisioner(t *testing.T) {
	ctx := context.Background()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Registry: fake.NewRegistry(), Database: d,
		Clock: fake.NewClock(time.Now()), IDs: fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web"); !errors.Is(err, cp.ErrNotImplemented) {
		t.Errorf("attach without provisioner err = %v, want ErrNotImplemented", err)
	}
}

// TestDetachPostgres asserts detach removes the DATABASE_URL key, drops the database, and rolls the
// workload, behind the confirm guardrail.
func TestDetachPostgres(t *testing.T) {
	ctx := context.Background()
	e, k, _, prov := newPostgresEngine(t)
	if err := k.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Kind: cp.WorkloadDeployment, Image: "busybox", Replicas: 1}); err != nil {
		t.Fatalf("seed workload: %v", err)
	}
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web"); err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}

	// Without confirm the detach guardrail holds it.
	if err := e.DetachAddon(ctx, cp.AddonPostgres, "web", false); err == nil {
		t.Fatal("detach without confirm should be held by the guardrail")
	} else {
		mustGuardrail(t, err, cp.GuardrailAddonDetach)
	}
	if got := prov.Dropped(); len(got) != 0 {
		t.Errorf("a held detach must not drop anything, got %v", got)
	}

	// With confirm it drops the database and removes the key.
	if err := e.DetachAddon(ctx, cp.AddonPostgres, "web", true); err != nil {
		t.Fatalf("DetachAddon confirmed: %v", err)
	}
	if got := prov.Dropped(); len(got) != 1 || got[0] != "web" {
		t.Errorf("DropAppDatabase called with %v, want [web]", got)
	}
	if _, ok := k.SecretValue("web", "DATABASE_URL"); ok {
		t.Error("DATABASE_URL should be removed after detach")
	}
}

// TestAttachAuditRedactsURL drives attach and detach and asserts every audit row carries only the
// {addon, app} args — never the connection-string value, the key name as a value, or anything that
// looks like a secret. The audit log is the redaction boundary (ADR-0027/0031).
func TestAttachAuditRedactsURL(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web"); err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	if err := e.DetachAddon(ctx, cp.AddonPostgres, "web", true); err != nil {
		t.Fatalf("DetachAddon: %v", err)
	}

	rows, err := d.Audit(ctx, cp.AuditFilter{})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("expected audit rows for attach/detach")
	}
	url := fake.URLFor("web")
	sawAttach := false
	for _, row := range rows {
		if row.Operation == "addon_attach" {
			sawAttach = true
		}
		// Args carry only the allowlist: addon + app names, nothing resembling a URL or password.
		for key, v := range row.Args {
			if key != "addon" && key != "app" {
				t.Errorf("audit row %s has unexpected arg key %q (only addon/app allowed)", row.Operation, key)
			}
			if strings.Contains(v, "postgres://") || strings.Contains(v, "@burrow-postgres") || v == url {
				t.Errorf("audit arg %q leaks a connection string: %q", key, v)
			}
		}
		// The whole row, serialized, must not contain the URL or a password fragment.
		if strings.Contains(row.Result, url) || strings.Contains(row.Result, "fakepw") {
			t.Errorf("audit row Result leaks the URL/password: %q", row.Result)
		}
	}
	if !sawAttach {
		t.Error("no addon_attach audit row recorded")
	}
}

// TestAttachDoesNotLogTheURL captures slog output during attach and asserts the generated
// connection string (and its password fragment) never reaches a log line (ADR-0031).
func TestAttachDoesNotLogTheURL(t *testing.T) {
	ctx := context.Background()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e, _, _, _ := newPostgresEngine(t)
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web"); err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	if err := e.DetachAddon(ctx, cp.AddonPostgres, "web", true); err != nil {
		t.Fatalf("DetachAddon: %v", err)
	}

	logged := buf.String()
	if strings.Contains(logged, "postgres://") || strings.Contains(logged, "fakepw") || strings.Contains(logged, fake.URLFor("web")) {
		t.Errorf("attach/detach logged the connection string:\n%s", logged)
	}
}
