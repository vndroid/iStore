package processing

import (
	"fmt"
	"net/http"

	"github.com/kane/istore/internal/errctx"
	"github.com/kane/istore/internal/imagetype"
)

type (
	SaveFormatError struct{ *errctx.TextError }
)

func newSaveFormatError(format imagetype.Type) error {
	return SaveFormatError{errctx.NewTextError(
		fmt.Sprintf("Can't save %s, probably not supported by your libvips", format),
		1,
		errctx.WithStatusCode(http.StatusUnprocessableEntity),
		errctx.WithPublicMessage("Invalid URL"),
		errctx.WithShouldReport(false),
	)}
}

type (
	AnimationNotReadableError struct{ *errctx.TextError }
)

// newAnimationNotReadableError reports a source whose animation this build of
// libvips cannot decode, asked for in a format that could have carried it.
//
// The message names the format and the frame count for the same reason the
// frame-cap refusal does: image/info already reports that count for the same
// object, so nothing is disclosed by repeating it, and "invalid image" would
// leave the caller with no idea that asking for a still format works fine.
func newAnimationNotReadableError(format imagetype.Type, frames int) error {
	msg := fmt.Sprintf(
		"This build of libvips cannot read %s animation; the source has %d frames. "+
			"Request a still format instead.",
		format, frames,
	)

	return AnimationNotReadableError{errctx.NewTextError(
		msg,
		1,
		errctx.WithStatusCode(http.StatusUnprocessableEntity),
		errctx.WithPublicMessage(msg),
		errctx.WithShouldReport(false),
	)}
}

type (
	UnsupportedFormatError struct{ *errctx.TextError }
)

// newUnsupportedFormatError reports an input format iStore deliberately does not
// handle. Today that is only SVG: imgproxy sanitizes it with its xmlparser, and
// iStore dropped that package along with vector support.
func newUnsupportedFormatError(format imagetype.Type) error {
	return UnsupportedFormatError{errctx.NewTextError(
		fmt.Sprintf("%s is not supported", format),
		1,
		errctx.WithStatusCode(http.StatusUnprocessableEntity),
		errctx.WithPublicMessage("Unsupported image format"),
		errctx.WithShouldReport(false),
	)}
}
