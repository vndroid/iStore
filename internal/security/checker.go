package security

import (
	"github.com/vndroid/istore/internal/options"
	"github.com/vndroid/istore/internal/options/keys"
)

// Checker represents the security package instance
type Checker struct {
	config *Config
}

// New creates a new Security instance
func New(config *Config) (*Checker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	return &Checker{
		config: config,
	}, nil
}

// MaxSrcResolution returns the maximum allowed source image resolution
func (s *Checker) MaxSrcResolution(o *options.Options) int {
	return o.GetInt(keys.MaxSrcResolution, s.config.MaxSrcResolution)
}

// MaxSrcFileSize returns the maximum allowed source file size
func (s *Checker) MaxSrcFileSize(o *options.Options) int {
	return o.GetInt(keys.MaxSrcFileSize, s.config.MaxSrcFileSize)
}

// MaxAnimationFrames returns the maximum allowed animation frames
func (s *Checker) MaxAnimationFrames(o *options.Options) int {
	return o.GetInt(keys.MaxAnimationFrames, s.config.MaxAnimationFrames)
}

// MaxAnimationFrameResolution returns the maximum allowed animation frame resolution
func (s *Checker) MaxAnimationFrameResolution(o *options.Options) int {
	return o.GetInt(
		keys.MaxAnimationFrameResolution,
		s.config.MaxAnimationFrameResolution,
	)
}

// MaxResultDimension returns the maximum allowed result image dimension
func (s *Checker) MaxResultDimension(o *options.Options) int {
	return o.GetInt(keys.MaxResultDimension, s.config.MaxResultDimension)
}

// CheckAnimationFrames refuses a source with more frames than the cap allows.
//
// Separate from CheckDimensions for two reasons. It has to run *before* the
// frames are loaded, because the alternative is what this replaced: the pipeline
// loaded the first MaxAnimationFrames of them and carried on, so a 500-frame GIF
// came back as a well-formed 300-frame animation with nothing to say it had been
// cut. And the pixel check cannot catch that, because the frame count it
// multiplies by is the *loaded* one — truncating first makes the product check
// pass trivially, every time.
//
// Refusing is what OSS does with a source past its limits, and it is the only
// answer consistent with the info endpoint, which reports the source's real
// frame count. Two endpoints disagreeing about how long an animation is would be
// worse than either answer on its own.
//
// Only animated *output* gets here. The same GIF asked for as JPEG or AVIF drops
// its animation on a different path and is unaffected.
func (s *Checker) CheckAnimationFrames(o *options.Options, frames int) error {
	if limit := s.MaxAnimationFrames(o); frames > limit {
		return newAnimationFramesError(frames, limit)
	}
	return nil
}

// CheckDimensions checks if the given dimensions are within the allowed limits
func (s *Checker) CheckDimensions(o *options.Options, width, height, frames int) error {
	frames = max(frames, 1)

	maxFrameRes := s.MaxAnimationFrameResolution(o)

	if frames > 1 && maxFrameRes > 0 {
		if width*height > maxFrameRes {
			return newImageResolutionError("Source image frame resolution is too big")
		}
		return nil
	}

	if width*height*frames > s.MaxSrcResolution(o) {
		return newImageResolutionError("Source image resolution is too big")
	}

	return nil
}
