package auximageprovider

import (
	"github.com/vndroid/ixoss/internal/ensure"
	"github.com/vndroid/ixoss/internal/env"
)

var (
	IXOSS_WATERMARK_DATA = env.String("IXOSS_WATERMARK_DATA")
	IXOSS_WATERMARK_PATH = env.ExistingFilePath("IXOSS_WATERMARK_PATH")
	IXOSS_WATERMARK_URL  = env.String("IXOSS_WATERMARK_URL")

	IXOSS_FALLBACK_IMAGE_DATA = env.String("IXOSS_FALLBACK_IMAGE_DATA")
	IXOSS_FALLBACK_IMAGE_PATH = env.ExistingFilePath("IXOSS_FALLBACK_IMAGE_PATH")
	IXOSS_FALLBACK_IMAGE_URL  = env.String("IXOSS_FALLBACK_IMAGE_URL")
)

// StaticConfig holds the configuration for the auxiliary image provider
type StaticConfig struct {
	Base64Data string
	Path       string
	URL        string
}

// NewDefaultStaticConfig creates a new default configuration for the auxiliary image provider
func NewDefaultStaticConfig() StaticConfig {
	return StaticConfig{
		Base64Data: "",
		Path:       "",
		URL:        "",
	}
}

// LoadWatermarkStaticConfigFromEnv loads the watermark configuration from the environment
func LoadWatermarkStaticConfigFromEnv(c *StaticConfig) (*StaticConfig, error) {
	c = ensure.Ensure(c, NewDefaultStaticConfig)

	IXOSS_WATERMARK_DATA.Parse(&c.Base64Data)
	IXOSS_WATERMARK_PATH.Parse(&c.Path)
	IXOSS_WATERMARK_URL.Parse(&c.URL)

	return c, nil
}

// LoadFallbackStaticConfigFromEnv loads the fallback configuration from the environment
func LoadFallbackStaticConfigFromEnv(c *StaticConfig) (*StaticConfig, error) {
	c = ensure.Ensure(c, NewDefaultStaticConfig)

	IXOSS_FALLBACK_IMAGE_DATA.Parse(&c.Base64Data)
	IXOSS_FALLBACK_IMAGE_PATH.Parse(&c.Path)
	IXOSS_FALLBACK_IMAGE_URL.Parse(&c.URL)

	return c, nil
}
