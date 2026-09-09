// Package cmd implements the invarios CLI: the root command that runs as
// the appliance's PID 1 supervisor, and the build command that assembles
// the bootable appliance image.
package cmd

import (
	"context"
	"debug/pe"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/siderolabs/talos/pkg/makefs"

	"github.com/invarios/invarios/internal/bootimage"
	"github.com/invarios/invarios/internal/initramfs"
	"github.com/invarios/invarios/internal/ociimage"
	"github.com/invarios/invarios/internal/openbao"
	"github.com/invarios/invarios/internal/uki"
	"github.com/invarios/invarios/internal/version"
)

// modulePath is this project's Go module path, used to target -ldflags
// -X at internal/version's build-time variables when cross-compiling
// the invarios binary in buildInvariosBinary.
const modulePath = "github.com/invarios/invarios"

// kernelCmdline is the command line embedded in the UKI. Both consoles
// are listed so kernel/systemd boot messages appear on serial
// (ttyS0, e.g. a hypervisor's serial console) and on a graphical
// console (tty0, e.g. VNC/SPICE), matching internal/console's dual
// stdout/stderr fan-out for everything after the kernel hands off.
const kernelCmdline = "console=tty0 console=ttyS0,115200"

// loaderConf is written to loader/loader.conf on the ESP: systemd-boot's
// own configuration file. A timeout of 0 skips the boot menu and boots
// the default (newest) UKI entry immediately.
const loaderConf = "timeout 0\n"

// espImageSize is the size of both the removable EFI boot disk image
// and the ISO's embedded El Torito EFI boot image. The ESP holds the
// systemd-boot loader, one UKI (currently ~95 MiB, dominated by the
// embedded initramfs), and a config file; 128 MiB leaves some room to
// grow before this needs revisiting (e.g. a larger initramfs, or a
// second UKI kept around for rollback).
const espImageSize = 128 << 20 // 128 MiB

var (
	// buildArch selects the OCI platform pulled for the kernel,
	// systemd-boot, and boot-artifact images, and the GOARCH used to
	// compile invarios itself. Only "amd64" is exercised today, but
	// it's a flag rather than a constant so a second architecture
	// doesn't need every one of those call sites changed later.
	buildArch string

	buildKernelImage      string
	buildSystemdBootImage string
	buildFsutilsImage     string
	buildOpenBaoVersion   string
	buildRoot             string

	// buildBootImagePushRepo is where this command pushes the boot
	// artifact (UKI + sd-boot + loader.conf) to. buildBootImagePullRepo
	// is baked into the compiled invarios binary for Install to pull it
	// back down from. The two differ for local testing (see the
	// Makefile's local registry, reachable at different addresses from
	// the build tool vs. from inside the test VM); real builds leave
	// both at their shared default.
	buildBootImagePushRepo string
	buildBootImagePullRepo string
	buildBootImageInsecure bool
)

// buildCmd builds the appliance image: it fetches OpenBao and the
// prebuilt kernel/systemd-boot artifacts, compiles this same binary for
// the target, assembles the initramfs and UKI, and writes a bootable
// EFI disk image and ISO. It's the native-Go replacement for what used
// to be scripts/build.sh.
var buildCmd = &cobra.Command{
	Use:   "build",
	Short: "Build the OpenBao appliance image (initramfs, UKI, EFI disk, ISO).",
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runBuild(cmd.Context())
	},
}

func init() {
	buildCmd.Flags().StringVar(&buildRoot, "root", ".", "Repository root.")
	buildCmd.Flags().StringVar(&buildArch, "arch", "amd64", "Target architecture (GOARCH and OCI platform).")
	buildCmd.Flags().StringVar(&buildKernelImage, "kernel-image", "ghcr.io/invarios/pkgs/kernel:6.18.49-amd64", "OCI image to pull the kernel from.")
	buildCmd.Flags().StringVar(&buildSystemdBootImage, "systemd-boot-image", "ghcr.io/invarios/pkgs/systemd-boot:261.2-amd64", "OCI image to pull systemd-boot from.")
	buildCmd.Flags().StringVar(&buildFsutilsImage, "fsutils-image", "ghcr.io/invarios/pkgs/fsutils:main", "OCI image to pull mkfs.vfat/mkfs.xfs from.")
	buildCmd.Flags().StringVar(&buildOpenBaoVersion, "openbao-version", "2.6.2", "OpenBao release version to bundle.")
	buildCmd.Flags().StringVar(&buildBootImagePushRepo, "boot-image-push-repo", bootimage.Repository, "OCI repository to push the boot artifact (UKI + sd-boot) to.")
	buildCmd.Flags().StringVar(&buildBootImagePullRepo, "boot-image-pull-repo", bootimage.Repository, "OCI repository the built invarios binary pulls the boot artifact from.")
	buildCmd.Flags().BoolVar(&buildBootImageInsecure, "boot-image-insecure", false, "Allow plain HTTP against the boot image repositories (for a local test registry).")

	rootCmd.AddCommand(buildCmd)
}

// buildLayout holds the filesystem paths used throughout the build.
type buildLayout struct {
	root      string // repository root (the builder container's WORKDIR is /work)
	downloads string // download cache, persisted across runs via root
	scratch   string // scratch workspace, not persisted
	rootfs    string // staged initramfs contents, under scratch
	out       string // build output, under root
}

func newBuildLayout(root string) buildLayout {
	scratch := "/tmp/invarios-build"

	return buildLayout{
		root:      root,
		downloads: filepath.Join(root, "downloads"),
		scratch:   scratch,
		rootfs:    filepath.Join(scratch, "rootfs"),
		out:       filepath.Join(root, "out"),
	}
}

func runBuild(ctx context.Context) error {
	layout := newBuildLayout(buildRoot)

	if err := prepare(layout); err != nil {
		return fmt.Errorf("preparing build directories: %w", err)
	}

	tag, sha, err := gitDescribe(ctx, layout.root)
	if err != nil {
		return fmt.Errorf("determining version: %w", err)
	}

	logStep("version %s (%s)", tag, sha)

	if err := openbao.Fetch(ctx, buildOpenBaoVersion, layout.downloads, filepath.Join(layout.rootfs, "usr/bin/bao")); err != nil {
		return fmt.Errorf("fetching OpenBao: %w", err)
	}

	logStep("OpenBao %s installed", buildOpenBaoVersion)

	if err := buildInvariosBinary(ctx, layout, tag, sha); err != nil {
		return fmt.Errorf("building invarios binary: %w", err)
	}

	if err := verifyRootfs(layout); err != nil {
		return fmt.Errorf("verifying rootfs: %w", err)
	}

	if err := installKernel(ctx, layout); err != nil {
		return fmt.Errorf("installing kernel: %w", err)
	}

	if err := installFsutils(ctx, layout); err != nil {
		return fmt.Errorf("installing fsutils: %w", err)
	}

	sdbootDir, err := installSystemdBoot(ctx, layout)
	if err != nil {
		return fmt.Errorf("installing systemd-boot: %w", err)
	}

	osRelease := version.OSReleaseFor(version.Name, tag)
	if err := os.WriteFile(filepath.Join(layout.rootfs, "etc/os-release"), osRelease, 0o644); err != nil {
		return fmt.Errorf("writing os-release: %w", err)
	}

	initrdPath := filepath.Join(layout.out, "initramfs.cpio.gz")
	if err := initramfs.WriteCPIO(layout.rootfs, initrdPath); err != nil {
		return fmt.Errorf("building initramfs: %w", err)
	}

	logStep("initramfs written to %s", initrdPath)

	ukiPath := filepath.Join(layout.out, "invarios.efi")

	if err := uki.Build(uki.Config{
		StubPath:   filepath.Join(sdbootDir, "linuxx64.efi.stub"),
		KernelPath: filepath.Join(layout.out, "vmlinuz"),
		InitrdPath: initrdPath,
		Cmdline:    kernelCmdline,
		OSRelease:  osRelease,
		OutputPath: ukiPath,
	}); err != nil {
		return fmt.Errorf("building UKI: %w", err)
	}

	logStep("UKI written to %s", ukiPath)

	if err := verifyUKI(ukiPath); err != nil {
		return fmt.Errorf("verifying UKI: %w", err)
	}

	versionID, err := osReleaseVersionID(osRelease)
	if err != nil {
		return fmt.Errorf("reading os-release VERSION_ID: %w", err)
	}

	espStage, err := stageESP(layout, sdbootDir, ukiPath, versionID)
	if err != nil {
		return fmt.Errorf("staging ESP contents: %w", err)
	}

	if err := pushBootImage(ctx, espStage, versionID); err != nil {
		return fmt.Errorf("pushing boot image: %w", err)
	}

	efiDiskPath := filepath.Join(layout.out, "invarios-efi.img")
	if err := buildFATImage(ctx, efiDiskPath, espStage); err != nil {
		return fmt.Errorf("building EFI boot disk: %w", err)
	}

	logStep("EFI boot disk written to %s", efiDiskPath)

	if err := buildISO(ctx, layout, espStage); err != nil {
		return fmt.Errorf("building ISO: %w", err)
	}

	logStep("build complete")

	return nil
}

func prepare(layout buildLayout) error {
	logStep("preparing build directories")

	if err := os.RemoveAll(layout.scratch); err != nil {
		return err
	}

	if err := os.RemoveAll(layout.out); err != nil {
		return err
	}

	for _, dir := range []string{layout.downloads, layout.scratch, layout.rootfs, layout.out} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	srcRootfs := filepath.Join(layout.root, "rootfs")

	if _, err := os.Stat(srcRootfs); err == nil {
		if err := copyTree(srcRootfs, layout.rootfs); err != nil {
			return fmt.Errorf("copying rootfs template: %w", err)
		}
	}

	// Git doesn't track empty directories, so these need to exist even
	// when the checked-in rootfs template above didn't create them.
	for _, dir := range []string{"usr/bin", "etc", "proc", "sys", "dev", "run", "tmp"} {
		if err := os.MkdirAll(filepath.Join(layout.rootfs, dir), 0o755); err != nil {
			return err
		}
	}

	return nil
}

// gitDescribe returns the version this build was produced from: tag is
// the nearest git tag (falling back to an abbreviated commit hash when
// there is no tag reachable), and sha is always an abbreviated commit
// hash. Both carry a "-dirty" suffix when root has uncommitted changes.
//
// sha is computed with --match=none, a git idiom that guarantees no
// tag can match, forcing --always's abbreviated-hash fallback even in
// a tree that does have tags.
func gitDescribe(ctx context.Context, root string) (tag, sha string, err error) {
	tag, err = runGit(ctx, root, "describe", "--tags", "--always", "--dirty", "--match", "v[0-9]*")
	if err != nil {
		return "", "", fmt.Errorf("describing tag: %w", err)
	}

	sha, err = runGit(ctx, root, "describe", "--always", "--dirty", "--match=none", "--abbrev=8")
	if err != nil {
		return "", "", fmt.Errorf("describing commit: %w", err)
	}

	return tag, sha, nil
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(out)), nil
}

// buildInvariosBinary compiles this same program for the target
// architecture and installs it into the staged rootfs as /usr/bin/invarios,
// then points /init at it. tag and sha are baked into internal/version
// via -ldflags, so the resulting binary reports the same version used
// to generate os-release. internal/bootimage is baked in with
// buildBootImagePullRepo/buildBootImageInsecure, not the push repo:
// this binary is the one that later runs Install and pulls the boot
// artifact back down, possibly from a different address than the one
// this build pushed it to (see the local dev registry in the Makefile).
func buildInvariosBinary(ctx context.Context, layout buildLayout, tag, sha string) error {
	logStep("building invarios Go binary")

	binPath := filepath.Join(layout.rootfs, "usr/bin/invarios")

	ldflags := fmt.Sprintf(
		"-X %[1]s/internal/version.Tag=%[2]s -X %[1]s/internal/version.SHA=%[3]s "+
			"-X %[1]s/internal/bootimage.Repository=%[4]s -X %[1]s/internal/bootimage.Insecure=%[5]t",
		modulePath, tag, sha, buildBootImagePullRepo, buildBootImageInsecure,
	)

	build := exec.CommandContext(ctx, "go", "build", "-ldflags", ldflags, "-o", binPath, ".")
	build.Dir = layout.root
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+buildArch)
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr

	if err := build.Run(); err != nil {
		return err
	}

	// The kernel execs /init directly as PID 1. A relative target (not
	// /usr/bin/invarios) is required here: it must resolve correctly
	// both under this staging directory, when verifyRootfs below
	// stats it, and at actual boot, relative to the real /. An
	// absolute target would resolve against the build container's own
	// root and silently fail the staging-directory check.
	initPath := filepath.Join(layout.rootfs, "init")

	if err := os.Remove(initPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return os.Symlink("usr/bin/invarios", initPath)
}

func verifyRootfs(layout buildLayout) error {
	logStep("verifying rootfs")

	for _, rel := range []string{"init", "usr/bin/bao", "usr/bin/invarios"} {
		info, err := os.Stat(filepath.Join(layout.rootfs, rel))
		if err != nil {
			return fmt.Errorf("missing %s: %w", rel, err)
		}

		if info.Mode()&0o111 == 0 {
			return fmt.Errorf("%s is not executable", rel)
		}
	}

	return nil
}

// installKernel pulls buildKernelImage and installs the vmlinuz/config
// it contains into the build output directory.
func installKernel(ctx context.Context, layout buildLayout) error {
	logStep("installing prebuilt kernel")

	kernelDir := filepath.Join(layout.scratch, "kernel")
	if err := ociimage.PullAndExtract(ctx, buildKernelImage, buildArch, kernelDir); err != nil {
		return err
	}

	if err := copyFile(filepath.Join(kernelDir, "vmlinuz"), filepath.Join(layout.out, "vmlinuz"), 0o644); err != nil {
		return err
	}

	return copyFile(filepath.Join(kernelDir, "kernel.config"), filepath.Join(layout.out, "kernel.config"), 0o644)
}

// installFsutils pulls buildFsutilsImage and extracts it straight into
// the staged rootfs. That image (invarios-pkgs/fsutils) is itself
// already laid out at the exact paths its contents need to live at in
// the appliance (/sbin/mkfs.vfat, /sbin/mkfs.xfs, and the Alpine musl +
// shared libraries they're dynamically linked against), so no
// per-file copying/renaming is needed the way installKernel and
// installSystemdBoot do.
//
// This is a deliberate, temporary shortcut: everything else this
// command builds is compiled from verified upstream source, but these
// two binaries are not. See invarios-pkgs/fsutils/Dockerfile's own top
// comment for what trust boundary it relies on instead, and what
// replacing it with a from-source build would look like.
func installFsutils(ctx context.Context, layout buildLayout) error {
	logStep("installing prebuilt fsutils (mkfs.vfat/mkfs.xfs)")

	return ociimage.PullAndExtract(ctx, buildFsutilsImage, buildArch, layout.rootfs)
}

// installSystemdBoot pulls buildSystemdBootImage and returns the
// directory containing the bootloader and EFI stub binaries, renamed
// to the names the rest of the build expects.
func installSystemdBoot(ctx context.Context, layout buildLayout) (string, error) {
	logStep("installing prebuilt systemd-boot")

	pulledDir := filepath.Join(layout.scratch, "systemd-boot-pulled")
	if err := ociimage.PullAndExtract(ctx, buildSystemdBootImage, buildArch, pulledDir); err != nil {
		return "", err
	}

	sdbootDir := filepath.Join(layout.scratch, "systemd-boot")
	if err := os.MkdirAll(sdbootDir, 0o755); err != nil {
		return "", err
	}

	if err := copyFile(filepath.Join(pulledDir, "systemd-boot.efi"), filepath.Join(sdbootDir, "systemd-bootx64.efi"), 0o644); err != nil {
		return "", err
	}

	if err := copyFile(filepath.Join(pulledDir, "boot.efi.stub"), filepath.Join(sdbootDir, "linuxx64.efi.stub"), 0o644); err != nil {
		return "", err
	}

	return sdbootDir, nil
}

// verifyUKI checks that the assembled UKI's .osrel section looks like a
// real os-release file. systemd-boot builds its EFI/Linux/ menu entry
// out of .osrel; a UKI with a missing or malformed .osrel is silently
// skipped by the boot menu, which looks like an unbootable image rather
// than a bad section.
func verifyUKI(path string) error {
	logStep("verifying UKI")

	f, err := pe.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	section := f.Section(".osrel")
	if section == nil {
		return errors.New("UKI has no .osrel section")
	}

	data, err := io.ReadAll(io.LimitReader(section.Open(), int64(section.VirtualSize)))
	if err != nil {
		return fmt.Errorf("reading .osrel section: %w", err)
	}

	content := string(data)

	if !hasOSReleaseKey(content, "ID") {
		return errors.New("UKI .osrel has no ID=")
	}

	if !hasOSReleaseKey(content, "VERSION_ID") {
		return errors.New("UKI .osrel has no VERSION_ID=")
	}

	logStep(".osrel contents:\n%s", content)

	return nil
}

func hasOSReleaseKey(content, key string) bool {
	for _, line := range strings.Split(content, "\n") {
		if _, ok := strings.CutPrefix(line, key+"="); ok {
			return true
		}
	}

	return false
}

// osReleaseVersionID extracts VERSION_ID from os-release file contents.
// It names the UKI on the ESP (invarios-<version>.efi).
func osReleaseVersionID(osRelease []byte) (string, error) {
	for _, line := range strings.Split(string(osRelease), "\n") {
		val, ok := strings.CutPrefix(strings.TrimSpace(line), "VERSION_ID=")
		if !ok {
			continue
		}

		return strings.Trim(val, `"`), nil
	}

	return "", errors.New("VERSION_ID not found")
}

// stageESP lays out a local directory exactly matching the intended
// ESP/ISO EFI-boot-image file tree, for buildFATImage to copy onto the
// FAT filesystem it builds.
func stageESP(layout buildLayout, sdbootDir, ukiPath, versionID string) (string, error) {
	logStep("staging ESP contents")

	stageDir := filepath.Join(layout.scratch, "esp")
	if err := os.RemoveAll(stageDir); err != nil {
		return "", err
	}

	// systemd-boot is the removable-media bootloader; the UKI itself is
	// never placed at EFI/BOOT/BOOTX64.EFI directly.
	if err := copyFile(
		filepath.Join(sdbootDir, "systemd-bootx64.efi"),
		filepath.Join(stageDir, "EFI", "BOOT", "BOOTX64.EFI"),
		0o644,
	); err != nil {
		return "", err
	}

	ukiName := fmt.Sprintf("invarios-%s.efi", versionID)
	if err := copyFile(ukiPath, filepath.Join(stageDir, "EFI", "Linux", ukiName), 0o644); err != nil {
		return "", err
	}

	loaderConfPath := filepath.Join(stageDir, "loader", "loader.conf")
	if err := os.MkdirAll(filepath.Dir(loaderConfPath), 0o755); err != nil {
		return "", err
	}

	if err := os.WriteFile(loaderConfPath, []byte(loaderConf), 0o644); err != nil {
		return "", err
	}

	return stageDir, nil
}

// pushBootImage publishes stageDir (the same tree stageESP just wrote:
// UKI + sd-boot + loader.conf) as a single-layer OCI image tagged
// versionID, so Install can pull it back down instead of copying it
// from whatever booted the install media.
func pushBootImage(ctx context.Context, stageDir, versionID string) error {
	logStep("pushing boot image")

	ref := buildBootImagePushRepo + ":" + bootimage.Tag(versionID, buildArch)
	if err := ociimage.Push(ctx, ref, buildArch, stageDir, buildBootImageInsecure); err != nil {
		return err
	}

	logStep("boot image pushed to %s", ref)

	return nil
}

// buildFATImage creates a fresh espImageSize FAT32 image at imagePath
// and populates it with stageDir's contents.
func buildFATImage(ctx context.Context, imagePath, stageDir string) error {
	if err := os.RemoveAll(imagePath); err != nil {
		return err
	}

	if err := createSizedFile(imagePath, espImageSize); err != nil {
		return err
	}

	return makefs.VFAT(ctx, imagePath, makefs.WithSourceDirectory(stageDir))
}

func createSizedFile(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}

	if err := f.Truncate(size); err != nil {
		_ = f.Close()

		return err
	}

	return f.Close()
}

// buildISO wraps a FAT image with the same ESP contents as an El Torito
// EFI boot image inside an ISO9660 image, via xorriso.
func buildISO(ctx context.Context, layout buildLayout, stageDir string) error {
	logStep("building bootable UEFI ISO")

	isoRoot := filepath.Join(layout.scratch, "iso")
	if err := os.RemoveAll(isoRoot); err != nil {
		return err
	}

	if err := os.MkdirAll(isoRoot, 0o755); err != nil {
		return err
	}

	efiImg := filepath.Join(isoRoot, "efiboot.img")
	if err := buildFATImage(ctx, efiImg, stageDir); err != nil {
		return fmt.Errorf("building El Torito EFI boot image: %w", err)
	}

	isoPath := filepath.Join(layout.out, "invarios.iso")

	xorriso := exec.CommandContext(ctx, "xorriso",
		"-as", "mkisofs",
		"-R",
		"-J",
		"-V", "OPENBAO",
		"-o", isoPath,
		"-e", "efiboot.img",
		"-no-emul-boot",
		isoRoot,
	)
	xorriso.Stdout = os.Stdout
	xorriso.Stderr = os.Stderr

	if err := xorriso.Run(); err != nil {
		return err
	}

	logStep("ISO written to %s", isoPath)

	return nil
}

// copyTree recursively copies src to dst, preserving symlinks and file
// modes -- a Go equivalent of `cp -a`.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)

		info, err := d.Info()
		if err != nil {
			return err
		}

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}

			return os.Symlink(link, target)
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		default:
			return copyFile(path, target, info.Mode().Perm())
		}
	})
}

// copyFile copies src to dst with the given mode, creating dst's parent
// directory if needed.
func copyFile(src, dst string, mode fs.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}

	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	_, err = io.Copy(out, in)

	return err
}

func logStep(format string, args ...any) {
	fmt.Printf("[build] "+format+"\n", args...)
}
