// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
)

// defaultBurrowdImage is the published control-plane image `burrow install` deploys by
// default. Pinned to an immutable release tag so the cluster always pulls a known build;
// override with --burrowd-image to run a different build (the e2e builds one locally and
// imports it into k3d). Bump this in lockstep with each burrowd release tag.
const defaultBurrowdImage = "ghcr.io/burrow-cloud/burrowd:v0.2.0"

// installManifests is the control-plane install manifest template, embedded from
// manifests/install.yaml.tmpl (like the migrations are embedded in controlplane/postgres).
//
//go:embed manifests/install.yaml.tmpl
var installManifests string

var installTemplate = template.Must(template.New("install").Parse(installManifests))

// installOptions are the values rendered into the install manifests. Namespace holds the
// control plane (burrowd, Postgres); AppNamespace is where deployed apps go — separate, so
// app workloads aren't mixed in with the control-plane infrastructure.
type installOptions struct {
	Namespace    string
	AppNamespace string
	Image        string
	Token        string
	DBPassword   string
	Port         int
}

func newInstallCmd() *cobra.Command {
	var namespace, appNamespace, image, kubeconfig string
	var dryRun, wait, verbose bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the Burrow control plane into your cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInstall(cmd.Context(), namespace, appNamespace, image, kubeconfig, dryRun, wait, verbose, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&namespace, "namespace", connect.DefaultNamespace, "namespace to install the control plane into")
	cmd.Flags().StringVar(&appNamespace, "app-namespace", "default", "namespace to deploy applications into")
	cmd.Flags().StringVar(&image, "burrowd-image", defaultBurrowdImage, "burrowd container image to deploy (must be pullable by the cluster)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the manifests instead of applying them")
	cmd.Flags().BoolVar(&wait, "wait", true, "wait for the control plane to become ready")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "show every resource kubectl applies instead of a summary")
	return cmd
}

func runInstall(ctx context.Context, namespace, appNamespace, image, kubeconfig string, dryRun, wait, verbose bool, stdout, stderr io.Writer) error {
	token, err := randHex(16)
	if err != nil {
		return err
	}
	dbPassword, err := randHex(12)
	if err != nil {
		return err
	}

	manifests, err := renderManifests(installOptions{
		Namespace:    namespace,
		AppNamespace: appNamespace,
		Image:        image,
		Token:        token,
		DBPassword:   dbPassword,
		Port:         connect.DefaultPort,
	})
	if err != nil {
		return err
	}

	if dryRun {
		fmt.Fprint(stdout, manifests)
		return nil
	}

	cs, err := clientset(kubeconfig)
	if err != nil {
		return err
	}
	if installed, err := alreadyInstalled(ctx, cs, namespace); err != nil {
		return err
	} else if installed {
		return fmt.Errorf("Burrow is already installed in namespace %q; run `burrow upgrade` to update it "+
			"(re-running install would mint new secrets and break the existing control plane)", namespace)
	}

	if err := kubectlApply(ctx, kubeconfig, manifests, verbose, stdout, stderr); err != nil {
		return err
	}

	if wait {
		if err := waitForReady(ctx, kubeconfig, namespace, stdout); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nBurrow is installed and ready in namespace %q.\n"+
			"Deploy an app:\n"+
			"  burrow deploy <app> --image <ref>\n", namespace)
		return nil
	}
	fmt.Fprintf(stdout, "\nBurrow installed into namespace %q (not waiting for readiness).\n", namespace)
	return nil
}

// waitForReady blocks until the in-cluster Postgres and burrowd are ready, printing
// progress. burrowd only becomes ready after it has reached Postgres and applied its
// migrations, so this confirms the whole control plane is up.
func waitForReady(ctx context.Context, kubeconfig, namespace string, out io.Writer) error {
	cs, err := clientset(kubeconfig)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "\nWaiting for Burrow to become ready...")
	if err := waitForDeployment(ctx, cs, namespace, "postgres", "database", out, 3*time.Minute); err != nil {
		return err
	}
	return waitForDeployment(ctx, cs, namespace, "burrowd", "control plane", out, 3*time.Minute)
}

func waitForDeployment(ctx context.Context, cs kubernetes.Interface, namespace, name, label string, out io.Writer, timeout time.Duration) error {
	fmt.Fprintf(out, "  %s ...", label)
	deadline := time.Now().Add(timeout)
	for {
		d, err := cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			desired := int32(1)
			if d.Spec.Replicas != nil {
				desired = *d.Spec.Replicas
			}
			if desired > 0 && d.Status.ObservedGeneration >= d.Generation && d.Status.ReadyReplicas >= desired {
				fmt.Fprintln(out, " ready")
				return nil
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(out, " timed out")
			return fmt.Errorf("%s did not become ready within %s", label, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// kubectlApply pipes the manifests to `kubectl apply -f -`. By default it prints a one-line
// summary of how many resources changed; with verbose it streams kubectl's per-resource output.
func kubectlApply(ctx context.Context, kubeconfig, manifests string, verbose bool, stdout, stderr io.Writer) error {
	return applyAndSummarize(ctx, applyArgs(kubeconfig, "-"), manifests, verbose, stdout, stderr)
}

func applyArgs(kubeconfig, source string) []string {
	args := []string{"apply", "-f", source}
	if kubeconfig != "" {
		args = append([]string{"--kubeconfig", kubeconfig}, args...)
	}
	return args
}

// applyAndSummarize runs `kubectl apply` (optionally feeding manifests on stdin) and reports
// the result the way a non-Kubernetes user wants by default: a single line of what changed,
// rather than a wall of per-resource output. With verbose, or on failure (so the error is
// debuggable), it shows kubectl's full output. kubectl's stderr always passes through.
func applyAndSummarize(ctx context.Context, args []string, stdin string, verbose bool, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = stderr
	err := cmd.Run()
	if err != nil {
		fmt.Fprint(stdout, out.String())
		return fmt.Errorf("kubectl apply: %w", err)
	}
	if verbose {
		fmt.Fprint(stdout, out.String())
		return nil
	}
	summarizeApply(out.String(), stdout)
	return nil
}

// summarizeApply prints a one-line summary of kubectl apply output, counting resources by the
// trailing action word kubectl prints (e.g. "created", "configured", "unchanged").
func summarizeApply(out string, w io.Writer) {
	counts := map[string]int{}
	total := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		counts[strings.ToLower(fields[len(fields)-1])]++
		total++
	}
	if total == 0 {
		return
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
	fmt.Fprintf(w, "Applied %d resource(s): %s.\n", total, strings.Join(parts, ", "))
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func renderManifests(o installOptions) (string, error) {
	var sb strings.Builder
	if err := installTemplate.Execute(&sb, o); err != nil {
		return "", fmt.Errorf("rendering manifests: %w", err)
	}
	return sb.String(), nil
}
