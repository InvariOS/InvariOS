// Package install implements the invarios "Install" sequence: on an
// uninstalled system, partition the target disk into GPT (ESP/META/
// STATE/DATA), format it, pull the UKI and sd-boot for this exact
// build from their OCI registry and write them onto the new ESP,
// initialize META, point EFI Default/BootOrder at the new install, and
// reboot.
package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"

	"github.com/siderolabs/go-blockdevice/v2/partitioning"

	"github.com/invarios/invarios/internal/bootimage"
	"github.com/invarios/invarios/internal/disk"
	"github.com/invarios/invarios/internal/efi"
	"github.com/invarios/invarios/internal/meta"
	"github.com/invarios/invarios/internal/network"
	"github.com/invarios/invarios/internal/ociimage"
	"github.com/invarios/invarios/internal/version"
)

// bootEntryLabel names both the Boot#### NVRAM entry EnsureBootEntry
// creates/refreshes and the description shown for it in a firmware boot
// menu. invarios is the only thing this appliance ever boots, so a
// single fixed label (rather than one derived per-install, e.g. from a
// version string) is deliberate: it's what lets EnsureBootEntry recognize
// and reuse the same NVRAM entry across repeated installs instead of
// accumulating a duplicate one every time.
const bootEntryLabel = "invarios"

// sdbootESPPath and loaderConfPath mirror cmd/build.go's ESP staging
// layout (stageESP) exactly, since Install reproduces that same layout
// on the target disk from the pulled boot artifact, which is staged
// with those same paths.
const (
	sdbootESPPath  = "EFI/BOOT/BOOTX64.EFI"
	sdbootFWPath   = `\EFI\BOOT\BOOTX64.EFI` // same file, "\"-separated, as EFI_LOAD_OPTION device paths require.
	loaderConfPath = "loader/loader.conf"
)

// Detect reports whether the appliance's target disk already has an
// invarios install on it, per disk.FindSystemDisk/disk.IsInstalled.
func Detect() (installed bool, diskPath string, err error) {
	diskPath, err = disk.FindSystemDisk()
	if err != nil {
		return false, "", err
	}

	installed, err = disk.IsInstalled(diskPath)
	if err != nil {
		return false, "", err
	}

	return installed, diskPath, nil
}

// Run installs invarios onto diskPath and reboots. It does not return on
// success -- unix.Reboot hands control back to the firmware, which
// starts the boot process over from scratch.
func Run(ctx context.Context, diskPath string) error {
	fmt.Println("[install] target disk:", diskPath)

	if err := disk.CheckMinimumSize(diskPath); err != nil {
		return fmt.Errorf("install: %w", err)
	}

	// Unlike Boot's best-effort network bring-up, install has no
	// fallback without it: the boot artifact only comes from the
	// registry now.
	if err := network.Up(ctx, "eth0"); err != nil {
		return fmt.Errorf("install: bringing up network: %w", err)
	}

	ukiName, ukiBytes, sdbootBytes, loaderConfBytes, err := fetchBootArtifact(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("[install] fetched %s (%d bytes) and sd-boot (%d bytes) from %s\n", ukiName, len(ukiBytes), len(sdbootBytes), bootimage.Ref(version.Version(), runtime.GOARCH))

	layout, err := disk.Partition(diskPath)
	if err != nil {
		return fmt.Errorf("install: partitioning %s: %w", diskPath, err)
	}

	fmt.Println("[install] partitioned", diskPath)

	if err := disk.Format(diskPath, layout); err != nil {
		return fmt.Errorf("install: formatting %s: %w", diskPath, err)
	}

	fmt.Println("[install] formatted ESP/STATE/DATA")

	if err := writeESP(diskPath, layout, ukiName, ukiBytes, sdbootBytes, loaderConfBytes); err != nil {
		return err
	}

	fmt.Println("[install] wrote UKI + sd-boot + loader.conf to new ESP")

	metaPath := partitioning.DevName(diskPath, uint(layout.Meta.Number))
	if err := meta.Init(metaPath); err != nil {
		return fmt.Errorf("install: initializing META: %w", err)
	}

	fmt.Println("[install] initialized META")

	if err := efi.SetDefault(ukiName); err != nil {
		return fmt.Errorf("install: setting LoaderEntryDefault: %w", err)
	}

	if err := efi.EnsureBootEntry(
		bootEntryLabel,
		uint32(layout.ESP.Number), //nolint:gosec // partition numbers are always small
		layout.ESP.PartGUID,
		layout.ESP.FirstLBA,
		layout.ESP.LastLBA,
		sdbootFWPath,
	); err != nil {
		return fmt.Errorf("install: ensuring EFI boot entry: %w", err)
	}

	fmt.Println("[install] set LoaderEntryDefault and Boot#### entry")
	fmt.Println("[install] rebooting into new install")

	return reboot(ctx)
}

// fetchBootArtifact pulls this exact build's boot artifact (UKI +
// sd-boot + loader.conf, published by cmd/build.go's pushBootImage)
// from its OCI registry and reads it into memory, before anything is
// written to the target disk. version.Version() names the same tag
// pushBootImage pushed under, since both come from the same build.
func fetchBootArtifact(ctx context.Context) (ukiName string, ukiBytes, sdbootBytes, loaderConfBytes []byte, err error) {
	extractDir, err := os.MkdirTemp("/tmp", "boot-artifact-*")
	if err != nil {
		return "", nil, nil, nil, fmt.Errorf("install: creating extract dir: %w", err)
	}
	defer os.RemoveAll(extractDir) //nolint:errcheck

	ref := bootimage.Ref(version.Version(), runtime.GOARCH)
	if err := ociimage.PullAndExtract(ctx, ref, runtime.GOARCH, extractDir); err != nil {
		return "", nil, nil, nil, fmt.Errorf("install: pulling boot artifact: %w", err)
	}

	ukiName = fmt.Sprintf("invarios-%s.efi", version.Version())

	ukiBytes, err = os.ReadFile(filepath.Join(extractDir, "EFI", "Linux", ukiName))
	if err != nil {
		return "", nil, nil, nil, fmt.Errorf("install: reading UKI: %w", err)
	}

	sdbootBytes, err = os.ReadFile(filepath.Join(extractDir, sdbootESPPath))
	if err != nil {
		return "", nil, nil, nil, fmt.Errorf("install: reading sd-boot: %w", err)
	}

	loaderConfBytes, err = os.ReadFile(filepath.Join(extractDir, loaderConfPath))
	if err != nil {
		return "", nil, nil, nil, fmt.Errorf("install: reading loader.conf: %w", err)
	}

	return ukiName, ukiBytes, sdbootBytes, loaderConfBytes, nil
}

// writeESP mounts the freshly formatted ESP on diskPath and writes the
// fetched UKI, sd-boot, and loader.conf onto it, matching
// cmd/build.go's stageESP layout.
func writeESP(diskPath string, layout disk.Layout, ukiName string, ukiBytes, sdbootBytes, loaderConfBytes []byte) error {
	espPath := partitioning.DevName(diskPath, uint(layout.ESP.Number))

	mountPoint, err := mountESP(espPath)
	if err != nil {
		return fmt.Errorf("install: mounting target ESP %s: %w", espPath, err)
	}
	defer unmount(mountPoint)

	if err := writeFile(filepath.Join(mountPoint, "EFI", "Linux", ukiName), ukiBytes); err != nil {
		return err
	}

	if err := writeFile(filepath.Join(mountPoint, sdbootESPPath), sdbootBytes); err != nil {
		return err
	}

	return writeFile(filepath.Join(mountPoint, loaderConfPath), loaderConfBytes)
}

// mountESP mounts devPath (a FAT32 ESP) read-write at a freshly created
// temporary directory and returns the mount point.
func mountESP(devPath string) (string, error) {
	mountPoint, err := os.MkdirTemp("/tmp", "esp-*")
	if err != nil {
		return "", fmt.Errorf("install: creating mount point: %w", err)
	}

	if err := unix.Mount(devPath, mountPoint, "vfat", 0, ""); err != nil {
		_ = os.Remove(mountPoint)

		return "", fmt.Errorf("install: mounting %s at %s: %w", devPath, mountPoint, err)
	}

	return mountPoint, nil
}

// unmount unmounts and removes a mount point created by mountESP. It
// only logs failures: it always runs as a deferred cleanup after the
// mount point has already served its purpose, where the caller has
// nothing left to do differently even if this fails.
func unmount(mountPoint string) {
	if err := unix.Unmount(mountPoint, 0); err != nil {
		fmt.Println("[install] warning: unmounting", mountPoint, "failed:", err)
	}

	_ = os.Remove(mountPoint)
}

// writeFile writes data to path, creating any missing parent
// directories first (an ESP's EFI/Linux, EFI/BOOT, and loader
// directories don't exist yet on a just-formatted filesystem).
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("install: creating %s: %w", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("install: writing %s: %w", path, err)
	}

	return nil
}

// reboot flushes pending writes and restarts the machine.
func reboot(_ context.Context) error {
	unix.Sync()

	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART); err != nil {
		return fmt.Errorf("install: rebooting: %w", err)
	}

	return nil
}
