package vips

import (
	"net/http"
	"regexp"

	"github.com/vndroid/istore/internal/errctx"
)

// badImageErrRe recognises a libvips error as the source's fault: 422, not 500.
//
// Every pattern is multi-line. libvips accumulates its error buffer, so the
// loader's line is often not the first — libtiff logs TIFFFillStrip before
// tiff2vips reports, libheif's reader logs "bad seek" before heif does. An
// anchor that only looked at the first line sent every one of those to 500.
//
// Each prefix is a *load* domain on purpose. "heif:" is shared with the
// encoder, so only libheif's input-side categories count; an encoder failure
// is still ours. webp's saver reports as "vips2webp", never "webp:".
var badImageErrRe = []*regexp.Regexp{
	regexp.MustCompile(`(?m)^(\S+)load_source: `),
	regexp.MustCompile(`(?m)^(\S+)2vips: `),
	regexp.MustCompile(`(?m)^VipsJpeg: `),
	regexp.MustCompile(`XML parse error: `),
	// iStore's own BMP and ICO loaders (bmpload.c, icoload.c).
	regexp.MustCompile(`(?m)^vips_foreign_load_\S+: `),
	regexp.MustCompile(`(?m)^vips__tiff_openin_source: `),
	regexp.MustCompile(`(?m)^webp: `),
	regexp.MustCompile(`(?m)^heif: (Invalid input|Unsupported feature|Unsupported file-?type)`),
}

type VipsError struct{ *errctx.TextError }

func newVipsError(msg string) error {
	var opts []errctx.Option

	for _, re := range badImageErrRe {
		if re.MatchString(msg) {
			opts = []errctx.Option{
				errctx.WithStatusCode(http.StatusUnprocessableEntity),
				errctx.WithPublicMessage("Broken or unsupported image"),
			}
			break
		}
	}

	return VipsError{errctx.NewTextError(msg, 1, opts...)}
}
