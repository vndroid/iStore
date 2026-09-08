package auximageprovider

import (
	"github.com/vndroid/istore/internal/ensure"
	"github.com/vndroid/istore/internal/env"
)

var (
	ISTORE_WATERMARK_DATA = env.String("ISTORE_WATERMARK_DATA")
	ISTORE_WATERMARK_PATH = env.ExistingFilePath("ISTORE_WATERMARK_PATH")
	ISTORE_WATERMARK_URL  = env.String("ISTORE_WATERMARK_URL")

	ISTORE_FALLBACK_IMAGE_DATA = env.String("ISTORE_FALLBACK_IMAGE_DATA")
	ISTORE_FALLBACK_IMAGE_PATH = env.ExistingFilePath("ISTORE_FALLBACK_IMAGE_PATH")
	ISTORE_FALLBACK_IMAGE_URL  = env.String("ISTORE_FALLBACK_IMAGE_URL")
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

	ISTORE_WATERMARK_DATA.Parse(&c.Base64Data)
	ISTORE_WATERMARK_PATH.Parse(&c.Path)
	ISTORE_WATERMARK_URL.Parse(&c.URL)

	return c, nil
}

// LoadFallbackStaticConfigFromEnv loads the fallback configuration from the environment
func LoadFallbackStaticConfigFromEnv(c *StaticConfig) (*StaticConfig, error) {
	c = ensure.Ensure(c, NewDefaultStaticConfig)

	ISTORE_FALLBACK_IMAGE_DATA.Parse(&c.Base64Data)
	ISTORE_FALLBACK_IMAGE_PATH.Parse(&c.Path)
	ISTORE_FALLBACK_IMAGE_URL.Parse(&c.URL)

	return c, nil
}
