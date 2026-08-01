// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The `Cluster` Burrow writes is a hand-built map, because this project does not import
// CloudNativePG's Go types (the reason is on cnpgClusterGVR). Nothing in the compiler therefore
// knows whether `managed.services.additional` is a field CNPG has.
//
// AND A FIELD IT DOES NOT HAVE IS NOT AN ERROR. A CRD's structural schema PRUNES what it does not
// describe, silently and with a 201 — the same property ADR-0077 §3 built the placement refusal
// around, arriving here on the rest of the spec. The consequences are not uniform:
//
//   - A pruned `managed.services.additional` costs the Service the instance is NAMED by. Every
//     DATABASE_URL, the provisioner's admin connection and both backup Jobs resolve
//     `<instance>.<addon-ns>.svc`, and nothing would answer.
//   - A pruned `managed.roles` costs `burrow_admin`. The database comes up; nothing Burrow does to
//     it works.
//   - A pruned `inheritedMetadata` costs the metrics add-on its scrape target, quietly.
//
// So the same recording-and-walk the placement seam uses is applied to the whole composed spec:
// cnpg_cluster_schema.json holds the subtree of the pinned release's CRD covering exactly the paths
// postgresClusterSpec writes, and the production validator (placementGaps) walks the composed value
// against it.
//
// The honest limit is the placement recording's, for the same reason: a schema cannot move under a
// pinned release, so this fails when postgresClusterSpec grows a path the recording does not have,
// and the recording is re-derived when CNPGVersion moves. What re-recording catches is the other
// direction — a field CNPG dropped between releases, which is exactly when this is worth knowing.

// cnpgClusterSchemaJSON is the recorded schema of the `Cluster` spec paths Burrow writes.
//
// Re-record it with: BURROW_CNPG_SCHEMA=record go test ./controlplane/kube -run CNPGClusterSchema
//
//go:embed cnpg_cluster_schema.json
var cnpgClusterSchemaJSON []byte

// cnpgClusterSchemaFile is that recording, relative to this package.
const cnpgClusterSchemaFile = "cnpg_cluster_schema.json"

// TestCNPGClusterSpecHasADestinationForEveryFieldBurrowWrites walks the composed `Cluster` spec
// against the recorded schema and fails on any path the CRD has no field for. It runs offline, on
// every build.
func TestCNPGClusterSpecHasADestinationForEveryFieldBurrowWrites(t *testing.T) {
	schema, err := recordedCNPGClusterSchema()
	if err != nil {
		t.Fatalf("loading the recorded Cluster schema: %v", err)
	}
	if schema.Version != CNPGVersion {
		t.Fatalf("%s records CloudNativePG %s but CNPGVersion pins %s; re-record with %s=record",
			cnpgClusterSchemaFile, schema.Version, CNPGVersion, cnpgSchemaEnv)
	}

	value, err := composedClusterSpecAsJSON()
	if err != nil {
		t.Fatalf("composing the Cluster spec: %v", err)
	}
	if gaps := placementGaps("spec", value, schema.Spec); len(gaps) > 0 {
		t.Errorf("the CloudNativePG %s Cluster schema has no destination for part of the spec Burrow "+
			"writes:\n%s\n\n"+
			"A field the CRD does not describe is PRUNED on write — silently, with no API error — so "+
			"the object comes back without it and the add-on is subtly wrong rather than broken. Decide "+
			"which happened: postgresClusterSpec gained a path (re-record with %s=record go test "+
			"./controlplane/kube -run CNPGClusterSchema), or CNPG moved the field and the composition "+
			"needs rewriting rather than re-recording.",
			schema.Version, strings.Join(gaps, "\n"), cnpgSchemaEnv)
	}
}

// TestCNPGClusterSchemaIsTheRecordedRelease re-derives the recording from the pinned release. It
// needs the network, so it runs on request rather than by default — the same split the placement
// recording uses, and for the same reason: CI must stay offline, and provenance is a question only
// the release artifact can answer.
func TestCNPGClusterSchemaIsTheRecordedRelease(t *testing.T) {
	mode := os.Getenv(cnpgSchemaEnv)
	switch mode {
	case "record", "verify":
	default:
		t.Skipf("set %s=verify to re-derive the recording from the pinned CNPG release (needs the "+
			"network), or %s=record to rewrite it", cnpgSchemaEnv, cnpgSchemaEnv)
	}

	source := CNPGManifestURL(CNPGVersion)
	manifest, err := fetch(source)
	if err != nil {
		t.Fatalf("fetching %s: %v", source, err)
	}
	full, apiVersion, err := cnpgClusterSpecSchema(manifest)
	if err != nil {
		t.Fatalf("extracting the Cluster spec schema from %s: %v", source, err)
	}
	value, err := composedClusterSpecAsJSON()
	if err != nil {
		t.Fatalf("composing the Cluster spec: %v", err)
	}
	pruned, err := pruneSchemaToValue("spec", value, full)
	if err != nil {
		t.Fatalf("CloudNativePG %s does not carry the whole spec Burrow writes, so there is nothing "+
			"honest to record: %v\n\nThis is a composition to rewrite, not a recording to refresh.", CNPGVersion, err)
	}

	fresh := recordedPlacementSchema{
		Controller: "CloudNativePG",
		Version:    CNPGVersion,
		Source:     source,
		CRD:        cnpgClusterCRD,
		APIVersion: apiVersion,
		Spec:       pruned,
	}
	encoded, err := json.MarshalIndent(fresh, "", "  ")
	if err != nil {
		t.Fatalf("encoding the schema: %v", err)
	}
	encoded = append(encoded, '\n')

	if mode == "record" {
		if err := os.WriteFile(cnpgClusterSchemaFile, encoded, 0o644); err != nil {
			t.Fatalf("writing %s: %v", cnpgClusterSchemaFile, err)
		}
		t.Logf("recorded CloudNativePG %s Cluster schema (%d bytes) from %s", CNPGVersion, len(encoded), source)
		return
	}
	if string(encoded) != string(cnpgClusterSchemaJSON) {
		t.Errorf("%s does not match what CloudNativePG %s publishes at %s. Re-record it:\n\n"+
			"  %s=record go test ./controlplane/kube -run CNPGClusterSchema",
			cnpgClusterSchemaFile, CNPGVersion, source, cnpgSchemaEnv)
	}
}

// recordedCNPGClusterSchema parses the embedded recording.
func recordedCNPGClusterSchema() (*recordedPlacementSchema, error) {
	var s recordedPlacementSchema
	if err := json.Unmarshal(cnpgClusterSchemaJSON, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", cnpgClusterSchemaFile, err)
	}
	if s.Spec == nil {
		return nil, fmt.Errorf("%s records no spec", cnpgClusterSchemaFile)
	}
	return &s, nil
}

// composedClusterSpecAsJSON builds the union of every `Cluster` spec path Burrow writes and
// round-trips it through JSON, which is what writing it to the API server does. The round trip
// matters: the composition holds Go integers, and only after it are numbers the shape placementGaps
// checks.
//
// IT IS A UNION RATHER THAN ONE SPEC, and the union is what the recording has to cover. Burrow
// composes two `Cluster` specs — an install's (bootstrap.initdb) and a physical restore's
// (bootstrap.recovery plus externalClusters, ADR-0066 §4) — and they differ in exactly the subtree
// most likely to be pruned silently. A recording taken from the install spec alone would say nothing
// about the recovery fields, and a pruned `bootstrap.recovery` does not fail: it initializes an EMPTY
// database where a recovered one was asked for, which is the worst failure in this file's whole
// subject. So both bootstraps appear together here. No `Cluster` is ever written in this shape;
// nothing writes this value, it only walks the paths.
//
// The placement policy is FILLED, not left empty, so the merged fragment is exercised here too — an
// operator who wires placement is the reader most exposed to a silently pruned field.
func composedClusterSpecAsJSON() (any, error) {
	a := &Adapter{addonNamespace: defaultAddonNamespace, controllerPlacement: PodPlacement{
		NodeSelector:              map[string]string{"kubernetes.io/os": "linux"},
		Tolerations:               []corev1.Toleration{{Key: "burrow", Operator: corev1.TolerationOpExists}},
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{MaxSkew: 1, TopologyKey: "kubernetes.io/hostname"}},
	}}
	spec, ok := controlplane.LookupAddon(controlplane.AddonPostgres)
	if !ok {
		return nil, fmt.Errorf("the catalog has no postgres add-on")
	}
	name, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{nameLabel: name, managedByLabel: managedByValue}
	// The archive destination is FILLED, so the `plugins` entry that wires pgBackRest is exercised
	// here too (ADR-0066 §3). It is the field with the most to lose from silent pruning: a pruned
	// `plugins` costs the instance its write-ahead-log archiving, the `Cluster` comes up healthy, and
	// the first symptom is a base backup that has nowhere to go.
	archive := &controlplane.ArchiveDestination{
		Provider: "backups",
		Config: controlplane.ObjectStoreConfig{
			Endpoint: "https://s3.example.invalid",
			Region:   "us-east-1",
			Bucket:   "burrow-backups-abc123",
		},
		RepoPath: controlplane.PgBackRestRepoPath(controlplane.DefaultEnvironment),
	}
	install := a.postgresClusterSpec(spec, controlplane.DefaultEnvironment, name, labels, archive)
	recovery := a.postgresRecoveryClusterSpec(spec, controlplane.DefaultEnvironment, name, labels, controlplane.RestoreInstanceRequest{
		Environment: controlplane.DefaultEnvironment,
		// A recovery target is FILLED so `bootstrap.recovery.recoveryTarget.backupID` is walked. It is
		// the field a restore names the base backup with, and a pruned one recovers the latest backup
		// instead of the one the operator asked for — silently, and only discoverable by reading the
		// data afterwards.
		BackupLabel: "20260210-101333F",
		Archive:     archive,
	})
	// The union: the recovery composition already carries everything the install one does apart from
	// the bootstrap, which is the one key they genuinely disagree about.
	bootstrap := map[string]any{}
	for k, v := range install["bootstrap"].(map[string]any) {
		bootstrap[k] = v
	}
	for k, v := range recovery["bootstrap"].(map[string]any) {
		bootstrap[k] = v
	}
	recovery["bootstrap"] = bootstrap
	// And `targetTime`, which is the other half of `recoveryTarget` and takes the same path a
	// point-in-time restore does.
	recovery["bootstrap"].(map[string]any)["recovery"].(map[string]any)["recoveryTarget"].(map[string]any)["targetTime"] = "2026-08-01T14:30:00Z"
	return asJSON(recovery)
}

// pruneSchemaToValue returns the subtree of node covering exactly the paths value occupies, and an
// error naming the first path that has no destination.
//
// Pruning to the VALUE rather than recording the whole `Cluster` spec is what keeps the recording
// reviewable: CNPG's spec carries the entire PodSpec, ServiceSpec and backup surface, and a recording
// nobody reads is a recording nobody notices going wrong. What is kept is what Burrow writes.
func pruneSchemaToValue(path string, value any, node *schemaNode) (*schemaNode, error) {
	if node == nil {
		return nil, fmt.Errorf("%s has no field in the CRD, so the API server would prune it on write", path)
	}
	switch v := value.(type) {
	case map[string]any:
		out := &schemaNode{Type: node.Type}
		if node.Properties == nil && node.AdditionalProperties == nil {
			// A free-form object (no declared shape) prunes nothing, so there is nothing to record.
			return out, nil
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child, ok := node.Properties[k]
			if !ok {
				if node.AdditionalProperties != nil {
					// Every key is carried; record the value schema once.
					out.AdditionalProperties = node.AdditionalProperties
					continue
				}
				return nil, fmt.Errorf("%s.%s has no field in the CRD, so the API server would prune it on write", path, k)
			}
			pruned, err := pruneSchemaToValue(path+"."+k, v[k], child)
			if err != nil {
				return nil, err
			}
			if out.Properties == nil {
				out.Properties = map[string]*schemaNode{}
			}
			out.Properties[k] = pruned
		}
		return out, nil
	case []any:
		out := &schemaNode{Type: node.Type}
		for i, item := range v {
			pruned, err := pruneSchemaToValue(fmt.Sprintf("%s[%d]", path, i), item, node.Items)
			if err != nil {
				return nil, err
			}
			out.Items = mergeSchemaNodes(out.Items, pruned)
		}
		return out, nil
	default:
		return &schemaNode{Type: node.Type}, nil
	}
}

// mergeSchemaNodes unions two pruned nodes, so an array whose elements occupy different fields
// records the union rather than only the last element's.
func mergeSchemaNodes(a, b *schemaNode) *schemaNode {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := &schemaNode{Type: a.Type, Items: mergeSchemaNodes(a.Items, b.Items), AdditionalProperties: a.AdditionalProperties}
	if out.AdditionalProperties == nil {
		out.AdditionalProperties = b.AdditionalProperties
	}
	for _, src := range []map[string]*schemaNode{a.Properties, b.Properties} {
		for k, v := range src {
			if out.Properties == nil {
				out.Properties = map[string]*schemaNode{}
			}
			out.Properties[k] = mergeSchemaNodes(out.Properties[k], v)
		}
	}
	return out
}
