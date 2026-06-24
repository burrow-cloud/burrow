// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"text/template"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
)

// defaultBurrowdImage is where the control-plane image is expected to live. Until it is
// published, `burrow install` needs --burrowd-image pointing at an image the cluster can
// pull (the e2e builds one and imports it into k3d).
const defaultBurrowdImage = "ghcr.io/burrow-cloud/burrowd:v0.1.0"

// installManifests is the control-plane install manifest template, embedded from
// manifests/install.yaml.tmpl (like the migrations are embedded in controlplane/postgres).
//
//go:embed manifests/install.yaml.tmpl
var installManifests string

var installTemplate = template.Must(template.New("install").Parse(installManifests))

// installOptions are the values rendered into the install manifests.
type installOptions struct {
	Namespace  string
	Image      string
	Token      string
	DBPassword string
	Port       int
}

func cmdInstall(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	namespace := fs.String("namespace", connect.DefaultNamespace, "namespace to install Burrow into")
	image := fs.String("burrowd-image", defaultBurrowdImage, "burrowd container image to deploy (must be pullable by the cluster)")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: ambient)")
	dryRun := fs.Bool("dry-run", false, "print the manifests instead of applying them")
	wait := fs.Bool("wait", true, "wait for the control plane to become ready")
	_, flagArgs := splitArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	token, err := randHex(16)
	if err != nil {
		return err
	}
	dbPassword, err := randHex(12)
	if err != nil {
		return err
	}

	manifests, err := renderManifests(installOptions{
		Namespace:  *namespace,
		Image:      *image,
		Token:      token,
		DBPassword: dbPassword,
		Port:       connect.DefaultPort,
	})
	if err != nil {
		return err
	}

	if *dryRun {
		fmt.Fprint(stdout, manifests)
		return nil
	}

	if err := kubectlApply(ctx, *kubeconfig, manifests, stdout, stderr); err != nil {
		return err
	}

	if *wait {
		if err := waitForReady(ctx, *kubeconfig, *namespace, stdout); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nBurrow is installed and ready in namespace %q.\n"+
			"Deploy with your kubeconfig — no further config:\n"+
			"  burrow deploy <app> --image <ref>\n", *namespace)
		return nil
	}
	fmt.Fprintf(stdout, "\nBurrow installed into namespace %q (not waiting for readiness).\n", *namespace)
	return nil
}

// waitForReady blocks until the in-cluster Postgres and burrowd are ready, printing
// progress. burrowd only becomes ready after it has reached Postgres and applied its
// migrations, so this confirms the whole control plane is up.
func waitForReady(ctx context.Context, kubeconfig, namespace string, out io.Writer) error {
	cfg, err := connect.RESTConfig(kubeconfig)
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("building clientset: %w", err)
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

// kubectlApply pipes the manifests to `kubectl apply -f -`.
func kubectlApply(ctx context.Context, kubeconfig, manifests string, stdout, stderr io.Writer) error {
	args := []string{"apply", "-f", "-"}
	if kubeconfig != "" {
		args = append([]string{"--kubeconfig", kubeconfig}, args...)
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = strings.NewReader(manifests)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply: %w", err)
	}
	return nil
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
