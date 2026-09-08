// Copied from siderolabs/talos (https://github.com/siderolabs/talos),
// internal/pkg/uki/internal/pe/pe.go, at commit
// 9abd05af449ebf9cb1827648298291afce18d714 (tag v1.14.0). See
// /THIRD_PARTY_NOTICES.md for details.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package pe handles appending sections to PE files.
package pe

// Section is a UKI file section.
type Section struct {
	// Section name.
	Name string
	// Path to the contents of the section.
	Path string
	// Should the section be measured to the TPM?
	Measure bool
	// Should the section be appended, or is it already in the PE file.
	Append bool
	// Virtual virtualSize & VMA of the section.
	virtualSize    uint64
	virtualAddress uint64
}
