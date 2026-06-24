// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

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
