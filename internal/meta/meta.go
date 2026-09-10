// Package meta owns invarios's on-disk metadata block on the META
// partition: a two-copy, checksummed ADV (Auxiliary Data Vector)
// structure, written via github.com/siderolabs/go-adv/adv/talos, so any
// later code that needs to read or write tags (e.g. an Upgrade tag for
// A/B upgrades) can reuse that package directly against META unchanged.
//
// A valid ADV on META also doubles as the "install complete" marker
// (see IsInitialized and internal/disk.IsInstalled): Install writes it
// as its very last step, after every other partition and EFI variable
// is in place, so its presence means the whole sequence finished.
package meta

import (
	"fmt"
	"io"
	"os"

	"github.com/siderolabs/go-adv/adv/talos"
)

// Init writes an empty, checksummed two-copy ADV block (talos.Size, 512
// KiB total) to metaPartition, the raw META partition device path (e.g.
// "/dev/vda2"). Unlike ESP/STATE/DATA, META is never formatted with a
// filesystem -- the ADV structure is its entire contents, written
// directly to the partition's raw bytes.
//
// No tags are set. Reading and writing an Upgrade tag to drive an A/B
// upgrade commit protocol is future work this package doesn't implement
// yet.
//
// Because IsInitialized treats a valid ADV as proof that Install
// finished, Init must be the last thing Install writes: calling it any
// earlier would mark a machine installed while the ESP, the XFS
// volumes, or the firmware's boot entry could still be missing.
func Init(metaPartition string) error {
	adv, err := talos.NewADV(nil)
	if err != nil {
		return fmt.Errorf("meta: creating empty ADV: %w", err)
	}

	buf, err := adv.Bytes()
	if err != nil {
		return fmt.Errorf("meta: marshaling empty ADV: %w", err)
	}

	return writeAt0(metaPartition, buf)
}

// Wipe zeroes the ADV region of metaPartition (both copies) and syncs,
// so IsInitialized reports false until the next Init. Install calls it
// right after partitioning, before anything else is written: a repeat
// install lays META out at exactly the same LBA as the previous one,
// so without this the *previous* install's still-valid ADV would keep
// reporting "installed" through the whole re-install -- and a failure
// partway through would leave a disk that boots instead of re-entering
// Install, the exact outcome the marker exists to prevent.
func Wipe(metaPartition string) error {
	return writeAt0(metaPartition, make([]byte, talos.Size))
}

// IsInitialized reports whether metaPartition holds a valid ADV, i.e.
// whether Init has run against it and nothing has wiped it since. Only
// a failure to open or read the device is an error; a short read or a
// block that fails go-adv's magic/checksum validation is simply "not
// initialized" -- that's what a blank, wiped, or half-written META
// looks like, and it's the case callers exist to detect.
func IsInitialized(metaPartition string) (bool, error) {
	f, err := os.Open(metaPartition)
	if err != nil {
		return false, fmt.Errorf("meta: opening %s: %w", metaPartition, err)
	}
	defer f.Close() //nolint:errcheck

	// talos.NewADV validates the first copy and falls back to the
	// second, and reads exactly talos.Size bytes in the process. Wrap
	// the file in a LimitReader so a device shorter than that surfaces
	// as an ordinary short read (-> false) rather than anything odd.
	// go-adv returns an error for a short read, bad magic, or a
	// checksum mismatch alike; every one of those means "not a usable
	// ADV", which is the expected negative answer, not a failure. A
	// real I/O error on the device would resurface the moment Boot
	// tries to mount STATE from the same disk, so it's not worth
	// telling apart here.
	if _, err := talos.NewADV(io.LimitReader(f, talos.Size)); err != nil {
		return false, nil //nolint:nilerr // not-an-ADV is the expected negative case, see above
	}

	return true, nil
}

// writeAt0 writes buf at offset 0 of the raw device at path and fsyncs
// it, so the write is on the platter (or flash) before the caller
// moves on to a step that assumes it -- Init is immediately followed
// by reboot(2), and Wipe by writes to other partitions.
func writeAt0(path string, buf []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("meta: opening %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	if _, err := f.WriteAt(buf, 0); err != nil {
		return fmt.Errorf("meta: writing %s: %w", path, err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("meta: syncing %s: %w", path, err)
	}

	return nil
}
