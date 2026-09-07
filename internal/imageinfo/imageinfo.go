// Package imageinfo answers `x-oss-process=image/info` — the metadata of a
// source image, without decoding its pixels.
//
// Why not just hand the file to libvips and read Width()/Height(): libvips would
// do the job, but it also constructs a pipeline, allocates, and for some formats
// decodes more than the header. The info endpoint is the cheap one — the whole
// point of it, for a caller filling in `<img width height>`, is that it costs
// far less than fetching the image. So the dimensions come from parsing the
// container header directly, and nothing here allocates more than the header
// buffer.
//
// Fields mirror Alibaba Cloud OSS's image/info response, whose values are all
// JSON strings wrapped in {"value": ...}.
package imageinfo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/kane/istore/internal/imagetype"
)

// HeaderBytes is how much of the file is read to answer an info query.
//
// 64 KiB clears a JPEG's APP0/APP1 segments (EXIF is capped at 64 KiB per
// segment) plus the SOF marker in every image we have seen. If a pathological
// file buries SOF deeper, Read reports ErrHeaderTooShort rather than guessing.
const HeaderBytes = 64 << 10

// ErrHeaderTooShort means the dimensions were not within the first HeaderBytes.
var ErrHeaderTooShort = errors.New("imageinfo: image header not found within the first 64 KiB")

// Info is the metadata reported for a source image.
type Info struct {
	FileSize       int64
	Format         imagetype.Type
	FrameCount     int
	ImageWidth     int
	ImageHeight    int
	ResolutionUnit int    // EXIF semantics: 1 none, 2 inch, 3 cm
	XResolution    string // rational, e.g. "72/1"
	YResolution    string

	// Exif holds the source's EXIF tags by their standard names, nil when the
	// image carries none. OSS merges these into the same flat object as the
	// fields above, so MarshalJSON does too.
	Exif map[string]string
}

// value is the {"value": "..."} wrapper OSS uses for every field.
type value struct {
	Value string `json:"value"`
}

// MarshalJSON renders the OSS-compatible shape: one flat object of
// {"value": "..."} entries, basic fields and EXIF tags together.
//
// A map rather than a struct because the EXIF tag set is not known at compile
// time. encoding/json sorts map keys, which is the order OSS uses and the order
// the previous struct spelled out by hand — so an image without EXIF marshals
// byte for byte as before.
func (i Info) MarshalJSON() ([]byte, error) {
	out := make(map[string]value, 8+len(i.Exif))

	// EXIF first, so the measured fields below always win. ImageWidth and
	// friends are read from the container itself; an EXIF block claiming
	// something else is either stale or lying, and either way the caller wants
	// the real dimensions.
	for k, v := range i.Exif {
		out[k] = value{v}
	}

	format := i.Format.String()
	if i.Format == imagetype.JPEG {
		format = "jpg" // OSS spells it jpg
	}

	out["FileSize"] = value{strconv.FormatInt(i.FileSize, 10)}
	out["Format"] = value{format}
	out["FrameCount"] = value{strconv.Itoa(i.FrameCount)}
	out["ImageHeight"] = value{strconv.Itoa(i.ImageHeight)}
	out["ImageWidth"] = value{strconv.Itoa(i.ImageWidth)}
	out["ResolutionUnit"] = value{strconv.Itoa(i.ResolutionUnit)}
	out["XResolution"] = value{i.XResolution}
	out["YResolution"] = value{i.YResolution}

	return marshalOSS(out)
}

// Read extracts info from r, which should be positioned at the start of the
// image. size is the total file size in bytes.
func Read(r io.Reader, size int64) (*Info, error) {
	head := make([]byte, HeaderBytes)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	head = head[:n]

	t, err := imagetype.Detect(newSeekableReader(head), "", "")
	if err != nil {
		return nil, err
	}

	info := &Info{
		FileSize: size,
		Format:   t,
		// Defaults match what OSS reports for an image carrying no resolution
		// metadata: unit 1 ("none"), square 1/1 pixel aspect.
		FrameCount:     1,
		ResolutionUnit: 1,
		XResolution:    "1/1",
		YResolution:    "1/1",
	}

	switch t {
	case imagetype.JPEG:
		err = readJPEG(head, info)
	case imagetype.PNG:
		err = readPNG(head, info)
	case imagetype.GIF:
		err = readGIF(head, info)
	case imagetype.WEBP:
		err = readWebP(head, info)
	default:
		// AVIF, HEIC, JXL, TIFF, BMP, ICO: the container walk for each is a
		// project of its own, and the caller is better served by an honest
		// error than by zeros. The HTTP layer falls back to libvips for these.
		return info, ErrUnsupportedContainer{t}
	}
	if err != nil {
		return nil, err
	}

	return info, nil
}

// ErrUnsupportedContainer reports a format whose header this package does not
// parse. The dimensions in the returned Info are unset.
type ErrUnsupportedContainer struct{ Type imagetype.Type }

func (e ErrUnsupportedContainer) Error() string {
	return fmt.Sprintf("imageinfo: no header parser for %s", e.Type)
}

// ---------------------------------------------------------------- JPEG

// readJPEG walks the marker segments for SOFn (dimensions) and APP0/APP1
// (resolution).
func readJPEG(b []byte, info *Info) error {
	if len(b) < 4 {
		return ErrHeaderTooShort
	}
	pos := 2 // skip SOI

	for pos+3 < len(b) {
		if b[pos] != 0xFF {
			// Not on a marker boundary. Resynchronise rather than bail: some
			// encoders emit fill bytes between segments.
			pos++
			continue
		}
		marker := b[pos+1]
		pos += 2

		// Standalone markers carry no length.
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			continue
		}
		if marker == 0xFF { // fill byte
			pos--
			continue
		}
		if pos+1 >= len(b) {
			return ErrHeaderTooShort
		}
		segLen := int(binary.BigEndian.Uint16(b[pos:]))
		if segLen < 2 || pos+segLen > len(b) {
			return ErrHeaderTooShort
		}
		seg := b[pos+2 : pos+segLen]

		switch {
		// SOF0..SOF15, excluding DHT (C4), JPG (C8) and DAC (CC)
		case marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC:
			if len(seg) < 5 {
				return ErrHeaderTooShort
			}
			info.ImageHeight = int(binary.BigEndian.Uint16(seg[1:]))
			info.ImageWidth = int(binary.BigEndian.Uint16(seg[3:]))
			return nil // SOF always follows the APPn segments we care about

		case marker == 0xE0 && len(seg) >= 12 && string(seg[:5]) == "JFIF\x00":
			// JFIF density: units 0 = aspect ratio only, 1 = dpi, 2 = dpcm.
			// EXIF numbers those 1, 2, 3 respectively, and OSS reports the EXIF
			// numbering, so shift by one.
			info.ResolutionUnit = int(seg[7]) + 1
			info.XResolution = ratio(binary.BigEndian.Uint16(seg[8:]))
			info.YResolution = ratio(binary.BigEndian.Uint16(seg[10:]))

		case marker == 0xE1 && len(seg) >= 6 && string(seg[:6]) == "Exif\x00\x00":
			readEXIF(seg[6:], info)

		case marker == 0xDA: // start of scan; no header left to find
			return ErrHeaderTooShort
		}

		pos += segLen
	}

	return ErrHeaderTooShort
}

func ratio(n uint16) string {
	if n == 0 {
		return "1/1"
	}
	return strconv.Itoa(int(n)) + "/1"
}

// readEXIF parses an EXIF block and records both the full tag set and the three
// resolution fields the basic response reports.
//
// Resolution comes out of the same walk rather than a second one: XResolution,
// YResolution and ResolutionUnit are ordinary IFD0 tags, and having two readers
// disagree about them would be worse than either. Failures leave the defaults
// in place — resolution is decoration here, not the answer the caller came for.
func readEXIF(b []byte, info *Info) {
	tags := parseEXIF(b)
	if tags == nil {
		return
	}

	info.Exif = tags

	if v, ok := tags["XResolution"]; ok && strings.Contains(v, "/") {
		info.XResolution = v
	}
	if v, ok := tags["YResolution"]; ok && strings.Contains(v, "/") {
		info.YResolution = v
	}
	if v, ok := tags["ResolutionUnit"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			info.ResolutionUnit = n
		}
	}
}

// ---------------------------------------------------------------- PNG

func readPNG(b []byte, info *Info) error {
	// 8-byte signature, then IHDR: 4 length + 4 type + 13 data
	if len(b) < 33 {
		return ErrHeaderTooShort
	}
	if string(b[12:16]) != "IHDR" {
		return ErrHeaderTooShort
	}
	info.ImageWidth = int(binary.BigEndian.Uint32(b[16:]))
	info.ImageHeight = int(binary.BigEndian.Uint32(b[20:]))

	// pHYs, if present, gives pixels per unit; unit 1 is metres.
	pos := 8
	for pos+8 <= len(b) {
		l := int(binary.BigEndian.Uint32(b[pos:]))
		typ := string(b[pos+4 : pos+8])
		if typ == "IDAT" || typ == "IEND" {
			break
		}
		if typ == "acTL" && pos+8+8 <= len(b) {
			// APNG animation control: frame count is the first field.
			info.FrameCount = int(binary.BigEndian.Uint32(b[pos+8:]))
		}
		if typ == "eXIf" && l > 0 && pos+8+l <= len(b) {
			// PNG carries EXIF as a bare TIFF block, without JPEG's
			// "Exif\0\0" prefix.
			readEXIF(b[pos+8:pos+8+l], info)
		}
		if typ == "pHYs" && pos+8+9 <= len(b) {
			x := binary.BigEndian.Uint32(b[pos+8:])
			y := binary.BigEndian.Uint32(b[pos+12:])
			if b[pos+16] == 1 { // metres
				// EXIF has no "per metre" unit; report centimetres (3).
				info.ResolutionUnit = 3
				info.XResolution = strconv.FormatUint(uint64(x), 10) + "/100"
				info.YResolution = strconv.FormatUint(uint64(y), 10) + "/100"
			}
		}
		next := pos + 12 + l
		if next <= pos { // overflow or corrupt length
			break
		}
		pos = next
	}

	return nil
}

// ---------------------------------------------------------------- GIF

func readGIF(b []byte, info *Info) error {
	if len(b) < 10 {
		return ErrHeaderTooShort
	}
	info.ImageWidth = int(binary.LittleEndian.Uint16(b[6:]))
	info.ImageHeight = int(binary.LittleEndian.Uint16(b[8:]))
	info.FrameCount = countGIFFrames(b)
	return nil
}

// countGIFFrames walks the block structure counting image descriptors.
//
// It only sees what fits in the header buffer, so for a long animation the count
// is a floor, not the truth. That is flagged by returning at least 1 and never
// erroring: a caller sizing an <img> does not care, and a caller that does
// should decode the file.
func countGIFFrames(b []byte) int {
	pos := 13
	if len(b) > 10 && b[10]&0x80 != 0 { // global colour table present
		pos += 3 * (1 << ((b[10] & 0x07) + 1))
	}

	frames := 0
	for pos < len(b) {
		switch b[pos] {
		case 0x2C: // image descriptor
			frames++
			if pos+10 > len(b) {
				return max(frames, 1)
			}
			flags := b[pos+9]
			pos += 10
			if flags&0x80 != 0 { // local colour table
				pos += 3 * (1 << ((flags & 0x07) + 1))
			}
			pos++ // LZW minimum code size
			pos = skipGIFSubBlocks(b, pos)
		case 0x21: // extension
			if pos+2 > len(b) {
				return max(frames, 1)
			}
			pos += 2
			pos = skipGIFSubBlocks(b, pos)
		case 0x3B: // trailer
			return max(frames, 1)
		default:
			return max(frames, 1)
		}
		if pos <= 0 {
			return max(frames, 1)
		}
	}
	return max(frames, 1)
}

func skipGIFSubBlocks(b []byte, pos int) int {
	for pos < len(b) {
		n := int(b[pos])
		if n == 0 {
			return pos + 1
		}
		pos += n + 1
	}
	return pos
}

// ---------------------------------------------------------------- WebP

func readWebP(b []byte, info *Info) error {
	// RIFF....WEBP then a chunk: VP8 (lossy), VP8L (lossless) or VP8X (extended)
	if len(b) < 30 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return ErrHeaderTooShort
	}

	switch string(b[12:16]) {
	case "VP8X":
		// 24-bit canvas dimensions minus one, little endian.
		info.ImageWidth = int(b[24]) | int(b[25])<<8 | int(b[26])<<16 + 1
		info.ImageHeight = int(b[27]) | int(b[28])<<8 | int(b[29])<<16 + 1
		if b[20]&0x02 != 0 {
			// Animation flag. The frame count lives in ANMF chunks; counting
			// them means walking the whole file, so report it as animated
			// without a precise count.
			info.FrameCount = 0
		}
		// Only the extended format can carry metadata chunks, and the EXIF one
		// holds a bare TIFF block as PNG's does.
		if exif := findRIFFChunk(b, "EXIF"); exif != nil {
			readEXIF(exif, info)
		}
		return nil
	case "VP8 ":
		if len(b) < 30 {
			return ErrHeaderTooShort
		}
		info.ImageWidth = int(binary.LittleEndian.Uint16(b[26:])) & 0x3FFF
		info.ImageHeight = int(binary.LittleEndian.Uint16(b[28:])) & 0x3FFF
		return nil
	case "VP8L":
		if len(b) < 25 {
			return ErrHeaderTooShort
		}
		bits := binary.LittleEndian.Uint32(b[21:])
		info.ImageWidth = int(bits&0x3FFF) + 1
		info.ImageHeight = int((bits>>14)&0x3FFF) + 1
		return nil
	}

	return ErrHeaderTooShort
}

// findRIFFChunk returns the payload of the first chunk with this four-character
// id, or nil. Chunks are padded to an even length, which the walk has to honour
// or every chunk after an odd-sized one is misread.
func findRIFFChunk(b []byte, id string) []byte {
	pos := 12
	for pos+8 <= len(b) {
		l := int(binary.LittleEndian.Uint32(b[pos+4:]))
		if l < 0 || pos+8+l > len(b) {
			return nil
		}
		if string(b[pos:pos+4]) == id {
			return b[pos+8 : pos+8+l]
		}
		next := pos + 8 + l + l%2
		if next <= pos {
			return nil
		}
		pos = next
	}
	return nil
}
