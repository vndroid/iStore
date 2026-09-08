package security

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/kane/istore/internal/ensure"
	"github.com/kane/istore/internal/env"
)

var (
	ISTORE_ALLOWED_SOURCES    = env.URLPatterns("ISTORE_ALLOWED_SOURCES")
	ISTORE_KEY                = env.HexSlice("ISTORE_KEY")
	ISTORE_SALT               = env.HexSlice("ISTORE_SALT")
	ISTORE_SIGNATURE_SIZE     = env.Int("ISTORE_SIGNATURE_SIZE")
	ISTORE_TRUSTED_SIGNATURES = env.StringSlice("ISTORE_TRUSTED_SIGNATURES")

	ISTORE_MAX_SRC_RESOLUTION             = env.MegaInt("ISTORE_MAX_SRC_RESOLUTION")
	ISTORE_MAX_SRC_FILE_SIZE              = env.Int("ISTORE_MAX_SRC_FILE_SIZE")
	ISTORE_MAX_ANIMATION_FRAMES           = env.Int("ISTORE_MAX_ANIMATION_FRAMES")
	ISTORE_MAX_ANIMATION_FRAME_RESOLUTION = env.MegaInt("ISTORE_MAX_ANIMATION_FRAME_RESOLUTION")
	ISTORE_MAX_RESULT_DIMENSION           = env.Int("ISTORE_MAX_RESULT_DIMENSION")
)

// Config is the package-local configuration
type Config struct {
	AllowedSources    []*regexp.Regexp // List of allowed source URL patterns (empty = allow all)
	Keys              [][]byte         // List of the HMAC keys
	Salts             [][]byte         // List of the HMAC salts
	SignatureSize     int              // Size of the HMAC signature in bytes
	TrustedSignatures []string         // List of trusted signature sources

	MaxSrcResolution            int // Maximum allowed source image resolution
	MaxSrcFileSize              int // Maximum allowed source image file size in bytes
	MaxAnimationFrames          int // Maximum allowed animation frames
	MaxAnimationFrameResolution int // Maximum allowed resolution per animation frame
	MaxResultDimension          int // Maximum allowed result image dimension (width or height)
}

// DefaultMaxAnimationFrames is the frame cap, and the one place iStore departs
// from imgproxy's defaults on purpose.
//
// imgproxy ships 1, which means "never process anything as animated": a
// three-frame GIF asked for as WebP comes back as a still of its first frame,
// silently. That is a defensible default for a proxy pointed at the open
// internet. It is the wrong one here, because iStore's job is to answer the way
// OSS answers, and OSS keeps the animation through resize, crop and watermark.
//
// The number is not the real budget. That is CheckDimensions' width x height x
// frames against MaxSrcResolution — the same product OSS uses, and the reason
// raising this cap does not open a hole: 300 frames of 500x500 is 75 MP and is
// refused at 50 MP whatever this says. What this cap governs is the per-frame
// overhead a pixel count cannot see, which is why a small-but-endless animation
// still needs a limit of its own. 300 clears essentially every real animation —
// most are under 100 frames, and a long screen recording runs to a few hundred.
const DefaultMaxAnimationFrames = 300

// NewDefaultConfig returns a new Config instance with default values.
func NewDefaultConfig() Config {
	return Config{
		SignatureSize: 32,

		MaxSrcResolution:            50_000_000,
		MaxSrcFileSize:              0,
		MaxAnimationFrames:          DefaultMaxAnimationFrames,
		MaxAnimationFrameResolution: 0,
		MaxResultDimension:          0,
	}
}

// LoadConfigFromEnv overrides configuration variables from environment
func LoadConfigFromEnv(c *Config) (*Config, error) {
	c = ensure.Ensure(c, NewDefaultConfig)

	err := errors.Join(
		ISTORE_ALLOWED_SOURCES.Parse(&c.AllowedSources),
		ISTORE_SIGNATURE_SIZE.Parse(&c.SignatureSize),
		ISTORE_TRUSTED_SIGNATURES.Parse(&c.TrustedSignatures),

		ISTORE_MAX_SRC_RESOLUTION.Parse(&c.MaxSrcResolution),
		ISTORE_MAX_SRC_FILE_SIZE.Parse(&c.MaxSrcFileSize),
		ISTORE_MAX_ANIMATION_FRAMES.Parse(&c.MaxAnimationFrames),
		ISTORE_MAX_ANIMATION_FRAME_RESOLUTION.Parse(&c.MaxAnimationFrameResolution),
		ISTORE_MAX_RESULT_DIMENSION.Parse(&c.MaxResultDimension),

		ISTORE_KEY.Parse(&c.Keys),
		ISTORE_SALT.Parse(&c.Salts),
	)

	return c, err
}

// Validate validates the configuration
func (c *Config) Validate() error {
	if c.MaxSrcResolution <= 0 {
		return ISTORE_MAX_SRC_RESOLUTION.ErrorZeroOrNegative()
	}

	if c.MaxSrcFileSize < 0 {
		return ISTORE_MAX_SRC_FILE_SIZE.ErrorNegative()
	}

	if c.MaxAnimationFrames <= 0 {
		return ISTORE_MAX_ANIMATION_FRAMES.ErrorZeroOrNegative()
	}

	if len(c.Keys) != len(c.Salts) {
		return fmt.Errorf(
			"number of keys and number of salts should be equal. Keys: %d, salts: %d",
			len(c.Keys), len(c.Salts),
		)
	}

	if len(c.Keys) == 0 {
		ISTORE_KEY.Warn("No keys defined, signature checking is disabled")
	}

	if len(c.Salts) == 0 {
		ISTORE_SALT.Warn("No salts defined, signature checking is disabled")
	}

	if c.SignatureSize < 1 || c.SignatureSize > 32 {
		return ISTORE_SIGNATURE_SIZE.Errorf("invalid size")
	}

	return nil
}
