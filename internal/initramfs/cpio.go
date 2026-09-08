// Package initramfs writes a Linux kernel initramfs: a gzip-compressed
// "newc" format cpio archive, as documented in the kernel source at
// Documentation/driver-api/early-userspace/buffer-format.rst.
package initramfs

import (
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// newc entry types, matching the st_mode file-type bits cpio's newc
// format expects (the low 9 bits are the usual permission bits).
const (
	typeDirectory = 0o040000
	typeRegular   = 0o100000
	typeSymlink   = 0o120000
)

const (
	newcMagic   = "070701"
	trailerName = "TRAILER!!!"
)

// WriteCPIO walks rootDir and writes its contents as a gzip-compressed
// newc cpio archive to outPath. Entries are written in sorted path order
// and with a zeroed mtime, so the same input tree always produces a
// byte-identical archive.
func WriteCPIO(rootDir, outPath string) (err error) {
	out, createErr := os.Create(outPath)
	if createErr != nil {
		return fmt.Errorf("creating %s: %w", outPath, createErr)
	}

	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing %s: %w", outPath, cerr)
		}
	}()

	gz, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("creating gzip writer: %w", err)
	}

	var paths []string

	if err = filepath.WalkDir(rootDir, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if path != rootDir {
			paths = append(paths, path)
		}

		return nil
	}); err != nil {
		return fmt.Errorf("walking %s: %w", rootDir, err)
	}

	// Sort so archive contents (and thus the resulting bytes) don't
	// depend on directory-read order.
	sort.Strings(paths)

	var ino uint32

	for _, path := range paths {
		ino++

		name, err := filepath.Rel(rootDir, path)
		if err != nil {
			return fmt.Errorf("computing relative path for %s: %w", path, err)
		}

		if err := writeEntry(gz, path, filepath.ToSlash(name), ino); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}

	if err := writeTrailer(gz); err != nil {
		return err
	}

	return gz.Close()
}

func writeEntry(w io.Writer, path, name string, ino uint32) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}

	var (
		fileType    uint32
		fileSize    uint32
		data        []byte
		sourceFile  *os.File
		closeSource = func() error { return nil }
	)

	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("reading symlink: %w", err)
		}

		fileType = typeSymlink
		data = []byte(target)
		fileSize = uint32(len(data))

	case info.IsDir():
		fileType = typeDirectory

	default:
		fileType = typeRegular
		fileSize = uint32(info.Size())

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("opening file: %w", err)
		}

		sourceFile = f
		closeSource = f.Close
	}

	defer func() { _ = closeSource() }()

	mode := fileType | uint32(info.Mode().Perm())

	if err := writeHeader(w, header{
		ino:      ino,
		mode:     mode,
		nlink:    1,
		fileSize: fileSize,
		nameSize: uint32(len(name) + 1), // cpio counts the trailing NUL
	}, name); err != nil {
		return err
	}

	switch {
	case sourceFile != nil:
		n, err := io.Copy(w, sourceFile)
		if err != nil {
			return fmt.Errorf("writing file data: %w", err)
		}

		if uint32(n) != fileSize {
			return fmt.Errorf("file size changed while archiving (expected %d, wrote %d)", fileSize, n)
		}
	case len(data) > 0:
		if _, err := w.Write(data); err != nil {
			return fmt.Errorf("writing symlink target: %w", err)
		}
	}

	return writePadding(w, fileSize)
}

func writeTrailer(w io.Writer) error {
	if err := writeHeader(w, header{nameSize: uint32(len(trailerName) + 1)}, trailerName); err != nil {
		return err
	}

	return writePadding(w, 0)
}

// header holds the fields of a single newc entry header. Fields left at
// their zero value (uid, gid, mtime, dev*) are fine for an initramfs:
// nothing here needs a real owner, timestamp, or backing device.
type header struct {
	ino      uint32
	mode     uint32
	fileSize uint32
	nlink    uint32
	nameSize uint32
}

// writeHeader writes the 110-byte newc header followed by name (with its
// trailing NUL) and padding out to a 4-byte boundary. Entries always
// start 4-byte aligned in the archive (this padding, plus the data
// padding in writePadding, keep it that way), so aligning relative to
// the start of the header is equivalent to aligning the whole stream.
func writeHeader(w io.Writer, h header, name string) error {
	fields := []uint32{
		h.ino, h.mode, 0 /* uid */, 0 /* gid */, h.nlink,
		0, /* mtime */
		h.fileSize,
		0, 0, /* devmajor, devminor */
		0, 0, /* rdevmajor, rdevminor */
		h.nameSize,
		0, /* check */
	}

	var sb strings.Builder

	sb.WriteString(newcMagic)

	for _, f := range fields {
		fmt.Fprintf(&sb, "%08X", f)
	}

	if _, err := io.WriteString(w, sb.String()); err != nil {
		return err
	}

	if _, err := io.WriteString(w, name+"\x00"); err != nil {
		return err
	}

	return writePadding(w, uint32(sb.Len())+h.nameSize)
}

var zeros [4]byte

// writePadding pads out n bytes already written to the next 4-byte
// boundary.
func writePadding(w io.Writer, n uint32) error {
	if pad := (4 - n%4) % 4; pad > 0 {
		_, err := w.Write(zeros[:pad])

		return err
	}

	return nil
}
