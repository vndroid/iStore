package vips

/*
#include "vips.h"
*/
import "C"
import (
	"fmt"
	"os"
	"strconv"

	"github.com/kane/istore/internal/ensure"
)

// Config mirrors imgproxy's vips.Config field for field, so the C-side option
// structs and every save path below behave identically.
//
// What changed is only how it gets filled. imgproxy's env package can pull
// values out of AWS Secrets Manager, AWS SSM and GCP Secret Manager, which is
// why it drags in three cloud SDKs. iStore reads plain environment variables.
type Config struct {
	// Whether to save JPEG as progressive
	JpegProgressive bool

	// Whether to save PNG as interlaced
	PngInterlaced bool
	// Whether to save PNG with adaptive palette
	PngQuantize bool
	// Number of colors for adaptive palette
	PngQuantizationColors int

	// WebP preset to use when saving WebP images
	WebpPreset WebpPreset

	// AVIF saving speed (0 slowest/smallest .. 9 fastest/largest)
	AvifSpeed int
	// WebP saving effort
	WebpEffort int
	// JPEG XL saving effort
	JxlEffort int

	// Whether to not apply any limits when loading PNG
	PngUnlimited bool
	// Whether to not apply any limits when loading SVG
	SvgUnlimited bool
	// Whether to not apply any limits when loading TIFF
	TiffUnlimited bool

	// Whether to enable libvips memory leak check
	LeakCheck bool
	// Whether to enable libvips operation cache tracing
	CacheTrace bool
}

func NewDefaultConfig() Config {
	return Config{
		JpegProgressive: false,

		PngInterlaced:         false,
		PngQuantize:           false,
		PngQuantizationColors: 256,

		WebpPreset: C.VIPS_FOREIGN_WEBP_PRESET_DEFAULT,

		// AVIF speed 8 is imgproxy's default and matches what we measured:
		// on 2 vCPU a 2400x1350 frame encodes in ~0.19s at s8 versus ~0.38s at
		// s6 and ~7.6s at s0. s6 is the size/time knee if you have the CPU.
		AvifSpeed:  8,
		WebpEffort: 4,
		JxlEffort:  4,

		PngUnlimited:  false,
		SvgUnlimited:  false,
		TiffUnlimited: false,

		LeakCheck:  false,
		CacheTrace: false,
	}
}

func LoadConfigFromEnv(c *Config) (*Config, error) {
	c = ensure.Ensure(c, NewDefaultConfig)

	var errs []error
	envBool := func(name string, dst *bool) {
		if v, ok := os.LookupEnv(name); ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: expected a boolean, got %q", name, v))
				return
			}
			*dst = b
		}
	}
	envInt := func(name string, dst *int) {
		if v, ok := os.LookupEnv(name); ok {
			i, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: expected an integer, got %q", name, v))
				return
			}
			*dst = i
		}
	}

	envBool("ISTORE_JPEG_PROGRESSIVE", &c.JpegProgressive)
	envBool("ISTORE_PNG_INTERLACED", &c.PngInterlaced)
	envBool("ISTORE_PNG_QUANTIZE", &c.PngQuantize)
	envInt("ISTORE_PNG_QUANTIZATION_COLORS", &c.PngQuantizationColors)
	envInt("ISTORE_AVIF_SPEED", &c.AvifSpeed)
	envInt("ISTORE_WEBP_EFFORT", &c.WebpEffort)
	envInt("ISTORE_JXL_EFFORT", &c.JxlEffort)
	envBool("ISTORE_PNG_UNLIMITED", &c.PngUnlimited)
	envBool("ISTORE_SVG_UNLIMITED", &c.SvgUnlimited)
	envBool("ISTORE_TIFF_UNLIMITED", &c.TiffUnlimited)
	envBool("ISTORE_VIPS_LEAK_CHECK", &c.LeakCheck)
	envBool("ISTORE_VIPS_CACHE_TRACE", &c.CacheTrace)

	if v, ok := os.LookupEnv("ISTORE_WEBP_PRESET"); ok {
		p, ok := WebpPresets[v]
		if !ok {
			errs = append(errs, fmt.Errorf("ISTORE_WEBP_PRESET: unknown preset %q", v))
		} else {
			c.WebpPreset = p
		}
	}

	if len(errs) > 0 {
		return c, joinErrors(errs)
	}
	return c, nil
}

func joinErrors(errs []error) error {
	if len(errs) == 1 {
		return errs[0]
	}
	msg := ""
	for i, e := range errs {
		if i > 0 {
			msg += "; "
		}
		msg += e.Error()
	}
	return fmt.Errorf("%s", msg)
}

func (c *Config) Validate() error {
	if c.PngQuantizationColors < 2 || c.PngQuantizationColors > 256 {
		return fmt.Errorf("ISTORE_PNG_QUANTIZATION_COLORS: must be between 2 and 256, got %d", c.PngQuantizationColors)
	}

	if c.WebpPreset < C.VIPS_FOREIGN_WEBP_PRESET_DEFAULT || c.WebpPreset >= C.VIPS_FOREIGN_WEBP_PRESET_LAST {
		return fmt.Errorf("ISTORE_WEBP_PRESET: out of range")
	}

	if c.AvifSpeed < 0 || c.AvifSpeed > 9 {
		return fmt.Errorf("ISTORE_AVIF_SPEED: must be between 0 and 9, got %d", c.AvifSpeed)
	}

	if c.JxlEffort < 1 || c.JxlEffort > 9 {
		return fmt.Errorf("ISTORE_JXL_EFFORT: must be between 1 and 9, got %d", c.JxlEffort)
	}

	if c.WebpEffort < 1 || c.WebpEffort > 6 {
		return fmt.Errorf("ISTORE_WEBP_EFFORT: must be between 1 and 6, got %d", c.WebpEffort)
	}

	return nil
}
