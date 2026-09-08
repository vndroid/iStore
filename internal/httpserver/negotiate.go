package httpserver

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/vndroid/istore/internal/imagetype"
	"github.com/vndroid/istore/internal/options"
	"github.com/vndroid/istore/internal/options/keys"
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

// applyAutoFormat sets the preference flag for the already-negotiated format.
func applyAutoFormat(o *options.Options, format imagetype.Type) {
	for _, f := range negotiatedFormats {
		if f.typ == format {
			o.Set(f.key, true)
			return
		}
	}
}

// negotiateFormat returns the supported modern format with the highest client
// quality. Ties use the server preference order in negotiatedFormats.
func negotiateFormat(accept string, supports func(imagetype.Type) bool) imagetype.Type {
	best := imagetype.Unknown
	bestQ := 0.0
	for _, f := range negotiatedFormats {
		q := mimeQuality(accept, f.mime)
		if q > bestQ && supports(f.typ) {
			best, bestQ = f.typ, q
		}
	}
	return best
}

// mimeQuality returns the highest valid quality for an exact media type.
// Wildcards are deliberately ignored: */* is not evidence that a client can
// decode AVIF. A malformed q value makes that media range unacceptable.
func mimeQuality(accept, mime string) float64 {
	best := 0.0
	for _, part := range strings.Split(accept, ",") {
		pieces := strings.Split(part, ";")
		if !strings.EqualFold(strings.TrimSpace(pieces[0]), mime) {
			continue
		}
		q := 1.0
		for _, param := range pieces[1:] {
			key, val, ok := strings.Cut(strings.TrimSpace(param), "=")
			if !ok || !strings.EqualFold(key, "q") {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
			if err != nil || parsed < 0 || parsed > 1 {
				q = 0
			} else {
				q = parsed
			}
			break
		}
		if q > best {
			best = q
		}
	}
	return best
}

func acceptKey(format imagetype.Type) string {
	if format == imagetype.Unknown {
		return "source"
	}
	return format.Mime()
}

// varyOnAccept marks a response as depending on the Accept header, so shared
// caches downstream do not serve an AVIF to a client that cannot read it.
func varyOnAccept(w http.ResponseWriter) {
	w.Header().Add("Vary", "Accept")
}
