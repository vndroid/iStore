package imageinfo

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"
)

// OSS merges the source's EXIF tags into the same object as the basic fields,
// each in the same {"value": "..."} wrapper, and returns only the basic fields
// when the image carries no EXIF. This file reproduces that: it walks the TIFF
// IFD structure an EXIF block is made of and returns the tags it can name.
//
// Two deliberate differences from OSS, both about *formatting* rather than
// which tags appear:
//
//   - Values are rendered from the raw EXIF types. A rational stays "72/1", a
//     SHORT stays "7". OSS runs the same tags through exiv2's pretty-printer,
//     which turns a few enum-like tags into prose — GPSLatitudeRef becomes
//     "North", GPSLatitude becomes "0deg". Reproducing that means carrying
//     exiv2's label tables for every enum in the standard, which is a large
//     surface to keep correct for a decorative field.
//   - Tags with no name in the table below are skipped rather than emitted as
//     "Tag0x8298". So are UNDEFINED-typed tags: MakerNote and friends are
//     vendor binary blobs, and putting them in a JSON string helps nobody.

// exifMaxTags caps how many tags one image can contribute, and exifMaxValue caps
// the length of any single value.
//
// Both exist because this data is attacker-controlled: an image can declare a
// 65535-entry IFD of 4 KB ASCII strings, and the info response is small,
// cacheable and meant to be cheap. Truncating is better than serving a
// multi-megabyte JSON document built from a 300-byte header.
const (
	exifMaxTags  = 256
	exifMaxValue = 256
)

// exifIFDLimit bounds how many IFDs one walk will follow, so a file whose
// sub-IFD pointers form a cycle terminates.
const exifIFDLimit = 8

// parseEXIF reads a TIFF-structured EXIF block — b starts at the byte-order
// mark, i.e. after JPEG's "Exif\0\0" — and returns the named tags it finds in
// IFD0, the Exif sub-IFD, the GPS IFD and the interoperability IFD.
//
// It never returns an error: EXIF is decoration on this endpoint, and a
// malformed block should cost the caller the tags, not the response.
func parseEXIF(b []byte) map[string]string {
	if len(b) < 8 {
		return nil
	}

	var bo binary.ByteOrder
	switch string(b[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return nil
	}
	if bo.Uint16(b[2:]) != 0x002A {
		return nil
	}

	out := make(map[string]string)

	// Queue of (offset, name table) pairs. IFD0 comes first; sub-IFDs are
	// appended as their pointer tags are met.
	type ifd struct {
		off   int
		names map[uint16]string
	}
	queue := []ifd{{int(bo.Uint32(b[4:])), exifTagNames}}
	seen := map[int]bool{}

	for i := 0; i < len(queue) && i < exifIFDLimit; i++ {
		cur := queue[i]
		if cur.off <= 0 || cur.off+2 > len(b) || seen[cur.off] {
			continue
		}
		seen[cur.off] = true

		count := int(bo.Uint16(b[cur.off:]))
		base := cur.off + 2

		for e := 0; e < count; e++ {
			p := base + e*12
			if p+12 > len(b) {
				break
			}

			tag := bo.Uint16(b[p:])
			typ := bo.Uint16(b[p+2:])
			n := int(bo.Uint32(b[p+4:]))

			// A pointer tag names another IFD rather than carrying a value.
			// It is still reported, because OSS reports it (its example has
			// "ExifTag": {"value": "2212"}).
			if sub, ok := exifSubIFDs[tag]; ok && typ == 4 && n == 1 {
				queue = append(queue, ifd{int(bo.Uint32(b[p+8:])), sub})
			}

			name, ok := cur.names[tag]
			if !ok || len(out) >= exifMaxTags {
				continue
			}

			if v, ok := exifValue(b, bo, p, typ, n); ok {
				out[name] = v
			}
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// exifTypeSize is the byte width of each EXIF value type, indexed by type code.
// Zero marks a type this package does not render.
var exifTypeSize = [...]int{
	0, // 0 unused
	1, // 1 BYTE
	1, // 2 ASCII
	2, // 3 SHORT
	4, // 4 LONG
	8, // 5 RATIONAL
	1, // 6 SBYTE
	0, // 7 UNDEFINED — vendor binary, deliberately skipped
	2, // 8 SSHORT
	4, // 9 SLONG
	8, // 10 SRATIONAL
	4, // 11 FLOAT
	8, // 12 DOUBLE
}

// exifValue renders one IFD entry. p is the offset of the 12-byte entry.
func exifValue(b []byte, bo binary.ByteOrder, p int, typ uint16, n int) (string, bool) {
	if int(typ) >= len(exifTypeSize) || n <= 0 {
		return "", false
	}
	size := exifTypeSize[typ]
	if size == 0 {
		return "", false
	}

	total := size * n
	if total <= 0 || total > len(b) {
		return "", false
	}

	// Up to four bytes live in the entry itself; anything longer is at an
	// offset from the start of the TIFF block.
	off := p + 8
	if total > 4 {
		off = int(bo.Uint32(b[p+8:]))
	}
	if off < 0 || off+total > len(b) {
		return "", false
	}
	data := b[off : off+total]

	if typ == 2 { // ASCII
		s := strings.TrimRight(string(data), "\x00")
		s = strings.TrimSpace(s)
		if s == "" || !isPrintableASCII(s) {
			return "", false
		}
		return truncate(s), true
	}

	parts := make([]string, 0, min(n, 16))
	for i := 0; i < n && i < 16; i++ {
		d := data[i*size:]
		var s string

		switch typ {
		case 1:
			s = strconv.FormatUint(uint64(d[0]), 10)
		case 6:
			s = strconv.FormatInt(int64(int8(d[0])), 10)
		case 3:
			s = strconv.FormatUint(uint64(bo.Uint16(d)), 10)
		case 8:
			s = strconv.FormatInt(int64(int16(bo.Uint16(d))), 10)
		case 4:
			s = strconv.FormatUint(uint64(bo.Uint32(d)), 10)
		case 9:
			s = strconv.FormatInt(int64(int32(bo.Uint32(d))), 10)
		case 5:
			s = strconv.FormatUint(uint64(bo.Uint32(d)), 10) + "/" +
				strconv.FormatUint(uint64(bo.Uint32(d[4:])), 10)
		case 10:
			s = strconv.FormatInt(int64(int32(bo.Uint32(d))), 10) + "/" +
				strconv.FormatInt(int64(int32(bo.Uint32(d[4:]))), 10)
		case 11:
			s = strconv.FormatFloat(float64(f32(bo, d)), 'g', -1, 32)
		case 12:
			s = strconv.FormatFloat(f64(bo, d), 'g', -1, 64)
		}

		parts = append(parts, s)
	}

	return truncate(strings.Join(parts, " ")), true
}

func f32(bo binary.ByteOrder, b []byte) float32 {
	return math.Float32frombits(bo.Uint32(b))
}

func f64(bo binary.ByteOrder, b []byte) float64 {
	return math.Float64frombits(bo.Uint64(b))
}

func truncate(s string) string {
	if len(s) > exifMaxValue {
		return s[:exifMaxValue]
	}
	return s
}

// isPrintableASCII rejects control bytes, which show up in truncated or
// misdeclared ASCII fields and would otherwise reach the JSON body as escapes.
func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return true
}

// exifSubIFDs maps a pointer tag to the name table its target IFD uses. GPS tag
// numbers start again at zero and mean entirely different things, so it needs
// its own table; the Exif and interoperability IFDs share IFD0's numbering.
var exifSubIFDs = map[uint16]map[uint16]string{
	0x8769: exifTagNames, // ExifTag
	0x8825: gpsTagNames,  // GPSTag
	0xA005: exifTagNames, // InteroperabilityTag
}

// exifTagNames covers IFD0 and the Exif sub-IFD. Names are the standard EXIF
// 2.3 spellings, which is also what OSS reports.
var exifTagNames = map[uint16]string{
	0x0100: "ImageWidth",
	0x0101: "ImageLength",
	0x0102: "BitsPerSample",
	0x0103: "Compression",
	0x0106: "PhotometricInterpretation",
	0x010E: "ImageDescription",
	0x010F: "Make",
	0x0110: "Model",
	0x0111: "StripOffsets",
	0x0112: "Orientation",
	0x0115: "SamplesPerPixel",
	0x0116: "RowsPerStrip",
	0x0117: "StripByteCounts",
	0x011A: "XResolution",
	0x011B: "YResolution",
	0x011C: "PlanarConfiguration",
	0x0128: "ResolutionUnit",
	0x0131: "Software",
	0x0132: "DateTime",
	0x013B: "Artist",
	0x013E: "WhitePoint",
	0x013F: "PrimaryChromaticities",
	0x0201: "JPEGInterchangeFormat",
	0x0202: "JPEGInterchangeFormatLength",
	0x0211: "YCbCrCoefficients",
	0x0212: "YCbCrSubSampling",
	0x0213: "YCbCrPositioning",
	0x0214: "ReferenceBlackWhite",
	0x8298: "Copyright",
	0x8769: "ExifTag",
	0x8825: "GPSTag",
	0x829A: "ExposureTime",
	0x829D: "FNumber",
	0x8822: "ExposureProgram",
	0x8824: "SpectralSensitivity",
	0x8827: "ISOSpeedRatings",
	0x8830: "SensitivityType",
	0x8832: "RecommendedExposureIndex",
	0x9003: "DateTimeOriginal",
	0x9004: "DateTimeDigitized",
	0x9102: "CompressedBitsPerPixel",
	0x9201: "ShutterSpeedValue",
	0x9202: "ApertureValue",
	0x9203: "BrightnessValue",
	0x9204: "ExposureBiasValue",
	0x9205: "MaxApertureValue",
	0x9206: "SubjectDistance",
	0x9207: "MeteringMode",
	0x9208: "LightSource",
	0x9209: "Flash",
	0x920A: "FocalLength",
	0x9214: "SubjectArea",
	0x9290: "SubSecTime",
	0x9291: "SubSecTimeOriginal",
	0x9292: "SubSecTimeDigitized",
	0xA001: "ColorSpace",
	0xA002: "PixelXDimension",
	0xA003: "PixelYDimension",
	0xA004: "RelatedSoundFile",
	0xA005: "InteroperabilityTag",
	0xA20E: "FocalPlaneXResolution",
	0xA20F: "FocalPlaneYResolution",
	0xA210: "FocalPlaneResolutionUnit",
	0xA214: "SubjectLocation",
	0xA215: "ExposureIndex",
	0xA217: "SensingMethod",
	0xA401: "CustomRendered",
	0xA402: "ExposureMode",
	0xA403: "WhiteBalance",
	0xA404: "DigitalZoomRatio",
	0xA405: "FocalLengthIn35mmFilm",
	0xA406: "SceneCaptureType",
	0xA407: "GainControl",
	0xA408: "Contrast",
	0xA409: "Saturation",
	0xA40A: "Sharpness",
	0xA40C: "SubjectDistanceRange",
	0xA420: "ImageUniqueID",
	0xA430: "CameraOwnerName",
	0xA431: "BodySerialNumber",
	0xA432: "LensSpecification",
	0xA433: "LensMake",
	0xA434: "LensModel",
	0xA435: "LensSerialNumber",
}

// gpsTagNames covers the GPS IFD, whose tag numbers restart at zero.
var gpsTagNames = map[uint16]string{
	0x0000: "GPSVersionID",
	0x0001: "GPSLatitudeRef",
	0x0002: "GPSLatitude",
	0x0003: "GPSLongitudeRef",
	0x0004: "GPSLongitude",
	0x0005: "GPSAltitudeRef",
	0x0006: "GPSAltitude",
	0x0007: "GPSTimeStamp",
	0x0008: "GPSSatellites",
	0x0009: "GPSStatus",
	0x000A: "GPSMeasureMode",
	0x000B: "GPSDOP",
	0x000C: "GPSSpeedRef",
	0x000D: "GPSSpeed",
	0x000E: "GPSTrackRef",
	0x000F: "GPSTrack",
	0x0010: "GPSImgDirectionRef",
	0x0011: "GPSImgDirection",
	0x0012: "GPSMapDatum",
	0x001D: "GPSDateStamp",
}
