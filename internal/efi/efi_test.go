package efi

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/google/uuid"
)

// TestLoadOptionHeader_RoundTrip decodes the header of an entry
// buildLoadOption produced, which is exactly what findByLabel does on a
// re-install to recognize its own entry.
func TestLoadOptionHeader_RoundTrip(t *testing.T) {
	raw := buildLoadOption("invarios", 1, uuid.New(), 2048, 657407, `\EFI\BOOT\BOOTX64.EFI`)

	var hdr loadOptionHeader
	if err := hdr.Unmarshal(bytes.NewBuffer(raw)); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if hdr.Description != "invarios" {
		t.Fatalf("Description = %q, want %q", hdr.Description, "invarios")
	}

	if hdr.Attributes != loadOptionActive {
		t.Fatalf("Attributes = %#x, want %#x", hdr.Attributes, loadOptionActive)
	}

	// The device path list that follows the description is
	// FilePathListLength bytes; the total must add up exactly.
	descLen := 2 * (len("invarios") + 1)
	if want := 4 + 2 + descLen + int(hdr.FilePathListLength); len(raw) != want {
		t.Fatalf("len(raw) = %d, want %d", len(raw), want)
	}
}

// TestLoadOptionHeader_Malformed is the point of having a local decoder:
// every one of these inputs makes go-uefi's parser exit the process or
// panic, and PID 1 exiting is a kernel panic.
func TestLoadOptionHeader_Malformed(t *testing.T) {
	descr := func(s string) []byte {
		var b bytes.Buffer
		_ = binary.Write(&b, binary.LittleEndian, uint32(1))
		_ = binary.Write(&b, binary.LittleEndian, uint16(0))
		b.Write(utf16z(s).Bytes())

		return b.Bytes()
	}

	cases := map[string][]byte{
		"empty":                 {},
		"attributes only":       {1, 0, 0, 0},
		"header only":           {1, 0, 0, 0, 0, 0},
		"no NUL terminator":     descr("abc")[:6+4],
		"odd description bytes": descr("abc")[:6+3],
	}

	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var hdr loadOptionHeader
			if err := hdr.Unmarshal(bytes.NewBuffer(in)); err == nil {
				t.Fatalf("Unmarshal(%v) = nil error, want error", in)
			}
		})
	}
}

// TestLoadOptionHeader_IgnoresTrailingBytes: a real entry carries a
// device path (and possibly OptionalData) after the description; the
// header decoder must stop at the NUL and not care what follows, since
// that's the part go-uefi's parser chokes on.
func TestLoadOptionHeader_IgnoresTrailingBytes(t *testing.T) {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, uint32(1))
	_ = binary.Write(&b, binary.LittleEndian, uint16(4))
	b.Write(utf16z("Windows Boot Manager").Bytes())
	b.Write([]byte{0x02, 0x02, 0xff, 0xff, 0x00}) // garbage "device path"

	var hdr loadOptionHeader
	if err := hdr.Unmarshal(&b); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if hdr.Description != "Windows Boot Manager" {
		t.Fatalf("Description = %q", hdr.Description)
	}
}

func TestBootOrder_Unmarshal(t *testing.T) {
	var o bootOrder
	if err := o.Unmarshal(bytes.NewBuffer([]byte{0x03, 0x00, 0x01, 0x00, 0x10, 0x20})); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	want := []uint16{0x0003, 0x0001, 0x2010}
	if len(o) != len(want) {
		t.Fatalf("got %v, want %v", o, want)
	}

	for i := range want {
		if o[i] != want[i] {
			t.Fatalf("got %v, want %v", o, want)
		}
	}

	if err := o.Unmarshal(bytes.NewBuffer([]byte{0x03, 0x00, 0x01})); err == nil {
		t.Fatal("odd-length BootOrder: got nil error, want error")
	}
}

func TestBootEntryPath(t *testing.T) {
	want := "/sys/firmware/efi/efivars/Boot0003-8be4df61-93ca-11d2-aa0d-00e098032b8c"
	if got := bootEntryPath(3); got != want {
		t.Fatalf("bootEntryPath(3) = %q, want %q", got, want)
	}
}
