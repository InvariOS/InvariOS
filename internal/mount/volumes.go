package mount

import (
	"fmt"
	"os"

	"github.com/siderolabs/go-blockdevice/v2/partitioning"
	"golang.org/x/sys/unix"

	"github.com/invarios/invarios/internal/disk"
)

// stateMountPoint and dataMountPoint are where Volumes mounts STATE
// and DATA. Both directories ship in the rootfs image (see
// cmd/build.go's prepare), since by the time Volumes runs (Boot, after
// Ephemeral) / is already read-only and can't have them created on
// demand.
const (
	stateMountPoint = "/state"
	dataMountPoint  = "/data"
)

// Volumes mounts STATE and DATA -- both XFS, laid out by
// disk.Partition/disk.Format at install time -- at /state and /data
// respectively. Both are mounted NoExec: STATE holds machine config
// and the CA, DATA holds bao's raft directory, and neither is a store
// PID 1 or bao should ever execute a binary from, planted there by a
// bug or otherwise.
func Volumes(diskPath string) error {
	layout, err := disk.ReadLayout(diskPath)
	if err != nil {
		return fmt.Errorf("reading disk layout: %w", err)
	}

	if err := mountXFS(diskPath, layout.State.Number, stateMountPoint); err != nil {
		return err
	}

	return mountXFS(diskPath, layout.Data.Number, dataMountPoint)
}

// mountXFS mounts partition number of diskPath (an XFS filesystem
// created by disk.Format) at target. MkdirAll is a no-op given target
// already exists in the shipped rootfs; it's defensive, not how target
// is expected to come into being.
func mountXFS(diskPath string, number int, target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", target, err)
	}

	partName := partitioning.DevName(diskPath, uint(number))

	if err := unix.Mount(partName, target, "xfs", NoExec, ""); err != nil {
		return fmt.Errorf("mounting %s at %s: %w", partName, target, err)
	}

	return nil
}
