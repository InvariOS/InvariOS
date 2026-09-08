// Package mount brings up the virtual filesystems a Linux userspace
// expects to already be mounted (proc, sysfs, devtmpfs). Nothing else in
// this process can do useful work until these are in place: device nodes
// under /dev (including the console itself) come from devtmpfs, and much
// of the standard library's process/network introspection assumes /proc
// is mounted.
package mount

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// virtualFilesystem describes one mount(2) call: a pseudo-filesystem with
// no backing block device, so source and target are conventionally the
// same name.
type virtualFilesystem struct {
	source string
	target string
	fstype string
}

var virtualFilesystems = []virtualFilesystem{
	{source: "proc", target: "/proc", fstype: "proc"},
	{source: "sysfs", target: "/sys", fstype: "sysfs"},
	{source: "devtmpfs", target: "/dev", fstype: "devtmpfs"},
}

// VirtualFilesystems mounts proc, sysfs, and devtmpfs. It returns the
// first error encountered rather than continuing past a failed mount,
// since every filesystem after devtmpfs (device nodes) and proc (used
// throughout the appliance) is load-bearing for the rest of boot.
func VirtualFilesystems() error {
	for _, fs := range virtualFilesystems {
		if err := unix.Mount(fs.source, fs.target, fs.fstype, 0, ""); err != nil {
			return fmt.Errorf("mounting %s at %s: %w", fs.fstype, fs.target, err)
		}
	}

	return nil
}
