# Third-party notices

This project vendors source from other projects. Each copied file retains
its original license header; this document is an index so the provenance
is discoverable without digging through file headers.

## internal/uki/pe

[`pe.go`](internal/uki/pe/pe.go) and [`native.go`](internal/uki/pe/native.go)
are copied, unmodified, from
[siderolabs/talos](https://github.com/siderolabs/talos)
`internal/pkg/uki/internal/pe/{pe.go,native.go}`, at commit
`9abd05af449ebf9cb1827648298291afce18d714` (tag `v1.14.0`).

They implement appending extra sections (cmdline, os-release, initrd, ...)
to a PE binary (systemd-boot's sd-stub) to assemble a Unified Kernel
Image, without shelling out to `ukify`/`objcopy`. That package lives under
Talos's `internal/`, so Go's internal-import rule prevents importing it
directly from this module; it's copied here instead, per the terms of its
license.

Licensed under the Mozilla Public License, v. 2.0. A copy of the MPL 2.0
is available at <http://mozilla.org/MPL/2.0/>. Per the MPL, these two
files remain under MPL-2.0 regardless of the rest of this repository's
license, and their source is available in this repository.
