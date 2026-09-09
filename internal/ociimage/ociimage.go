// Package ociimage pulls OCI images from a registry and extracts their
// filesystem contents to disk, and pushes a directory's contents as a
// single-layer OCI image, without requiring a Docker daemon. Pull
// replaces what the Dockerfile's multi-stage FROM/COPY --from=
// mechanism did at `docker build` time: the invarios-pkgs kernel and
// systemd-boot images are pulled directly by the build tooling instead.
// Push publishes the boot artifact (see internal/bootimage) that
// Install later pulls back down with the same mechanism.
package ociimage

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
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

// Push archives srcDir into a single-layer image for the given
// architecture and pushes it to ref. insecure allows plain HTTP, for a
// local test registry.
func Push(ctx context.Context, ref, arch, srcDir string, insecure bool) error {
	img, cleanup, err := imageFromDir(ctx, srcDir, arch)
	if err != nil {
		return err
	}
	defer cleanup()

	opts := []crane.Option{crane.WithContext(ctx)}
	if insecure {
		opts = append(opts, crane.Insecure)
	}

	if err := crane.Push(img, ref, opts...); err != nil {
		return fmt.Errorf("pushing %s: %w", ref, err)
	}

	return nil
}

// imageFromDir builds a single-layer v1.Image out of srcDir's contents,
// tagged for the given architecture. The returned layer reads its
// content lazily from a temp file on disk, so the caller must run the
// returned cleanup func only once it's done with img (e.g. after
// pushing it), not before.
func imageFromDir(ctx context.Context, srcDir, arch string) (v1.Image, func(), error) {
	tgz, err := os.CreateTemp("", "ociimage-push-*.tar.gz")
	if err != nil {
		return nil, nil, fmt.Errorf("creating temp archive: %w", err)
	}
	cleanup := func() {
		tgz.Close()           //nolint:errcheck
		os.Remove(tgz.Name()) //nolint:errcheck
	}

	if err := archiver.TarGz(ctx, srcDir, tgz); err != nil {
		cleanup()

		return nil, nil, fmt.Errorf("archiving %s: %w", srcDir, err)
	}

	layer, err := tarball.LayerFromFile(tgz.Name())
	if err != nil {
		cleanup()

		return nil, nil, fmt.Errorf("building layer from %s: %w", tgz.Name(), err)
	}

	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		cleanup()

		return nil, nil, fmt.Errorf("appending layer: %w", err)
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		cleanup()

		return nil, nil, fmt.Errorf("reading image config: %w", err)
	}

	cfg = cfg.DeepCopy()
	cfg.OS = "linux"
	cfg.Architecture = arch

	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		cleanup()

		return nil, nil, fmt.Errorf("setting image config: %w", err)
	}

	return img, cleanup, nil
}
