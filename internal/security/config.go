package security

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/vndroid/iXoSS/internal/ensure"
	"github.com/vndroid/iXoSS/internal/env"
)

var (
	IXOSS_ALLOWED_SOURCES    = env.URLPatterns("IXOSS_ALLOWED_SOURCES")
	IXOSS_KEY                = env.HexSlice("IXOSS_KEY")
	IXOSS_SALT               = env.HexSlice("IXOSS_SALT")
	IXOSS_SIGNATURE_SIZE     = env.Int("IXOSS_SIGNATURE_SIZE")
	IXOSS_TRUSTED_SIGNATURES = env.StringSlice("IXOSS_TRUSTED_SIGNATURES")

	IXOSS_MAX_SRC_RESOLUTION             = env.MegaInt("IXOSS_MAX_SRC_RESOLUTION")
	IXOSS_MAX_SRC_FILE_SIZE              = env.Int("IXOSS_MAX_SRC_FILE_SIZE")
	IXOSS_MAX_ANIMATION_FRAMES           = env.Int("IXOSS_MAX_ANIMATION_FRAMES")
	IXOSS_MAX_ANIMATION_FRAME_RESOLUTION = env.MegaInt("IXOSS_MAX_ANIMATION_FRAME_RESOLUTION")
	IXOSS_MAX_RESULT_DIMENSION           = env.Int("IXOSS_MAX_RESULT_DIMENSION")
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

// DefaultMaxAnimationFrames is the frame cap, and the one place iXoSS departs
// from imgproxy's defaults on purpose.
//
// imgproxy ships 1, which means "never process anything as animated": a
// three-frame GIF asked for as WebP comes back as a still of its first frame,
// silently. That is a defensible default for a proxy pointed at the open
// internet. It is the wrong one here, because iXoSS's job is to answer the way
// OSS answers, and OSS keeps the animation through resize, crop and watermark.
//
// The number is not the real budget. That is CheckDimensions' width x height x
// frames against MaxSrcResolution — the same product OSS uses, and the reason
// raising this cap does not open a hole of its own: at 300 frames the pixel
// budget refuses anything past roughly 912x912 per frame whatever this says.
// What this cap governs is the per-frame overhead a pixel count cannot see,
// which is why a small-but-endless animation still needs a limit. 300 clears
// essentially every real animation — most are under 100 frames, and a long
// screen recording runs to a few hundred.
const DefaultMaxAnimationFrames = 300

// DefaultMaxSrcResolution is the pixel budget: width x height x frames, the same
// product OSS bounds, at the same number OSS bounds it.
//
// Matching OSS is the whole reason for the value, and it is worth being explicit
// about what it costs, because the answer depends entirely on what the request
// asks for. Measured on a 17000x14000 JPEG (238 MP) with IXOSS_CONCURRENCY=1,
// peak RSS over the request:
//
//	resize,w_200    58 MiB   shrink-on-load; the full frame never exists
//	format,jpg     787 MiB   full-size transcode
//	format,webp   1143 MiB   full-size transcode
//
// So a thumbnailing workload sits near nothing and a full-size transcode near
// the ceiling costs about a gigabyte, multiplied by IXOSS_CONCURRENCY.
// IXOSS_MAX_SOURCE_BYTES (100 MiB) bounds the file but not the pixel count —
// a 238 MP JPEG of a flat colour is under 4 MiB — so it is not a substitute for
// sizing the box.
//
// Lower it if the deployment does not need OSS's ceiling; 50_000_000 was the
// previous default and is a comfortable fit for a small instance.
const DefaultMaxSrcResolution = 250_000_000

// NewDefaultConfig returns a new Config instance with default values.
func NewDefaultConfig() Config {
	return Config{
		SignatureSize: 32,

		MaxSrcResolution:            DefaultMaxSrcResolution,
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
		IXOSS_ALLOWED_SOURCES.Parse(&c.AllowedSources),
		IXOSS_SIGNATURE_SIZE.Parse(&c.SignatureSize),
		IXOSS_TRUSTED_SIGNATURES.Parse(&c.TrustedSignatures),

		IXOSS_MAX_SRC_RESOLUTION.Parse(&c.MaxSrcResolution),
		IXOSS_MAX_SRC_FILE_SIZE.Parse(&c.MaxSrcFileSize),
		IXOSS_MAX_ANIMATION_FRAMES.Parse(&c.MaxAnimationFrames),
		IXOSS_MAX_ANIMATION_FRAME_RESOLUTION.Parse(&c.MaxAnimationFrameResolution),
		IXOSS_MAX_RESULT_DIMENSION.Parse(&c.MaxResultDimension),

		IXOSS_KEY.Parse(&c.Keys),
		IXOSS_SALT.Parse(&c.Salts),
	)

	return c, err
}

// Validate validates the configuration
func (c *Config) Validate() error {
	if c.MaxSrcResolution <= 0 {
		return IXOSS_MAX_SRC_RESOLUTION.ErrorZeroOrNegative()
	}

	if c.MaxSrcFileSize < 0 {
		return IXOSS_MAX_SRC_FILE_SIZE.ErrorNegative()
	}

	if c.MaxAnimationFrames <= 0 {
		return IXOSS_MAX_ANIMATION_FRAMES.ErrorZeroOrNegative()
	}

	if len(c.Keys) != len(c.Salts) {
		return fmt.Errorf(
			"number of keys and number of salts should be equal. Keys: %d, salts: %d",
			len(c.Keys), len(c.Salts),
		)
	}

	if len(c.Keys) == 0 {
		IXOSS_KEY.Warn("No keys defined, signature checking is disabled")
	}

	if len(c.Salts) == 0 {
		IXOSS_SALT.Warn("No salts defined, signature checking is disabled")
	}

	if c.SignatureSize < 1 || c.SignatureSize > 32 {
		return IXOSS_SIGNATURE_SIZE.Errorf("invalid size")
	}

	return nil
}
