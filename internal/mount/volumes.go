package mount

import (
	"fmt"
	"os"

	"github.com/siderolabs/go-blockdevice/v2/partitioning"
	"golang.org/x/sys/unix"

	"github.com/invarios/invarios/internal/disk"
	"github.com/invarios/invarios/internal/paths"
)

// Volumes mounts STATE and DATA -- both XFS, laid out by
// disk.Partition/disk.Format at install time -- at paths.StateDir and
// paths.DataDir respectively. Both are mounted NoExec: STATE holds
// machine config and the CA, DATA holds bao's raft directory, and
// neither is a store PID 1 or bao should ever execute a binary from,
// planted there by a bug or otherwise.
//
// Both mount points live in internal/paths rather than as constants
// here, so packages that need to know where they are (e.g.
// internal/supervise, starting bao pointed at DATA) don't have to
// import mount itself -- see that package's doc comment for why.
func Volumes(diskPath string) error {
	layout, err := disk.ReadLayout(diskPath)
	if err != nil {
		return fmt.Errorf("reading disk layout: %w", err)
	}

	if err := mountXFS(diskPath, layout.State.Number, paths.StateDir); err != nil {
		return err
	}

	return mountXFS(diskPath, layout.Data.Number, paths.DataDir)
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
