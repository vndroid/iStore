package processing

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"slices"

	"github.com/kane/istore/internal/imagedata"
	"github.com/kane/istore/internal/imageinfo"
	"github.com/kane/istore/internal/imagetype"
	"github.com/kane/istore/internal/options"
	"github.com/kane/istore/internal/timeout"
	"github.com/kane/istore/internal/vips"
)

// mainPipeline constructs the main image processing pipeline.
// This pipeline is applied to each image frame.
func (p *Processor) mainPipeline() Pipeline {
	return Pipeline{
		p.vectorGuardScale,
		p.trim,
		p.scaleOnLoad,
		p.colorspaceToProcessing,
		p.crop,
		p.scale,
		p.rotateAndFlip,
		p.cropToResult,
		p.rotateFree,
		p.applyFilters,
		p.adjust,
		p.extend,
		p.extendAspectRatio,
		p.padding,
		p.roundCorners,
		p.fixSize,
		p.flatten,
		p.watermark,
	}
}

// finalizePipeline constructs the finalization pipeline.
// This pipeline is applied before saving the image.
func (p *Processor) finalizePipeline() Pipeline {
	return Pipeline{
		p.colorspaceToResult,
		p.stripMetadata,
	}
}

// Result holds the result of image processing.
type Result struct {
	OutData      imagedata.ImageData
	OriginWidth  int
	OriginHeight int
	ResultWidth  int
	ResultHeight int
}

// ProcessImage processes the image according to the provided processing options
// and returns a [Result] that includes the processed image data and dimensions.
//
// The provided processing options may be modified during processing.
func (p *Processor) ProcessImage(
	ctx context.Context,
	imgdata imagedata.ImageData,
	o *options.Options,
) (*Result, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	defer vips.Cleanup()

	img := new(vips.Image)
	defer img.Clear()

	po := p.NewProcessingOptions(o)

	// Load a single page/frame of the image so we can analyze it
	// and decide how to process it further
	thumbnailLoaded, err := p.initialLoadImage(img, imgdata, po.EnforceThumbnail())
	if err != nil {
		return nil, err
	}

	// Let's check if we should skip standard processing
	if p.shouldSkipStandardProcessing(imgdata.Format(), po) {
		return p.skipStandardProcessing(img, imgdata, po)
	}

	// Check if we expect image to be processed as animated.
	// If MaxAnimationFrames is 1, we never process as animated since we can only
	// process a single frame.
	animated := po.MaxAnimationFrames() > 1 && img.IsAnimated()

	// The format the caller actually named, read before determineOutputFormat
	// overwrites the same key with what it resolved. Unknown here means the
	// caller named none — a bare transform, or format,auto — and that difference
	// is what checkAnimationIsReadable turns on.
	requestedFormat := po.Format()

	// Determine output format and check if it's supported.
	// The determined format is stored in po[KeyFormat].
	outFormat, err := p.determineOutputFormat(img, imgdata, po, animated)
	if err != nil {
		return nil, err
	}

	// Now, as we know the output format, we know for sure if the image
	// should be processed as animated
	animated = animated && outFormat.SupportsAnimationSave()

	// An animation libvips cannot see is not the same thing as a still.
	if !animated {
		if err := p.checkAnimationIsReadable(img, imgdata, po, requestedFormat); err != nil {
			return nil, err
		}
	}

	// Load required number of frames/pages for processing
	// and remove animation-related data if not animated.
	// Don't reload if we initially loaded a thumbnail.
	if !thumbnailLoaded {
		if err = p.reloadImageForProcessing(img, imgdata, po, animated); err != nil {
			return nil, err
		}
	}

	// Check image dimensions and number of frames for security reasons
	originWidth, originHeight, err := p.checkImageSize(img, imgdata.Format(), po)
	if err != nil {
		return nil, err
	}

	// Transform the image (resize, crop, etc)
	if err = p.transformImage(ctx, img, po, imgdata, animated); err != nil {
		return nil, err
	}

	// Finalize the image (colorspace conversion, metadata stripping, etc)
	if err = p.finalizePipeline().Run(ctx, img, po, imgdata); err != nil {
		return nil, err
	}

	outData, err := p.saveImage(ctx, img, po)
	if err != nil {
		return nil, err
	}

	resultWidth, resultHeight, _ := p.getImageSize(img)

	return &Result{
		OutData:      outData,
		OriginWidth:  originWidth,
		OriginHeight: originHeight,
		ResultWidth:  resultWidth,
		ResultHeight: resultHeight,
	}, nil
}

// initialLoadImage loads a single page/frame of the image.
// If the image format supports thumbnails and thumbnail loading is enforced,
// it tries to load the thumbnail first.
func (p *Processor) initialLoadImage(
	img *vips.Image,
	imgdata imagedata.ImageData,
	enforceThumbnail bool,
) (bool, error) {
	if enforceThumbnail && imgdata.Format().SupportsThumbnail() {
		if err := img.LoadThumbnail(imgdata); err == nil {
			return true, nil
		} else {
			slog.Debug(fmt.Sprintf("Can't load thumbnail: %s", err))
		}
	}

	return false, img.Load(imgdata, 1.0, 0, 1)
}

// checkAnimationIsReadable refuses a source whose container holds an animation
// this build of libvips cannot see, when the caller explicitly asked for a
// format that could have carried it.
//
// APNG is the case that exists today. iStore's own header walk reads the acTL
// chunk, so image/info reports the real frame count; libvips has no APNG decoder
// before 8.19, so it loads the default image and reports one page. The result
// was a silent flattening of exactly the shape the frame cap's refusal removed:
// ask for a three-frame APNG as WebP — a format that holds animation perfectly
// well — and get a well-formed one-frame WebP back, while image/info goes on
// saying three.
//
// The test is a comparison, not a list of formats and not a version check, so it
// needs no maintenance. On the day libvips reads APNG, Pages() returns three,
// the two numbers agree, and this stops firing by itself; any future container
// where iStore's parser sees more frames than libvips does is covered by the
// same line.
//
// Two things deliberately do not reach here:
//
//   - format,auto. There the caller delegated the choice of format, so choosing
//     one that happens to be a still is the server's to make — answering a
//     browser's Accept negotiation with 422 would be absurd.
//   - a request with no format at all, and any still format. Flattening is the
//     honest answer to "give me a JPEG", and it is the only possible answer for
//     PNG output on a build whose pngsave cannot write APNG either.
func (p *Processor) checkAnimationIsReadable(
	img *vips.Image,
	imgdata imagedata.ImageData,
	po ProcessingOptions,
	requestedFormat imagetype.Type,
) error {
	// requestedFormat is what the caller named, captured before
	// determineOutputFormat writes its own answer over the same key. Unknown is
	// what keeps format,auto and bare transforms out of here; reading po.Format()
	// at this point would instead see the *resolved* format and refuse both.
	if requestedFormat == imagetype.Unknown || !requestedFormat.SupportsAnimationSave() {
		return nil
	}
	if po.MaxAnimationFrames() <= 1 {
		// Configured to flatten everything. Still a valid way to run this, and
		// refusing would contradict it.
		return nil
	}
	if img.IsAnimated() {
		return nil // libvips can see the frames; nothing is being lost
	}

	claimed := containerFrameCount(imgdata)
	if claimed > img.Pages() {
		return newAnimationNotReadableError(imgdata.Format(), claimed)
	}

	return nil
}

// containerFrameCount asks iStore's own header walk how many frames the file
// claims, or 0 when it cannot say.
//
// The bytes are already in memory, so this is a parse and not a read. A count it
// reports as a floor rather than the truth — a GIF longer than the header
// window — is still safe to compare against: a floor can only be lower than the
// real number, so it cannot manufacture a disagreement that is not there.
func containerFrameCount(d imagedata.ImageData) int {
	size, err := d.Size()
	if err != nil {
		return 0
	}

	info, err := imageinfo.Read(d.Reader(), int64(size))
	if err != nil || info == nil {
		// A container imageinfo does not parse, or a header it could not finish.
		// Either way it has no claim to make.
		return 0
	}

	return info.FrameCount
}

// reloadImageForProcessing reloads the image for processing.
// For animated images, it loads every frame, having first checked there are not
// too many.
func (p *Processor) reloadImageForProcessing(
	img *vips.Image,
	imgdata imagedata.ImageData,
	po ProcessingOptions,
	asAnimated bool,
) error {
	// This was min(img.Pages(), po.MaxAnimationFrames()), which quietly produced
	// a shorter animation than the source: well-formed output, wrong length, and
	// nothing anywhere to say so. checkImageSize below cannot catch it either —
	// the frame count it multiplies by is the *loaded* one, so truncating first
	// makes its arithmetic pass every time. Hence the check here, before the
	// load, where the real count is still known.
	if asAnimated {
		pages := img.Pages()
		if err := p.securityChecker.CheckAnimationFrames(po.Options, pages); err != nil {
			return err
		}
		return img.Load(imgdata, 1.0, 0, pages)
	}

	// Otherwise, we just need to remove any animation-related data
	return img.RemoveAnimation()
}

// checkImageSize checks the image dimensions and number of frames against security options.
// It returns the image width, height and a security check error, if any.
func (p *Processor) checkImageSize(
	img *vips.Image,
	imgtype imagetype.Type,
	po ProcessingOptions,
) (int, int, error) {
	width, height, frames := p.getImageSize(img)

	if imgtype.IsVector() {
		// We don't check vector image dimensions as we can render it in any size
		return width, height, nil
	}

	err := p.securityChecker.CheckDimensions(po.Options, width, height, frames)

	return width, height, err
}

// getImageSize returns the width and height of the image, taking into account
// orientation and animation.
func (p *Processor) getImageSize(img *vips.Image) (int, int, int) {
	width, height := img.Width(), img.Height()
	frames := 1

	if img.IsAnimated() {
		// Animated images contain multiple frames, and libvips loads them stacked vertically.
		// We want to return the size of a single frame
		height = img.PageHeight()
		frames = img.PagesLoaded()
	}

	// If the image is rotated by 90 or 270 degrees, we need to swap width and height
	orientation := img.Orientation()
	if orientation == 5 || orientation == 6 || orientation == 7 || orientation == 8 {
		width, height = height, width
	}

	return width, height, frames
}

// Returns true if image should not be processed as usual
func (p *Processor) shouldSkipStandardProcessing(
	inFormat imagetype.Type,
	po ProcessingOptions,
) bool {
	outFormat := po.Format()
	skipProcessingFormatEnabled := po.ShouldSkipFormatProcessing(inFormat)

	if inFormat == imagetype.SVG {
		isOutUnknown := outFormat == imagetype.Unknown

		switch {
		case outFormat == imagetype.SVG:
			return true
		case isOutUnknown && !p.config.AlwaysRasterizeSvg:
			return true
		case isOutUnknown && p.config.AlwaysRasterizeSvg && skipProcessingFormatEnabled:
			return true
		default:
			return false
		}
	} else {
		return skipProcessingFormatEnabled && (inFormat == outFormat || outFormat == imagetype.Unknown)
	}
}

// skipStandardProcessing skips standard image processing and returns the original image data.
//
// SVG images may be sanitized if configured to do so.
func (p *Processor) skipStandardProcessing(
	img *vips.Image,
	imgdata imagedata.ImageData,
	po ProcessingOptions,
) (*Result, error) {
	// Even if we skip standard processing, we still need to check image dimensions
	// to not send an image bomb to the client
	originWidth, originHeight, err := p.checkImageSize(img, imgdata.Format(), po)
	if err != nil {
		return nil, err
	}

	// imgproxy sanitizes SVG here via its svg.Processor. iStore dropped that
	// path together with SVG detection: sanitizing needs imgproxy's xmlparser,
	// which needs golang.org/x/net/html/charset, and iStore does not rasterize
	// or serve vectors. Passing the bytes through unsanitized would be worse
	// than not supporting them, so refuse instead.
	if imgdata.Format() == imagetype.SVG {
		return nil, newUnsupportedFormatError(imgdata.Format())
	}

	processedData := imgdata.Ref()

	return &Result{
		OutData:      processedData,
		OriginWidth:  originWidth,
		OriginHeight: originHeight,
		ResultWidth:  originWidth,
		ResultHeight: originHeight,
	}, nil
}

// determineOutputFormat determines the output image format based on the processing options
// and image properties.
//
// It modifies the ProcessingOptions in place to set the output format.
func (p *Processor) determineOutputFormat(
	img *vips.Image,
	imgdata imagedata.ImageData,
	po ProcessingOptions,
	animated bool,
) (imagetype.Type, error) {
	// Check if the image may have transparency
	// circle and rounded-corners punch holes in an otherwise opaque image, so
	// they have to count here too: without them a `circle/format,auto` request
	// would be answered with JPEG and the corners flattened to the background.
	expectTransparency := !po.ShouldFlatten() &&
		(img.HasAlpha() || po.PaddingEnabled() || po.ExtendEnabled() ||
			po.CircleEnabled() || po.RoundedCornersEnabled() ||
			po.RotateFreeEnabled())

	format := po.Format()

	switch {
	case format == imagetype.SVG:
		// At this point we can't allow requested format to be SVG as we can't save SVGs
		return imagetype.Unknown, newSaveFormatError(format)
	case format == imagetype.Unknown:
		switch {
		case po.PreferJxl() && !animated:
			format = imagetype.JXL
		case po.PreferAvif() && !animated:
			format = imagetype.AVIF
		case po.PreferWebP():
			format = imagetype.WEBP
		case p.isImageTypePreferred(imgdata.Format()):
			format = imgdata.Format()
		default:
			format = p.findPreferredFormat(animated, expectTransparency)
		}
	case po.EnforceJxl() && !animated:
		format = imagetype.JXL
	case po.EnforceAvif() && !animated:
		format = imagetype.AVIF
	case po.EnforceWebP():
		format = imagetype.WEBP
	}

	po.SetFormat(format)

	if !vips.SupportsSave(format) {
		return format, newSaveFormatError(format)
	}

	return format, nil
}

// isImageTypePreferred checks if the given image type is in the list of preferred formats.
func (p *Processor) isImageTypePreferred(imgtype imagetype.Type) bool {
	return slices.Contains(p.config.PreferredFormats, imgtype)
}

// isImageTypeCompatible checks if the given image type is compatible with the image properties.
func (p *Processor) isImageTypeCompatible(
	imgtype imagetype.Type,
	animated, expectTransparency bool,
) bool {
	if animated && !imgtype.SupportsAnimationSave() {
		return false
	}

	if expectTransparency && !imgtype.SupportsAlpha() {
		return false
	}

	return true
}

// findPreferredFormat finds a suitable preferred format based on image's properties.
func (p *Processor) findPreferredFormat(animated, expectTransparency bool) imagetype.Type {
	for _, t := range p.config.PreferredFormats {
		if p.isImageTypeCompatible(t, animated, expectTransparency) {
			return t
		}
	}

	return p.config.PreferredFormats[0]
}

func (p *Processor) transformImage(
	ctx context.Context,
	img *vips.Image,
	po ProcessingOptions,
	imgdata imagedata.ImageData,
	asAnimated bool,
) error {
	if asAnimated {
		return p.transformAnimated(ctx, img, po)
	}

	return p.mainPipeline().Run(ctx, img, po, imgdata)
}

func (p *Processor) transformAnimated(
	ctx context.Context,
	img *vips.Image,
	po ProcessingOptions,
) error {
	if po.TrimEnabled() {
		slog.Warn("Trim is not supported for animated images")
		po.DisableTrim()
	}

	imgWidth := img.Width()
	frameHeight := img.PageHeight()
	framesCount := img.PagesLoaded()

	// Get frame delays. We'll need to set them back later.
	// If we don't have delay info, we'll set a default delay later.
	delay, err := img.GetIntSliceDefault("delay", nil)
	if err != nil {
		return err
	}

	// Get loop count. We'll need to set it back later.
	// 0 means infinite looping.
	loop, err := img.GetIntDefault("loop", 0)
	if err != nil {
		return err
	}

	// Disable watermarking for individual frames.
	// It's more efficient to apply watermark to all frames at once after they are processed.
	watermarkOpacity := po.WatermarkOpacity()
	if watermarkOpacity > 0 {
		po.DeleteWatermarkOpacity()
		defer func() { po.SetWatermarkOpacity(watermarkOpacity) }()
	}

	// Make a slice to hold processed frames and ensure they are cleared on function exit
	frames := make([]*vips.Image, 0, framesCount)
	defer func() {
		for _, frame := range frames {
			if frame != nil {
				frame.Clear()
			}
		}
	}()

	for i := range framesCount {
		frame := new(vips.Image)

		// Extract an individual frame from the image.
		// Libvips loads animated images as a single image with frames stacked vertically.
		if err = img.Extract(frame, 0, i*frameHeight, imgWidth, frameHeight); err != nil {
			return err
		}

		frames = append(frames, frame)

		// Transform the frame using the main pipeline.
		// We don't provide imgdata here to prevent scale-on-load.
		// Watermarking is disabled for individual frames (see above)
		if err = p.mainPipeline().Run(ctx, frame, po, nil); err != nil {
			return err
		}

		// If the frame was scaled down, it's better to copy it to RAM
		// to speed up further processing.
		if r, _ := frame.GetIntDefault("imgproxy-scaled-down", 0); r == 1 {
			if err = frame.CopyMemory(); err != nil {
				return err
			}

			if err = timeout.Check(ctx); err != nil {
				return err
			}
		}
	}

	// Join processed frames back into a single image.
	if err = img.Arrayjoin(frames); err != nil {
		return err
	}

	// Apply watermark to all frames at once if it was requested.
	// This is much more efficient than applying watermark to individual frames.
	if watermarkOpacity > 0 && p.watermarkProvider != nil {
		// Get DPR scale to apply watermark correctly on HiDPI images.
		// `imgproxy-dpr-scale` is set by the pipeline.
		dprScale, derr := img.GetDoubleDefault("imgproxy-dpr-scale", 1.0)
		if derr != nil {
			dprScale = 1.0
		}

		// Set watermark opacity back
		po.SetWatermarkOpacity(watermarkOpacity)

		if err = p.applyWatermark(ctx, img, po, dprScale, framesCount); err != nil {
			return err
		}
	}

	if len(delay) == 0 {
		// if we don't have delay info, set it to 40ms for each frame (25 FPS).
		delay = make([]int, framesCount)
		for i := range delay {
			delay[i] = 40
		}
	} else if len(delay) > framesCount {
		// if we have more delay entries than frames, truncate it.
		delay = delay[:framesCount]
	}

	// Mark the image as animated so img.Strip() doesn't remove animation data.
	img.SetInt("imgproxy-is-animated", 1)
	// Set animation data back.
	img.SetInt("page-height", frames[0].Height())
	img.SetIntSlice("delay", delay)
	img.SetInt("loop", loop)
	img.SetInt("n-pages", img.Height()/frames[0].Height())

	return nil
}

func (p *Processor) saveImage(
	ctx context.Context,
	img *vips.Image,
	po ProcessingOptions,
) (imagedata.ImageData, error) {
	outFormat := po.Format()

	// AVIF has a minimal dimension of 16 pixels.
	// If one of the dimensions is less, we need to switch to another format.
	if outFormat == imagetype.AVIF && (img.Width() < 16 || img.Height() < 16) {
		if img.HasAlpha() {
			outFormat = imagetype.PNG
		} else {
			outFormat = imagetype.JPEG
		}

		po.SetFormat(outFormat)

		slog.Warn(fmt.Sprintf(
			"Minimal dimension of AVIF is 16, current image size is %dx%d. Image will be saved as %s",
			img.Width(), img.Height(), outFormat,
		))
	}

	quality := po.Quality(outFormat)

	// If we want and can fit the image into the specified number of bytes,
	// let's do it.
	ov := po.saveOverrides()

	if maxBytes := po.MaxBytes(); maxBytes > 0 && outFormat.SupportsQuality() {
		return saveImageToFitBytes(ctx, img, outFormat, quality, maxBytes, po.Options, ov)
	}

	// Otherwise, just save the image with the specified quality.
	return img.Save(outFormat, quality, ov)
}
