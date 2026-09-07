package httpserver

import (
	"net/http"
	"strings"

	"github.com/kane/istore/internal/imagetype"
	"github.com/kane/istore/internal/options"
	"github.com/kane/istore/internal/options/keys"
	"github.com/kane/istore/internal/vips"
)

// negotiatedFormats are the candidates for `format,auto`, best first.
//
// AVIF before WebP because it is both smaller and, at 95% global support
// (caniuse), no longer the narrower target. JPEG XL is deliberately absent: at
// ~15% support, with Chrome behind a flag and Edge not supporting it at all,
// serving it from `auto` would mean encoding a format almost nobody can read —
// and it is the slowest of the three to produce. Ask for it explicitly with
// `format,jxl` if you want it.
var negotiatedFormats = []struct {
	mime string
	typ  imagetype.Type
	key  string
}{
	{"image/avif", imagetype.AVIF, keys.PreferAvif},
	{"image/webp", imagetype.WEBP, keys.PreferWebP},
}

// applyAutoFormat sets the pipeline's Prefer* flags from the request's Accept
// header. Nothing is set when the client listed no modern format, in which case
// the pipeline keeps the source's own format.
//
// Unlike acceptKey this runs per request, after vips.Init, so it can also skip a
// format this build of libvips cannot save.
//
// The result varies by client, so the caller must fold the outcome into the
// cache key — see acceptKey.
func applyAutoFormat(o *options.Options, accept string) {
	for _, f := range negotiatedFormats {
		if acceptsMIME(accept, f.mime) && vips.SupportsSave(f.typ) {
			o.Set(f.key, true)
			return
		}
	}
}

// acceptKey collapses an Accept header to the part that changes the output, so
// two clients with different header orderings but the same capability share a
// cache entry.
//
// This deliberately does NOT consult libvips. vips.SupportsSave calls into the C
// library, which aborts the process with SIGABRT before vips_init has run — the
// same trap that split ossprocess.Validate from CheckEncoders. Keeping the key
// derived from the header alone also keeps it a pure function of the request,
// which is what a cache key should be.
//
// The cost of not checking is a coarser key on a build that cannot save AVIF:
// AVIF-accepting clients still key as "image/avif" while receiving WebP or the
// source. Every such client gets the same answer, so the cache stays correct —
// it just holds one entry that could have been shared with the WebP bucket.
func acceptKey(accept string) string {
	for _, f := range negotiatedFormats {
		if acceptsMIME(accept, f.mime) {
			return f.mime
		}
	}
	return "source"
}

// acceptsMIME reports whether an Accept header lists the exact type. Wildcards
// are ignored on purpose: every browser sends `*/*`, and treating that as "AVIF
// is fine" would send AVIF to clients that cannot read it.
func acceptsMIME(accept, mime string) bool {
	for _, part := range strings.Split(accept, ",") {
		if t, _, _ := strings.Cut(strings.TrimSpace(part), ";"); strings.EqualFold(t, mime) {
			return true
		}
	}
	return false
}

// varyOnAccept marks a response as depending on the Accept header, so shared
// caches downstream do not serve an AVIF to a client that cannot read it.
func varyOnAccept(w http.ResponseWriter) {
	w.Header().Add("Vary", "Accept")
}
