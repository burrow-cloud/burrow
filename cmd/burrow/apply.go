// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/term"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/burrow-cloud/burrow/connect"
)

// fieldManager is the server-side-apply field-manager name Burrow owns its applied fields under
// (ADR-0037). A stable name lets repeated applies (install, then upgrade) update the same fields
// instead of conflicting with a different manager.
const fieldManager = "burrow"

// serverSideApply applies a multi-document YAML manifest to the cluster targeted by kubeconfig +
// kubeContext using client-go server-side apply (ADR-0037), so `burrow` is a self-contained binary
// that needs only a reachable kubeconfig and never the `kubectl` binary. Its signature matches the
// former kubectl shell-out so it drops into the existing applyFn seam unchanged. By default it
// prints a one-line summary of what changed; with verbose it lists every resource.
func serverSideApply(ctx context.Context, kubeconfig, kubeContext, manifests string, verbose bool, stdout, stderr io.Writer) error {
	cfg, err := connect.RESTConfig(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	ap, err := newApplier(cfg)
	if err != nil {
		return err
	}
	return ap.apply(ctx, manifests, false, verbose, stdout, stderr)
}

// serverSideApplyURL fetches a remote manifest over HTTP and feeds it through the same server-side
// applier, so applying an upstream manifest by URL (the ingress/cert-manager stacks) also needs no
// kubectl. It routes through applyFn so a test can fake the apply.
func serverSideApplyURL(ctx context.Context, kubeconfig, url string, verbose bool, stdout, stderr io.Writer) error {
	manifests, err := fetchManifest(ctx, url)
	if err != nil {
		return err
	}
	return applyFn(ctx, kubeconfig, "", manifests, verbose, stdout, stderr)
}

// fetchManifest GETs a manifest URL and returns its body. The standard client follows the
// redirects upstream release manifests use (e.g. a GitHub release download to its storage backend).
func fetchManifest(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("requesting %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching %s: unexpected status %s", url, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", url, err)
	}
	return string(b), nil
}

// applier server-side-applies parsed manifests. It bundles a dynamic client and a RESTMapper so the
// apply logic is independent of how they are built: production wires a real discovery-backed mapper
// (newApplier), and a test injects a dynamicfake client and a static mapper.
type applier struct {
	dyn    dynamic.Interface
	mapper meta.RESTMapper
}

// newApplier builds an applier from a REST config: a dynamic client for typed-free applies and a
// discovery-backed RESTMapper (cached, and deferred so the cache is filled lazily) to resolve each
// object's GroupVersionKind to its resource and scope.
func newApplier(cfg *rest.Config) (*applier, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building dynamic client: %w", err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))
	return &applier{dyn: dyn, mapper: mapper}, nil
}

// applyResult records one object's outcome for the summary: a human description and the action
// taken (created, configured, or unchanged), matching the words the former kubectl summary counted.
type applyResult struct {
	desc   string
	action string
}

// apply parses the manifest into objects and server-side-applies each. Namespaces and
// CustomResourceDefinitions are applied first so the resources that live in them resolve; anything
// whose kind the RESTMapper cannot resolve yet (e.g. a custom resource whose CRD was applied in the
// same manifest) is retried once after the mapper cache is reset. When dryRun is set, every apply
// uses DryRunAll so the server validates and merges without persisting.
func (ap *applier) apply(ctx context.Context, manifests string, dryRun, verbose bool, stdout, stderr io.Writer) error {
	objs, err := parseManifests(manifests)
	if err != nil {
		return err
	}
	if len(objs) == 0 {
		return nil
	}
	sortForApply(objs)

	// On a real terminal, animate a single rewriting progress line as each resource is applied so
	// the install does not sit silent during the apply lag. When stdout is not a terminal (CI, a
	// pipe, or the e2e capturing output) or verbose is listing every resource, emit no carriage
	// returns: only the final summary/listing prints, keeping captured output clean.
	total := len(objs)
	showProgress := !verbose && isTerminal(stdout)
	progressWidth := len(fmt.Sprintf("Applying resources... %d/%d", total, total))
	reportProgress := func(done int) {
		if showProgress {
			fmt.Fprintf(stdout, "\r%-*s", progressWidth, fmt.Sprintf("Applying resources... %d/%d", done, total))
		}
	}

	var results []applyResult
	var pending []*unstructured.Unstructured
	for _, obj := range objs {
		action, err := ap.applyOne(ctx, obj, dryRun)
		if err != nil {
			// Ordering retry: a kind the mapper does not know yet, or a resource whose namespace
			// or CRD is created earlier in this same manifest. Defer it to a second pass.
			if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				pending = append(pending, obj)
				continue
			}
			return err
		}
		results = append(results, applyResult{describeObject(obj), action})
		reportProgress(len(results))
	}

	if len(pending) > 0 {
		ap.resetMapper()
		for _, obj := range pending {
			action, err := ap.applyOne(ctx, obj, dryRun)
			if err != nil {
				return err
			}
			results = append(results, applyResult{describeObject(obj), action})
			reportProgress(len(results))
		}
	}

	// Close the progress line with a newline so the summary starts on its own line.
	if showProgress && len(results) > 0 {
		fmt.Fprintln(stdout)
	}
	writeApplyResults(results, verbose, stdout)
	return nil
}

// isTerminal reports whether w is a real terminal: an *os.File whose file descriptor is a tty. A
// bytes.Buffer or any other non-file writer (what tests and captured/piped output pass) is not a
// terminal, so the carriage-return progress animation is suppressed there and only plain lines print.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// applyOne resolves an object's resource and scope via the RESTMapper, then server-side-applies it
// with Force so Burrow's field manager takes ownership of the fields it sets. It reports whether the
// object was created, configured, or left unchanged so the summary can count outcomes the way the
// former kubectl output did.
func (ap *applier) applyOne(ctx context.Context, obj *unstructured.Unstructured, dryRun bool) (string, error) {
	gvk := obj.GroupVersionKind()
	mapping, err := ap.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		// Returned unwrapped so the caller's meta.IsNoMatchError check can classify it for retry.
		return "", err
	}

	var ri dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := obj.GetNamespace()
		if ns == "" {
			ns = metav1.NamespaceDefault
		}
		ri = ap.dyn.Resource(mapping.Resource).Namespace(ns)
	} else {
		ri = ap.dyn.Resource(mapping.Resource)
	}

	existing, getErr := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
	existed := getErr == nil

	data, err := json.Marshal(obj.Object)
	if err != nil {
		return "", fmt.Errorf("marshaling %s: %w", describeObject(obj), err)
	}
	force := true
	opts := metav1.PatchOptions{FieldManager: fieldManager, Force: &force}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	applied, err := ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, opts)
	if err != nil {
		return "", fmt.Errorf("applying %s: %w", describeObject(obj), err)
	}

	switch {
	case !existed:
		return "created", nil
	case dryRun:
		// A dry run does not persist, so resourceVersion cannot distinguish configured from
		// unchanged; report the conservative "configured" for an object that already exists.
		return "configured", nil
	case applied.GetResourceVersion() != existing.GetResourceVersion():
		return "configured", nil
	default:
		return "unchanged", nil
	}
}

// resetMapper drops the discovery cache so a second apply pass can resolve kinds (e.g. custom
// resources) whose definitions were applied in the first pass. It is a no-op for a static mapper
// (the test mapper), which has nothing to refresh.
func (ap *applier) resetMapper() {
	if d, ok := ap.mapper.(*restmapper.DeferredDiscoveryRESTMapper); ok {
		d.Reset()
	}
}

// yamlDocumentSeparator matches a YAML document boundary: a line that is exactly "---" (optionally
// with trailing whitespace), per the manifest-splitting convention.
var yamlDocumentSeparator = regexp.MustCompile(`(?m)^---\s*$`)

// parseManifests splits a multi-document YAML string on document separators and decodes each
// non-empty document into an unstructured object. Empty documents (blank or comment-only) are
// skipped. YAML is converted through sigs.k8s.io/yaml so nested maps use string keys, as
// unstructured requires.
func parseManifests(manifests string) ([]*unstructured.Unstructured, error) {
	var objs []*unstructured.Unstructured
	for _, doc := range yamlDocumentSeparator.Split(manifests, -1) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		m := map[string]interface{}{}
		if err := sigsyaml.Unmarshal([]byte(doc), &m); err != nil {
			return nil, fmt.Errorf("parsing manifest document: %w", err)
		}
		if len(m) == 0 {
			continue
		}
		obj := &unstructured.Unstructured{Object: m}
		// A document with no kind is not an apiserver object (e.g. a stray comment block that
		// parsed to a scalar map); skip it rather than fail the whole apply.
		if obj.GetKind() == "" {
			continue
		}
		objs = append(objs, obj)
	}
	return objs, nil
}

// sortForApply stably orders objects so the things others depend on are applied first: namespaces
// and CustomResourceDefinitions before everything else. Anything still out of order is caught by the
// retry pass.
func sortForApply(objs []*unstructured.Unstructured) {
	sort.SliceStable(objs, func(i, j int) bool {
		return applyPriority(objs[i]) < applyPriority(objs[j])
	})
}

// applyPriority ranks an object for apply ordering: namespaces and CRDs first (0), everything else
// after (1).
func applyPriority(obj *unstructured.Unstructured) int {
	switch obj.GetKind() {
	case "Namespace", "CustomResourceDefinition":
		return 0
	default:
		return 1
	}
}

// describeObject renders a short "<kind>/<name>" label for summary and verbose output, matching the
// shape the former kubectl output used.
func describeObject(obj *unstructured.Unstructured) string {
	return strings.ToLower(obj.GetKind()) + "/" + obj.GetName()
}

// writeApplyResults reports the apply outcome: with verbose, one "<resource> <action>" line per
// object; otherwise a single summary line counting actions.
func writeApplyResults(results []applyResult, verbose bool, w io.Writer) {
	if verbose {
		for _, r := range results {
			fmt.Fprintf(w, "%s %s\n", r.desc, r.action)
		}
		return
	}
	summarizeApplyResults(results, w)
}

// summarizeApplyResults prints a one-line summary of an apply, counting resources by action. The
// common actions print in a stable order (created, configured, unchanged); any others follow,
// sorted, so the line is deterministic. No results prints nothing (a no-op apply).
func summarizeApplyResults(results []applyResult, w io.Writer) {
	if len(results) == 0 {
		return
	}
	counts := map[string]int{}
	for _, r := range results {
		counts[r.action]++
	}
	var parts []string
	for _, action := range []string{"created", "configured", "unchanged", "deleted", "pruned"} {
		if n := counts[action]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, action))
			delete(counts, action)
		}
	}
	others := make([]string, 0, len(counts))
	for action := range counts {
		others = append(others, action)
	}
	sort.Strings(others)
	for _, action := range others {
		parts = append(parts, fmt.Sprintf("%d %s", counts[action], action))
	}
	fmt.Fprintf(w, "Applied %d resource(s): %s.\n", len(results), strings.Join(parts, ", "))
}
