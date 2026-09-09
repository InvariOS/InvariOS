// Package paths defines filesystem paths shared across packages that
// otherwise have no reason to depend on each other. It exists so a
// package like internal/supervise (plain Go, builds and tests on any
// platform) can agree with internal/mount on where STATE lives without
// importing internal/mount itself: internal/mount calls Linux-only
// mount(2) flags and depends transitively on internal/disk's
// block-device access, neither of which type-check outside GOOS=linux.
package paths

// StateDir is where internal/mount.Volumes mounts the STATE partition.
const StateDir = "/state"

// DataDir is where internal/mount.Volumes mounts the DATA partition.
const DataDir = "/data"
