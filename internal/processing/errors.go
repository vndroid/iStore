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
