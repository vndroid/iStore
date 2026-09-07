package vips

/*
#include "options.h"
*/
import "C"

func newLoadOptions(shrink float64, page, pages int) C.ImgproxyLoadOptions {
	return C.ImgproxyLoadOptions{
		Shrink:    C.double(shrink),
		Thumbnail: 0, // Don't load thumbnail by default. Set it explicitly when needed.

		Page:  C.int(page),
		Pages: C.int(pages),

		PngUnlimited:  gbool(config.PngUnlimited),
		SvgUnlimited:  gbool(config.SvgUnlimited),
		TiffUnlimited: gbool(config.TiffUnlimited),
	}
}

// SaveOverrides are per-request adjustments to the process-wide save config.
//
// Everything else in ImgproxySaveOptions is a deployment decision — encoder
// effort, quantisation, WebP preset — and stays in the config. Interlacing is
// the exception because OSS exposes it per URL (`image/interlace,1`), so it
// needs a way in that does not mean handing this package the options bag it was
// deliberately decoupled from.
//
// A nil field means "whatever the config says".
type SaveOverrides struct {
	// Interlace turns on progressive JPEG and interlaced PNG.
	Interlace *bool
}

// newSaveOptions builds the C-side save options from the process-wide config,
// with any per-request overrides applied on top.
//
// imgproxy took a *options.Options here but never read it; iStore drops the
// parameter so the whole options package stays out of the dependency graph.
func newSaveOptions(ov SaveOverrides) C.ImgproxySaveOptions {
	progressive := config.JpegProgressive
	interlaced := config.PngInterlaced

	if ov.Interlace != nil {
		progressive = *ov.Interlace
		interlaced = *ov.Interlace
	}

	return C.ImgproxySaveOptions{
		JpegProgressive: gbool(progressive),

		PngInterlaced:         gbool(interlaced),
		PngQuantize:           gbool(config.PngQuantize),
		PngQuantizationColors: C.int(config.PngQuantizationColors),

		WebpPreset: C.VipsForeignWebpPreset(config.WebpPreset),
		WebpEffort: C.int(config.WebpEffort),

		AvifSpeed: C.int(config.AvifSpeed),

		JxlEffort: C.int(config.JxlEffort),
	}
}
