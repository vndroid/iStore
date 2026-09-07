package processing

// roundCorners applies the `circle` and `rounded-corners` options, both of which
// end up as an alpha mask over the finished image.
//
// The two differ only in whether the image is squared first: OSS's `circle`
// returns a (2r+1)-square containing an inscribed circle, while
// `rounded-corners` keeps the frame and only rounds it.
//
// This step is an iStore addition; imgproxy has neither option.
func (p *Processor) roundCorners(c *Context) error {
	circle := c.PO.CircleEnabled()
	rounded := c.PO.RoundedCornersEnabled()

	if !circle && !rounded {
		return nil
	}

	w, h := c.Img.Width(), c.Img.Height()
	if c.Img.IsAnimated() {
		// Animated images are processed frame by frame, but the guard costs
		// nothing and keeps a stacked strip from being masked as one tall image
		// if that ever changes.
		h = c.Img.PageHeight()
	}

	// The mask is built in 8-bit sRGB, so the alpha it produces matches the
	// colour bands it is joined to.
	if err := c.Img.RgbColourspace(); err != nil {
		return err
	}

	var radius int

	if circle {
		// OSS clamps a too-large radius to the largest inscribed circle and
		// returns a (2r+1)-square, so an odd-sized result is expected even from
		// an even-sized source.
		radius = min(c.PO.CircleRadius(), (min(w, h)-1)/2)
		if radius < 1 {
			return nil
		}

		size := 2*radius + 1
		if err := c.Img.Crop((w-size)/2, (h-size)/2, size, size); err != nil {
			return err
		}
	} else {
		// A radius of half the shorter side is a full arc on that axis; beyond
		// that the shape stops changing, so OSS clamps there too.
		radius = min(c.PO.RoundedCornersRadius(), min(w, h)/2)
		if radius < 1 {
			return nil
		}
	}

	// The mask walks the whole image; a sequential source behind it would be
	// read out of order.
	if err := c.Img.CopyMemory(); err != nil {
		return err
	}

	return c.Img.RoundCorners(float64(radius))
}
