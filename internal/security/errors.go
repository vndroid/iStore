package security

import (
	"context"
	"fmt"
	"net/http"

	"github.com/kane/istore/internal/errctx"
)

type (
	SignatureError       struct{ *errctx.TextError }
	ImageResolutionError struct{ *errctx.TextError }
	AnimationFramesError struct{ *errctx.TextError }
	SourceURLError       struct{ *errctx.TextError }
)

// newAnimationFramesError reports a source animation longer than the cap.
//
// The public message carries both numbers, unlike its resolution sibling which
// says only "Invalid source image". Nothing is given away by that: the frame
// count is exactly what `image/info` returns for the same object, and the caller
// cannot act on "invalid" — they can act on "this is 500 frames and the limit is
// 300".
func newAnimationFramesError(frames, limit int) error {
	msg := fmt.Sprintf("Source animation has %d frames, limit is %d", frames, limit)

	return AnimationFramesError{errctx.NewTextError(
		msg,
		1,
		errctx.WithStatusCode(http.StatusUnprocessableEntity),
		errctx.WithPublicMessage(msg),
		errctx.WithShouldReport(false),
	)}
}

func newSignatureError(msg string) error {
	return SignatureError{errctx.NewTextError(
		msg,
		1,
		errctx.WithStatusCode(http.StatusForbidden),
		errctx.WithPublicMessage("Forbidden"),
		errctx.WithShouldReport(false),
		errctx.WithDocsURL("https://docs.imgproxy.net/usage/signing_url"),
	)}
}

func newMalformedSignatureError(ctx context.Context) error {
	msg := "The signature appears to be a processing option. The signature section should always be present in the URL."

	return SignatureError{errctx.NewTextError(
		msg,
		1,
		errctx.WithStatusCode(http.StatusForbidden),
		errctx.WithPublicMessage(msg),
		errctx.WithShouldReport(false),
		errctx.WithDocsURL(errctx.DocsBaseURL(ctx, "https://docs.imgproxy.net/usage/processing")),
	)}
}

func newImageResolutionError(msg string) error {
	return ImageResolutionError{errctx.NewTextError(
		msg,
		1,
		errctx.WithStatusCode(http.StatusUnprocessableEntity),
		errctx.WithPublicMessage("Invalid source image"),
		errctx.WithShouldReport(false),
		errctx.WithDocsURL("https://docs.imgproxy.net/configuration/options#security"),
	)}
}

func newSourceURLError(imageURL string) error {
	return SourceURLError{errctx.NewTextError(
		fmt.Sprintf("Source URL is not allowed: %s", imageURL),
		1,
		errctx.WithStatusCode(http.StatusNotFound),
		errctx.WithPublicMessage("Invalid source URL"),
		errctx.WithShouldReport(false),
		errctx.WithDocsURL("https://docs.imgproxy.net/configuration/options#ISTORE_ALLOWED_SOURCES"),
	)}
}
