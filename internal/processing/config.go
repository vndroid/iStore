package processing

import (
	"errors"
	"maps"

	"github.com/kane/istore/internal/ensure"
	"github.com/kane/istore/internal/env"
	"github.com/kane/istore/internal/imagetype"
	"github.com/kane/istore/internal/vips"
)

var (
	ISTORE_PREFERRED_FORMATS       = env.ImageTypes("ISTORE_PREFERRED_FORMATS")
	ISTORE_SKIP_PROCESSING_FORMATS = env.ImageTypes("ISTORE_SKIP_PROCESSING_FORMATS")
	ISTORE_WATERMARK_OPACITY       = env.Float("ISTORE_WATERMARK_OPACITY")
	ISTORE_DISABLE_SHRINK_ON_LOAD  = env.Bool("ISTORE_DISABLE_SHRINK_ON_LOAD")
	ISTORE_USE_LINEAR_COLORSPACE   = env.Bool("ISTORE_USE_LINEAR_COLORSPACE")
	ISTORE_ALWAYS_RASTERIZE_SVG    = env.Bool("ISTORE_ALWAYS_RASTERIZE_SVG")
	ISTORE_QUALITY                 = env.Int("ISTORE_QUALITY")
	ISTORE_FORMAT_QUALITY          = env.ImageTypesQuality("ISTORE_FORMAT_QUALITY")
	ISTORE_STRIP_METADATA          = env.Bool("ISTORE_STRIP_METADATA")
	ISTORE_KEEP_COPYRIGHT          = env.Bool("ISTORE_KEEP_COPYRIGHT")
	ISTORE_STRIP_COLOR_PROFILE     = env.Bool("ISTORE_STRIP_COLOR_PROFILE")
	ISTORE_AUTO_ROTATE             = env.Bool("ISTORE_AUTO_ROTATE")
	ISTORE_ENFORCE_THUMBNAIL       = env.Bool("ISTORE_ENFORCE_THUMBNAIL")
	ISTORE_PRESERVE_HDR            = env.Bool("ISTORE_PRESERVE_HDR")
)

// Config holds pipeline-related configuration.
type Config struct {
	PreferredFormats      []imagetype.Type
	SkipProcessingFormats []imagetype.Type
	WatermarkOpacity      float64
	DisableShrinkOnLoad   bool
	UseLinearColorspace   bool
	AlwaysRasterizeSvg    bool
	Quality               int
	FormatQuality         map[imagetype.Type]int
	StripMetadata         bool
	KeepCopyright         bool
	StripColorProfile     bool
	AutoRotate            bool
	EnforceThumbnail      bool
	PreserveHDR           bool
}

// NewDefaultConfig creates a new Config instance with the given parameters.
func NewDefaultConfig() Config {
	return Config{
		WatermarkOpacity: 1,
		PreferredFormats: []imagetype.Type{
			imagetype.JPEG,
			imagetype.PNG,
			imagetype.GIF,
		},
		Quality: 80,
		FormatQuality: map[imagetype.Type]int{
			imagetype.WEBP: 79,
			imagetype.AVIF: 63,
			imagetype.JXL:  77,
		},
		StripMetadata:     true,
		KeepCopyright:     true,
		StripColorProfile: true,
		AutoRotate:        true,
		EnforceThumbnail:  false,
		PreserveHDR:       false,
	}
}

// LoadConfigFromEnv creates a new Config instance with the given parameters.
func LoadConfigFromEnv(c *Config) (*Config, error) {
	c = ensure.Ensure(c, NewDefaultConfig)

	var fq map[imagetype.Type]int

	err := errors.Join(
		ISTORE_WATERMARK_OPACITY.Parse(&c.WatermarkOpacity),
		ISTORE_DISABLE_SHRINK_ON_LOAD.Parse(&c.DisableShrinkOnLoad),
		ISTORE_USE_LINEAR_COLORSPACE.Parse(&c.UseLinearColorspace),
		ISTORE_ALWAYS_RASTERIZE_SVG.Parse(&c.AlwaysRasterizeSvg),
		ISTORE_QUALITY.Parse(&c.Quality),
		ISTORE_FORMAT_QUALITY.Parse(&fq),
		ISTORE_STRIP_METADATA.Parse(&c.StripMetadata),
		ISTORE_KEEP_COPYRIGHT.Parse(&c.KeepCopyright),
		ISTORE_STRIP_COLOR_PROFILE.Parse(&c.StripColorProfile),
		ISTORE_AUTO_ROTATE.Parse(&c.AutoRotate),
		ISTORE_ENFORCE_THUMBNAIL.Parse(&c.EnforceThumbnail),
		ISTORE_PRESERVE_HDR.Parse(&c.PreserveHDR),

		ISTORE_PREFERRED_FORMATS.Parse(&c.PreferredFormats),
		ISTORE_SKIP_PROCESSING_FORMATS.Parse(&c.SkipProcessingFormats),
	)

	maps.Copy(c.FormatQuality, fq)

	return c, err
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.WatermarkOpacity <= 0 || c.WatermarkOpacity > 1 {
		return ISTORE_WATERMARK_OPACITY.Errorf("must be between 0 and 1")
	}

	if c.Quality < 1 || c.Quality > 100 {
		return ISTORE_QUALITY.Errorf("must be between 1 and 100")
	}

	for imgtype, minQ := range c.FormatQuality {
		if minQ < 1 || minQ > 100 {
			return ISTORE_FORMAT_QUALITY.Errorf("format %s: must be between 1 and 100", imgtype.String())
		}
	}

	filtered := c.PreferredFormats[:0]

	for _, t := range c.PreferredFormats {
		if !vips.SupportsSave(t) {
			ISTORE_PREFERRED_FORMATS.Warn("can't be a preferred format as it's saving is not supported", "format", t)
		} else {
			filtered = append(filtered, t)
		}
	}

	if len(filtered) == 0 {
		return ISTORE_PREFERRED_FORMATS.Errorf("no supported preferred formats specified")
	}

	c.PreferredFormats = filtered

	return nil
}
