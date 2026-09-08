// Package efi reads and writes the EFI NVRAM variables invarios needs
// for install and boot: the boot loader interface variables systemd-boot
// sets/reads under its own vendor GUID (LoaderDevicePartUUID,
// LoaderEntrySelected, LoaderEntryDefault -- see
// https://systemd.io/BOOT_LOADER_INTERFACE/), and the firmware's own
// Boot####/BootOrder NVRAM entries under the EFI Global GUID.
//
// It wraps github.com/foxboron/go-uefi, but not that module's top-level
// "efi" package: that package only names accessors for Secure Boot state
// and the EFI Global GUID's BootOrder/Boot#### variables, with no
// accessors at all for the systemd-boot vendor GUID. Both its top-level
// package and its lower-level efivarfs/device packages are also
// read-oriented for boot entries: device.EFILoadOption and the device
// path types it parses have Unmarshal methods but no matching Marshal,
// and BootOrder has no exported writer either. This package reads
// LoaderDevicePartUUID/LoaderEntrySelected and writes LoaderEntryDefault
// through the lower-level efivarfs/attributes packages directly, and
// hand-encodes EFI_LOAD_OPTION (UEFI spec section 3.1.3) and the
// BootOrder array for the two things go-uefi itself cannot write,
// matching byte-for-byte the layout go-uefi's own device package
// already parses on read (verified against efi/device/device.go and
// efi/device/media_device.go in the module source).
package efi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/foxboron/go-uefi/efi/device"
	"github.com/foxboron/go-uefi/efi/util"
	"github.com/foxboron/go-uefi/efivar"
	"github.com/foxboron/go-uefi/efivarfs"
	"github.com/google/uuid"
)

// loadOptionActive is LOAD_OPTION_ACTIVE from UEFI spec section 3.1.3:
// set on an EFI_LOAD_OPTION's Attributes field, it makes the firmware
// consider the entry a candidate during normal boot (as opposed to only
// via an explicit BootNext).
const loadOptionActive uint32 = 0x00000001

// gptPartitionFormat and gptSignatureType are the Hard Drive Media
// Device Path field values (UEFI spec 10.3.5.1) for a GPT-partitioned
// disk identified by its partition GUID, as opposed to an MBR disk
// identified by a 32-bit signature.
const (
	gptPartitionFormat uint8 = 0x02
	gptSignatureType   uint8 = 0x02
)

// varsOnce holds the package's single efivarfs handle. It is opened
// lazily (rather than at package init) so importing this package never
// touches the filesystem, only actually calling one of its functions
// does -- e.g. `go build`/`go vet` and unit tests that don't call this
// package still work outside of an environment with efivarfs mounted.
var vars = efivarfs.NewFS().Open()

// utf16z is a NUL-terminated UTF-16LE string, the wire format most
// boot-loader-interface variables use. efivar.Efistring already decodes
// this shape but only implements Unmarshallable; utf16z adds the
// Marshallable side, needed to write LoaderEntryDefault.
type utf16z string

func (s utf16z) Marshal(b *bytes.Buffer) { b.Write(util.MarshalUtf16Var(string(s))) }
func (s utf16z) Bytes() []byte           { var b bytes.Buffer; s.Marshal(&b); return b.Bytes() }

// rawBytes is a Marshallable that writes itself out verbatim. It backs
// the two variables (Boot#### and BootOrder) this package hand-encodes
// as raw binary rather than through one of go-uefi's own Marshallable
// types.
type rawBytes []byte

func (b rawBytes) Marshal(buf *bytes.Buffer) { buf.Write(b) }
func (b rawBytes) Bytes() []byte             { return b }

// BootedEntry reads LoaderDevicePartUUID and LoaderEntrySelected, both
// set by systemd-boot immediately before it execs the chosen UKI. It
// identifies which ESP (by GPT partition UUID) and which UKI filename
// (the boot loader entry identifier, e.g. "invarios.efi") the running
// system was booted from, so Install knows what to read and copy onto
// the freshly-partitioned target disk.
func BootedEntry() (partUUID, entryFilename string, err error) {
	var uuidVar, entryVar efivar.Efistring

	if err := vars.GetVar(efivar.LoaderDevicePartUUID, &uuidVar); err != nil {
		return "", "", fmt.Errorf("efi: read LoaderDevicePartUUID: %w", err)
	}

	if err := vars.GetVar(efivar.LoaderEntrySelected, &entryVar); err != nil {
		return "", "", fmt.Errorf("efi: read LoaderEntrySelected: %w", err)
	}

	return string(uuidVar), string(entryVar), nil
}

// SetDefault writes LoaderEntryDefault so systemd-boot boots
// entryFilename by default on every subsequent boot, without an operator
// having to pick it from the menu, per the boot loader interface spec.
func SetDefault(entryFilename string) error {
	if err := vars.WriteVar(efivar.LoaderEntryDefault, utf16z(entryFilename)); err != nil {
		return fmt.Errorf("efi: write LoaderEntryDefault: %w", err)
	}

	return nil
}

// swapGUIDEndian converts a GUID between the big-endian byte order
// github.com/google/uuid uses (matching go-blockdevice's own
// gpt.Partition.PartGUID) and the mixed-endian byte order UEFI uses
// on-disk for both GPT partition entries and the Hard Drive Media
// Device Path's PartitionSignature field: the first three fields
// byte-swapped, the last eight bytes untouched. The swap is its own
// inverse, so the same function converts in either direction. This
// mirrors go-blockdevice/v2's internal/gptutil.UUIDToGUID/GUIDToUUID,
// which is unexported and so can't be imported directly.
func swapGUIDEndian(g [16]byte) [16]byte {
	return [16]byte{
		g[3], g[2], g[1], g[0],
		g[5], g[4],
		g[7], g[6],
		g[8], g[9], g[10], g[11], g[12], g[13], g[14], g[15],
	}
}

// buildLoadOption hand-encodes an EFI_LOAD_OPTION (UEFI spec 3.1.3)
// named label, pointing at loaderPath (a "\"-separated path relative to
// the ESP root, e.g. `\EFI\BOOT\BOOTX64.EFI`) on the GPT partition
// identified by partNumber (1-indexed, matching
// gpt.Table.AllocatePartition's return value)/partGUID/firstLBA/lastLBA.
//
// go-uefi's device package can already *parse* exactly this structure
// (see ParseEFILoadOption and ParseMediaDevicePath's HardDriveDevicePath
// and FilePathDevicePath cases in efi/device), but has no corresponding
// encoder, so this mirrors that parser's field layout and ordering
// byte for byte: a Hard Drive Media Device Path node identifying the
// ESP by GPT partition GUID, followed by a File Path Media Device Path
// node naming loaderPath, followed by the End Entire Device Path node
// that terminates every EFI device path.
func buildLoadOption(label string, partNumber uint32, partGUID uuid.UUID, firstLBA, lastLBA uint64, loaderPath string) []byte {
	var devicePath bytes.Buffer

	sig := swapGUIDEndian(partGUID)

	// Hard Drive Media Device Path (UEFI spec 10.3.5.1): 4-byte node
	// header, then PartitionNumber(4) + PartitionStart(8) +
	// PartitionSize(8) + PartitionSignature(16) + PartitionFormat(1) +
	// SignatureType(1) = 38 bytes, 42 total.
	const hdNodeLen = 4 + 38

	_ = binary.Write(&devicePath, binary.LittleEndian, uint8(device.MediaDevicePath))
	_ = binary.Write(&devicePath, binary.LittleEndian, uint8(device.HardDriveDevicePath))
	_ = binary.Write(&devicePath, binary.LittleEndian, uint16(hdNodeLen))
	_ = binary.Write(&devicePath, binary.LittleEndian, partNumber)
	_ = binary.Write(&devicePath, binary.LittleEndian, firstLBA)
	_ = binary.Write(&devicePath, binary.LittleEndian, lastLBA-firstLBA+1)
	devicePath.Write(sig[:])
	devicePath.WriteByte(gptPartitionFormat)
	devicePath.WriteByte(gptSignatureType)

	// File Path Media Device Path (UEFI spec 10.3.5.4): 4-byte node
	// header, then the NUL-terminated UTF-16LE path.
	pathBytes := util.MarshalUtf16Var(loaderPath)
	fpNodeLen := 4 + len(pathBytes)

	_ = binary.Write(&devicePath, binary.LittleEndian, uint8(device.MediaDevicePath))
	_ = binary.Write(&devicePath, binary.LittleEndian, uint8(device.FilePathDevicePath))
	_ = binary.Write(&devicePath, binary.LittleEndian, uint16(fpNodeLen))
	devicePath.Write(pathBytes)

	// End Entire Device Path (UEFI spec 10.3.1): a bare 4-byte header,
	// no node-specific data.
	_ = binary.Write(&devicePath, binary.LittleEndian, uint8(device.EndOfHardwareDevicePath))
	_ = binary.Write(&devicePath, binary.LittleEndian, uint8(device.NoNewDevicePath))
	_ = binary.Write(&devicePath, binary.LittleEndian, uint16(4))

	var loadOption bytes.Buffer

	_ = binary.Write(&loadOption, binary.LittleEndian, loadOptionActive)
	_ = binary.Write(&loadOption, binary.LittleEndian, uint16(devicePath.Len()))
	loadOption.Write(util.MarshalUtf16Var(label))
	loadOption.Write(devicePath.Bytes())
	// OptionalData: none.

	return loadOption.Bytes()
}

// parseBootNum extracts the numeric part of a "Boot####" name, as
// returned by (*efivarfs.Efivarfs).GetBootOrder.
func parseBootNum(name string) (uint16, error) {
	num, ok := strings.CutPrefix(name, "Boot")
	if !ok || len(num) != 4 {
		return 0, fmt.Errorf("efi: not a Boot#### name: %q", name)
	}

	n, err := strconv.ParseUint(num, 16, 16)

	return uint16(n), err
}

// findByLabel returns the Boot#### number already in order whose stored
// EFI_LOAD_OPTION description matches label, if any. Reusing that number
// on a re-install (rather than always allocating a new one) keeps NVRAM
// from accumulating a duplicate entry every time Install runs.
func findByLabel(order []string, label string) (uint16, bool) {
	for _, name := range order {
		num, err := parseBootNum(name)
		if err != nil {
			continue
		}

		entryVar := efivar.BootEntry
		entryVar.Name = name

		var loadOption device.EFILoadOption
		if err := vars.GetVar(entryVar, &loadOption); err != nil {
			continue
		}

		if loadOption.Description == label {
			return num, true
		}
	}

	return 0, false
}

// firstFreeNum returns the lowest Boot#### number that is neither in
// order nor already present in NVRAM (probed directly, since an entry
// can exist without being listed in BootOrder).
func firstFreeNum(order []string) (uint16, bool) {
	inOrder := make(map[uint16]bool, len(order))

	for _, name := range order {
		if n, err := parseBootNum(name); err == nil {
			inOrder[n] = true
		}
	}

	for n := uint16(0); n < 0xffff; n++ {
		if inOrder[n] {
			continue
		}

		entryVar := efivar.BootEntry
		entryVar.Name = fmt.Sprintf("Boot%04X", n)

		var loadOption device.EFILoadOption
		if err := vars.GetVar(entryVar, &loadOption); err != nil {
			// Unreadable (almost always "does not exist") -- treat the
			// slot as free.
			return n, true
		}
	}

	return 0, false
}

// setBootOrder writes BootOrder as the little-endian uint16 array the
// UEFI spec defines it as (each element a Boot#### number).
func setBootOrder(nums []uint16) error {
	var buf bytes.Buffer

	for _, n := range nums {
		_ = binary.Write(&buf, binary.LittleEndian, n)
	}

	if err := vars.WriteVar(efivar.BootOrder, rawBytes(buf.Bytes())); err != nil {
		return fmt.Errorf("efi: write BootOrder: %w", err)
	}

	return nil
}

// EnsureBootEntry creates or refreshes a Boot#### NVRAM entry named
// label, pointing at loaderPath on the GPT partition described by
// partNumber/partGUID/firstLBA/lastLBA, and makes sure it is first in
// BootOrder so the firmware boots it by default. Re-running this (e.g.
// across reinstalls) reuses the existing Boot#### number for label
// instead of growing NVRAM with duplicate entries.
func EnsureBootEntry(label string, partNumber uint32, partGUID uuid.UUID, firstLBA, lastLBA uint64, loaderPath string) error {
	order := vars.GetBootOrder()

	num, ok := findByLabel(order, label)
	if !ok {
		num, ok = firstFreeNum(order)
		if !ok {
			return errors.New("efi: no free Boot#### slot")
		}
	}

	entryVar := efivar.BootEntry
	entryVar.Name = fmt.Sprintf("Boot%04X", num)

	loadOption := buildLoadOption(label, partNumber, partGUID, firstLBA, lastLBA, loaderPath)
	if err := vars.WriteVar(entryVar, rawBytes(loadOption)); err != nil {
		return fmt.Errorf("efi: write %s: %w", entryVar.Name, err)
	}

	newOrder := make([]uint16, 0, len(order)+1)
	newOrder = append(newOrder, num)

	for _, name := range order {
		if name == entryVar.Name {
			continue
		}

		if n, err := parseBootNum(name); err == nil {
			newOrder = append(newOrder, n)
		}
	}

	return setBootOrder(newOrder)
}
