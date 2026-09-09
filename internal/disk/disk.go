// Package disk discovers the appliance's target disk and lays out its
// GPT partition table: an EFI System Partition (ESP, FAT32), a small META
// partition holding invarios's own two-copy ADV metadata (see
// internal/meta), a STATE partition, and a DATA partition taking up the
// remainder of the disk. STATE and DATA are both XFS: its default
// ftype=1 inode format is required for overlayfs, which workloads
// storing container/OCI layers on DATA will need.
//
// It wraps github.com/siderolabs/go-blockdevice/v2 (block device access
// and GPT read/write) and github.com/siderolabs/talos/pkg/makefs
// (mkfs.vfat/mkfs.xfs invocation) -- disk discovery itself
// (ListNonRemovableDisks) has no equivalent in either module and is
// local sysfs-walking logic.
package disk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/siderolabs/go-blockdevice/v2/block"
	"github.com/siderolabs/go-blockdevice/v2/partitioning"
	"github.com/siderolabs/go-blockdevice/v2/partitioning/gpt"
	"github.com/siderolabs/talos/pkg/makefs"

	"github.com/invarios/invarios/internal/parttype"
)

// Partition names. IsInstalled looks for exactly these four names on an
// existing GPT table to decide whether the disk already has an invarios
// install on it.
const (
	espName   = "ESP"
	metaName  = "META"
	stateName = "STATE"
	dataName  = "DATA"
)

// xfsMinSize is mkfs.xfs's own hard floor: it refuses to format
// anything smaller, regardless of how much is asked for.
const xfsMinSize = 300 * 1024 * 1024

// Partition sizes, in bytes. ESP fits two UKIs plus sd-boot with
// headroom (a single current UKI is ~95 MiB); META only needs to hold
// the 512 KiB two-copy block internal/meta.Init writes, and is rounded
// up generously to 1 MiB; STATE is comfortably above xfsMinSize, not
// sized for the configuration/CA material it will eventually hold.
// DATA is not a fixed size -- Partition sizes it to whatever is left
// on the disk, but that remainder still has to clear xfsMinSize itself
// (see MinimumDiskSize).
const (
	espSize   = 320 * 1024 * 1024
	metaSize  = 1 * 1024 * 1024
	stateSize = 512 * 1024 * 1024
)

// MinimumDiskSize is the smallest disk CheckMinimumSize accepts: the
// three fixed-size partitions plus a DATA partition at exactly
// xfsMinSize, with no margin beyond that. A disk this size installs
// successfully but leaves DATA with no real room to store anything.
const MinimumDiskSize = espSize + metaSize + stateSize + xfsMinSize

// CheckMinimumSize returns a descriptive error if diskPath is smaller
// than MinimumDiskSize, without writing anything to it.
//
// Partition/Format would eventually fail on an undersized disk too --
// either immediately, if there's no room left for a DATA partition at
// all, or later in Format, if DATA lands under xfsMinSize -- but only
// after Partition has already written a GPT table to the disk, and
// with mkfs.xfs's own terse "Filesystem must be larger than 300MB."
// rather than something that names the disk and the actual shortfall.
func CheckMinimumSize(diskPath string) error {
	dev, err := block.NewFromPath(diskPath)
	if err != nil {
		return fmt.Errorf("disk: open %s: %w", diskPath, err)
	}
	defer dev.Close() //nolint:errcheck

	gdev, err := gpt.DeviceFromBlockDevice(dev)
	if err != nil {
		return fmt.Errorf("disk: wrap %s: %w", diskPath, err)
	}

	if size := gdev.GetSize(); size < MinimumDiskSize {
		return fmt.Errorf("disk: %s is too small for invarios: %d MiB available, %d MiB required",
			diskPath, size/(1<<20), MinimumDiskSize/(1<<20))
	}

	return nil
}

// PartitionInfo is a GPT partition entry together with its 1-indexed
// partition number (gpt.Table.AllocatePartition's other return value,
// not carried on gpt.Partition itself), which is what's needed to
// derive the kernel's partition device path (e.g. partition number 1 on
// /dev/vda is /dev/vda1) via partitioning.DevName, and to build a Hard
// Drive Media Device Path in internal/efi.
type PartitionInfo struct {
	Number int
	gpt.Partition
}

// Layout is the four partitions Partition lays out, in creation order.
type Layout struct {
	ESP, Meta, State, Data PartitionInfo
}

// sysClassBlock is where the kernel exposes one symlink per block
// device (whole disks and partitions alike).
const sysClassBlock = "/sys/class/block"

// ListNonRemovableDisks returns the device paths (e.g. "/dev/vda") of
// every whole-disk block device that isn't removable, by walking
// sysClassBlock directly. go-blockdevice has no equivalent enumeration
// function -- block.Device.IsWholeDisk (checked once a specific device
// is already open) and this function's own partition/loop/ram filtering
// answer overlapping but not identical questions, so this is
// implemented directly against sysfs rather than opening every device
// on the system just to ask it.
func ListNonRemovableDisks() ([]string, error) {
	entries, err := os.ReadDir(sysClassBlock)
	if err != nil {
		return nil, fmt.Errorf("disk: reading %s: %w", sysClassBlock, err)
	}

	var disks []string

	for _, entry := range entries {
		name := entry.Name()

		// loopN (loopback) and ramN (brd) devices are never install
		// targets.
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") {
			continue
		}

		// A "partition" sysfs attribute file only exists for partitions
		// of some other disk, never for a whole disk itself.
		if _, err := os.Stat(filepath.Join(sysClassBlock, name, "partition")); err == nil {
			continue
		}

		removable, err := os.ReadFile(filepath.Join(sysClassBlock, name, "removable"))
		if err != nil || strings.TrimSpace(string(removable)) != "0" {
			continue
		}

		disks = append(disks, filepath.Join("/dev", name))
	}

	return disks, nil
}

// FindSystemDisk returns the appliance's target disk. This phase
// assumes exactly one non-removable disk exists on the machine -- no
// cmdline flag, no "largest disk" heuristic -- and errors if it finds
// zero or more than one.
func FindSystemDisk() (string, error) {
	disks, err := ListNonRemovableDisks()
	if err != nil {
		return "", err
	}

	switch len(disks) {
	case 0:
		return "", errors.New("disk: no non-removable disk found")
	case 1:
		return disks[0], nil
	default:
		return "", fmt.Errorf("disk: expected exactly one non-removable disk, found %d: %v", len(disks), disks)
	}
}

// IsInstalled reports whether diskPath already has an invarios GPT
// layout on it, by checking that all four expected partition names are
// present.
//
// TODO: any failure to read a GPT table at all -- most commonly the
// expected case of a blank disk with no table yet, but also a genuine
// disk I/O failure -- is treated the same way here, as "not installed".
// go-blockdevice/v2/partitioning/gpt exports no sentinel error for "no
// GPT header found" to tell the two apart without resorting to parsing
// gpt.Read's error text. Safe to leave unresolved for now only because
// on today's single-disk appliance, a real I/O failure on diskPath
// surfaces again immediately when Partition tries to write to that same
// disk; revisit if that assumption ever changes.
func IsInstalled(diskPath string) (bool, error) {
	dev, err := block.NewFromPath(diskPath)
	if err != nil {
		return false, fmt.Errorf("disk: open %s: %w", diskPath, err)
	}
	defer dev.Close() //nolint:errcheck

	gdev, err := gpt.DeviceFromBlockDevice(dev)
	if err != nil {
		return false, fmt.Errorf("disk: wrap %s: %w", diskPath, err)
	}

	table, err := gpt.Read(gdev)
	if err != nil {
		return false, nil //nolint:nilerr
	}

	want := map[string]bool{espName: true, metaName: true, stateName: true, dataName: true}
	found := make(map[string]bool, len(want))

	for _, p := range table.Partitions() {
		if p != nil && want[p.Name] {
			found[p.Name] = true
		}
	}

	return len(found) == len(want), nil
}

// ReadLayout reads diskPath's existing GPT table and returns the four
// partitions Partition originally created, by name. It's Volumes'
// mount-time counterpart to Partition/Format's install-time GPT write:
// callers use it once IsInstalled has already confirmed all four names
// are present, to get each partition's number for
// partitioning.DevName.
func ReadLayout(diskPath string) (Layout, error) {
	dev, err := block.NewFromPath(diskPath)
	if err != nil {
		return Layout{}, fmt.Errorf("disk: open %s: %w", diskPath, err)
	}
	defer dev.Close() //nolint:errcheck

	gdev, err := gpt.DeviceFromBlockDevice(dev)
	if err != nil {
		return Layout{}, fmt.Errorf("disk: wrap %s: %w", diskPath, err)
	}

	table, err := gpt.Read(gdev)
	if err != nil {
		return Layout{}, fmt.Errorf("disk: reading GPT table on %s: %w", diskPath, err)
	}

	var layout Layout

	remaining := map[string]*PartitionInfo{
		espName:   &layout.ESP,
		metaName:  &layout.Meta,
		stateName: &layout.State,
		dataName:  &layout.Data,
	}

	// Partitions() is zero-indexed; the kernel's partition device
	// nodes (and partitioning.DevName) are one-indexed, hence i+1 --
	// the same convention allocate uses for AllocatePartition's own
	// partition-number return value.
	for i, p := range table.Partitions() {
		if p == nil {
			continue
		}

		if dst, ok := remaining[p.Name]; ok {
			*dst = PartitionInfo{Number: i + 1, Partition: *p}
			delete(remaining, p.Name)
		}
	}

	if len(remaining) > 0 {
		missing := make([]string, 0, len(remaining))
		for name := range remaining {
			missing = append(missing, name)
		}

		return Layout{}, fmt.Errorf("disk: %s missing partitions: %v", diskPath, missing)
	}

	return layout, nil
}

// allocate wraps gpt.Table.AllocatePartition into a PartitionInfo,
// pairing its partition-number return value with the entry itself.
func allocate(table *gpt.Table, size uint64, name string, partType uuid.UUID) (PartitionInfo, error) {
	num, entry, err := table.AllocatePartition(size, name, partType)
	if err != nil {
		return PartitionInfo{}, fmt.Errorf("disk: allocating %s partition: %w", name, err)
	}

	return PartitionInfo{Number: num, Partition: entry}, nil
}

// Partition writes a fresh GPT table to diskPath: ESP, META, STATE, then
// DATA sized to whatever contiguous space remains, in that order. It
// does not format any of the four partitions -- see Format for that --
// only creates and persists the table itself.
func Partition(diskPath string) (Layout, error) {
	dev, err := block.NewFromPath(diskPath, block.OpenForWrite())
	if err != nil {
		return Layout{}, fmt.Errorf("disk: open %s: %w", diskPath, err)
	}
	defer dev.Close() //nolint:errcheck

	gdev, err := gpt.DeviceFromBlockDevice(dev)
	if err != nil {
		return Layout{}, fmt.Errorf("disk: wrap %s: %w", diskPath, err)
	}

	table, err := gpt.New(gdev)
	if err != nil {
		return Layout{}, fmt.Errorf("disk: creating GPT table on %s: %w", diskPath, err)
	}

	var layout Layout

	if layout.ESP, err = allocate(table, espSize, espName, parttype.ESP); err != nil {
		return Layout{}, err
	}

	if layout.Meta, err = allocate(table, metaSize, metaName, parttype.LinuxFilesystem); err != nil {
		return Layout{}, err
	}

	if layout.State, err = allocate(table, stateSize, stateName, parttype.LinuxFilesystem); err != nil {
		return Layout{}, err
	}

	dataSize := table.LargestContiguousAllocatable()
	if dataSize == 0 {
		return Layout{}, errors.New("disk: no space left on disk for DATA partition")
	}

	if layout.Data, err = allocate(table, dataSize, dataName, parttype.LinuxFilesystem); err != nil {
		return Layout{}, err
	}

	if err := table.Write(); err != nil {
		return Layout{}, fmt.Errorf("disk: writing GPT table to %s: %w", diskPath, err)
	}

	return layout, nil
}

// waitForDevice polls for path to appear. gpt.Table.Write adds the new
// kernel partitions via the BLKPG ioctl (see (*gpt.Table).syncKernel),
// which triggers a uevent that devtmpfs reacts to asynchronously -- the
// new /dev/<disk>N node is not guaranteed to exist the instant Write
// returns.
func waitForDevice(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("disk: %s did not appear within %s", path, timeout)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// mkfsFunc is the shape shared by makefs.VFAT and makefs.XFS.
type mkfsFunc func(ctx context.Context, partname string, setters ...makefs.Option) error

// formatOne waits for partition's device node to appear and then runs
// mkfs on it.
func formatOne(ctx context.Context, diskPath string, partition PartitionInfo, label string, mkfs mkfsFunc) error {
	partname := partitioning.DevName(diskPath, uint(partition.Number))

	if err := waitForDevice(partname, 5*time.Second); err != nil {
		return err
	}

	if err := mkfs(ctx, partname, makefs.WithLabel(label), makefs.WithForce(true)); err != nil {
		return fmt.Errorf("disk: formatting %s (%s): %w", partname, label, err)
	}

	return nil
}

// Format creates filesystems on the ESP (FAT32, via mkfs.vfat), STATE,
// and DATA (XFS, via mkfs.xfs) partitions of a layout Partition just
// created. META is deliberately not formatted with a filesystem at all
// -- internal/meta writes its ADV structure directly to the raw
// partition.
func Format(diskPath string, layout Layout) error {
	// pkg/makefs shells out to the bare command names "mkfs.vfat" and
	// "mkfs.xfs", resolved via $PATH -- but PID 1 in the initramfs starts
	// with no PATH set at all. Ensure /sbin (where invarios-pkgs/fsutils'
	// binaries land in the rootfs, see cmd/build.go) is searched.
	_ = os.Setenv("PATH", "/sbin:/usr/sbin:/bin:/usr/bin:"+os.Getenv("PATH"))

	ctx := context.Background()

	if err := formatOne(ctx, diskPath, layout.ESP, espName, makefs.VFAT); err != nil {
		return err
	}

	if err := formatOne(ctx, diskPath, layout.State, stateName, makefs.XFS); err != nil {
		return err
	}

	if err := formatOne(ctx, diskPath, layout.Data, dataName, makefs.XFS); err != nil {
		return err
	}

	return nil
}
