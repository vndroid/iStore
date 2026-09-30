package processing

import (
	"errors"
	"maps"

	"github.com/vndroid/iXoSS/internal/ensure"
	"github.com/vndroid/iXoSS/internal/env"
	"github.com/vndroid/iXoSS/internal/imagetype"
	"github.com/vndroid/iXoSS/internal/vips"
)

var (
	IXOSS_PREFERRED_FORMATS       = env.ImageTypes("IXOSS_PREFERRED_FORMATS")
	IXOSS_SKIP_PROCESSING_FORMATS = env.ImageTypes("IXOSS_SKIP_PROCESSING_FORMATS")
	IXOSS_WATERMARK_OPACITY       = env.Float("IXOSS_WATERMARK_OPACITY")
	IXOSS_DISABLE_SHRINK_ON_LOAD  = env.Bool("IXOSS_DISABLE_SHRINK_ON_LOAD")
	IXOSS_USE_LINEAR_COLORSPACE   = env.Bool("IXOSS_USE_LINEAR_COLORSPACE")
	IXOSS_ALWAYS_RASTERIZE_SVG    = env.Bool("IXOSS_ALWAYS_RASTERIZE_SVG")
	IXOSS_QUALITY                 = env.Int("IXOSS_QUALITY")
	IXOSS_FORMAT_QUALITY          = env.ImageTypesQuality("IXOSS_FORMAT_QUALITY")
	IXOSS_STRIP_METADATA          = env.Bool("IXOSS_STRIP_METADATA")
	IXOSS_KEEP_COPYRIGHT          = env.Bool("IXOSS_KEEP_COPYRIGHT")
	IXOSS_STRIP_COLOR_PROFILE     = env.Bool("IXOSS_STRIP_COLOR_PROFILE")
	IXOSS_AUTO_ROTATE             = env.Bool("IXOSS_AUTO_ROTATE")
	IXOSS_ENFORCE_THUMBNAIL       = env.Bool("IXOSS_ENFORCE_THUMBNAIL")
	IXOSS_PRESERVE_HDR            = env.Bool("IXOSS_PRESERVE_HDR")
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
		IXOSS_WATERMARK_OPACITY.Parse(&c.WatermarkOpacity),
		IXOSS_DISABLE_SHRINK_ON_LOAD.Parse(&c.DisableShrinkOnLoad),
		IXOSS_USE_LINEAR_COLORSPACE.Parse(&c.UseLinearColorspace),
		IXOSS_ALWAYS_RASTERIZE_SVG.Parse(&c.AlwaysRasterizeSvg),
		IXOSS_QUALITY.Parse(&c.Quality),
		IXOSS_FORMAT_QUALITY.Parse(&fq),
		IXOSS_STRIP_METADATA.Parse(&c.StripMetadata),
		IXOSS_KEEP_COPYRIGHT.Parse(&c.KeepCopyright),
		IXOSS_STRIP_COLOR_PROFILE.Parse(&c.StripColorProfile),
		IXOSS_AUTO_ROTATE.Parse(&c.AutoRotate),
		IXOSS_ENFORCE_THUMBNAIL.Parse(&c.EnforceThumbnail),
		IXOSS_PRESERVE_HDR.Parse(&c.PreserveHDR),

		IXOSS_PREFERRED_FORMATS.Parse(&c.PreferredFormats),
		IXOSS_SKIP_PROCESSING_FORMATS.Parse(&c.SkipProcessingFormats),
	)

	maps.Copy(c.FormatQuality, fq)

	return c, err
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.WatermarkOpacity <= 0 || c.WatermarkOpacity > 1 {
		return IXOSS_WATERMARK_OPACITY.Errorf("must be between 0 and 1")
	}

	if c.Quality < 1 || c.Quality > 100 {
		return IXOSS_QUALITY.Errorf("must be between 1 and 100")
	}

	for imgtype, minQ := range c.FormatQuality {
		if minQ < 1 || minQ > 100 {
			return IXOSS_FORMAT_QUALITY.Errorf("format %s: must be between 1 and 100", imgtype.String())
		}
	}

	filtered := c.PreferredFormats[:0]

	for _, t := range c.PreferredFormats {
		if !vips.SupportsSave(t) {
			IXOSS_PREFERRED_FORMATS.Warn("can't be a preferred format as it's saving is not supported", "format", t)
		} else {
			filtered = append(filtered, t)
		}
	}

	if len(filtered) == 0 {
		return IXOSS_PREFERRED_FORMATS.Errorf("no supported preferred formats specified")
	}

	c.PreferredFormats = filtered

	return nil
}
