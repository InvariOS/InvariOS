// Package parttype holds fixed GPT partition type GUIDs shared by the
// invarios binary (internal/disk) and the build tool (cmd), which
// can't import internal/disk itself since that package is a runtime
// dependency of the installed appliance, not of the build tool.
package parttype

import "github.com/google/uuid"

// ESP is the EFI System Partition type GUID (UEFI Specification
// section 5.7).
var ESP = uuid.MustParse("c12a7328-f81f-11d2-ba4b-00a0c93ec93b")

// LinuxFilesystem is the generic "Linux filesystem" type GUID
// (the Discoverable Partitions Specification's "Generic Linux Data
// Partition"). Its spec text guarantees no automatic mounting, which
// invarios relies on since its own partitions are identified by name,
// not type.
var LinuxFilesystem = uuid.MustParse("0fc63daf-8483-4772-8e79-3d69d8477de4")
