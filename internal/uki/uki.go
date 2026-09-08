// Package uki assembles a Unified Kernel Image: a single EFI binary
// combining systemd's sd-stub, a Linux kernel, an initramfs, a kernel
// command line, and an os-release file, bootable directly by UEFI
// firmware or systemd-boot.
//
// Unlike `ukify build` (the tool this replaces), this package doesn't
// yet support SecureBoot signing, PCR measurement, sbat, splash images,
// or multiple boot-menu profiles -- only the four sections below are
// implemented so far. Adding any of those later is a matter of writing
// more sections/logic here and in internal/uki/pe, not a rearchitecture.
package uki

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/invarios/invarios/internal/uki/pe"
)

// Well-known UKI PE section names, per systemd's Boot Loader
// Specification / sd-stub conventions.
const (
	sectionCmdline = ".cmdline"
	sectionOSRel   = ".osrel"
	sectionInitrd  = ".initrd"
	sectionLinux   = ".linux"
)

// Config describes the inputs needed to assemble a UKI.
type Config struct {
	// StubPath is systemd's sd-stub EFI binary (linuxx64.efi.stub) that
	// the sections below get appended to.
	StubPath string
	// KernelPath is the Linux kernel image (vmlinuz/Image).
	KernelPath string
	// InitrdPath is the initramfs.
	InitrdPath string
	// Cmdline is the kernel command line.
	Cmdline string
	// OSRelease is the contents of an os-release file to embed.
	OSRelease []byte

	// OutputPath is where the assembled UKI is written.
	OutputPath string
}

// Build assembles a UKI per cfg.
func Build(cfg Config) error {
	scratchDir, err := os.MkdirTemp("", "invarios-uki-*")
	if err != nil {
		return fmt.Errorf("creating scratch dir: %w", err)
	}

	defer func() { _ = os.RemoveAll(scratchDir) }()

	cmdlinePath := filepath.Join(scratchDir, "cmdline")
	if err := os.WriteFile(cmdlinePath, []byte(cfg.Cmdline), 0o600); err != nil {
		return fmt.Errorf("writing cmdline section: %w", err)
	}

	osReleasePath := filepath.Join(scratchDir, "os-release")
	if err := os.WriteFile(osReleasePath, cfg.OSRelease, 0o600); err != nil {
		return fmt.Errorf("writing os-release section: %w", err)
	}

	sections := []pe.Section{
		{Name: sectionCmdline, Path: cmdlinePath, Append: true},
		{Name: sectionOSRel, Path: osReleasePath, Append: true},
		{Name: sectionInitrd, Path: cfg.InitrdPath, Append: true},
		{Name: sectionLinux, Path: cfg.KernelPath, Append: true},
	}

	if err := pe.AssembleNative(cfg.StubPath, cfg.OutputPath, sections); err != nil {
		return fmt.Errorf("assembling UKI: %w", err)
	}

	return nil
}
