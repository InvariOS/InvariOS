package mount

import (
	"errors"
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

// UnmountVolumes unmounts DATA and STATE, in the reverse of the order
// Volumes mounted them, so a reboot or power-off finds both XFS
// filesystems cleanly closed (unmount record written, no log replay on
// the next mount). It is only correct to call once nothing is using
// them: the supervised workload has been stopped and reaped, and PID 1
// itself holds no open files under either.
//
// A mount that's still busy is remounted read-only instead, the same
// fallback sysvinit and systemd use at shutdown: it can't detach the
// mount, but it does force the filesystem's log clean and stop any
// further writes, which is what actually matters before reboot(2). A
// lazy unmount (MNT_DETACH) is deliberately not used -- it only removes
// the name from the tree and quiesces nothing.
//
// Every failure (including a successful read-only fallback, since that
// still means something was holding the volume) is collected and
// returned rather than stopping at the first. Callers are expected to
// log it and carry on to sync and reboot regardless: refusing to power
// off a machine an operator asked to power off, over a volume the
// kernel will replay the journal for next boot anyway, is the worse
// outcome.
func UnmountVolumes() error {
	var errs []error

	for _, target := range []string{paths.DataDir, paths.StateDir} {
		if err := unmountOrRemountReadOnly(target); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// unmountOrRemountReadOnly unmounts target, falling back to a read-only
// remount if it's busy (see UnmountVolumes). The remount keeps NoExec
// so the fallback never loosens the flags Volumes mounted with.
func unmountOrRemountReadOnly(target string) error {
	err := unix.Unmount(target, 0)
	if err == nil {
		return nil
	}

	if !errors.Is(err, unix.EBUSY) {
		return fmt.Errorf("unmounting %s: %w", target, err)
	}

	if rerr := unix.Mount("", target, "", unix.MS_REMOUNT|unix.MS_RDONLY|NoExec, ""); rerr != nil {
		return fmt.Errorf("unmounting %s: %w; remounting read-only: %w", target, err, rerr)
	}

	return fmt.Errorf("unmounting %s: %w (remounted read-only instead)", target, err)
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
