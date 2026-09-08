// Package openbao fetches and verifies OpenBao release tarballs for
// bundling into the appliance image.
package openbao

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const releaseURLTemplate = "https://github.com/openbao/openbao/releases/download/v%s"

// Fetch downloads the OpenBao release tarball for version (caching it,
// and the upstream checksums.txt, under downloadsDir so repeated builds
// against the same version don't re-download), verifies its checksum,
// and installs the "bao" binary it contains to destPath.
func Fetch(ctx context.Context, version, downloadsDir, destPath string) error {
	archiveName := fmt.Sprintf("openbao_%s_linux_amd64.tar.gz", version)
	baseURL := fmt.Sprintf(releaseURLTemplate, version)

	archivePath := filepath.Join(downloadsDir, archiveName)
	if err := downloadIfMissing(ctx, baseURL+"/"+archiveName, archivePath); err != nil {
		return fmt.Errorf("downloading %s: %w", archiveName, err)
	}

	checksumsPath := filepath.Join(downloadsDir, "checksums.txt")
	if err := downloadIfMissing(ctx, baseURL+"/checksums.txt", checksumsPath); err != nil {
		return fmt.Errorf("downloading checksums.txt: %w", err)
	}

	if err := verifyChecksum(archivePath, archiveName, checksumsPath); err != nil {
		return err
	}

	if err := extractBaoBinary(archivePath, destPath); err != nil {
		return fmt.Errorf("extracting bao from %s: %w", archiveName, err)
	}

	return nil
}

// downloadIfMissing is a no-op when dest already exists, so re-running a
// build against a version already fetched doesn't hit the network.
func downloadIfMissing(ctx context.Context, url, dest string) (err error) {
	if _, statErr := os.Stat(dest); statErr == nil {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s fetching %s", resp.Status, url)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}

	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}

		// Don't leave a truncated/partial file around to be mistaken
		// for a complete, cached download on the next run.
		if err != nil {
			_ = os.Remove(dest)
		}
	}()

	_, err = io.Copy(out, resp.Body)

	return err
}

// verifyChecksum checks archivePath's sha256 against the line naming
// archiveName in the sha256sum-format checksumsPath.
func verifyChecksum(archivePath, archiveName, checksumsPath string) error {
	checksums, err := os.ReadFile(checksumsPath)
	if err != nil {
		return fmt.Errorf("reading checksums.txt: %w", err)
	}

	var want string

	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == archiveName {
			want = fields[0]

			break
		}
	}

	if want == "" {
		return fmt.Errorf("%s not listed in checksums.txt", archiveName)
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hashing %s: %w", archivePath, err)
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("checksum mismatch for %s: want %s, got %s", archiveName, want, got)
	}

	return nil
}

// extractBaoBinary extracts the top-level "bao" file from the gzipped
// tar archive at archivePath to destPath, executable.
func extractBaoBinary(archivePath, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("opening gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("archive did not contain a bao binary")
		}

		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}

		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != "bao" {
			continue
		}

		out, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		defer func() { _ = out.Close() }()

		if _, err := io.Copy(out, tr); err != nil {
			return err
		}

		return out.Close()
	}
}
