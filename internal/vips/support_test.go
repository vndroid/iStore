package vips

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"

	"github.com/kane/istore/internal/imagedata"
	"github.com/kane/istore/internal/imagetype"
)

// These tests need a live libvips, which is the point of them: everything here
// is a claim about the library this binary was linked against, and none of it
// can be checked without asking it.

func TestMain(m *testing.M) {
	c := NewDefaultConfig()
	if err := Init(&c); err != nil {
		// Nothing in this file can run, and pretending otherwise would report a
		// green suite for an untested build.
		panic(err)
	}
	code := m.Run()
	Shutdown()
	os.Exit(code)
}

// ------------------------------------------------------------------ save probe

func TestSupportsSaveIsNotJustTheOperationTable(t *testing.T) {
	// Whatever this build has, the two answers must agree in one direction: a
	// format cannot be saveable without its saver. The other direction is the
	// whole reason SupportsSave probes — heifsave_target exists on builds that
	// cannot encode AVIF or HEIC — so it is deliberately not asserted.
	for _, it := range saveableTypes {
		if SupportsSave(it) && !hasSaveOperation(it) {
			t.Errorf("%s: SupportsSave is true with no saver operation", it)
		}
	}
}

func TestSupportsSaveRejectsTypesWithNoSaver(t *testing.T) {
	for _, it := range []imagetype.Type{imagetype.Unknown, imagetype.SVG} {
		if SupportsSave(it) {
			t.Errorf("%s: SupportsSave is true, but Image.Save has no branch for it", it)
		}
	}
}

// The probe encodes 32x32. If that size were unrepresentative — an encoder that
// accepts a tiny image and fails a real one, or the reverse — the probe would be
// worse than the operation-table check it replaced. So: everything it says yes
// to must survive a larger encode, and everything it says no to must still fail.
func TestProbeAgreesWithRealEncodes(t *testing.T) {
	for _, it := range saveableTypes {
		if !hasSaveOperation(it) {
			continue
		}

		// A size no encoder can mistake for a special case.
		img, err := testImage(256, 192)
		if err != nil {
			t.Fatal(err)
		}

		d, saveErr := img.Save(it, 75, SaveOverrides{})
		if saveErr == nil {
			d.Close()
		}
		img.Clear()

		if got, want := SupportsSave(it), saveErr == nil; got != want {
			t.Errorf("%s: SupportsSave = %v, but a 256x192 encode %s",
				it, got, encodeOutcome(saveErr))
		}
	}
}

func encodeOutcome(err error) string {
	if err == nil {
		return "succeeded"
	}
	return "failed: " + err.Error()
}

func testImage(w, h int) (*Image, error) {
	img, err := newProbeImage()
	if err != nil {
		return nil, err
	}
	if err := img.Embed(w, h, 0, 0); err != nil {
		img.Clear()
		return nil, err
	}
	return img, nil
}

func TestSaveableTypesMatchesSupportsSave(t *testing.T) {
	in := make(map[imagetype.Type]bool)
	for _, it := range SaveableTypes() {
		in[it] = true
	}

	for _, it := range saveableTypes {
		if in[it] != SupportsSave(it) {
			t.Errorf("%s: in SaveableTypes = %v, SupportsSave = %v", it, in[it], SupportsSave(it))
		}
	}
}

// ---------------------------------------------------------------- tiff loading

// minimalTIFF is a 2x2 8-bit greyscale uncompressed TIFF, written by hand so the
// test needs no encoder.
func minimalTIFF() []byte {
	const (
		entries    = 9
		ifdOffset  = 8
		entriesLen = 2 + entries*12 + 4
		pixOffset  = ifdOffset + entriesLen
	)

	var b bytes.Buffer
	b.WriteString("II")
	binary.Write(&b, binary.LittleEndian, uint16(42))
	binary.Write(&b, binary.LittleEndian, uint32(ifdOffset))

	binary.Write(&b, binary.LittleEndian, uint16(entries))
	entry := func(tag, typ uint16, count, val uint32) {
		binary.Write(&b, binary.LittleEndian, tag)
		binary.Write(&b, binary.LittleEndian, typ)
		binary.Write(&b, binary.LittleEndian, count)
		binary.Write(&b, binary.LittleEndian, val)
	}

	const (
		short = 3
		long  = 4
	)

	entry(256, short, 1, 2)        // ImageWidth
	entry(257, short, 1, 2)        // ImageLength
	entry(258, short, 1, 8)        // BitsPerSample
	entry(259, short, 1, 1)        // Compression: none
	entry(262, short, 1, 1)        // PhotometricInterpretation: black is zero
	entry(273, long, 1, pixOffset) // StripOffsets
	entry(277, short, 1, 1)        // SamplesPerPixel
	entry(278, short, 1, 2)        // RowsPerStrip
	entry(279, long, 1, 4)         // StripByteCounts

	binary.Write(&b, binary.LittleEndian, uint32(0)) // no next IFD
	b.Write([]byte{0x00, 0x40, 0x80, 0xFF})          // the four pixels

	return b.Bytes()
}

// TestTIFFLoads is the regression test for the `unlimited` trap.
//
// tiffload gained that property in libvips 8.17, and only when built against
// libtiff 4.7+. Passing it to a loader that does not have it is not ignored —
// the load fails with "no property named `unlimited'" — so before this was
// guarded, every TIFF request on an older libvips returned 422, info and
// transforms alike. The failure is invisible on a new enough build, which is
// exactly why it needs a test rather than a reading.
func TestTIFFLoads(t *testing.T) {
	if !SupportsLoad(imagetype.TIFF) {
		t.Skip("this build has no tiffload_source")
	}

	b := minimalTIFF()

	data, err := imagedata.NewFromBytes(b)
	if err != nil {
		t.Fatalf("the fixture is not detected as an image: %v", err)
	}
	defer data.Close()

	if data.Format() != imagetype.TIFF {
		t.Fatalf("fixture detected as %s, want tiff", data.Format())
	}

	img := new(Image)
	defer img.Clear()

	if err := img.Load(data, 1.0, 0, 1); err != nil {
		t.Fatalf("loading a 2x2 TIFF failed: %v", err)
	}
	if img.Width() != 2 || img.Height() != 2 {
		t.Errorf("got %dx%d, want 2x2", img.Width(), img.Height())
	}
}

// operationHasArgument is the mechanism the TIFF guard rests on, so it gets its
// own test: a wrong answer here silently reintroduces the bug on one side or
// disables `unlimited` for everyone on the other.
func TestOperationHasArgument(t *testing.T) {
	if !SupportsLoad(imagetype.TIFF) {
		t.Skip("this build has no tiffload_source")
	}

	// `page` has been on tiffload_source since long before this project's
	// libvips floor, so its absence would mean the lookup itself is broken.
	if !operationHasArgument("tiffload_source", "page") {
		t.Error("operationHasArgument says tiffload_source has no `page`; the lookup is not working")
	}
	if operationHasArgument("tiffload_source", "no-such-argument") {
		t.Error("operationHasArgument invented an argument")
	}
	if operationHasArgument("no_such_operation", "page") {
		t.Error("operationHasArgument answered for an operation that does not exist")
	}

	// Whichever way this build answers, the two sides have to agree — the Go
	// wrapper and the flag the C loader branches on are read from the same place.
	if got := tiffSupportsUnlimited(); got != operationHasArgument("tiffload_source", "unlimited") {
		t.Errorf("tiffSupportsUnlimited() = %v, disagrees with a direct lookup", got)
	}

	t.Logf("libvips build: tiffload_source has `unlimited` = %v", tiffSupportsUnlimited())
}
