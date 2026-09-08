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

// Partition sizes, in bytes. ESP fits two UKIs plus sd-boot with
// headroom (a single current UKI is ~95 MiB); META only needs to hold
// the 512 KiB two-copy block internal/meta.Init writes, and is rounded
// up generously to 1 MiB; STATE is 256 MiB, comfortably larger than the
// configuration and CA material it will eventually hold. DATA is not a
// fixed size -- Partition sizes it to whatever is left on the disk.
const (
	espSize   = 320 * 1024 * 1024
	metaSize  = 1 * 1024 * 1024
	stateSize = 256 * 1024 * 1024
)

// Fixed GPT partition type GUIDs, not ours to choose: espPartitionType
// is the EFI System Partition type defined by the UEFI Specification
// itself (section 5.7) -- firmware and boot loaders rely on this exact
// value to find the ESP at all. dataPartitionType is the generic
// "Linux filesystem" type (the Discoverable Partitions Specification's
// formal name for the same value is "Generic Linux Data Partition");
// its spec text guarantees no automatic mounting ever happens for it,
// which is useful here, since META, STATE, and DATA are identified by
// partition name (see IsInstalled below), not type, and nothing but
// this package's own code should ever mount them.
var (
	espPartitionType  = uuid.MustParse("c12a7328-f81f-11d2-ba4b-00a0c93ec93b")
	dataPartitionType = uuid.MustParse("0fc63daf-8483-4772-8e79-3d69d8477de4")
)

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
	return listDisks(true)
}

// listAllDisks is like ListNonRemovableDisks but includes removable
// media too. FindPartitionByGUID needs this: the source media for an
// install (e.g. an installer ISO/USB) is very often removable, unlike
// the non-removable-only target disk ListNonRemovableDisks/
// FindSystemDisk look for.
func listAllDisks() ([]string, error) {
	return listDisks(false)
}

func listDisks(nonRemovableOnly bool) ([]string, error) {
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

		if nonRemovableOnly {
			removable, err := os.ReadFile(filepath.Join(sysClassBlock, name, "removable"))
			if err != nil || strings.TrimSpace(string(removable)) != "0" {
				continue
			}
		}

		disks = append(disks, filepath.Join("/dev", name))
	}

	return disks, nil
}

// FindPartitionByGUID scans every whole-disk block device (removable
// media included, see listAllDisks) for a GPT partition whose PartGUID
// matches partUUID, and returns the disk it was found on together with
// its partition info. There is no /dev/disk/by-partuuid/ symlink to
// look up directly: invarios's minimal initramfs runs no udev, so
// nothing populates that tree.
//
// This is how Install locates the source ESP it booted from: systemd-
// boot's LoaderDevicePartUUID variable (see internal/efi.BootedEntry)
// names that ESP by its GPT partition GUID, not by a device path, since
// the same UKI can end up booted from different device paths (a USB
// stick's /dev/sda1 vs. a virtio disk's /dev/vda1) depending on the
// machine.
//
// A partition GUID is only unique by convention, not by construction: a
// disk cloned byte-for-byte from another (e.g. an installer USB imaged
// from the same source as a target disk from a prior, incomplete
// install) carries an identical one. Since this function exists to
// locate the exact device Install is about to read its own boot files
// from, silently picking one of several matches would risk buffering
// the wrong disk's UKI/sd-boot without any indication that happened.
// Finding more than one match is therefore a hard error, not a
// best-effort pick.
func FindPartitionByGUID(partUUID uuid.UUID) (diskPath string, info PartitionInfo, err error) {
	disks, err := listAllDisks()
	if err != nil {
		return "", PartitionInfo{}, err
	}

	var matchDisks []string

	var matchInfo []PartitionInfo

	for _, d := range disks {
		found, ok := findPartitionOnDisk(d, partUUID)
		if ok {
			matchDisks = append(matchDisks, d)
			matchInfo = append(matchInfo, found)
		}
	}

	switch len(matchDisks) {
	case 0:
		return "", PartitionInfo{}, fmt.Errorf("disk: no partition with GUID %s found", partUUID)
	case 1:
		return matchDisks[0], matchInfo[0], nil
	default:
		return "", PartitionInfo{}, fmt.Errorf("disk: partition GUID %s is not unique, found on %v", partUUID, matchDisks)
	}
}

// findPartitionOnDisk reads d's GPT table (if any) and returns the
// partition matching partUUID. A disk with no GPT table at all, or any
// other read error, is treated as a non-match rather than a hard error
// -- FindPartitionByGUID needs to keep looking at the remaining disks
// either way.
func findPartitionOnDisk(d string, partUUID uuid.UUID) (PartitionInfo, bool) {
	dev, err := block.NewFromPath(d)
	if err != nil {
		return PartitionInfo{}, false
	}
	defer dev.Close() //nolint:errcheck

	gdev, err := gpt.DeviceFromBlockDevice(dev)
	if err != nil {
		return PartitionInfo{}, false
	}

	table, err := gpt.Read(gdev)
	if err != nil {
		return PartitionInfo{}, false
	}

	// Partitions() is zero-indexed by slice position; the Linux kernel
	// (and gpt.Table.AllocatePartition's own return value) number
	// partitions 1-indexed, one past the slice position.
	for i, p := range table.Partitions() {
		if p != nil && p.PartGUID == partUUID {
			return PartitionInfo{Number: i + 1, Partition: *p}, true
		}
	}

	return PartitionInfo{}, false
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

	if layout.ESP, err = allocate(table, espSize, espName, espPartitionType); err != nil {
		return Layout{}, err
	}

	if layout.Meta, err = allocate(table, metaSize, metaName, dataPartitionType); err != nil {
		return Layout{}, err
	}

	if layout.State, err = allocate(table, stateSize, stateName, dataPartitionType); err != nil {
		return Layout{}, err
	}

	dataSize := table.LargestContiguousAllocatable()
	if dataSize == 0 {
		return Layout{}, errors.New("disk: no space left on disk for DATA partition")
	}

	if layout.Data, err = allocate(table, dataSize, dataName, dataPartitionType); err != nil {
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
