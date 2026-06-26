// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
)

// newVersionCmd reports this CLI's version and, best effort, the version of the control plane
// installed in the cluster — read from the burrowd Deployment's image, so it works even if
// burrowd is unhealthy and needs no API token.
func newVersionCmd() *cobra.Command {
	var kubeconfig, namespace string
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version and the installed control-plane version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "burrow (CLI):  %s\n", cliVersion())

			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			cs, err := clientset(kubeconfig)
			if err != nil {
				fmt.Fprintf(out, "control plane: unknown (%v)\n", err)
				return nil
			}
			img, err := burrowdImage(ctx, cs, namespace)
			switch {
			case apierrors.IsNotFound(err):
				fmt.Fprintf(out, "control plane: not installed in namespace %q\n", namespace)
			case err != nil:
				fmt.Fprintf(out, "control plane: unknown (%v)\n", err)
			default:
				fmt.Fprintf(out, "control plane: %s (namespace %q)\n", imageTag(img), namespace)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	cmd.Flags().StringVar(&namespace, "namespace", connect.DefaultNamespace, "namespace the control plane is installed in")
	return cmd
}

// cliVersion returns this CLI's release version from the build info — set when it is installed
// with `go install …@version` — or "dev" for an unversioned local build.
func cliVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// imageTag returns just the version tag of an image reference (the part after the last colon,
// ignoring any registry-host port and stripping a digest), e.g.
// "ghcr.io/burrow-cloud/burrowd:v0.2.1" -> "v0.2.1". An untagged image is returned unchanged.
func imageTag(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at] // drop a digest: name@sha256:...
	}
	if colon := strings.LastIndex(image, ":"); colon > strings.LastIndex(image, "/") {
		return image[colon+1:]
	}
	return image
}

// burrowdImage returns the image of the installed burrowd Deployment, or an error — an
// IsNotFound error when no control plane is installed in the namespace.
func burrowdImage(ctx context.Context, cs kubernetes.Interface, namespace string) (string, error) {
	d, err := cs.AppsV1().Deployments(namespace).Get(ctx, "burrowd", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == "burrowd" {
			return c.Image, nil
		}
	}
	return "", fmt.Errorf("the burrowd deployment in %s has no burrowd container", namespace)
}
