package processing

// adjust applies the brightness and contrast options.
//
// Both are linear transforms of the colour channels, so they are folded into a
// single pass: contrast scales around mid-grey, brightness shifts afterwards.
//
//	out = in*c + 127.5*(1 - c) + b
//
// Doing them separately would clip twice — a dark image pushed through
// `contrast,-100` and then `bright,50` would lose the shadow detail that the
// combined form keeps, because the intermediate never lands in a uchar buffer.
//
// This step is an iStore addition; imgproxy has no brightness or contrast
// option.
func (p *Processor) adjust(c *Context) error {
	brightness := c.PO.Brightness()
	contrast := c.PO.Contrast()

	if brightness == 0 && contrast == 1 {
		return nil
	}

	// The 127.5 pivot assumes 8-bit sRGB, which is also what applyFilters
	// assumes; converting here makes the step independent of whether that one
	// ran.
	if err := c.Img.RgbColourspace(); err != nil {
		return err
	}

	return c.Img.Linear(contrast, 127.5*(1-contrast)+brightness)
}
