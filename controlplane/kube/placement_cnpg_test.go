// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// [ADR-0077](../../docs/adr/0077-placement-policy-for-pods-burrow-does-not-author.md)'s Consequences
// name the risk this file exists for:
//
//	The translation is a maintenance surface. CNPG's placement fields can change between versions,
//	and a mapping that silently stops covering a field reintroduces §3's failure. It needs a test
//	that fails when the target schema moves, not only when Burrow's code does.
//
// # Why that is hard, and what "the target schema" is here
//
// Burrow does not import CloudNativePG's Go types, so there is no compiler to notice a renamed
// field. The reason is stated on recordedPlacementSchema: the CRD's OpenAPI schema — not the Go
// types — is what the API server validates and PRUNES against, so it is the contract that decides
// whether policy survives a write, and importing the API module would drag barman-cloud, the
// prometheus-operator APIs, ginkgo and gomega into a dependency graph this project keeps small
// deliberately.
//
// So the target schema is materialised instead: cnpg_placement_schema.json is the placement subtree
// of the pinned release's CRD, recorded from the release artifact. Two checks then hold it in place,
// and they fail for different reasons:
//
//   - TestCNPGCarriesEveryFieldOfThePlacementVocabulary is the one that matters. It fills EVERY
//     field of PodPlacement, translates the result, and walks it against the recorded schema with
//     the production validator. It fails when CNPG drops, renames or reshapes a placement field —
//     and equally when k8s.io/api gains a placement field CNPG's schema has not picked up, which is
//     the same silent drop arriving from the other side.
//   - TestCNPGPlacementSchemaIsTheRecordedRelease re-derives the recording from the pinned release
//     and fails if it does not match. It guards the recording's provenance rather than drift, needs
//     the network, and so runs on request rather than by default.
//
// # The honest limit
//
// A schema cannot move under a pinned release, so drift arrives when the PIN moves — which is why
// CNPGVersion and the recording are checked against each other by
// TestRecordedCNPGSchemaMatchesThePinnedVersion, and why bumping CNPG without re-recording fails.
// What is NOT covered: an install running a CNPG older than the pin has a schema Burrow never saw,
// and can prune a field this recording says is carried. Closing that means reading the CRD from the
// live cluster at start-up, which needs a cluster and belongs with the code that installs the
// operator.

// TestCNPGCarriesEveryFieldOfThePlacementVocabulary is the drift guard. It sets every field the
// vocabulary has, down to every leaf of every Kubernetes type it is built from, and asserts the CNPG
// `Cluster` has a destination for all of it.
//
// It uses the production validator rather than a test-local comparison, so what it proves is exactly
// what a wiring author would be told: if this fails, WithControllerPodPlacement refuses that policy,
// and the failure names the JSON paths that lost their destination.
func TestCNPGCarriesEveryFieldOfThePlacementVocabulary(t *testing.T) {
	var p PodPlacement
	fillEveryField(t, reflect.ValueOf(&p).Elem(), 0)

	fragment, err := cnpgPlacement(p)
	if err != nil {
		t.Fatalf("translating a fully-populated placement: %v", err)
	}
	schema, err := cnpgPlacementSchema()
	if err != nil {
		t.Fatalf("loading the recorded schema: %v", err)
	}
	if gaps := placementGaps("spec", fragment, schema.Spec); len(gaps) > 0 {
		t.Errorf("the CloudNativePG %s Cluster schema no longer carries every field of the placement "+
			"vocabulary:\n%s\n\n"+
			"One of two things moved:\n"+
			"  - CNPG dropped, renamed or reshaped a placement field. A field with no destination is "+
			"PRUNED by the API server on write, silently, so the policy an operator wired would stop "+
			"being in force with nothing to say so (ADR-0077 §3).\n"+
			"  - k8s.io/api gained a placement field CNPG's CRD has not picked up. Same silent drop, "+
			"arriving from the other side.\n\n"+
			"Either way the mapping in cnpgPlacement is now a claim that is no longer true. Decide "+
			"which: move the pin and re-record (BURROW_CNPG_SCHEMA=record go test ./controlplane/kube "+
			"-run CNPGPlacementSchema), or remove the field from PodPlacement so it cannot be "+
			"expressed at all.",
			schema.Version, strings.Join(gaps, "\n"))
	}
}

// TestRecordedCNPGSchemaMatchesThePinnedVersion ties the recording to the release constant, so a
// CNPG upgrade cannot be a one-line constant change. It runs offline, on every build, because it is
// the check that makes the drift guard above mean something: without it, the pin and the schema
// could describe different releases and both tests would still pass.
func TestRecordedCNPGSchemaMatchesThePinnedVersion(t *testing.T) {
	schema, err := cnpgPlacementSchema()
	if err != nil {
		t.Fatalf("loading the recorded schema: %v", err)
	}
	if schema.Version != CNPGVersion {
		t.Errorf("cnpg_placement_schema.json records CloudNativePG %s, but CNPGVersion pins %s.\n"+
			"The placement translation is a claim about a specific release's schema, and a controller "+
			"upgrade is exactly when a placement field can be renamed or removed. Re-record against "+
			"the new pin:\n\n"+
			"  BURROW_CNPG_SCHEMA=record go test ./controlplane/kube -run CNPGPlacementSchema\n\n"+
			"then check TestCNPGCarriesEveryFieldOfThePlacementVocabulary still passes.",
			schema.Version, CNPGVersion)
	}
	if !strings.Contains(schema.Source, CNPGVersion) {
		t.Errorf("the recorded source %q does not name the pinned release %s, so the recording cannot "+
			"be re-derived from it", schema.Source, CNPGVersion)
	}
}

// cnpgSchemaEnv selects what TestCNPGPlacementSchemaIsTheRecordedRelease does: "verify" re-derives
// the recording from the pinned release and compares, "record" rewrites it. Both need the network,
// so neither is the default — the offline checks above are what run in CI.
const cnpgSchemaEnv = "BURROW_CNPG_SCHEMA"

// TestCNPGPlacementSchemaIsTheRecordedRelease re-derives cnpg_placement_schema.json from the pinned
// release's install manifest. In "record" mode it writes the file, which is how a CNPG upgrade is
// carried out; in "verify" mode it fails if the checked-in recording differs from what the release
// actually publishes.
//
// This is provenance rather than drift: it answers "is the recorded schema really CNPG 1.30.0's",
// which nothing else can, since the recording is otherwise self-asserting.
func TestCNPGPlacementSchemaIsTheRecordedRelease(t *testing.T) {
	mode := os.Getenv(cnpgSchemaEnv)
	switch mode {
	case "record", "verify":
	default:
		t.Skipf("set %s=verify to re-derive the recording from the pinned CNPG release (needs the "+
			"network), or %s=record to rewrite it after moving CNPGVersion", cnpgSchemaEnv, cnpgSchemaEnv)
	}

	source := cnpgManifestURL(CNPGVersion)
	manifest, err := fetch(source)
	if err != nil {
		t.Fatalf("fetching %s: %v", source, err)
	}
	spec, apiVersion, err := extractCNPGPlacementSchema(manifest)
	if err != nil {
		t.Fatalf("extracting the placement schema from %s: %v", source, err)
	}
	fresh := recordedPlacementSchema{
		Controller: "CloudNativePG",
		Version:    CNPGVersion,
		Source:     source,
		CRD:        cnpgClusterCRD,
		APIVersion: apiVersion,
		Spec:       spec,
	}
	encoded, err := json.MarshalIndent(fresh, "", "  ")
	if err != nil {
		t.Fatalf("encoding the schema: %v", err)
	}
	encoded = append(encoded, '\n')

	if mode == "record" {
		if err := os.WriteFile(cnpgPlacementSchemaFile, encoded, 0o644); err != nil {
			t.Fatalf("writing %s: %v", cnpgPlacementSchemaFile, err)
		}
		t.Logf("recorded CloudNativePG %s placement schema (%d bytes) from %s", CNPGVersion, len(encoded), source)
		return
	}
	if string(encoded) != string(cnpgPlacementSchemaJSON) {
		t.Errorf("%s does not match what CloudNativePG %s publishes at %s.\n"+
			"The recording is what the wiring-time refusal validates against, so a recording that is "+
			"not the release's own schema refuses the wrong policy and accepts policy the API server "+
			"would prune. Re-record it:\n\n"+
			"  %s=record go test ./controlplane/kube -run CNPGPlacementSchema",
			cnpgPlacementSchemaFile, CNPGVersion, source, cnpgSchemaEnv)
	}
}

const (
	// cnpgPlacementSchemaFile is the embedded recording, relative to this package.
	cnpgPlacementSchemaFile = "cnpg_placement_schema.json"
	// cnpgClusterCRD is the resource the placement fields live on.
	cnpgClusterCRD = "clusters.postgresql.cnpg.io"
)

// cnpgManifestURL is the release artifact a CNPG version publishes its CRDs in — the same artifact
// an install applies, so the schema recorded from it is the schema the cluster will hold.
func cnpgManifestURL(version string) string {
	return fmt.Sprintf("https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v%s/cnpg-%s.yaml", version, version)
}

func fetch(url string) ([]byte, error) {
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// extractCNPGPlacementSchema finds the Cluster CRD in a release manifest and returns the pruned
// schema of the placement fields on its storage version, plus that version's name.
//
// It takes the STORAGE version rather than the first served one: the storage version is what the
// API server persists and prunes against, which is the behaviour ADR-0077 §3 is about.
func extractCNPGPlacementSchema(manifest []byte) (*schemaNode, string, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(string(manifest))))
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("reading the manifest: %w", err)
		}
		var crd struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Group    string `json:"group"`
				Versions []struct {
					Name    string `json:"name"`
					Storage bool   `json:"storage"`
					Schema  struct {
						OpenAPIV3Schema *schemaNode `json:"openAPIV3Schema"`
					} `json:"schema"`
				} `json:"versions"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(doc, &crd); err != nil {
			continue // not every document in the manifest is a CRD
		}
		if crd.Kind != "CustomResourceDefinition" || crd.Metadata.Name != cnpgClusterCRD {
			continue
		}
		for _, v := range crd.Spec.Versions {
			if !v.Storage || v.Schema.OpenAPIV3Schema == nil {
				continue
			}
			resourceSpec := v.Schema.OpenAPIV3Schema.Properties["spec"]
			if resourceSpec == nil {
				return nil, "", fmt.Errorf("the %s schema has no spec", cnpgClusterCRD)
			}
			placement := &schemaNode{Type: "object", Properties: map[string]*schemaNode{}}
			for _, field := range []string{"affinity", "topologySpreadConstraints"} {
				node := resourceSpec.Properties[field]
				if node == nil {
					return nil, "", fmt.Errorf("the %s spec has no %s field; CNPG's placement surface "+
						"has moved and the mapping in cnpgPlacement needs rewriting, not re-recording",
						cnpgClusterCRD, field)
				}
				placement.Properties[field] = node
			}
			return placement, crd.Spec.Group + "/" + v.Name, nil
		}
		return nil, "", fmt.Errorf("the %s CRD has no storage version", cnpgClusterCRD)
	}
	return nil, "", fmt.Errorf("the manifest holds no %s CRD", cnpgClusterCRD)
}

// fillEveryField recursively sets every field of v to a non-zero value, so a translation is checked
// against the WHOLE vocabulary rather than against whichever fields a test author happened to think
// of. That is the difference between a guard that notices CNPG dropping `matchLabelKeys` and one
// that notices only what it was told to look for.
//
// The values are meaningless on purpose — this is a shape check, not a validity check. The types
// reachable from PodPlacement are finite and acyclic; the depth cap is a guard against a future
// Kubernetes type that is not, which would otherwise hang the suite rather than fail it.
func fillEveryField(t *testing.T, v reflect.Value, depth int) {
	t.Helper()
	if depth > 12 {
		t.Fatalf("filling %s exceeded the depth cap; a placement type has become recursive and this "+
			"guard needs a cycle check rather than a cap", v.Type())
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillEveryField(t, v.Elem(), depth+1)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if !v.Field(i).CanSet() {
				continue // unexported: not part of the JSON the API server sees
			}
			fillEveryField(t, v.Field(i), depth+1)
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillEveryField(t, s.Index(0), depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		fillEveryField(t, key, depth+1)
		val := reflect.New(v.Type().Elem()).Elem()
		fillEveryField(t, val, depth+1)
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	default:
		t.Fatalf("a placement type reached %s, which this guard cannot fill; extend it rather than "+
			"letting the field go unchecked", v.Kind())
	}
}
