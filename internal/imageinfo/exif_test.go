package imageinfo

import (
	"encoding/binary"
	"testing"
)

// exifEntry is one IFD entry for the builder below. data holds the raw value
// bytes; the builder decides whether they fit in the entry or go out of line.
type exifEntry struct {
	tag  uint16
	typ  uint16
	n    uint32
	data []byte
}

// buildEXIF assembles a little-endian TIFF block: header, IFD0, then any extra
// IFDs appended after it. ifdOffsets is filled with each IFD's byte offset so a
// pointer entry can name one.
func buildEXIF(t *testing.T, ifds ...[]exifEntry) []byte {
	t.Helper()

	le := binary.LittleEndian

	// Header is 8 bytes; IFD0 starts right after.
	out := []byte{'I', 'I', 0x2A, 0x00, 8, 0, 0, 0}

	// Lay the IFDs out back to back, then all out-of-line data after them.
	offsets := make([]int, len(ifds))
	pos := 8
	for i, entries := range ifds {
		offsets[i] = pos
		pos += 2 + len(entries)*12 + 4
	}
	dataAt := pos

	var body, extra []byte
	for _, entries := range ifds {
		ifd := make([]byte, 2)
		le.PutUint16(ifd, uint16(len(entries)))

		for _, e := range entries {
			ent := make([]byte, 12)
			le.PutUint16(ent[0:], e.tag)
			le.PutUint16(ent[2:], e.typ)
			le.PutUint32(ent[4:], e.n)

			if len(e.data) <= 4 {
				copy(ent[8:], e.data)
			} else {
				le.PutUint32(ent[8:], uint32(dataAt+len(extra)))
				extra = append(extra, e.data...)
			}

			ifd = append(ifd, ent...)
		}

		ifd = append(ifd, 0, 0, 0, 0) // no next IFD
		body = append(body, ifd...)
	}

	out = append(out, body...)
	out = append(out, extra...)

	// Hand the caller the offsets by patching nothing — tests that need a
	// pointer compute it the same way, so assert the layout instead.
	if offsets[0] != 8 {
		t.Fatalf("IFD0 should start at 8, got %d", offsets[0])
	}

	return out
}

func le32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

func le16(v uint16) []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, v)
	return b
}

func rational(num, den uint32) []byte { return append(le32(num), le32(den)...) }

func TestParseEXIFValueTypes(t *testing.T) {
	b := buildEXIF(t, []exifEntry{
		{0x010F, 2, 6, []byte("Canon\x00")},                   // ASCII, out of line
		{0x0112, 3, 1, le16(7)},                               // SHORT, inline
		{0x011A, 5, 1, rational(96, 1)},                       // RATIONAL, out of line
		{0x9204, 10, 1, append(le32(^uint32(0)), le32(3)...)}, // SRATIONAL -1/3
		{0xA002, 4, 1, le32(424)},                             // LONG, inline
	})

	got := parseEXIF(b)
	want := map[string]string{
		"Make":              "Canon",
		"Orientation":       "7",
		"XResolution":       "96/1",
		"ExposureBiasValue": "-1/3",
		"PixelXDimension":   "424",
	}

	if len(got) != len(want) {
		t.Fatalf("got %d tags, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestParseEXIFSkipsUndefinedAndUnnamed(t *testing.T) {
	b := buildEXIF(t, []exifEntry{
		{0x9286, 7, 8, []byte("\x00\x00\x00\x00\x00\x00\x00\x00")}, // UserComment, UNDEFINED
		{0x927C, 7, 4, []byte("junk")},                             // MakerNote, UNDEFINED
		{0xDEAD, 3, 1, le16(1)},                                    // no name in the table
		{0x0112, 3, 1, le16(1)},                                    // one real tag so the map is not nil
	})

	got := parseEXIF(b)
	if len(got) != 1 || got["Orientation"] != "1" {
		t.Errorf("UNDEFINED and unnamed tags should be dropped, got %v", got)
	}
}

func TestParseEXIFGPSHasItsOwnNumbering(t *testing.T) {
	// GPS tag numbers restart at zero and mean different things, so they must be
	// resolved against the GPS table and only when reached through the GPSTag
	// pointer. Tag 0x0002 is GPSLatitude there and nothing in IFD0.
	lat := append(append(rational(39, 1), rational(54, 1)...), rational(2668, 100)...)

	ifd0 := []exifEntry{
		{0x0112, 3, 1, le16(1)},
		{0x8825, 4, 1, nil}, // GPSTag, filled in below
	}
	gps := []exifEntry{
		{0x0002, 5, 3, lat},
		{0x0001, 2, 2, []byte("N\x00")},
	}

	ifd0[1].data = le32(uint32(8 + (2 + len(ifd0)*12 + 4)))

	got := parseEXIF(buildEXIF(t, ifd0, gps))

	// Three rationals, rendered as their raw components. OSS renders this one
	// as prose via exiv2; that difference is documented in exif.go.
	if got["GPSLatitude"] != "39/1 54/1 2668/100" {
		t.Errorf("GPSLatitude = %q", got["GPSLatitude"])
	}
	if got["GPSLatitudeRef"] != "N" {
		t.Errorf("GPSLatitudeRef = %q", got["GPSLatitudeRef"])
	}
	// 0x0002 in IFD0's own numbering has no name, so nothing leaked the other
	// way either.
	if len(got) != 4 {
		t.Errorf("want Orientation, GPSTag, GPSLatitude, GPSLatitudeRef; got %v", got)
	}
}

func TestParseEXIFSubIFD(t *testing.T) {
	// A real block: IFD0 carries Orientation and a pointer to an Exif IFD that
	// carries FNumber. The pointer value is the second IFD's offset, which
	// buildEXIF lays out immediately after the first.
	ifd0 := []exifEntry{
		{0x0112, 3, 1, le16(6)},
		{0x8769, 4, 1, nil}, // filled in below
	}
	sub := []exifEntry{
		{0x829D, 5, 1, rational(56, 10)},
	}

	subOffset := 8 + (2 + len(ifd0)*12 + 4)
	ifd0[1].data = le32(uint32(subOffset))

	got := parseEXIF(buildEXIF(t, ifd0, sub))

	if got["Orientation"] != "6" {
		t.Errorf("Orientation = %q, want 6", got["Orientation"])
	}
	// The pointer tag itself is reported too, as OSS reports it.
	if got["ExifTag"] == "" {
		t.Error("the ExifTag pointer should be reported")
	}
	if got["FNumber"] != "56/10" {
		t.Errorf("FNumber = %q, want 56/10 — the sub-IFD was not followed", got["FNumber"])
	}
}

func TestParseEXIFFollowsIFD1(t *testing.T) {
	ifd0 := []exifEntry{{0x0112, 3, 1, le16(1)}}
	ifd1 := []exifEntry{
		{0x0201, 4, 1, le32(1234)},
		{0x0202, 4, 1, le32(5678)},
	}
	b := buildEXIF(t, ifd0, ifd1)

	// IFD1 is laid out immediately after IFD0. Patch IFD0's next-IFD pointer,
	// which buildEXIF otherwise leaves at zero.
	nextAt := 8 + 2 + len(ifd0)*12
	ifd1At := nextAt + 4
	binary.LittleEndian.PutUint32(b[nextAt:], uint32(ifd1At))

	got := parseEXIF(b)
	if got["JPEGInterchangeFormat"] != "1234" {
		t.Errorf("JPEGInterchangeFormat = %q, want 1234", got["JPEGInterchangeFormat"])
	}
	if got["JPEGInterchangeFormatLength"] != "5678" {
		t.Errorf("JPEGInterchangeFormatLength = %q, want 5678", got["JPEGInterchangeFormatLength"])
	}
}

func TestParseEXIFRejectsGarbage(t *testing.T) {
	// None of these may panic or return tags.
	for name, b := range map[string][]byte{
		"empty":        {},
		"short":        {'I', 'I', 0x2A},
		"bad order":    {'X', 'X', 0x2A, 0x00, 8, 0, 0, 0},
		"bad magic":    {'I', 'I', 0x2B, 0x00, 8, 0, 0, 0},
		"ifd past end": {'I', 'I', 0x2A, 0x00, 0xFF, 0xFF, 0, 0},
	} {
		if got := parseEXIF(b); got != nil {
			t.Errorf("%s: expected no tags, got %v", name, got)
		}
	}
}

func TestParseEXIFTruncatedEntry(t *testing.T) {
	// An out-of-line value pointing past the end of the block must be dropped,
	// not read: this is the bounds check that keeps a hostile header from
	// reading adjacent memory.
	b := buildEXIF(t, []exifEntry{
		{0x010F, 2, 64, []byte("Canon\x00")}, // claims 64 bytes, supplies 6
		{0x0112, 3, 1, le16(3)},
	})

	got := parseEXIF(b)
	if _, ok := got["Make"]; ok {
		t.Error("a value claiming more bytes than the block holds must be dropped")
	}
	if got["Orientation"] != "3" {
		t.Error("a bad entry must not abort the rest of the IFD")
	}
}

func TestInfoMarshalOmitsUnknownResolution(t *testing.T) {
	// OSS always returns five basic fields. Resolution is conditional metadata,
	// even though its example image happens to carry 1/1 values.
	i := Info{
		FileSize: 638, Format: 0, FrameCount: 1,
		ImageWidth: 300, ImageHeight: 200,
		ResolutionUnit: 1, XResolution: "1/1", YResolution: "1/1",
	}

	b, err := i.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{`"FileSize"`, `"FrameCount"`, `"ImageHeight"`, `"ImageWidth"`} {
		if !contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
	for _, unwanted := range []string{`"ResolutionUnit"`, `"XResolution"`, `"YResolution"`} {
		if contains(string(b), unwanted) {
			t.Errorf("unexpected %s in %s", unwanted, b)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
