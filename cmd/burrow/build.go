// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/controlplane"
)

// newBuildCmd builds an app's image from a git source reference inside the user's own cluster and,
// on success, deploys it through the same guarded path an explicit deploy uses (ADR-0053). It is the
// optional in-cluster build path, not the deploy spine: deploy stays by image reference (ADR-0007),
// and this is a front-end that ends where deploy begins. Only the git reference crosses the control
// channel — a repository URL plus a commit or tag; the builder clones the source inside the cluster,
// so no code travels over the API (ADR-0004). The source is split across --source (the repository)
// and --ref (the commit or tag) rather than crammed into one string, so an SSH URL (which itself
// contains '@') stays unambiguous and each half maps directly to the SourceRef the control plane
// validates. --image names the target the built image is pushed to (required in this phase; the
// in-cluster registry default is a later phase).
func newBuildCmd() *cobra.Command {
	o := &commonOpts{}
	var repo, ref, image string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "build <app> --source <repo> --ref <commit-or-tag> [--image <target>]",
		Short: "Build an app's image from a git source inside the cluster, then deploy it",
		Long: "Build an app's image from a git source reference inside your own cluster, then deploy it\n" +
			"through the same guarded path an explicit deploy uses (ADR-0053).\n\n" +
			"Only the git reference crosses the control channel — a repository URL (--source) plus a\n" +
			"commit or tag (--ref); the build clones the source inside the cluster, so no code travels\n" +
			"over the API. The built image is pushed to --image, or to the in-cluster registry when --image\n" +
			"is omitted and one is installed (`burrow cluster registry install`); the resulting deploy pins\n" +
			"its digest.\n\n" +
			"This is the optional in-cluster build path, not the default: deploy stays by image reference,\n" +
			"and build is a front-end that ends where deploy begins. Environment configuration is sourced\n" +
			"at deploy time as usual — set it with `burrow app config set <app> KEY=VALUE` beforehand.\n\n" +
			"A source with a Dockerfile builds with buildah; a source without one builds with Cloud Native\n" +
			"Buildpacks, which cannot yet push to the plain-HTTP in-cluster registry — for the no-Dockerfile\n" +
			"case, push to an external registry with --image (ADR-0054).",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app := args[0]
			// Validate the git reference client-side, before any call, with the same semantics the
			// control plane enforces (SourceRef.Validate): a repository URL and a commit or tag are
			// both required.
			source := controlplane.SourceRef{Repo: repo, Ref: ref}
			if err := source.Validate(); err != nil {
				return err
			}
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.Build(ctx, app, client.BuildRequest{
				Env:         env,
				Source:      client.SourceRef{Repo: repo, Ref: ref},
				TargetImage: image,
				Confirm:     confirm,
			})
			if err != nil {
				return err
			}
			rel := res.Deploy.Release
			human := fmt.Sprintf("built %s (digest %s) and deployed as release %s (image %s, %d replica(s), %s)",
				app, res.Digest, rel.ID, rel.Image, rel.Replicas, rel.Status)
			if res.Deploy.SupersededReleaseID != "" {
				human += fmt.Sprintf("; superseded release %s", res.Deploy.SupersededReleaseID)
			}
			return emit(cmd.OutOrStdout(), o.json, res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&repo, "source", "", "git repository URL to clone and build (required)")
	cmd.Flags().StringVar(&ref, "ref", "", "commit SHA or tag to build (required)")
	cmd.Flags().StringVar(&image, "image", "", "target image reference to push to; omit to use the in-cluster registry if installed")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	return cmd
}

// runner runs an external command. It is a seam: production shells out to docker; tests
// substitute a recorder so build/push can be verified without Docker.
type runner func(ctx context.Context, name string, args ...string) error

// execRunner runs commands for real, streaming their output to the given writers.
func execRunner(stdout, stderr io.Writer) runner {
	return func(ctx context.Context, name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		return cmd.Run()
	}
}

// buildAndPush builds the image from dir and pushes it to its registry (the client-side
// build path, ADR-0008). The built image moves through the registry, never through
// Burrow (ADR-0004); the deploy that follows references it by tag.
func buildAndPush(ctx context.Context, dir, image string, run runner) error {
	if image == "" {
		return errors.New("--build requires --image (the tag to build and push)")
	}
	if err := run(ctx, "docker", "build", "-t", image, dir); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	if err := run(ctx, "docker", "push", image); err != nil {
		return fmt.Errorf("docker push: %w", err)
	}
	return nil
}
