package imageinfo

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/vndroid/istore/internal/imagetype"
)

// jpegWithFatSegments returns a JPEG with n padding segments spliced in after
// SOI, pushing SOF past the header window.
//
// A segment's length field is 16 bits, so one segment cannot quite clear 64 KiB
// on its own — which is exactly why the real version of this file needs no
// exotic explanation. A large EXIF thumbnail is one segment, an ICC profile is
// split across several more, and a camera that writes both has moved SOF past
// the window without doing anything unusual.
func jpegWithFatSegments(t *testing.T, n int) []byte {
	t.Helper()

	const pad = 60000

	src := encodeJPEG(t, testW, testH)

	out := make([]byte, 0, len(src)+n*(pad+4))
	out = append(out, src[:2]...) // SOI

	for range n {
		out = append(out, 0xFF, 0xEF) // APP15
		out = binary.BigEndian.AppendUint16(out, uint16(pad+2))
		out = append(out, make([]byte, pad)...)
	}

	return append(out, src[2:]...)
}

// A header window overrun must come back as ErrHeaderTooShort *with* the Info
// filled in as far as it got. The HTTP layer builds on that half-answer: FileSize
// and Format are already right and only the geometry has to come from libvips.
// Returning nil here is what made a fat JPEG a 422 on both info and resize.
func TestReadReturnsPartialInfoOnHeaderTooShort(t *testing.T) {
	b := jpegWithFatSegments(t, 2)

	info, err := Read(bytes.NewReader(b), int64(len(b)))

	if !errors.Is(err, ErrHeaderTooShort) {
		t.Fatalf("err = %v, want ErrHeaderTooShort", err)
	}
	if info == nil {
		t.Fatal("Info is nil; the caller has nothing to complete")
	}
	if info.Format != imagetype.JPEG {
		t.Errorf("Format = %v, want jpeg — it came from the magic bytes and is not in doubt", info.Format)
	}
	if info.FileSize != int64(len(b)) {
		t.Errorf("FileSize = %d, want %d", info.FileSize, len(b))
	}
	if info.ImageWidth != 0 {
		t.Errorf("ImageWidth = %d, want 0: zero is how the caller knows it must supply the geometry", info.ImageWidth)
	}
}

// The same guarantee for a container with no header walk at all.
func TestReadReturnsPartialInfoOnUnsupportedContainer(t *testing.T) {
	// Minimal AVIF ftyp box.
	b := append([]byte{0, 0, 0, 0x20, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f'}, make([]byte, 64)...)

	info, err := Read(bytes.NewReader(b), int64(len(b)))

	var unsupported ErrUnsupportedContainer
	if !errors.As(err, &unsupported) {
		t.Skipf("this build does not detect the stub as a container: %v", err)
	}
	if info == nil {
		t.Fatal("Info is nil")
	}
	if info.FileSize != int64(len(b)) {
		t.Errorf("FileSize = %d, want %d", info.FileSize, len(b))
	}
	if info.ImageWidth != 0 {
		t.Errorf("ImageWidth = %d, want 0", info.ImageWidth)
	}
}

// A JPEG whose SOF is comfortably inside the window still parses normally; the
// change above must not have turned every JPEG into a fallback.
func TestReadStillAnswersOrdinaryJPEG(t *testing.T) {
	info := readBytes(t, encodeJPEG(t, testW, testH))
	if info.ImageWidth != testW || info.ImageHeight != testH {
		t.Errorf("got %dx%d, want %dx%d", info.ImageWidth, info.ImageHeight, testW, testH)
	}
}

// ---------------------------------------------------------------- ApplyEXIF

// exifBlock builds a little-endian TIFF block holding one IFD0 entry per tag.
// Only SHORT and RATIONAL are needed here, which is what the resolution fields
// use.
func exifBlock(t *testing.T) []byte {
	t.Helper()

	var b bytes.Buffer
	b.WriteString("II")                               // little endian
	binary.Write(&b, binary.LittleEndian, uint16(42)) // magic
	binary.Write(&b, binary.LittleEndian, uint32(8))  // IFD0 offset
	binary.Write(&b, binary.LittleEndian, uint16(3))  // three entries

	// Each entry: tag, type, count, value/offset.
	entry := func(tag, typ uint16, count uint32, val uint32) {
		binary.Write(&b, binary.LittleEndian, tag)
		binary.Write(&b, binary.LittleEndian, typ)
		binary.Write(&b, binary.LittleEndian, count)
		binary.Write(&b, binary.LittleEndian, val)
	}

	// The two rationals live past the IFD: 8 header + 2 count + 3*12 entries +
	// 4 next-IFD = 50.
	const ratOffset = 50

	entry(0x011A, 5, 1, ratOffset)   // XResolution, RATIONAL
	entry(0x011B, 5, 1, ratOffset+8) // YResolution, RATIONAL
	entry(0x0128, 3, 1, 2)           // ResolutionUnit, SHORT, 2 = inch

	binary.Write(&b, binary.LittleEndian, uint32(0)) // no next IFD

	binary.Write(&b, binary.LittleEndian, uint32(72)) // XResolution 72/1
	binary.Write(&b, binary.LittleEndian, uint32(1))
	binary.Write(&b, binary.LittleEndian, uint32(72)) // YResolution 72/1
	binary.Write(&b, binary.LittleEndian, uint32(1))

	return b.Bytes()
}

// ApplyEXIF is the entry point the HTTP layer uses for the containers this
// package cannot walk: libvips hands back the same bare TIFF block, and it has
// to produce the same fields the JPEG path does. In particular the three
// resolution fields, which are in every response — before this they were left at
// their defaults for AVIF, so the answer was not merely thin but wrong.
func TestApplyEXIF(t *testing.T) {
	info := &Info{ResolutionUnit: 1, XResolution: "1/1", YResolution: "1/1"}

	ApplyEXIF(info, exifBlock(t))

	if info.XResolution != "72/1" {
		t.Errorf("XResolution = %q, want \"72/1\"", info.XResolution)
	}
	if info.YResolution != "72/1" {
		t.Errorf("YResolution = %q, want \"72/1\"", info.YResolution)
	}
	if info.ResolutionUnit != 2 {
		t.Errorf("ResolutionUnit = %d, want 2", info.ResolutionUnit)
	}
	if info.Exif == nil {
		t.Error("Exif map is nil; the tags should be merged into the response too")
	}
}

// JPEG's APP1 carries "Exif\0\0" ahead of the TIFF block and readJPEG strips it;
// the blob libvips exposes has been seen both ways. Both spellings must give the
// same answer.
func TestApplyEXIFToleratesExifPrefix(t *testing.T) {
	bare := exifBlock(t)
	prefixed := append([]byte("Exif\x00\x00"), bare...)

	a := &Info{ResolutionUnit: 1, XResolution: "1/1", YResolution: "1/1"}
	c := &Info{ResolutionUnit: 1, XResolution: "1/1", YResolution: "1/1"}

	ApplyEXIF(a, bare)
	ApplyEXIF(c, prefixed)

	if a.XResolution != c.XResolution || a.ResolutionUnit != c.ResolutionUnit {
		t.Errorf("prefixed block parsed differently: bare %s/%d, prefixed %s/%d",
			a.XResolution, a.ResolutionUnit, c.XResolution, c.ResolutionUnit)
	}
	if c.XResolution != "72/1" {
		t.Errorf("XResolution = %q, want \"72/1\"", c.XResolution)
	}
}

// Garbage must leave the Info untouched rather than half-written: the request has
// already been answered by the time EXIF is looked at.
func TestApplyEXIFIgnoresGarbage(t *testing.T) {
	for _, b := range [][]byte{
		nil,
		{},
		[]byte("not a tiff block at all"),
		[]byte("Exif\x00\x00"),
	} {
		info := &Info{ResolutionUnit: 1, XResolution: "1/1", YResolution: "1/1"}

		ApplyEXIF(info, b)

		if info.XResolution != "1/1" || info.ResolutionUnit != 1 || info.Exif != nil {
			t.Errorf("ApplyEXIF(%q) changed the Info: %+v", b, info)
		}
	}
}
