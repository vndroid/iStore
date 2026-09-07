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

// newSaveOptions builds the C-side save options from the process-wide config.
// imgproxy took a *options.Options here but never read it; iStore drops the
// parameter so the whole options package stays out of the dependency graph.
func newSaveOptions() C.ImgproxySaveOptions {
	return C.ImgproxySaveOptions{
		JpegProgressive: gbool(config.JpegProgressive),

		PngInterlaced:         gbool(config.PngInterlaced),
		PngQuantize:           gbool(config.PngQuantize),
		PngQuantizationColors: C.int(config.PngQuantizationColors),

		WebpPreset: C.VipsForeignWebpPreset(config.WebpPreset),
		WebpEffort: C.int(config.WebpEffort),

		AvifSpeed: C.int(config.AvifSpeed),

		JxlEffort: C.int(config.JxlEffort),
	}
}
