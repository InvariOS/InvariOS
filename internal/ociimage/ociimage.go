// Package ociimage pulls OCI images from a registry and extracts their
// filesystem contents to disk, without requiring a Docker daemon. It
// replaces what the Dockerfile's multi-stage FROM/COPY --from= mechanism
// did at `docker build` time: the invarios-pkgs kernel and systemd-boot
// images are pulled directly by the build tooling instead.
package ociimage

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/siderolabs/talos/pkg/archiver"
	"golang.org/x/sync/errgroup"
)

// Pull fetches ref from its registry for the given architecture (e.g.
// "amd64", "arm64") and returns its manifest/layers without writing
// anything to disk yet.
func Pull(ctx context.Context, ref, arch string) (v1.Image, error) {
	img, err := crane.Pull(
		ref,
		crane.WithContext(ctx),
		crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: arch}),
	)
	if err != nil {
		return nil, fmt.Errorf("pulling %s: %w", ref, err)
	}

	return img, nil
}

// Extract flattens img's layers into a single filesystem tree and writes
// it under destDir. Layer flattening and tar extraction run concurrently,
// streamed through a pipe, so the full layer set is never buffered in
// memory or written to a temporary tarball on disk.
func Extract(ctx context.Context, img v1.Image, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", destDir, err)
	}

	r, w := io.Pipe()

	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		if err := crane.Export(img, w); err != nil {
			_ = w.CloseWithError(err)

			return fmt.Errorf("exporting image: %w", err)
		}

		return w.Close()
	})

	eg.Go(func() error {
		if err := archiver.Untar(ctx, r, destDir, nil); err != nil {
			_ = r.CloseWithError(err)

			return fmt.Errorf("extracting image: %w", err)
		}

		return nil
	})

	return eg.Wait()
}

// PullAndExtract pulls ref and extracts it to destDir in one step.
func PullAndExtract(ctx context.Context, ref, arch, destDir string) error {
	img, err := Pull(ctx, ref, arch)
	if err != nil {
		return err
	}

	return Extract(ctx, img, destDir)
}
