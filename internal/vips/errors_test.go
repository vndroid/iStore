package vips

import (
	"errors"
	"net/http"
	"testing"
)

// Messages as libvips 8.15.1 actually produced them for truncated or garbled
// sources. Each of these used to come back as a 500.
func TestBrokenSourceErrorsAre422(t *testing.T) {
	for _, msg := range []string{
		"webp: unable to parse image",
		"bad seek to 327\nheif: Invalid input: No 'meta' box (2.104)",
		"bad seek to 1348\nbad seek to 1158\nheif: Invalid input: Unexpected end of file: Extent in iloc box references data outside of file bounds (2.100)",
		"TIFFFillStrip: Read error at scanline 126; got 1730 bytes, expected 7200\ntiff2vips: read error",
		"TIFFReadDirectory: Failed to read directory at offset 8\nvips__tiff_openin_source: unable to open source for input",
		"vips_foreign_load_bmp_header: unsupported BMP image: planes != 1",
		"vips_foreign_load_bmp_24_32_generate_strip: failed to read raw data",
		"vips_foreign_load_ico_header: unable to read ICO image data from the source",
		// The patterns that were already there must keep working.
		"jpegload_source: out of order read at line 48",
		"VipsJpeg: Premature end of JPEG file",
	} {
		if got := statusOf(t, newVipsError(msg)); got != http.StatusUnprocessableEntity {
			t.Errorf("%q: status %d, want 422", msg, got)
		}
	}
}

// Failures that are not the source's fault must not be laundered into 422.
func TestOwnFailuresStay500(t *testing.T) {
	for _, msg := range []string{
		"heif: Encoder plugin generated an error: Unsupported bit depth (6.1)",
		"vips2webp: unable to encode",
		"vips_foreign_save_bmp_build: unable to write BMP pixel data to target",
		"out of memory",
	} {
		if got := statusOf(t, newVipsError(msg)); got != http.StatusInternalServerError {
			t.Errorf("%q: status %d, want 500", msg, got)
		}
	}
}

func statusOf(t *testing.T, err error) int {
	t.Helper()
	var coded interface{ StatusCode() int }
	if !errors.As(err, &coded) {
		t.Fatalf("%v carries no status", err)
	}
	return coded.StatusCode()
}
