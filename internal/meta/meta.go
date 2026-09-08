// Package meta initializes invarios's on-disk metadata block on the META
// partition: a two-copy, checksummed ADV (Auxiliary Data Vector)
// structure, written via github.com/siderolabs/go-adv/adv/talos, so any
// later code that needs to read or write tags (e.g. an Upgrade tag for
// A/B upgrades) can reuse that package directly against META unchanged.
package meta

import (
	"fmt"
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
func Init(metaPartition string) error {
	adv, err := talos.NewADV(nil)
	if err != nil {
		return fmt.Errorf("meta: creating empty ADV: %w", err)
	}

	buf, err := adv.Bytes()
	if err != nil {
		return fmt.Errorf("meta: marshaling empty ADV: %w", err)
	}

	f, err := os.OpenFile(metaPartition, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("meta: opening %s: %w", metaPartition, err)
	}
	defer f.Close() //nolint:errcheck

	if _, err := f.WriteAt(buf, 0); err != nil {
		return fmt.Errorf("meta: writing ADV to %s: %w", metaPartition, err)
	}

	return f.Sync()
}
